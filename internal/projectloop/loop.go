// Package projectloop drives Madar's v2 delivery cycle. It is the piece that
// turns the implemented v2 components into a running agent: without it the
// modes, controllers, and review cycle exist but nothing executes them.
package projectloop

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	"github.com/eslam-mahmoud/go-ai-agent/internal/notify"
	"github.com/eslam-mahmoud/go-ai-agent/internal/policy"
	"github.com/eslam-mahmoud/go-ai-agent/internal/project"
	"github.com/eslam-mahmoud/go-ai-agent/internal/workflow"
)

var ErrInvalidLoop = errors.New("invalid project loop")

// Action names what one tick did, so a caller can log a real cycle rather
// than guess at it.
type Action string

const (
	ActionIdle       Action = "idle"
	ActionInitialize Action = "initialize"
	ActionSelect     Action = "select"
	ActionDeliver    Action = "deliver"
	ActionReview     Action = "review"
	ActionPaused     Action = "paused"
	// ActionBlocked reports that a budget stopped work on the current task.
	ActionBlocked Action = "blocked"
)

// Outcome reports one tick.
type Outcome struct {
	Action Action
	TaskID int64
	Status domain.TaskStatus
	Detail string
}

// Controller is the narrow project-control surface the loop needs.
type Controller interface {
	Snapshot(projectID int64) (*project.Snapshot, error)
}

// Delivery advances one task through the sequential workflow.
type Delivery interface {
	Run(ctx context.Context, projectID, taskID int64) (*workflow.FeatureResult, error)
}

// Reviewer runs the Engineering Manager cycle.
type Reviewer interface {
	ReviewAfterTask(ctx context.Context, projectID, taskID int64) (*project.ReviewResult, error)
	ReviewProject(ctx context.Context, projectID int64) (*project.ReviewResult, error)
}

// Initializer bootstraps an empty project.
type Initializer interface {
	Initialize(ctx context.Context, projectID int64) (*project.InitializationResult, error)
}

// BudgetGuard decides whether the current task may keep consuming provider
// runs. Budgets are derived from the immutable execution history, so a restart
// cannot reset them.
type BudgetGuard interface {
	ListTaskExecutions(taskID int64) ([]*domain.Execution, error)
	AppendWorkflowEvent(event *domain.WorkflowEvent) (*domain.WorkflowEvent, bool, error)
}

// PullRequestDiscoverer attaches a task's pull request once its branch has
// one. Optional: without GitHub credentials a task's PR is only learned from
// reconciliation.
type PullRequestDiscoverer interface {
	Discover(ctx context.Context, projectID int64) (*project.PullRequestDiscoveryResult, error)
}

// StatusPublisher keeps the owner's live status message current. It is
// optional: a deployment without Telegram still delivers, silently.
type StatusPublisher interface {
	Publish(ctx context.Context, status notify.Status) (*notify.StatusOutcome, error)
}

type Options struct {
	// Initializer bootstraps a project with no backlog. Optional: without it
	// the loop expects a backlog to already exist.
	Initializer Initializer
	Status      StatusPublisher
	// PullRequests attaches pull requests to the tasks that opened them.
	PullRequests PullRequestDiscoverer
	// Budgets bound one task's consumption. The zero value is unlimited.
	Budgets policy.Budgets
	// BudgetGuard supplies the execution history budgets are measured
	// against. Without it budgets cannot be enforced and are ignored.
	BudgetGuard BudgetGuard
	Log         *slog.Logger
}

// Loop advances one managed project by one step per tick. It is deliberately
// single-step: the permanent invariant is one delivery decision at a time, and
// a loop that did several things per tick would be harder to reason about
// after a restart.
type Loop struct {
	projectID    int64
	controller   Controller
	delivery     Delivery
	reviewer     Reviewer
	initializer  Initializer
	status       StatusPublisher
	pullRequests PullRequestDiscoverer
	budgets      policy.Budgets
	budgetGuard  BudgetGuard
	log          *slog.Logger
}

func New(
	projectID int64,
	controller Controller,
	delivery Delivery,
	reviewer Reviewer,
	options Options,
) (*Loop, error) {
	switch {
	case projectID <= 0:
		return nil, fmt.Errorf("%w: project ID must be positive", ErrInvalidLoop)
	case controller == nil:
		return nil, errors.New("project loop controller is required")
	case delivery == nil:
		return nil, errors.New("project loop delivery is required")
	case reviewer == nil:
		return nil, errors.New("project loop reviewer is required")
	}
	log := options.Log
	if log == nil {
		log = slog.Default()
	}
	return &Loop{
		projectID:    projectID,
		controller:   controller,
		delivery:     delivery,
		reviewer:     reviewer,
		initializer:  options.Initializer,
		status:       options.Status,
		pullRequests: options.PullRequests,
		budgets:      options.Budgets,
		budgetGuard:  options.BudgetGuard,
		log:          log,
	}, nil
}

// Tick advances the project one step and reports what it did.
//
// The order encodes the delivery model: a paused project does nothing, an
// empty project initializes, a terminal task is reviewed, an in-flight task is
// delivered, and an idle project asks the manager what to do next.
func (loop *Loop) Tick(ctx context.Context) (*Outcome, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	snapshot, err := loop.controller.Snapshot(loop.projectID)
	if err != nil {
		return nil, err
	}
	// The owner sees where delivery stands even when this tick changes
	// nothing, which is exactly when they are most likely to be wondering.
	loop.publishStatus(ctx, snapshot)
	if snapshot.Project.State == domain.ProjectPaused {
		return &Outcome{Action: ActionPaused, Detail: "project is paused"}, nil
	}
	// A task's pull request is discovered before anything reads it, so the
	// verifier sees the PR that its own branch opened.
	loop.discoverPullRequests(ctx)
	if len(snapshot.Tasks) == 0 {
		if loop.initializer == nil {
			return &Outcome{
				Action: ActionIdle,
				Detail: "project has no backlog and no initializer is configured",
			}, nil
		}
		result, err := loop.initializer.Initialize(ctx, loop.projectID)
		if err != nil {
			return nil, fmt.Errorf("initialize project: %w", err)
		}
		return &Outcome{
			Action: ActionInitialize,
			Detail: fmt.Sprintf("created %d task(s)", initializedTasks(result)),
		}, nil
	}

	current := snapshot.CurrentTask
	if current == nil {
		// Nothing is in the lane: the manager decides what runs next.
		result, err := loop.reviewer.ReviewProject(ctx, loop.projectID)
		if err != nil {
			return nil, fmt.Errorf("project review: %w", err)
		}
		return selectionOutcome(result), nil
	}
	if workflow.ManagerReviewRequired(current.Status) {
		result, err := loop.reviewer.ReviewAfterTask(ctx, loop.projectID, current.ID)
		if err != nil {
			return nil, fmt.Errorf("review after task %d: %w", current.ID, err)
		}
		outcome := selectionOutcome(result)
		outcome.Action = ActionReview
		outcome.TaskID = current.ID
		return outcome, nil
	}

	if blocked, err := loop.budgetBlocked(current); err != nil {
		return nil, err
	} else if blocked != nil {
		return blocked, nil
	}

	delivered, err := loop.delivery.Run(ctx, loop.projectID, current.ID)
	if err != nil {
		return nil, fmt.Errorf("deliver task %d: %w", current.ID, err)
	}
	return &Outcome{
		Action: ActionDeliver,
		TaskID: current.ID,
		Status: delivered.FinalStatus,
		Detail: fmt.Sprintf("delivery reached %s", delivered.FinalStatus),
	}, nil
}

// Run ticks until the context is cancelled. A failing tick is logged and
// retried on the next interval: one bad cycle must not stop the agent.
func (loop *Loop) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		return fmt.Errorf("%w: interval must be positive", ErrInvalidLoop)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		outcome, err := loop.Tick(ctx)
		switch {
		case err != nil && ctx.Err() != nil:
			return ctx.Err()
		case err != nil:
			loop.log.Warn("project tick failed", "project", loop.projectID, "err", err)
		case outcome.Action != ActionIdle && outcome.Action != ActionPaused:
			loop.log.Info("project advanced",
				"project", loop.projectID,
				"action", outcome.Action,
				"task", outcome.TaskID,
				"detail", outcome.Detail)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// discoverPullRequests attaches pull requests to their tasks. A GitHub
// failure is logged, never returned: delivery must not stop because a lookup
// was unavailable.
func (loop *Loop) discoverPullRequests(ctx context.Context) {
	if loop.pullRequests == nil {
		return
	}
	if _, err := loop.pullRequests.Discover(ctx, loop.projectID); err != nil {
		loop.log.Debug("pull request discovery failed",
			"project", loop.projectID, "err", err)
	}
}

// budgetBlocked reports a blocking outcome when the current task has spent
// its budget. It returns nil when work may continue, so the caller reads as
// "carry on unless stopped".
func (loop *Loop) budgetBlocked(task *domain.Task) (*Outcome, error) {
	if loop.budgetGuard == nil || loop.budgets == (policy.Budgets{}) {
		return nil, nil
	}
	executions, err := loop.budgetGuard.ListTaskExecutions(task.ID)
	if err != nil {
		return nil, fmt.Errorf("read execution history for task %d: %w", task.ID, err)
	}
	result := loop.budgets.Evaluate(
		policy.UsageFromExecutions(executions, task.CreatedAt, time.Now().UTC()),
	)
	if !result.Exhausted {
		return nil, nil
	}
	// The event is idempotent by key, so repeated ticks against an exhausted
	// budget record the fact once rather than flooding the audit trail.
	event := domain.NewWorkflowEvent(
		loop.projectID,
		domain.WorkflowSourceWorkflow,
		domain.WorkflowBudgetExhausted,
		result.Reason,
	)
	event.TaskID = &task.ID
	event.IdempotencyKey = fmt.Sprintf("budget-exhausted:%d:%s", task.ID, result.Kind)
	if _, _, err := loop.budgetGuard.AppendWorkflowEvent(event); err != nil {
		return nil, fmt.Errorf("record budget exhaustion: %w", err)
	}
	return &Outcome{
		Action: ActionBlocked,
		TaskID: task.ID,
		Status: task.Status,
		Detail: result.Reason,
	}, nil
}

// publishStatus updates the owner's live message. A Telegram failure is
// logged, never returned: notification problems must not stop delivery.
func (loop *Loop) publishStatus(ctx context.Context, snapshot *project.Snapshot) {
	if loop.status == nil || snapshot == nil {
		return
	}
	status := notify.Status{
		Project:     snapshot.Project,
		CurrentTask: snapshot.CurrentTask,
		Now:         time.Now().UTC(),
	}
	if snapshot.CurrentTask != nil {
		status.Since = snapshot.CurrentTask.UpdatedAt
	}
	outcome, err := loop.status.Publish(ctx, status)
	switch {
	case err != nil:
		loop.log.Debug("status publish failed", "project", loop.projectID, "err", err)
	case outcome != nil && outcome.Err != nil:
		loop.log.Debug("status delivery failed", "project", loop.projectID, "err", outcome.Err)
	}
}

func selectionOutcome(result *project.ReviewResult) *Outcome {
	outcome := &Outcome{Action: ActionSelect}
	switch {
	case result == nil:
		outcome.Action = ActionIdle
		outcome.Detail = "review produced no result"
	case result.AlreadyDone:
		outcome.Action = ActionIdle
		outcome.Detail = "review was already recorded"
	case result.Selection != nil && result.Selection.Task != nil:
		outcome.TaskID = result.Selection.Task.ID
		outcome.Status = result.Selection.Task.Status
		outcome.Detail = "selected " + result.Selection.Task.Title
	case result.NoNextTask:
		outcome.Action = ActionIdle
		outcome.Detail = "manager selected no next task"
	default:
		outcome.Action = ActionIdle
		outcome.Detail = "review recorded without a selection"
	}
	return outcome
}

func initializedTasks(result *project.InitializationResult) int {
	if result == nil || result.Backlog == nil {
		return 0
	}
	return len(result.Backlog.Tasks)
}
