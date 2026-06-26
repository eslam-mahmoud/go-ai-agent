package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"os"
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
	cfg         *config.Config
	gh          githubclient.Client
	claude      claude.Runner
	telegram    telegram.Gateway
	store       *store.Store
	log         *slog.Logger
	lastPruneAt time.Time
	botUsername  string // GitHub login of the authenticated token
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
	// Resolve the GitHub username of the authenticated token once on startup.
	// Used to filter Madar's own comments from human reply detection.
	if username, err := l.gh.GetAuthenticatedUsername(ctx); err != nil {
		l.log.Warn("could not resolve bot GitHub username, falling back to body-prefix filter", "err", err)
	} else {
		l.botUsername = username
		l.log.Info("bot username resolved", "username", username)
	}

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
	// Daily housekeeping: prune old audit log entries and completed tasks.
	if time.Since(l.lastPruneAt) > 24*time.Hour {
		const retention = 30 * 24 * time.Hour
		if n, err := l.store.PruneAuditLog(retention); err != nil {
			l.log.Warn("audit log prune failed", "err", err)
		} else if n > 0 {
			l.log.Info("pruned audit log", "rows_deleted", n)
		}
		if n, err := l.store.PruneCompletedTasks(retention); err != nil {
			l.log.Warn("completed task prune failed", "err", err)
		} else if n > 0 {
			l.log.Info("pruned completed tasks", "rows_deleted", n)
		}
		l.lastPruneAt = time.Now()
	}

	// Run CI and feedback checks unconditionally — they don't need the active
	// count and should not be skipped by a transient SQLite error.

	// 1. Check CI-pending tasks.
	if err := l.checkCIPending(ctx); err != nil {
		l.log.Error("CI pending check failed", "err", err)
	}

	// 2. Check awaiting-feedback issues for human replies.
	if err := l.checkAwaitingFeedback(ctx); err != nil {
		l.log.Error("awaiting-feedback check failed", "err", err)
	}

	// 3. Concurrency guard — gate new work on the active count.
	active, err := l.store.CountActive()
	if err != nil {
		l.log.Error("count active failed", "err", err)
		return nil // can't safely pick new work without knowing the count
	}
	maxParallel := 1
	if l.cfg.Concurrency.Enabled {
		maxParallel = l.cfg.Concurrency.MaxParallel
	}
	if active >= maxParallel {
		l.log.Debug("at capacity, skipping poll", "active", active, "max", maxParallel)
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
		if c.Author == "" {
			continue
		}
		// Skip Madar's own comments by username (primary) and body prefix (fallback).
		if l.botUsername != "" && c.Author == l.botUsername {
			continue
		}
		if isAgentComment(c.Body) {
			continue
		}
		humanReply = c.Body
		break
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

		// Ensure workspace exists for this specific repo before claiming.
		// Handles repos added to config after initial startup.
		repoCfg := *l.cfg
		repoCfg.Repos = []string{fullRepo}
		if err := EnsureWorkspaces(ctx, &repoCfg, l.log); err != nil {
			l.log.Error("workspace setup failed, skipping issue", "repo", fullRepo, "err", err)
			continue
		}

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

		// Pull latest changes so Claude works against current main.
		l.pullWorkspace(ctx, owner, repo)

		// Build first-run prompt from issue + thread (human comments only).
		comments, _ := l.gh.GetComments(ctx, owner, repo, issue.Number, nil)
		threadStr := l.formatHumanThread(comments)
		prompt := claude.BuildFirstRunPrompt(issue.Title, issue.Body, threadStr, issue.Number)

		return l.runClaude(ctx, owner, repo, issue.Number, issue, sessionID, prompt, false)
	}
	return nil
}

func (l *Loop) runClaude(ctx context.Context, owner, repo string, issueNumber int, issue *githubclient.Issue, sessionID, prompt string, isResume bool) error {
	workDir := filepath.Join(l.cfg.WorkspaceDir, owner, repo)

	if _, err := os.Stat(workDir); err != nil {
		missing := fmt.Errorf("workspace %s does not exist — run EnsureWorkspaces or clone manually", workDir)
		l.log.Error("workspace missing", "path", workDir)
		_ = l.store.Log(owner+"/"+repo, issueNumber, "error", missing.Error())
		return l.handleClarification(ctx, owner, repo, owner+"/"+repo, issueNumber, issue,
			fmt.Sprintf("Workspace directory is missing: `%s`\n\nPlease ensure the repo is cloned under `workspace_dir` and retry.", workDir))
	}

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

		// If the main context was cancelled (SIGTERM/shutdown), use a fresh
		// context for cleanup so GitHub API calls and store updates succeed.
		cleanupCtx := ctx
		if ctx.Err() != nil {
			var cancel context.CancelFunc
			cleanupCtx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			l.log.Info("agent shutting down, transitioning task to awaiting-feedback",
				"repo", fullRepo, "issue", issueNumber)
		}

		_ = l.store.Log(fullRepo, issueNumber, "error", err.Error())
		_ = l.telegram.NotifyError(cleanupCtx, issue.HTMLURL, err)
		// Transition to awaiting-feedback so the task doesn't stay in-progress.
		question := fmt.Sprintf("Claude process failed: %v\n\nPlease advise how to proceed.", err)
		if ctx.Err() != nil {
			question = "Madar shut down while working on this task. It will resume on the next start, or you can reply here to guide it."
		}
		return l.handleClarification(cleanupCtx, owner, repo, fullRepo, issueNumber, issue, question)
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
		// Same: don't leave the task in-progress — ask the human.
		return l.handleClarification(ctx, owner, repo, fullRepo, issueNumber, issue,
			fmt.Sprintf("Claude reported an error:\n\n%s\n\nPlease advise how to proceed.", result.Output))
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

// formatHumanThread formats only human comments for inclusion in Claude's prompt.
// Bot comments are excluded. If the total exceeds MaxThreadChars, the oldest
// comments are dropped (most recent are most relevant to the current task).
func (l *Loop) formatHumanThread(comments []*githubclient.Comment) string {
	var human []string
	for _, c := range comments {
		if c.Author == "" {
			continue
		}
		if l.botUsername != "" && c.Author == l.botUsername {
			continue
		}
		if isAgentComment(c.Body) {
			continue
		}
		human = append(human, fmt.Sprintf("@%s (%s):\n%s\n\n",
			c.Author, c.CreatedAt.Format(time.RFC3339), c.Body))
	}

	maxChars := l.cfg.Claude.MaxThreadChars
	if maxChars <= 0 {
		maxChars = 8000
	}

	// Build from most recent backward, then reverse to keep chronological order.
	var sb strings.Builder
	used := 0
	start := 0
	for i := len(human) - 1; i >= 0; i-- {
		if used+len(human[i]) > maxChars {
			start = i + 1
			break
		}
		used += len(human[i])
	}
	if start > 0 {
		sb.WriteString(fmt.Sprintf("[%d earlier comment(s) omitted]\n\n", start))
	}
	for _, entry := range human[start:] {
		sb.WriteString(entry)
	}
	return sb.String()
}

// formatThread is kept for tests that don't need bot filtering.
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
