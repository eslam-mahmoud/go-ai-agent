package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/eslam-mahmoud/go-ai-agent/internal/claude"
	"github.com/eslam-mahmoud/go-ai-agent/internal/config"
	githubclient "github.com/eslam-mahmoud/go-ai-agent/internal/github"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
	"github.com/eslam-mahmoud/go-ai-agent/internal/telegram"
)

type Loop struct {
	cfg      *config.Config
	gh       githubclient.Client
	claude   claude.Runner
	telegram telegram.Gateway
	store    *store.Store
	log      *slog.Logger
}

func New(
	cfg *config.Config,
	gh githubclient.Client,
	runner claude.Runner,
	tg telegram.Gateway,
	s *store.Store,
	log *slog.Logger,
) *Loop {
	return &Loop{
		cfg:      cfg,
		gh:       gh,
		claude:   runner,
		telegram: tg,
		store:    s,
		log:      log,
	}
}

// Run starts the poll loop and blocks until ctx is cancelled.
func (l *Loop) Run(ctx context.Context) error {
	l.log.Info("madar starting", "poll_interval", l.cfg.PollInterval)
	ticker := time.NewTicker(l.cfg.PollInterval)
	defer ticker.Stop()

	// Run immediately on start, then on each tick.
	if err := l.tick(ctx); err != nil {
		l.log.Error("tick error", "err", err)
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := l.tick(ctx); err != nil {
				l.log.Error("tick error", "err", err)
			}
		}
	}
}

func (l *Loop) tick(ctx context.Context) error {
	// 1. Concurrency guard.
	active, err := l.store.CountActive()
	if err != nil {
		return fmt.Errorf("count active: %w", err)
	}
	maxParallel := 1
	if l.cfg.Concurrency.Enabled {
		maxParallel = l.cfg.Concurrency.MaxParallel
	}
	if active >= maxParallel {
		l.log.Debug("at capacity, skipping poll", "active", active, "max", maxParallel)
	}

	// 2. Check CI-pending tasks.
	if err := l.checkCIPending(ctx); err != nil {
		l.log.Error("CI pending check failed", "err", err)
	}

	// 3. Check awaiting-feedback issues for human replies.
	if err := l.checkAwaitingFeedback(ctx); err != nil {
		l.log.Error("awaiting-feedback check failed", "err", err)
	}

	if active >= maxParallel {
		return nil
	}

	// 4. Pick next ready task.
	return l.pickAndRun(ctx)
}

func (l *Loop) checkAwaitingFeedback(ctx context.Context) error {
	waiting, err := l.store.ListByState(store.StateAwaitingFeedback)
	if err != nil {
		return err
	}
	for _, task := range waiting {
		owner, repo, err := githubclient.SplitRepo(task.Repo)
		if err != nil {
			continue
		}
		if err := l.resumeIfReplied(ctx, owner, repo, task); err != nil {
			l.log.Error("resume check failed", "repo", task.Repo, "issue", task.IssueNumber, "err", err)
		}
	}
	return nil
}

func (l *Loop) resumeIfReplied(ctx context.Context, owner, repo string, task *store.Task) error {
	if task.LastClarificationAt == nil {
		return nil
	}

	// Look for a human comment after our clarification timestamp.
	since := task.LastClarificationAt.Add(time.Second)
	comments, err := l.gh.GetComments(ctx, owner, repo, task.IssueNumber, &since)
	if err != nil {
		return err
	}

	// Find first comment not from a bot / our own agent.
	var humanReply string
	for _, c := range comments {
		if c.Author != "" && !isAgentComment(c.Body) {
			humanReply = c.Body
			break
		}
	}
	if humanReply == "" {
		return nil
	}

	l.log.Info("human replied, resuming task", "repo", task.Repo, "issue", task.IssueNumber)
	_ = l.store.Log(task.Repo, task.IssueNumber, "resume", "human replied")

	// Transition back to in-progress.
	if _, err := l.store.UpsertTask(task.Repo, task.IssueNumber, store.StateInProgress, ""); err != nil {
		return err
	}
	issue, err := l.gh.GetIssue(ctx, owner, repo, task.IssueNumber)
	if err != nil {
		return err
	}
	if err := l.transitionLabels(ctx, owner, repo, task.IssueNumber, issue.Labels,
		l.cfg.Labels.AwaitingFeedback, l.cfg.Labels.InProgress); err != nil {
		return err
	}

	// Resume Claude session with the human reply.
	prompt := claude.BuildResumePrompt(humanReply)
	return l.runClaude(ctx, owner, repo, task.IssueNumber, issue, task.SessionID, prompt, true)
}

func (l *Loop) pickAndRun(ctx context.Context) error {
	for _, fullRepo := range l.cfg.Repos {
		owner, repo, err := githubclient.SplitRepo(fullRepo)
		if err != nil {
			l.log.Warn("invalid repo", "repo", fullRepo)
			continue
		}

		issues, err := l.gh.ListReadyIssues(ctx, owner, repo, l.cfg.Labels.Ready)
		if err != nil {
			l.log.Error("list issues", "repo", fullRepo, "err", err)
			continue
		}
		if len(issues) == 0 {
			continue
		}

		issue := issues[0] // top of list wins
		l.log.Info("claiming issue", "repo", fullRepo, "issue", issue.Number, "title", issue.Title)

		sessionID := uuid.New().String()
		if _, err := l.store.UpsertTask(fullRepo, issue.Number, store.StateInProgress, sessionID); err != nil {
			return fmt.Errorf("claim task in store: %w", err)
		}
		_ = l.store.Log(fullRepo, issue.Number, "claimed", fmt.Sprintf("session=%s", sessionID))

		if err := l.transitionLabels(ctx, owner, repo, issue.Number, issue.Labels,
			l.cfg.Labels.Ready, l.cfg.Labels.InProgress); err != nil {
			l.log.Error("transition labels", "err", err)
		}

		// Build first-run prompt from issue + thread.
		comments, _ := l.gh.GetComments(ctx, owner, repo, issue.Number, nil)
		threadStr := formatThread(comments)
		prompt := claude.BuildFirstRunPrompt(issue.Title, issue.Body, threadStr)

		return l.runClaude(ctx, owner, repo, issue.Number, issue, sessionID, prompt, false)
	}
	return nil
}

func (l *Loop) runClaude(ctx context.Context, owner, repo string, issueNumber int, issue *githubclient.Issue, sessionID, prompt string, isResume bool) error {
	workDir := filepath.Join(l.cfg.WorkspaceDir, owner, repo)

	opts := claude.RunOptions{
		WorkDir:         workDir,
		Prompt:          prompt,
		MaxTurns:        l.cfg.Claude.MaxTurns,
		Timeout:         l.cfg.Claude.RunTimeout,
		SkipPermissions: l.cfg.Claude.SkipPermissions,
	}
	if isResume {
		opts.ResumeID = sessionID
	} else {
		opts.SessionID = sessionID
	}

	fullRepo := owner + "/" + repo
	l.log.Info("running claude", "repo", fullRepo, "issue", issueNumber, "session", sessionID, "resume", isResume)

	result, err := l.claude.Run(ctx, opts)
	if err != nil {
		l.log.Error("claude run failed", "err", err)
		_ = l.store.Log(fullRepo, issueNumber, "error", err.Error())
		_ = l.telegram.NotifyError(ctx, issue.HTMLURL, err)
		return err
	}

	// Capture session ID from stream if available (first run).
	if result.SessionID != "" && !isResume {
		_ = l.store.SetSessionID(fullRepo, issueNumber, result.SessionID)
		sessionID = result.SessionID
	}

	if result.NeedsInput {
		return l.handleClarification(ctx, owner, repo, fullRepo, issueNumber, issue, result.Question)
	}

	if result.IsError {
		errMsg := fmt.Errorf("claude reported an error: %s", result.Output)
		_ = l.store.Log(fullRepo, issueNumber, "error", result.Output)
		_ = l.telegram.NotifyError(ctx, issue.HTMLURL, errMsg)
		return errMsg
	}

	return l.handleCompletion(ctx, owner, repo, fullRepo, issueNumber, issue, result.Output)
}

func (l *Loop) handleClarification(ctx context.Context, owner, repo, fullRepo string, issueNumber int, issue *githubclient.Issue, question string) error {
	l.log.Info("claude needs clarification", "repo", fullRepo, "issue", issueNumber)

	commentBody := fmt.Sprintf("🤔 **Madar needs your input before continuing:**\n\n%s", question)
	comment, err := l.gh.PostComment(ctx, owner, repo, issueNumber, commentBody)
	if err != nil {
		return fmt.Errorf("post clarification comment: %w", err)
	}

	now := time.Now().UTC()
	_ = l.store.SetClarificationTime(fullRepo, issueNumber, now)
	if _, err := l.store.UpsertTask(fullRepo, issueNumber, store.StateAwaitingFeedback, ""); err != nil {
		return err
	}
	_ = l.store.Log(fullRepo, issueNumber, "awaiting_feedback", question)

	commentURL := issue.HTMLURL
	if comment != nil {
		commentURL = comment.HTMLURL
	}
	issueLabels, _ := l.getIssueLabels(ctx, owner, repo, issueNumber)
	if err := l.transitionLabels(ctx, owner, repo, issueNumber, issueLabels,
		l.cfg.Labels.InProgress, l.cfg.Labels.AwaitingFeedback); err != nil {
		l.log.Warn("label transition failed", "err", err)
	}

	_ = l.telegram.NotifyClarification(ctx, commentURL, question)
	return nil
}

func (l *Loop) handleCompletion(ctx context.Context, owner, repo, fullRepo string, issueNumber int, issue *githubclient.Issue, output string) error {
	l.log.Info("task completed", "repo", fullRepo, "issue", issueNumber)

	// If CI is enabled and Claude opened a PR, hand off to CI watching instead of finalizing now.
	if l.cfg.CI.Enabled {
		if prNumber := extractPRNumber(output); prNumber > 0 {
			l.log.Info("PR detected, starting CI watch", "repo", fullRepo, "issue", issueNumber, "pr", prNumber)
			commentBody := fmt.Sprintf(
				"🔄 **Madar opened PR #%d.** Waiting for CI checks before finalizing...\n\n%s",
				prNumber, truncate(output, 3000),
			)
			if _, err := l.gh.PostComment(ctx, owner, repo, issueNumber, commentBody); err != nil {
				l.log.Warn("post CI-watch comment failed", "err", err)
			}
			return l.StartCIWatch(ctx, fullRepo, issueNumber, prNumber)
		}
	}

	summary := output
	if len(summary) > 4000 {
		summary = summary[:4000] + "\n\n_[truncated]_"
	}
	commentBody := fmt.Sprintf("✅ **Madar completed this task:**\n\n%s", summary)
	if _, err := l.gh.PostComment(ctx, owner, repo, issueNumber, commentBody); err != nil {
		l.log.Warn("post completion comment failed", "err", err)
	}

	issueLabels, _ := l.getIssueLabels(ctx, owner, repo, issueNumber)
	if err := l.transitionLabels(ctx, owner, repo, issueNumber, issueLabels,
		l.cfg.Labels.InProgress, l.cfg.Labels.Done); err != nil {
		l.log.Warn("label transition to done failed", "err", err)
	}

	if _, err := l.store.UpsertTask(fullRepo, issueNumber, store.StateDone, ""); err != nil {
		return err
	}
	if err := l.gh.CloseIssue(ctx, owner, repo, issueNumber); err != nil {
		l.log.Warn("close issue failed", "issue", issueNumber, "err", err)
	}
	_ = l.store.Log(fullRepo, issueNumber, "done", "")
	_ = l.telegram.NotifyCompletion(ctx, issue.HTMLURL, summary)
	return nil
}

// transitionLabels removes fromLabel and adds toLabel on the issue.
func (l *Loop) transitionLabels(ctx context.Context, owner, repo string, issueNumber int, currentLabels []string, fromLabel, toLabel string) error {
	var newLabels []string
	for _, lbl := range currentLabels {
		if lbl != fromLabel {
			newLabels = append(newLabels, lbl)
		}
	}
	newLabels = append(newLabels, toLabel)
	return l.gh.ReplaceLabels(ctx, owner, repo, issueNumber, newLabels)
}

func (l *Loop) getIssueLabels(ctx context.Context, owner, repo string, issueNumber int) ([]string, error) {
	issue, err := l.gh.GetIssue(ctx, owner, repo, issueNumber)
	if err != nil {
		return nil, err
	}
	return issue.Labels, nil
}

func formatThread(comments []*githubclient.Comment) string {
	if len(comments) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, c := range comments {
		sb.WriteString(fmt.Sprintf("@%s (%s):\n%s\n\n", c.Author, c.CreatedAt.Format(time.RFC3339), c.Body))
	}
	return sb.String()
}

// isAgentComment returns true if the comment was posted by Madar itself.
func isAgentComment(body string) bool {
	return strings.HasPrefix(body, "🤔 **Madar") || strings.HasPrefix(body, "✅ **Madar") || strings.HasPrefix(body, "❌ **Madar")
}
