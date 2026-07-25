package project

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	githubclient "github.com/eslam-mahmoud/go-ai-agent/internal/github"
	"github.com/eslam-mahmoud/go-ai-agent/internal/githubops"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
	"github.com/eslam-mahmoud/go-ai-agent/internal/workflow"
)

var ErrReconciliation = errors.New("reconciliation failed")

// ReconcileClient is everything a reconciliation pass reads and writes.
type ReconcileClient interface {
	githubops.LabelClient
	githubops.CloseClient
	githubops.PullRequestClient
}

type ReconcileStore interface {
	LoadProjectAggregate(projectID int64) (*store.ProjectAggregate, error)
	RecordProjectTaskPullRequest(
		projectID, taskID int64,
		prNumber int,
	) (*domain.Task, error)
}

// TaskReconciliation reports what converged for one task.
type TaskReconciliation struct {
	TaskID           int64
	IssueNumber      int
	LabelsUpdated    bool
	IssueClosed      bool
	PullRequestBound int
	// Drift is state the reconciler deliberately did not change, because
	// resolving it is a workflow or recovery decision.
	Drift []string
}

type ReconcileResult struct {
	Tasks     []TaskReconciliation
	Ambiguous []AmbiguousBranch
}

// Reconciler converges GitHub with durable task state. It only makes changes
// that are safe to make blindly: label namespace and closing a completed
// task's issue. Everything else is reported as drift.
type Reconciler struct {
	store  ReconcileStore
	client ReconcileClient
}

func NewReconciler(
	reconcileStore ReconcileStore,
	client ReconcileClient,
) (*Reconciler, error) {
	if reconcileStore == nil {
		return nil, errors.New("reconciler store is required")
	}
	if client == nil {
		return nil, errors.New("reconciler client is required")
	}
	return &Reconciler{store: reconcileStore, client: client}, nil
}

func (reconciler *Reconciler) Reconcile(
	ctx context.Context,
	projectID int64,
) (*ReconcileResult, error) {
	if projectID <= 0 {
		return nil, fmt.Errorf("%w: project ID must be positive", ErrReconciliation)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	aggregate, err := reconciler.store.LoadProjectAggregate(projectID)
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

	result := &ReconcileResult{}
	for _, task := range aggregate.Tasks {
		if task == nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		reconciliation, err := reconciler.reconcileTask(
			ctx, projectID, owner, repo, task, result,
		)
		if err != nil {
			return nil, err
		}
		if reconciliation != nil {
			result.Tasks = append(result.Tasks, *reconciliation)
		}
	}
	return result, nil
}

func (reconciler *Reconciler) reconcileTask(
	ctx context.Context,
	projectID int64,
	owner, repo string,
	task *domain.Task,
	result *ReconcileResult,
) (*TaskReconciliation, error) {
	reconciliation := &TaskReconciliation{TaskID: task.ID, IssueNumber: task.IssueNumber}

	pull, err := reconciler.reconcileBranch(ctx, projectID, owner, repo, task, reconciliation, result)
	if err != nil {
		return nil, err
	}
	if task.IssueNumber == 0 {
		// Nothing on GitHub represents this task yet; item 48 files it.
		if len(reconciliation.Drift) == 0 && reconciliation.PullRequestBound == 0 {
			return nil, nil
		}
		return reconciliation, nil
	}

	issue, err := reconciler.client.GetIssue(ctx, owner, repo, task.IssueNumber)
	if err != nil {
		return nil, fmt.Errorf("%w: read issue #%d: %v", ErrReconciliation, task.IssueNumber, err)
	}
	if issue == nil {
		// A missing issue is drift Madar must not paper over by re-filing.
		reconciliation.Drift = append(reconciliation.Drift, fmt.Sprintf(
			"issue #%d is missing on GitHub", task.IssueNumber,
		))
		return reconciliation, nil
	}

	desired := desiredLabels(issue.Labels, task.Status)
	labels, err := githubops.EnsureLabels(
		ctx, reconciler.client, owner, repo, task.IssueNumber, desired,
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrReconciliation, err)
	}
	reconciliation.LabelsUpdated = labels.Performed

	if task.Status == domain.TaskCompleted {
		closed, err := githubops.EnsureIssueClosed(
			ctx, reconciler.client, owner, repo, task.IssueNumber,
		)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrReconciliation, err)
		}
		reconciliation.IssueClosed = closed.Performed
	}
	reconciliation.Drift = append(
		reconciliation.Drift, pullRequestDrift(task, pull)...,
	)
	return reconciliation, nil
}

// reconcileBranch binds a task's pull request when its branch resolves to one,
// and records an ambiguous branch for escalation.
func (reconciler *Reconciler) reconcileBranch(
	ctx context.Context,
	projectID int64,
	owner, repo string,
	task *domain.Task,
	reconciliation *TaskReconciliation,
	result *ReconcileResult,
) (*githubclient.PullRequest, error) {
	if strings.TrimSpace(task.BranchName) == "" {
		return nil, nil
	}
	match, err := githubops.DiscoverPullRequest(
		ctx, reconciler.client, owner, repo, task.BranchName,
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrReconciliation, err)
	}
	if match.Ambiguous {
		numbers := make([]int, 0, len(match.Matches))
		for _, candidate := range match.Matches {
			if strings.EqualFold(candidate.State, "open") {
				numbers = append(numbers, candidate.Number)
			}
		}
		result.Ambiguous = append(result.Ambiguous, AmbiguousBranch{
			TaskID:  task.ID,
			Branch:  task.BranchName,
			Numbers: numbers,
		})
		reconciliation.Drift = append(reconciliation.Drift, fmt.Sprintf(
			"branch %s has %d open pull requests", task.BranchName, len(numbers),
		))
		return nil, nil
	}
	if match.Current == nil {
		return nil, nil
	}
	if task.PRNumber == 0 {
		if _, err := reconciler.store.RecordProjectTaskPullRequest(
			projectID, task.ID, match.Current.Number,
		); err != nil {
			return nil, err
		}
		reconciliation.PullRequestBound = match.Current.Number
	} else if task.PRNumber != match.Current.Number {
		reconciliation.Drift = append(reconciliation.Drift, fmt.Sprintf(
			"task records pull request #%d but branch %s resolves to #%d",
			task.PRNumber, task.BranchName, match.Current.Number,
		))
	}
	return match.Current, nil
}

// pullRequestDrift reports mismatches between delivery state and the pull
// request. Resolving these is the workflow's decision, not the reconciler's.
func pullRequestDrift(
	task *domain.Task,
	pull *githubclient.PullRequest,
) []string {
	if pull == nil {
		return nil
	}
	var drift []string
	if pull.Merged && task.Status != domain.TaskCompleted &&
		task.Status != domain.TaskCancelled {
		drift = append(drift, fmt.Sprintf(
			"pull request #%d is merged while the task is %q",
			pull.Number, task.Status,
		))
	}
	if !pull.Merged && strings.EqualFold(pull.State, "open") &&
		task.Status == domain.TaskCompleted {
		drift = append(drift, fmt.Sprintf(
			"pull request #%d is still open for a completed task", pull.Number,
		))
	}
	return drift
}

// desiredLabels keeps every label outside Madar's namespace. Reconciliation
// replaces the whole label set, so anything not carried over would be deleted
// — including labels a human added.
func desiredLabels(existing []string, status domain.TaskStatus) []string {
	desired := make([]string, 0, len(existing)+1)
	for _, label := range existing {
		trimmed := strings.TrimSpace(label)
		if trimmed == "" || strings.HasPrefix(trimmed, workflow.LabelNamespace) {
			continue
		}
		desired = append(desired, trimmed)
	}
	if statusLabel, published := workflow.TaskStatusLabel(status); published {
		desired = append(desired, statusLabel)
	}
	return desired
}

var _ ReconcileStore = (*store.Store)(nil)
