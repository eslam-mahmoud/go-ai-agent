package project

import (
	"errors"
	"fmt"
	"strings"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
	"github.com/eslam-mahmoud/go-ai-agent/internal/workflow"
)

var (
	ErrNoNextTaskSelected  = errors.New("manager review selected no next task")
	ErrNextTaskNotEligible = errors.New("manager next task is not eligible")
)

type SelectionStore interface {
	LoadProjectAggregate(projectID int64) (*store.ProjectAggregate, error)
	SelectProjectNextTask(
		selection store.ProjectNextTaskSelection,
	) (*domain.Task, bool, error)
}

type SelectionResult struct {
	Task    *domain.Task
	Reason  string
	Applied bool
}

// SelectionController turns one Manager next-task decision into the project's
// single active task. It owns eligibility; the store owns atomicity.
type SelectionController struct {
	store SelectionStore
}

func NewSelectionController(selectionStore SelectionStore) (*SelectionController, error) {
	if selectionStore == nil {
		return nil, errors.New("selection controller store is required")
	}
	return &SelectionController{store: selectionStore}, nil
}

// SelectNextTask promotes the task named by the latest Manager review, with
// that review's reason, after every eligibility gate passes.
func (controller *SelectionController) SelectNextTask(
	projectID, managerReviewID int64,
) (*SelectionResult, error) {
	if projectID <= 0 || managerReviewID <= 0 {
		return nil, fmt.Errorf(
			"%w: project and manager review IDs must be positive",
			ErrNextTaskNotEligible,
		)
	}
	aggregate, err := controller.store.LoadProjectAggregate(projectID)
	if err != nil {
		return nil, err
	}
	if aggregate == nil || aggregate.Project == nil {
		return nil, fmt.Errorf("%w: project aggregate is nil", ErrInconsistentState)
	}
	review := aggregate.LatestManagerReview
	if review == nil || review.ID != managerReviewID || review.ProjectID != projectID {
		return nil, fmt.Errorf(
			"%w: review %d is not the latest review for project %d",
			ErrStaleManagerReview,
			managerReviewID,
			projectID,
		)
	}
	if review.NextTaskID == nil && review.NextTaskIssueNumber <= 0 {
		return nil, fmt.Errorf("%w: review %d", ErrNoNextTaskSelected, managerReviewID)
	}
	reason := strings.TrimSpace(review.NextTaskReason)
	if reason == "" {
		return nil, fmt.Errorf(
			"%w: review %d selected a task without a reason",
			ErrNextTaskNotEligible,
			managerReviewID,
		)
	}
	task, err := resolveNextTask(aggregate.Tasks, review, projectID)
	if err != nil {
		return nil, err
	}
	if err := requireEligibleSelection(aggregate, review, task); err != nil {
		return nil, err
	}

	projectState, _, _ := deriveProjectState(aggregate.Project, task, domain.TaskSelected)
	selected, applied, err := controller.store.SelectProjectNextTask(
		store.ProjectNextTaskSelection{
			ProjectID:       projectID,
			ManagerReviewID: managerReviewID,
			TaskID:          task.ID,
			ExpectedStatus:  task.Status,
			ProjectState:    projectState,
			Reason:          reason,
		},
	)
	if err != nil {
		return nil, err
	}
	if selected == nil {
		return nil, fmt.Errorf(
			"%w: selected task %d disappeared from project %d",
			ErrInconsistentState,
			task.ID,
			projectID,
		)
	}
	return &SelectionResult{Task: selected, Reason: reason, Applied: applied}, nil
}

func resolveNextTask(
	tasks []*domain.Task,
	review *domain.ManagerReview,
	projectID int64,
) (*domain.Task, error) {
	for _, task := range tasks {
		if task == nil {
			continue
		}
		if review.NextTaskID != nil {
			if task.ID == *review.NextTaskID {
				return task, nil
			}
			continue
		}
		if task.IssueNumber > 0 && task.IssueNumber == review.NextTaskIssueNumber {
			return task, nil
		}
	}
	if review.NextTaskID != nil {
		return nil, fmt.Errorf(
			"%w: task %d in project %d",
			ErrTaskNotFound,
			*review.NextTaskID,
			projectID,
		)
	}
	return nil, fmt.Errorf(
		"%w: issue %d in project %d",
		ErrTaskNotFound,
		review.NextTaskIssueNumber,
		projectID,
	)
}

// requireEligibleSelection applies every deterministic selection filter. It
// fails closed so an unresolved gate can never start work.
func requireEligibleSelection(
	aggregate *store.ProjectAggregate,
	review *domain.ManagerReview,
	task *domain.Task,
) error {
	if aggregate.Project.State == domain.ProjectPaused {
		return fmt.Errorf(
			"%w: resume project %d before selecting a task",
			ErrInvalidControl,
			aggregate.Project.ID,
		)
	}
	if review.HumanApprovalRequired {
		return fmt.Errorf(
			"%w: review %d requires human approval first",
			ErrNextTaskNotEligible,
			review.ID,
		)
	}
	if !task.DependenciesSatisfied() {
		return fmt.Errorf(
			"%w: task %d has unresolved dependencies %q",
			ErrNextTaskNotEligible,
			task.ID,
			task.DependencyState,
		)
	}
	for _, other := range aggregate.Tasks {
		if other == nil || other.ID == task.ID || !other.Status.Active() {
			continue
		}
		return fmt.Errorf(
			"%w: task %d is already %q",
			store.ErrActiveProjectTaskExists,
			other.ID,
			other.Status,
		)
	}
	if task.Status == domain.TaskSelected {
		// Already this project's active task; the store decides whether the
		// decision is a durable replay or a conflicting selection.
		return nil
	}
	return workflow.ValidateTaskTransition(workflow.TaskTransition{
		From: task.Status,
		To:   domain.TaskSelected,
		Evidence: workflow.TaskTransitionEvidence{
			ManagerReviewCompleted:  true,
			ArchitectureRiskPending: review.ArchitectureReviewRequired,
		},
	})
}

var _ SelectionStore = (*store.Store)(nil)
