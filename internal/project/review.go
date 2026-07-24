package project

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
	"github.com/eslam-mahmoud/go-ai-agent/internal/workflow"
)

var (
	ErrInvalidManagerOutput = errors.New("invalid manager review output")
	ErrManagerNeedsInput    = errors.New("manager review needs owner input")
	ErrManagerNotCompleted  = errors.New("manager review did not complete")
)

// ManagerRunner is the provider-neutral boundary to Engineering Manager mode.
// It returns output already validated against the manager output schema.
type ManagerRunner interface {
	RunManagerReview(
		ctx context.Context,
		projectID, completedTaskID int64,
	) (json.RawMessage, error)
}

type ReviewStore interface {
	LoadProjectAggregate(projectID int64) (*store.ProjectAggregate, error)
	CreateManagerReview(review *domain.ManagerReview) (*domain.ManagerReview, error)
}

type ReviewResult struct {
	Required    bool
	Discoveries *DiscoveryDecisionResult
	AlreadyDone bool
	Review      *domain.ManagerReview
	Backlog     *BacklogResult
	Selection   *SelectionResult
	Publication *PublicationResult
	NoNextTask  bool
	Question    string
}

// ReviewCoordinator closes Madar's delivery loop. When a task reaches a
// terminal outcome it runs Engineering Manager mode, persists the decision,
// and applies it through the backlog, selection, and publication controllers.
type ReviewCoordinator struct {
	store     ReviewStore
	runner    ManagerRunner
	discovery *DiscoveryController
	backlog   *BacklogController
	selection *SelectionController
	publisher *Publisher
}

// NewReviewCoordinator requires the backlog and selection controllers. The
// publisher is optional so a deployment without GitHub credentials still runs
// the loop and keeps its durable decisions.
func NewReviewCoordinator(
	reviewStore ReviewStore,
	runner ManagerRunner,
	discovery *DiscoveryController,
	backlog *BacklogController,
	selection *SelectionController,
	publisher *Publisher,
) (*ReviewCoordinator, error) {
	switch {
	case reviewStore == nil:
		return nil, errors.New("review coordinator store is required")
	case runner == nil:
		return nil, errors.New("review coordinator manager runner is required")
	case discovery == nil:
		return nil, errors.New("review coordinator discovery controller is required")
	case backlog == nil:
		return nil, errors.New("review coordinator backlog controller is required")
	case selection == nil:
		return nil, errors.New("review coordinator selection controller is required")
	}
	return &ReviewCoordinator{
		store:     reviewStore,
		runner:    runner,
		discovery: discovery,
		backlog:   backlog,
		selection: selection,
		publisher: publisher,
	}, nil
}

// ReviewAfterTask runs one manager cycle for a task that reached a terminal
// status. It is safe to call again after a restart: a review already recorded
// for the task's current state is reported rather than repeated.
func (coordinator *ReviewCoordinator) ReviewAfterTask(
	ctx context.Context,
	projectID, taskID int64,
) (*ReviewResult, error) {
	if projectID <= 0 || taskID <= 0 {
		return nil, fmt.Errorf(
			"%w: project and task IDs must be positive",
			ErrInvalidManagerOutput,
		)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	aggregate, err := coordinator.store.LoadProjectAggregate(projectID)
	if err != nil {
		return nil, err
	}
	if aggregate == nil || aggregate.Project == nil {
		return nil, fmt.Errorf("%w: project aggregate is nil", ErrInconsistentState)
	}
	task := findTask(aggregate.Tasks, taskID)
	if task == nil {
		return nil, fmt.Errorf("%w: task %d in project %d", ErrTaskNotFound, taskID, projectID)
	}
	if !workflow.ManagerReviewRequired(task.Status) {
		return &ReviewResult{}, nil
	}
	if reviewAlreadyRecorded(aggregate.LatestManagerReview, task) {
		return &ReviewResult{
			Required:    true,
			AlreadyDone: true,
			Review:      aggregate.LatestManagerReview,
		}, nil
	}

	raw, err := coordinator.runner.RunManagerReview(ctx, projectID, completedTaskID(task))
	if err != nil {
		return nil, fmt.Errorf("run manager review: %w", err)
	}
	output, err := decodeManagerOutput(raw)
	if err != nil {
		return nil, err
	}
	if output.Status != "completed" {
		return nil, incompleteManagerReview(output)
	}
	review, err := buildManagerReview(aggregate, task, output)
	if err != nil {
		return nil, err
	}
	stored, err := coordinator.store.CreateManagerReview(review)
	if err != nil {
		return nil, err
	}
	result := &ReviewResult{Required: true, Review: stored}
	if err := coordinator.applyDecisions(ctx, projectID, stored, result); err != nil {
		return nil, err
	}
	return result, nil
}

// applyDecisions runs the durable consequences of one persisted review. The
// review itself stays recorded when a stage fails, so the caller may retry.
func (coordinator *ReviewCoordinator) applyDecisions(
	ctx context.Context,
	projectID int64,
	review *domain.ManagerReview,
	result *ReviewResult,
) error {
	// Discovery verdicts land first: backlog changes may depend on work the
	// manager just accepted.
	discoveries, err := coordinator.discovery.ApplyManagerReview(projectID, review.ID)
	if err != nil {
		return fmt.Errorf("apply manager discovery decisions: %w", err)
	}
	result.Discoveries = discoveries

	backlog, err := coordinator.backlog.ApplyManagerReview(projectID, review.ID)
	if err != nil {
		return fmt.Errorf("apply manager backlog changes: %w", err)
	}
	result.Backlog = backlog

	selection, err := coordinator.selection.SelectNextTask(projectID, review.ID)
	switch {
	case errors.Is(err, ErrNoNextTaskSelected):
		result.NoNextTask = true
	case err != nil:
		return fmt.Errorf("select manager next task: %w", err)
	default:
		result.Selection = selection
	}

	if coordinator.publisher == nil {
		return nil
	}
	publication, err := coordinator.publisher.PublishManagerReview(ctx, projectID, review.ID)
	if err != nil {
		return err
	}
	result.Publication = publication
	return nil
}

// reviewAlreadyRecorded decides whether this terminal outcome was already
// evaluated. Completion is terminal and happens once, so naming the task is
// proof on its own; a task can be blocked more than once, so that case falls
// back to comparing the review against the task's last change.
func reviewAlreadyRecorded(review *domain.ManagerReview, task *domain.Task) bool {
	if review == nil {
		return false
	}
	if task.Status == domain.TaskCompleted {
		return review.CompletedTaskID != nil && *review.CompletedTaskID == task.ID
	}
	return !review.ReviewedAt.Before(task.UpdatedAt)
}

func completedTaskID(task *domain.Task) int64 {
	if task.Status == domain.TaskCompleted {
		return task.ID
	}
	// A blocked or cancelled task was never completed, so the manager
	// evaluates the project rather than a delivered result.
	return 0
}

type managerOutput struct {
	Status                     string          `json:"status"`
	Question                   *string         `json:"question"`
	ProjectHealth              string          `json:"project_health"`
	ProgressEstimate           int             `json:"progress_estimate"`
	CompletedTaskDecision      string          `json:"completed_task_decision"`
	ArchitectureReviewRequired bool            `json:"architecture_review_required"`
	HumanApprovalRequired      bool            `json:"human_approval_required"`
	DiscoveryDecisions         json.RawMessage `json:"discovery_decisions"`
	BacklogChanges             json.RawMessage `json:"backlog_changes"`
	NextTask                   *struct {
		IssueNumber int    `json:"issue_number"`
		Reason      string `json:"reason"`
	} `json:"next_task"`
	ReleaseReadiness string `json:"release_readiness"`
	OwnerUpdate      string `json:"owner_update"`
	Summary          string `json:"summary"`
}

func decodeManagerOutput(raw json.RawMessage) (*managerOutput, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var output managerOutput
	if err := decoder.Decode(&output); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidManagerOutput, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: trailing JSON", ErrInvalidManagerOutput)
	}
	if strings.TrimSpace(output.Status) == "" {
		return nil, fmt.Errorf("%w: status is required", ErrInvalidManagerOutput)
	}
	return &output, nil
}

func incompleteManagerReview(output *managerOutput) error {
	question := ""
	if output.Question != nil {
		question = strings.TrimSpace(*output.Question)
	}
	if output.Status == "needs_input" {
		if question == "" {
			return fmt.Errorf("%w: needs_input without a question", ErrInvalidManagerOutput)
		}
		return fmt.Errorf("%w: %s", ErrManagerNeedsInput, question)
	}
	detail := strings.TrimSpace(output.Summary)
	if detail == "" {
		detail = output.Status
	}
	return fmt.Errorf("%w: %s: %s", ErrManagerNotCompleted, output.Status, detail)
}

func buildManagerReview(
	aggregate *store.ProjectAggregate,
	task *domain.Task,
	output *managerOutput,
) (*domain.ManagerReview, error) {
	review := domain.NewManagerReview(aggregate.Project.ID)
	review.ProjectHealth = domain.ProjectHealth(output.ProjectHealth)
	review.ProgressEstimate = output.ProgressEstimate
	review.CompletedTaskDecision = domain.CompletedTaskDecision(output.CompletedTaskDecision)
	review.ArchitectureReviewRequired = output.ArchitectureReviewRequired
	review.HumanApprovalRequired = output.HumanApprovalRequired
	review.ReleaseReadiness = strings.TrimSpace(output.ReleaseReadiness)
	review.OwnerUpdate = strings.TrimSpace(output.OwnerUpdate)
	if len(output.DiscoveryDecisions) > 0 {
		review.DiscoveryDecisions = output.DiscoveryDecisions
	}
	if len(output.BacklogChanges) > 0 {
		review.BacklogChanges = output.BacklogChanges
	}
	if task.Status == domain.TaskCompleted {
		completed := task.ID
		review.CompletedTaskID = &completed
	}
	if output.NextTask != nil {
		next := findTaskByIssue(aggregate.Tasks, output.NextTask.IssueNumber)
		if next == nil {
			return nil, fmt.Errorf(
				"%w: next task issue %d is not in project %d",
				ErrInvalidManagerOutput,
				output.NextTask.IssueNumber,
				aggregate.Project.ID,
			)
		}
		review.NextTaskID = &next.ID
		review.NextTaskIssueNumber = next.IssueNumber
		review.NextTaskReason = strings.TrimSpace(output.NextTask.Reason)
	}
	if err := review.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidManagerOutput, err)
	}
	return review, nil
}

func findTaskByIssue(tasks []*domain.Task, issueNumber int) *domain.Task {
	if issueNumber <= 0 {
		return nil
	}
	for _, task := range tasks {
		if task != nil && task.IssueNumber == issueNumber {
			return task
		}
	}
	return nil
}

var _ ReviewStore = (*store.Store)(nil)
