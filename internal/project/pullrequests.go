package project

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	"github.com/eslam-mahmoud/go-ai-agent/internal/githubops"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
)

var ErrPullRequestDiscovery = errors.New("pull request discovery failed")

type PullRequestStore interface {
	LoadProjectAggregate(projectID int64) (*store.ProjectAggregate, error)
	RecordProjectTaskPullRequest(
		projectID, taskID int64,
		prNumber int,
	) (*domain.Task, error)
}

// AmbiguousBranch names a branch Madar refuses to resolve because more than
// one open pull request claims it.
type AmbiguousBranch struct {
	TaskID  int64
	Branch  string
	Numbers []int
}

type PullRequestDiscoveryResult struct {
	Discovered []*domain.Task
	// AlreadyKnown are tasks that already record a pull request.
	AlreadyKnown []*domain.Task
	// Unmatched are tasks whose branch has no pull request yet.
	Unmatched []*domain.Task
	Ambiguous []AmbiguousBranch
}

// PullRequestDiscoverer binds tasks to the pull requests opened for their
// branches, so durable state can be reconciled against GitHub.
type PullRequestDiscoverer struct {
	store  PullRequestStore
	client githubops.PullRequestClient
}

func NewPullRequestDiscoverer(
	discoveryStore PullRequestStore,
	client githubops.PullRequestClient,
) (*PullRequestDiscoverer, error) {
	if discoveryStore == nil {
		return nil, errors.New("pull request store is required")
	}
	if client == nil {
		return nil, errors.New("pull request client is required")
	}
	return &PullRequestDiscoverer{store: discoveryStore, client: client}, nil
}

// Discover resolves every task branch that has no recorded pull request. An
// ambiguous branch is reported rather than guessed, so recovery can escalate.
func (discoverer *PullRequestDiscoverer) Discover(
	ctx context.Context,
	projectID int64,
) (*PullRequestDiscoveryResult, error) {
	if projectID <= 0 {
		return nil, fmt.Errorf("%w: project ID must be positive", ErrPullRequestDiscovery)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	aggregate, err := discoverer.store.LoadProjectAggregate(projectID)
	if err != nil {
		return nil, err
	}
	if aggregate == nil || aggregate.Project == nil {
		return nil, fmt.Errorf("%w: project aggregate is nil", ErrInconsistentState)
	}
	owner, repo, err := splitRepository(aggregate.Project.Repo)
	if err != nil {
		return nil, err
	}

	result := &PullRequestDiscoveryResult{}
	for _, task := range aggregate.Tasks {
		if task == nil || strings.TrimSpace(task.BranchName) == "" {
			continue
		}
		if task.PRNumber != 0 {
			result.AlreadyKnown = append(result.AlreadyKnown, task)
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		match, err := githubops.DiscoverPullRequest(
			ctx, discoverer.client, owner, repo, task.BranchName,
		)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrPullRequestDiscovery, err)
		}
		if match.Ambiguous {
			numbers := make([]int, 0, len(match.Matches))
			for _, pull := range match.Matches {
				if strings.EqualFold(pull.State, "open") {
					numbers = append(numbers, pull.Number)
				}
			}
			result.Ambiguous = append(result.Ambiguous, AmbiguousBranch{
				TaskID:  task.ID,
				Branch:  task.BranchName,
				Numbers: numbers,
			})
			continue
		}
		if match.Current == nil {
			result.Unmatched = append(result.Unmatched, task)
			continue
		}
		stored, err := discoverer.store.RecordProjectTaskPullRequest(
			projectID, task.ID, match.Current.Number,
		)
		if err != nil {
			return nil, err
		}
		result.Discovered = append(result.Discovered, stored)
	}
	return result, nil
}

var _ PullRequestStore = (*store.Store)(nil)
