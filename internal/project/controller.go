// Package project owns the durable v2 project aggregate boundary.
package project

import (
	"errors"
	"fmt"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
	"github.com/eslam-mahmoud/go-ai-agent/internal/workflow"
)

var (
	ErrTaskNotFound          = errors.New("controller task not found")
	ErrTaskOwnership         = errors.New("controller task belongs to another project")
	ErrInconsistentState     = errors.New("inconsistent project aggregate")
	ErrInvalidControl        = errors.New("invalid project control action")
	ErrNoCurrentTask         = errors.New("project has no current task")
	ErrExecutionNotRetryable = errors.New("latest execution is not retryable")
)

type Store interface {
	LoadProjectAggregate(projectID int64) (*store.ProjectAggregate, error)
	GetProjectTaskByID(id int64) (*domain.Task, error)
	ApplyProjectTaskTransition(update store.ProjectTaskTransitionUpdate) error
	PauseProject(projectID int64, expected domain.ProjectState) error
	ResumeProject(projectID int64, target domain.ProjectState) error
	CancelProjectTask(update store.ProjectTaskCancellation) error
	GetLatestTaskExecution(taskID int64) (*domain.Execution, error)
	RetryProjectTaskExecution(
		update store.ProjectExecutionRetry,
	) (*domain.Execution, error)
}

type Snapshot struct {
	Project               *domain.Project
	Tasks                 []*domain.Task
	CurrentTask           *domain.Task
	LatestManagerReview   *domain.ManagerReview
	ManagerReviewRequired bool
}

type Controller struct {
	store Store
}

func NewController(projectStore Store) (*Controller, error) {
	if projectStore == nil {
		return nil, errors.New("project controller store is required")
	}
	return &Controller{store: projectStore}, nil
}

func (controller *Controller) Snapshot(projectID int64) (*Snapshot, error) {
	aggregate, err := controller.store.LoadProjectAggregate(projectID)
	if err != nil {
		return nil, err
	}
	if aggregate == nil || aggregate.Project == nil {
		return nil, fmt.Errorf("%w: project %d is missing", ErrInconsistentState, projectID)
	}
	snapshot := &Snapshot{
		Project:             aggregate.Project,
		Tasks:               aggregate.Tasks,
		LatestManagerReview: aggregate.LatestManagerReview,
	}
	if aggregate.Project.CurrentTaskID != nil {
		for _, task := range aggregate.Tasks {
			if task.ID == *aggregate.Project.CurrentTaskID {
				snapshot.CurrentTask = task
				break
			}
		}
		if snapshot.CurrentTask == nil {
			return nil, fmt.Errorf(
				"%w: current task %d is not in project %d",
				ErrInconsistentState,
				*aggregate.Project.CurrentTaskID,
				projectID,
			)
		}
	}
	snapshot.ManagerReviewRequired = managerReviewRequired(snapshot)
	return snapshot, nil
}

func (controller *Controller) TransitionTask(
	projectID, taskID int64,
	target domain.TaskStatus,
	evidence workflow.TaskTransitionEvidence,
) (*Snapshot, error) {
	snapshot, err := controller.Snapshot(projectID)
	if err != nil {
		return nil, err
	}
	task := findTask(snapshot.Tasks, taskID)
	if task == nil {
		stored, lookupErr := controller.store.GetProjectTaskByID(taskID)
		if lookupErr != nil {
			return nil, lookupErr
		}
		if stored != nil && stored.ProjectID != projectID {
			return nil, fmt.Errorf(
				"%w: task %d belongs to project %d, not %d",
				ErrTaskOwnership,
				taskID,
				stored.ProjectID,
				projectID,
			)
		}
		return nil, fmt.Errorf("%w: task %d in project %d", ErrTaskNotFound, taskID, projectID)
	}
	if err := workflow.ValidateTaskTransition(workflow.TaskTransition{
		From:     task.Status,
		To:       target,
		Evidence: evidence,
	}); err != nil {
		return nil, err
	}

	projectState, setCurrent, currentTaskID := deriveProjectState(
		snapshot.Project,
		task,
		target,
	)
	if err := controller.store.ApplyProjectTaskTransition(store.ProjectTaskTransitionUpdate{
		ProjectID:      projectID,
		TaskID:         taskID,
		ExpectedStatus: task.Status,
		NewStatus:      target,
		ProjectState:   projectState,
		SetCurrentTask: setCurrent,
		CurrentTaskID:  currentTaskID,
	}); err != nil {
		return nil, err
	}
	return controller.Snapshot(projectID)
}

// Pause durably suspends a non-terminal project and atomically interrupts its
// running provider execution. The current task phase remains unchanged so a
// later retry can resume the same unit of work.
func (controller *Controller) Pause(projectID int64) (*Snapshot, error) {
	snapshot, err := controller.Snapshot(projectID)
	if err != nil {
		return nil, err
	}
	switch snapshot.Project.State {
	case domain.ProjectPaused:
		return nil, fmt.Errorf("%w: project %d is already paused", ErrInvalidControl, projectID)
	case domain.ProjectCompleted:
		return nil, fmt.Errorf("%w: completed project %d cannot be paused", ErrInvalidControl, projectID)
	}
	if err := controller.store.PauseProject(projectID, snapshot.Project.State); err != nil {
		return nil, err
	}
	return controller.Snapshot(projectID)
}

// Resume restores the exact state persisted when Pause succeeded.
func (controller *Controller) Resume(projectID int64) (*Snapshot, error) {
	snapshot, err := controller.Snapshot(projectID)
	if err != nil {
		return nil, err
	}
	if snapshot.Project.State != domain.ProjectPaused ||
		!snapshot.Project.PausedFromState.Valid() ||
		snapshot.Project.PausedFromState == domain.ProjectPaused {
		return nil, fmt.Errorf("%w: project %d is not resumable", ErrInvalidControl, projectID)
	}
	if err := controller.store.ResumeProject(
		projectID,
		snapshot.Project.PausedFromState,
	); err != nil {
		return nil, err
	}
	return controller.Snapshot(projectID)
}

// Cancel cancels the current task through the canonical task transition
// validator and atomically cancels any pending/running execution.
func (controller *Controller) Cancel(projectID int64) (*Snapshot, error) {
	snapshot, err := controller.Snapshot(projectID)
	if err != nil {
		return nil, err
	}
	if snapshot.CurrentTask == nil {
		return nil, fmt.Errorf("%w: project %d", ErrNoCurrentTask, projectID)
	}
	if err := workflow.ValidateTaskTransition(workflow.TaskTransition{
		From: snapshot.CurrentTask.Status,
		To:   domain.TaskCancelled,
	}); err != nil {
		return nil, err
	}
	if err := controller.store.CancelProjectTask(store.ProjectTaskCancellation{
		ProjectID:            projectID,
		TaskID:               snapshot.CurrentTask.ID,
		ExpectedProjectState: snapshot.Project.State,
		ExpectedTaskStatus:   snapshot.CurrentTask.Status,
	}); err != nil {
		return nil, err
	}
	return controller.Snapshot(projectID)
}

// Retry creates a new pending attempt for the latest failed, cancelled, or
// interrupted execution. Historical attempts remain immutable.
func (controller *Controller) Retry(
	projectID int64,
) (*Snapshot, *domain.Execution, error) {
	snapshot, err := controller.Snapshot(projectID)
	if err != nil {
		return nil, nil, err
	}
	if snapshot.Project.State == domain.ProjectPaused {
		return nil, nil, fmt.Errorf(
			"%w: resume project %d before retrying",
			ErrInvalidControl,
			projectID,
		)
	}
	if snapshot.CurrentTask == nil {
		return nil, nil, fmt.Errorf("%w: project %d", ErrNoCurrentTask, projectID)
	}
	previous, err := controller.store.GetLatestTaskExecution(snapshot.CurrentTask.ID)
	if err != nil {
		return nil, nil, err
	}
	if previous == nil || !retryableExecutionStatus(previous.Status) {
		return nil, nil, fmt.Errorf(
			"%w: task %d",
			ErrExecutionNotRetryable,
			snapshot.CurrentTask.ID,
		)
	}
	target, ok := retryTaskStatus(previous.Mode)
	if !ok {
		return nil, nil, fmt.Errorf(
			"%w: execution mode %q has no task phase",
			ErrExecutionNotRetryable,
			previous.Mode,
		)
	}
	if snapshot.CurrentTask.Status != target {
		if snapshot.CurrentTask.Status != domain.TaskBlocked {
			return nil, nil, fmt.Errorf(
				"%w: task %d is %q, retry requires %q or blocked",
				ErrExecutionNotRetryable,
				snapshot.CurrentTask.ID,
				snapshot.CurrentTask.Status,
				target,
			)
		}
		if err := workflow.ValidateTaskTransition(workflow.TaskTransition{
			From: snapshot.CurrentTask.Status,
			To:   target,
			Evidence: workflow.TaskTransitionEvidence{
				BlockerResolved: true,
				PlanCompleted:   target == domain.TaskDeveloping,
			},
		}); err != nil {
			return nil, nil, err
		}
	}
	retry, err := controller.store.RetryProjectTaskExecution(
		store.ProjectExecutionRetry{
			ProjectID:            projectID,
			TaskID:               snapshot.CurrentTask.ID,
			ExecutionID:          previous.ID,
			ExpectedProjectState: snapshot.Project.State,
			ExpectedTaskStatus:   snapshot.CurrentTask.Status,
			NewTaskStatus:        target,
		},
	)
	if err != nil {
		return nil, nil, err
	}
	updated, err := controller.Snapshot(projectID)
	if err != nil {
		return nil, nil, err
	}
	return updated, retry, nil
}

// TaskStatus and ApplyTaskTransition form the narrow provider-neutral boundary
// consumed by workflow.FeatureWorkflow.
func (controller *Controller) TaskStatus(projectID, taskID int64) (domain.TaskStatus, error) {
	snapshot, err := controller.Snapshot(projectID)
	if err != nil {
		return "", err
	}
	if snapshot.Project.State == domain.ProjectPaused {
		return "", fmt.Errorf("%w: project %d", store.ErrProjectPaused, projectID)
	}
	task := findTask(snapshot.Tasks, taskID)
	if task == nil {
		return "", fmt.Errorf("%w: task %d in project %d", ErrTaskNotFound, taskID, projectID)
	}
	return task.Status, nil
}

func retryableExecutionStatus(status domain.ExecutionStatus) bool {
	switch status {
	case domain.ExecutionFailed,
		domain.ExecutionCancelled,
		domain.ExecutionInterrupted:
		return true
	default:
		return false
	}
}

func retryTaskStatus(mode string) (domain.TaskStatus, bool) {
	switch mode {
	case "planner":
		return domain.TaskPlanning, true
	case "developer", "legacy-developer":
		return domain.TaskDeveloping, true
	case "reviewer":
		return domain.TaskReviewing, true
	case "fixer":
		return domain.TaskFixing, true
	case "verifier":
		return domain.TaskVerifying, true
	default:
		return "", false
	}
}

func (controller *Controller) ApplyTaskTransition(
	projectID, taskID int64,
	target domain.TaskStatus,
	evidence workflow.TaskTransitionEvidence,
) (domain.TaskStatus, error) {
	snapshot, err := controller.TransitionTask(projectID, taskID, target, evidence)
	if err != nil {
		return "", err
	}
	task := findTask(snapshot.Tasks, taskID)
	if task == nil {
		return "", fmt.Errorf(
			"%w: transitioned task %d disappeared from project %d",
			ErrInconsistentState,
			taskID,
			projectID,
		)
	}
	return task.Status, nil
}

func deriveProjectState(
	project *domain.Project,
	task *domain.Task,
	target domain.TaskStatus,
) (domain.ProjectState, bool, *int64) {
	taskID := task.ID
	switch target {
	case domain.TaskSelected,
		domain.TaskPlanning,
		domain.TaskWaitingInput,
		domain.TaskDeveloping,
		domain.TaskReviewing,
		domain.TaskFixing,
		domain.TaskVerifying,
		domain.TaskWaitingCI:
		return domain.ProjectExecuting, true, &taskID
	case domain.TaskBlocked:
		return domain.ProjectBlocked, true, &taskID
	case domain.TaskCompleted:
		// Keep the completed task selected until the required manager review
		// evaluates it and chooses the next project action.
		return domain.ProjectExecuting, true, &taskID
	case domain.TaskQueued, domain.TaskCancelled, domain.TaskDeferred:
		if project.CurrentTaskID != nil && *project.CurrentTaskID == task.ID {
			return domain.ProjectPlanning, true, nil
		}
		return project.State, false, nil
	default:
		return project.State, false, nil
	}
}

func managerReviewRequired(snapshot *Snapshot) bool {
	if snapshot.CurrentTask == nil || snapshot.CurrentTask.Status != domain.TaskCompleted {
		return false
	}
	review := snapshot.LatestManagerReview
	if review == nil ||
		review.CompletedTaskID == nil ||
		*review.CompletedTaskID != snapshot.CurrentTask.ID {
		return true
	}
	return review.ReviewedAt.Before(snapshot.CurrentTask.UpdatedAt)
}

func findTask(tasks []*domain.Task, taskID int64) *domain.Task {
	for _, task := range tasks {
		if task.ID == taskID {
			return task
		}
	}
	return nil
}
