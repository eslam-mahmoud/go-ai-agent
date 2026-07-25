package githubops

import (
	"context"
	"fmt"
	"strings"

	githubclient "github.com/eslam-mahmoud/go-ai-agent/internal/github"
)

// PullRequestClient reads pull requests for a branch.
type PullRequestClient interface {
	ListPullRequestsForBranch(
		ctx context.Context,
		owner, repo, branch string,
	) ([]*githubclient.PullRequest, error)
}

// PullRequestMatch resolves a branch to at most one current pull request and
// keeps the full match set so an ambiguous branch can be reported honestly.
type PullRequestMatch struct {
	// Current is the pull request that represents the branch now, if any.
	Current *githubclient.PullRequest
	// Ambiguous is set when the branch has more than one open pull request,
	// which the plan treats as a situation to escalate rather than guess.
	Ambiguous bool
	Matches   []*githubclient.PullRequest
}

// DiscoverPullRequest resolves a branch to its current pull request.
//
// Exactly one open pull request is the answer even when closed ones exist for
// the same branch, since branches are reused. More than one open pull request
// is ambiguous: picking either could attach work to the wrong review. With no
// open pull request, the most recently merged one is the branch's outcome.
func DiscoverPullRequest(
	ctx context.Context,
	client PullRequestClient,
	owner, repo, branch string,
) (*PullRequestMatch, error) {
	if client == nil {
		return nil, fmt.Errorf("%w: pull request client is required", ErrInvalidOperation)
	}
	if strings.TrimSpace(owner) == "" || strings.TrimSpace(repo) == "" {
		return nil, fmt.Errorf("%w: owner and repository are required", ErrInvalidOperation)
	}
	if strings.TrimSpace(branch) == "" {
		return nil, fmt.Errorf("%w: branch is required", ErrInvalidOperation)
	}
	pulls, err := client.ListPullRequestsForBranch(ctx, owner, repo, branch)
	if err != nil {
		return nil, fmt.Errorf("list pull requests for %s: %w", branch, err)
	}

	match := &PullRequestMatch{Matches: make([]*githubclient.PullRequest, 0, len(pulls))}
	var open []*githubclient.PullRequest
	var merged *githubclient.PullRequest
	for _, pull := range pulls {
		if pull == nil {
			continue
		}
		// GitHub filters by head branch, but a mismatched result would bind
		// work to the wrong review, so confirm it.
		if strings.TrimSpace(pull.HeadBranch) != "" && pull.HeadBranch != branch {
			continue
		}
		match.Matches = append(match.Matches, pull)
		if strings.EqualFold(pull.State, "open") {
			open = append(open, pull)
			continue
		}
		if pull.Merged && isMoreRecentMerge(merged, pull) {
			merged = pull
		}
	}
	switch {
	case len(open) == 1:
		match.Current = open[0]
	case len(open) > 1:
		match.Ambiguous = true
	case merged != nil:
		match.Current = merged
	}
	return match, nil
}

// isMoreRecentMerge prefers a later merge, falling back to the higher number
// when GitHub reports no merge timestamp.
func isMoreRecentMerge(current, candidate *githubclient.PullRequest) bool {
	if current == nil {
		return true
	}
	if candidate.MergedAt != nil && current.MergedAt != nil {
		return candidate.MergedAt.After(*current.MergedAt)
	}
	if candidate.MergedAt != nil {
		return true
	}
	if current.MergedAt != nil {
		return false
	}
	return candidate.Number > current.Number
}
