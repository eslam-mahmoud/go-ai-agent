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
	ErrTaskNotFound      = errors.New("controller task not found")
	ErrTaskOwnership     = errors.New("controller task belongs to another project")
	ErrInconsistentState = errors.New("inconsistent project aggregate")
)

type Store interface {
	LoadProjectAggregate(projectID int64) (*store.ProjectAggregate, error)
	GetProjectTaskByID(id int64) (*domain.Task, error)
	ApplyProjectTaskTransition(update store.ProjectTaskTransitionUpdate) error
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
