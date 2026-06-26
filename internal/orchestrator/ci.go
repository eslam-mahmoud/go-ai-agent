package orchestrator

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/claude"
	githubclient "github.com/eslam-mahmoud/go-ai-agent/internal/github"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
)

// checkCIPending inspects all tasks in ci_state=waiting and advances them:
//   - still pending → skip
//   - passed        → finalize (label done, Telegram)
//   - failed        → re-invoke Claude with failure output (up to MaxRetries)
//   - retries exhausted → transition to awaiting-feedback
func (l *Loop) checkCIPending(ctx context.Context) error {
	if !l.cfg.CI.Enabled {
		return nil
	}

	waiting, err := l.store.ListByCIState(store.CIStateWaiting)
	if err != nil {
		return fmt.Errorf("list CI-waiting tasks: %w", err)
	}

	for _, task := range waiting {
		owner, repo, err := githubclient.SplitRepo(task.Repo)
		if err != nil {
			continue
		}
		if err := l.advanceCITask(ctx, owner, repo, task); err != nil {
			l.log.Error("CI advance failed", "repo", task.Repo, "issue", task.IssueNumber, "err", err)
		}
	}
	return nil
}

func (l *Loop) advanceCITask(ctx context.Context, owner, repo string, task *store.Task) error {
	// Enforce wait_timeout: if the task has been CI-waiting longer than the
	// configured limit, escalate to a human rather than waiting indefinitely.
	if l.cfg.CI.WaitTimeout > 0 && time.Since(task.UpdatedAt) > l.cfg.CI.WaitTimeout {
		l.log.Warn("CI wait_timeout exceeded, escalating",
			"repo", task.Repo, "issue", task.IssueNumber,
			"waited", time.Since(task.UpdatedAt).Round(time.Second),
			"timeout", l.cfg.CI.WaitTimeout)
		return l.giveCIUpToHuman(ctx, owner, repo, task)
	}

	branch := fmt.Sprintf("madar/issue-%d", task.IssueNumber)

	status, err := l.gh.GetCheckSuiteStatus(ctx, owner, repo, branch)
	if err != nil {
		l.log.Warn("check suite status error", "repo", task.Repo, "issue", task.IssueNumber, "err", err)
		return nil // transient — retry next tick
	}

	l.log.Debug("CI status", "repo", task.Repo, "issue", task.IssueNumber, "status", status)

	switch status {
	case githubclient.CheckPending:
		return nil // still running

	case githubclient.CheckSuccess:
		return l.finalizeCISuccess(ctx, owner, repo, task)

	case githubclient.CheckFailure:
		return l.handleCIFailure(ctx, owner, repo, task, branch)
	}
	return nil
}

func (l *Loop) finalizeCISuccess(ctx context.Context, owner, repo string, task *store.Task) error {
	l.log.Info("CI passed, finalizing task", "repo", task.Repo, "issue", task.IssueNumber)

	if err := l.store.SetCIState(task.Repo, task.IssueNumber, store.CIStatePassed); err != nil {
		return err
	}

	comment := "✅ **CI passed.** All checks are green."
	if _, err := l.gh.PostComment(ctx, owner, repo, task.IssueNumber, comment); err != nil {
		l.log.Warn("post CI-pass comment failed", "err", err)
	}

	issueLabels, _ := l.getIssueLabels(ctx, owner, repo, task.IssueNumber)
	if err := l.transitionLabels(ctx, owner, repo, task.IssueNumber, issueLabels,
		l.cfg.Labels.InProgress, l.cfg.Labels.Done); err != nil {
		l.log.Warn("label to done failed", "err", err)
	}

	if _, err := l.store.UpsertTask(task.Repo, task.IssueNumber, store.StateDone, ""); err != nil {
		return err
	}
	if err := l.gh.CloseIssue(ctx, owner, repo, task.IssueNumber); err != nil {
		l.log.Warn("close issue failed", "issue", task.IssueNumber, "err", err)
	}
	_ = l.store.Log(task.Repo, task.IssueNumber, "ci_passed", "")

	issue, err := l.gh.GetIssue(ctx, owner, repo, task.IssueNumber)
	if err == nil {
		_ = l.telegram.NotifyCompletion(ctx, issue.HTMLURL, "CI passed — task complete.")
	}
	return nil
}

func (l *Loop) handleCIFailure(ctx context.Context, owner, repo string, task *store.Task, branch string) error {
	retries, err := l.store.IncrementCIRetries(task.Repo, task.IssueNumber)
	if err != nil {
		return err
	}

	l.log.Info("CI failed", "repo", task.Repo, "issue", task.IssueNumber,
		"retry", retries, "max", l.cfg.CI.MaxRetries)

	if retries > l.cfg.CI.MaxRetries {
		return l.giveCIUpToHuman(ctx, owner, repo, task)
	}

	failureOutput, err := l.gh.GetFailedStepOutput(ctx, owner, repo, branch)
	if err != nil {
		l.log.Warn("could not fetch CI failure output", "err", err)
		failureOutput = "CI failed but no details could be retrieved."
	}

	_ = l.store.Log(task.Repo, task.IssueNumber, "ci_retry",
		fmt.Sprintf("attempt %d/%d", retries, l.cfg.CI.MaxRetries))

	// Notify on first failure only to avoid spamming.
	if retries == 1 {
		issue, err := l.gh.GetIssue(ctx, owner, repo, task.IssueNumber)
		if err == nil {
			_ = l.telegram.NotifyError(ctx, issue.HTMLURL,
				fmt.Errorf("CI failed — retrying automatically (%d/%d)", retries, l.cfg.CI.MaxRetries))
		}
	}

	commentBody := fmt.Sprintf(
		"⚠️ **CI failed (attempt %d/%d).** Re-running Claude to fix it...\n\n<details><summary>Failure output</summary>\n\n```\n%s\n```\n</details>",
		retries, l.cfg.CI.MaxRetries, truncate(failureOutput, 2000),
	)
	if _, err := l.gh.PostComment(ctx, owner, repo, task.IssueNumber, commentBody); err != nil {
		l.log.Warn("post CI-retry comment failed", "err", err)
	}

	// Resume the Claude session with the failure details.
	prompt := claude.BuildCIFixPrompt(failureOutput, retries, l.cfg.CI.MaxRetries)
	workDir := filepath.Join(l.cfg.WorkspaceDir, owner, repo)
	opts := claude.RunOptions{
		WorkDir:         workDir,
		ResumeID:        task.SessionID,
		Prompt:          prompt,
		MaxTurns:        l.cfg.Claude.MaxTurns,
		Timeout:         l.cfg.Claude.RunTimeout,
		SkipPermissions: l.cfg.Claude.SkipPermissions,
	}

	result, err := l.claude.Run(ctx, opts)
	if err != nil {
		return fmt.Errorf("claude CI fix run: %w", err)
	}

	if result.NeedsInput {
		issue, _ := l.gh.GetIssue(ctx, owner, repo, task.IssueNumber)
		if issue == nil {
			issue = &githubclient.Issue{Number: task.IssueNumber}
		}
		// Claude gave up — escalate to human.
		_ = l.store.SetCIState(task.Repo, task.IssueNumber, store.CIStateGaveUp)
		return l.handleClarification(ctx, owner, repo, task.Repo, task.IssueNumber, issue, result.Question)
	}

	if result.IsError {
		return fmt.Errorf("claude CI fix returned error: %s", result.Output)
	}

	// Claude pushed a fix — reset ci_state to waiting for the new run to complete.
	return l.store.SetCIState(task.Repo, task.IssueNumber, store.CIStateWaiting)
}

func (l *Loop) giveCIUpToHuman(ctx context.Context, owner, repo string, task *store.Task) error {
	l.log.Info("CI retries exhausted, escalating to human",
		"repo", task.Repo, "issue", task.IssueNumber)

	_ = l.store.SetCIState(task.Repo, task.IssueNumber, store.CIStateGaveUp)
	_ = l.store.Log(task.Repo, task.IssueNumber, "ci_gave_up",
		fmt.Sprintf("exhausted %d retries", l.cfg.CI.MaxRetries))

	question := fmt.Sprintf(
		"CI kept failing after %d automatic fix attempts. Please review the PR and advise how to proceed.",
		l.cfg.CI.MaxRetries,
	)
	issue, err := l.gh.GetIssue(ctx, owner, repo, task.IssueNumber)
	if err != nil {
		issue = &githubclient.Issue{Number: task.IssueNumber, HTMLURL: ""}
	}
	return l.handleClarification(ctx, owner, repo, task.Repo, task.IssueNumber, issue, question)
}

// StartCIWatch transitions a completed task into CI-waiting mode.
// Call this after Claude finishes and pushes a branch, before finalizing the issue.
func (l *Loop) StartCIWatch(ctx context.Context, fullRepo string, issueNumber int, prNumber int) error {
	if err := l.store.SetPRNumber(fullRepo, issueNumber, prNumber); err != nil {
		return err
	}
	if err := l.store.SetCIState(fullRepo, issueNumber, store.CIStateWaiting); err != nil {
		return err
	}
	_ = l.store.Log(fullRepo, issueNumber, "ci_watch_started",
		fmt.Sprintf("pr=%d", prNumber))
	l.log.Info("CI watch started", "repo", fullRepo, "issue", issueNumber, "pr", prNumber)
	return nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n[truncated]"
}

// extractPRNumber looks for a PR number anywhere in the Claude output.
// Scans for patterns like "PR: #42" or "PR #7".
func extractPRNumber(output string) int {
	// Walk through the string looking for "PR" followed by optional ": " or " " then "#<digits>".
	s := output
	for {
		idx := strings.Index(s, "PR")
		if idx < 0 {
			break
		}
		rest := strings.TrimLeft(s[idx+2:], ": ")
		if strings.HasPrefix(rest, "#") {
			var n int
			if _, err := fmt.Sscanf(rest[1:], "%d", &n); err == nil && n > 0 {
				return n
			}
		}
		s = s[idx+2:]
	}
	return 0
}
