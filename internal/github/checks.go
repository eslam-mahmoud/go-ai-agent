package github

import (
	"context"
	"fmt"
	"strings"

	gh "github.com/google/go-github/v66/github"
)

type CheckStatus string

const (
	CheckPending CheckStatus = "pending"
	CheckSuccess CheckStatus = "success"
	CheckFailure CheckStatus = "failure"
)

// GetCheckSuiteStatus returns the aggregate CI status for the latest commit on a branch.
// It looks up all check runs associated with the most recent commit on the given branch ref.
func (c *githubClient) GetCheckSuiteStatus(ctx context.Context, owner, repo, branch string) (CheckStatus, error) {
	// Get the branch to find the latest commit SHA.
	branchInfo, _, err := c.gh.Repositories.GetBranch(ctx, owner, repo, branch, 0)
	if err != nil {
		return CheckPending, fmt.Errorf("get branch %q: %w", branch, err)
	}
	sha := branchInfo.GetCommit().GetSHA()
	if sha == "" {
		return CheckPending, nil
	}

	// List all check runs for this commit.
	runs, _, err := c.gh.Checks.ListCheckRunsForRef(ctx, owner, repo, sha, &gh.ListCheckRunsOptions{
		ListOptions: gh.ListOptions{PerPage: 100},
	})
	if err != nil {
		return CheckPending, fmt.Errorf("list check runs: %w", err)
	}

	if runs.GetTotal() == 0 {
		// No CI configured — treat as success so we don't block indefinitely.
		return CheckSuccess, nil
	}

	anyPending := false
	for _, run := range runs.CheckRuns {
		status := run.GetStatus()
		conclusion := run.GetConclusion()

		if status != "completed" {
			anyPending = true
			continue
		}
		// A completed run with a non-success conclusion is a failure.
		switch conclusion {
		case "failure", "timed_out", "cancelled", "action_required":
			return CheckFailure, nil
		}
	}

	if anyPending {
		return CheckPending, nil
	}
	return CheckSuccess, nil
}

// GetFailedStepOutput collects the annotations and output summaries from all
// failed check runs on the branch's latest commit. This is fed back to Claude
// so it knows exactly what broke.
func (c *githubClient) GetFailedStepOutput(ctx context.Context, owner, repo, branch string) (string, error) {
	branchInfo, _, err := c.gh.Repositories.GetBranch(ctx, owner, repo, branch, 0)
	if err != nil {
		return "", fmt.Errorf("get branch: %w", err)
	}
	sha := branchInfo.GetCommit().GetSHA()

	runs, _, err := c.gh.Checks.ListCheckRunsForRef(ctx, owner, repo, sha, &gh.ListCheckRunsOptions{
		ListOptions: gh.ListOptions{PerPage: 100},
	})
	if err != nil {
		return "", fmt.Errorf("list check runs: %w", err)
	}

	var sb strings.Builder
	for _, run := range runs.CheckRuns {
		if run.GetStatus() != "completed" {
			continue
		}
		conclusion := run.GetConclusion()
		if conclusion != "failure" && conclusion != "timed_out" {
			continue
		}

		sb.WriteString(fmt.Sprintf("### Check: %s (conclusion: %s)\n", run.GetName(), conclusion))
		if out := run.GetOutput(); out != nil {
			if out.GetTitle() != "" {
				sb.WriteString(fmt.Sprintf("**%s**\n", out.GetTitle()))
			}
			if out.GetSummary() != "" {
				sb.WriteString(out.GetSummary())
				sb.WriteString("\n")
			}
			if out.GetText() != "" {
				// Truncate very long outputs to keep prompts manageable.
				text := out.GetText()
				if len(text) > 3000 {
					text = text[:3000] + "\n[truncated]"
				}
				sb.WriteString(text)
				sb.WriteString("\n")
			}
		}

		// Include annotations (inline errors).
		annotations, _, err := c.gh.Checks.ListCheckRunAnnotations(ctx, owner, repo, run.GetID(), &gh.ListOptions{PerPage: 50})
		if err == nil {
			for _, a := range annotations {
				sb.WriteString(fmt.Sprintf("- %s:%d: %s\n", a.GetPath(), a.GetStartLine(), a.GetMessage()))
			}
		}
		sb.WriteString("\n")
	}

	if sb.Len() == 0 {
		return "CI failed but no detailed output was available.", nil
	}
	return sb.String(), nil
}
