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
func (c *githubClient) GetCheckSuiteStatus(ctx context.Context, owner, repo, branch string) (CheckStatus, error) {
	branchInfo, _, err := c.gh.Repositories.GetBranch(ctx, owner, repo, branch, 0)
	if err != nil {
		return CheckPending, fmt.Errorf("get branch %q: %w", branch, err)
	}
	sha := branchInfo.GetCommit().GetSHA()
	if sha == "" {
		return CheckPending, nil
	}

	allRuns, err := c.listAllCheckRuns(ctx, owner, repo, sha)
	if err != nil {
		return CheckPending, err
	}

	if len(allRuns) == 0 {
		// No CI configured — treat as success so we don't block indefinitely.
		return CheckSuccess, nil
	}

	anyPending := false
	for _, run := range allRuns {
		if run.GetStatus() != "completed" {
			anyPending = true
			continue
		}
		switch run.GetConclusion() {
		case "failure", "timed_out", "cancelled", "action_required":
			return CheckFailure, nil
		}
	}
	if anyPending {
		return CheckPending, nil
	}
	return CheckSuccess, nil
}

// GetFailedStepOutput collects annotations and output summaries from all
// failed check runs on the branch's latest commit.
func (c *githubClient) GetFailedStepOutput(ctx context.Context, owner, repo, branch string) (string, error) {
	branchInfo, _, err := c.gh.Repositories.GetBranch(ctx, owner, repo, branch, 0)
	if err != nil {
		return "", fmt.Errorf("get branch: %w", err)
	}
	sha := branchInfo.GetCommit().GetSHA()

	allRuns, err := c.listAllCheckRuns(ctx, owner, repo, sha)
	if err != nil {
		return "", err
	}

	var sb strings.Builder
	for _, run := range allRuns {
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
				text := out.GetText()
				if len(text) > 3000 {
					text = text[:3000] + "\n[truncated]"
				}
				sb.WriteString(text)
				sb.WriteString("\n")
			}
		}

		// Paginate annotations.
		annotations, err := c.listAllAnnotations(ctx, owner, repo, run.GetID())
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

// listAllCheckRuns fetches every check run for a commit SHA across all pages.
func (c *githubClient) listAllCheckRuns(ctx context.Context, owner, repo, sha string) ([]*gh.CheckRun, error) {
	opts := &gh.ListCheckRunsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	var all []*gh.CheckRun
	for {
		result, resp, err := c.gh.Checks.ListCheckRunsForRef(ctx, owner, repo, sha, opts)
		if err != nil {
			return nil, fmt.Errorf("list check runs: %w", err)
		}
		all = append(all, result.CheckRuns...)
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return all, nil
}

// listAllAnnotations fetches every annotation for a check run across all pages.
func (c *githubClient) listAllAnnotations(ctx context.Context, owner, repo string, checkRunID int64) ([]*gh.CheckRunAnnotation, error) {
	opts := &gh.ListOptions{PerPage: 100}
	var all []*gh.CheckRunAnnotation
	for {
		page, resp, err := c.gh.Checks.ListCheckRunAnnotations(ctx, owner, repo, checkRunID, opts)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return all, nil
}
