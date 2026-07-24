package workflow

import (
	"context"
	"errors"
	"fmt"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

type ModeName string

const (
	ModePlanner   ModeName = "planner"
	ModeDeveloper ModeName = "developer"
	ModeReviewer  ModeName = "reviewer"
	ModeFixer     ModeName = "fixer"
	ModeVerifier  ModeName = "verifier"
)

type ModeStatus string

const (
	ModeCompleted  ModeStatus = "completed"
	ModeNeedsInput ModeStatus = "needs-input"
	ModeBlocked    ModeStatus = "blocked"
	ModeFailed     ModeStatus = "failed"
	ModeCancelled  ModeStatus = "cancelled"
)

var (
	ErrUnsupportedFeatureState = errors.New("unsupported feature workflow state")
	ErrInvalidModeOutcome      = errors.New("invalid mode outcome")
	ErrFeatureStepLimit        = errors.New("feature workflow step limit reached")
)

type ModeRequest struct {
	ProjectID int64
	TaskID    int64
	Mode      ModeName
	Status    domain.TaskStatus
}

type ModeOutcome struct {
	Status           ModeStatus
	BlockingFindings bool
	Summary          string
}

type ModeRunner interface {
	RunMode(ctx context.Context, request ModeRequest) (ModeOutcome, error)
}

// TaskController is implemented by project.Controller without exposing store
// or aggregate types to the workflow package.
type TaskController interface {
	TaskStatus(projectID, taskID int64) (domain.TaskStatus, error)
	ApplyTaskTransition(
		projectID, taskID int64,
		target domain.TaskStatus,
		evidence TaskTransitionEvidence,
	) (domain.TaskStatus, error)
}

type FeatureOptions struct {
	CIRequired bool
	MaxSteps   int
}

type FeatureResult struct {
	FinalStatus domain.TaskStatus
	ModesRun    []ModeName
}

type FeatureWorkflow struct {
	controller TaskController
	runner     ModeRunner
	options    FeatureOptions
}

func NewFeatureWorkflow(
	controller TaskController,
	runner ModeRunner,
	options FeatureOptions,
) (*FeatureWorkflow, error) {
	if controller == nil {
		return nil, errors.New("feature workflow controller is required")
	}
	if runner == nil {
		return nil, errors.New("feature workflow mode runner is required")
	}
	if options.MaxSteps < 0 {
		return nil, errors.New("feature workflow max steps cannot be negative")
	}
	if options.MaxSteps == 0 {
		options.MaxSteps = 100
	}
	return &FeatureWorkflow{controller: controller, runner: runner, options: options}, nil
}

func (workflow *FeatureWorkflow) Run(
	ctx context.Context,
	projectID, taskID int64,
) (*FeatureResult, error) {
	status, err := workflow.controller.TaskStatus(projectID, taskID)
	if err != nil {
		return nil, err
	}
	result := &FeatureResult{}
	for step := 0; step < workflow.options.MaxSteps; step++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if featureWorkflowTerminal(status) {
			result.FinalStatus = status
			return result, nil
		}

		switch status {
		case domain.TaskSelected:
			status, err = workflow.transition(
				projectID, taskID, domain.TaskPlanning, TaskTransitionEvidence{},
			)
		case domain.TaskPlanning:
			status, err = workflow.runMode(
				ctx, projectID, taskID, status, ModePlanner, result,
			)
		case domain.TaskDeveloping:
			status, err = workflow.runMode(
				ctx, projectID, taskID, status, ModeDeveloper, result,
			)
		case domain.TaskReviewing:
			status, err = workflow.runMode(
				ctx, projectID, taskID, status, ModeReviewer, result,
			)
		case domain.TaskFixing:
			status, err = workflow.runMode(
				ctx, projectID, taskID, status, ModeFixer, result,
			)
		case domain.TaskVerifying:
			status, err = workflow.runMode(
				ctx, projectID, taskID, status, ModeVerifier, result,
			)
		default:
			return nil, fmt.Errorf("%w: %s", ErrUnsupportedFeatureState, status)
		}
		if err != nil {
			return nil, err
		}
	}
	return nil, fmt.Errorf("%w: maximum %d", ErrFeatureStepLimit, workflow.options.MaxSteps)
}

func (workflow *FeatureWorkflow) runMode(
	ctx context.Context,
	projectID, taskID int64,
	current domain.TaskStatus,
	mode ModeName,
	result *FeatureResult,
) (domain.TaskStatus, error) {
	outcome, err := workflow.runner.RunMode(ctx, ModeRequest{
		ProjectID: projectID,
		TaskID:    taskID,
		Mode:      mode,
		Status:    current,
	})
	if err != nil {
		return current, fmt.Errorf("run %s mode: %w", mode, err)
	}
	result.ModesRun = append(result.ModesRun, mode)

	switch outcome.Status {
	case ModeCancelled:
		return workflow.transition(
			projectID, taskID, domain.TaskCancelled, TaskTransitionEvidence{},
		)
	case ModeNeedsInput:
		if current == domain.TaskPlanning || current == domain.TaskDeveloping {
			return workflow.transition(
				projectID, taskID, domain.TaskWaitingInput, TaskTransitionEvidence{},
			)
		}
		return workflow.transition(
			projectID, taskID, domain.TaskBlocked, TaskTransitionEvidence{},
		)
	case ModeBlocked:
		return workflow.transition(
			projectID, taskID, domain.TaskBlocked, TaskTransitionEvidence{},
		)
	case ModeFailed:
		switch current {
		case domain.TaskReviewing:
			return workflow.transition(
				projectID,
				taskID,
				domain.TaskFixing,
				TaskTransitionEvidence{BlockingReviewFindings: true},
			)
		case domain.TaskVerifying:
			return workflow.transition(
				projectID,
				taskID,
				domain.TaskFixing,
				TaskTransitionEvidence{VerificationFailed: true},
			)
		default:
			return workflow.transition(
				projectID, taskID, domain.TaskBlocked, TaskTransitionEvidence{},
			)
		}
	case ModeCompleted:
	default:
		return current, fmt.Errorf("%w: %q from %s", ErrInvalidModeOutcome, outcome.Status, mode)
	}

	switch current {
	case domain.TaskPlanning:
		return workflow.transition(
			projectID,
			taskID,
			domain.TaskDeveloping,
			TaskTransitionEvidence{PlanCompleted: true},
		)
	case domain.TaskDeveloping:
		return workflow.transition(
			projectID, taskID, domain.TaskReviewing, TaskTransitionEvidence{},
		)
	case domain.TaskReviewing:
		if outcome.BlockingFindings {
			return workflow.transition(
				projectID,
				taskID,
				domain.TaskFixing,
				TaskTransitionEvidence{BlockingReviewFindings: true},
			)
		}
		return workflow.transition(
			projectID, taskID, domain.TaskVerifying, TaskTransitionEvidence{},
		)
	case domain.TaskFixing:
		return workflow.transition(
			projectID, taskID, domain.TaskReviewing, TaskTransitionEvidence{},
		)
	case domain.TaskVerifying:
		if workflow.options.CIRequired {
			return workflow.transition(
				projectID,
				taskID,
				domain.TaskWaitingCI,
				TaskTransitionEvidence{VerificationSucceeded: true, CIRequired: true},
			)
		}
		return workflow.transition(
			projectID,
			taskID,
			domain.TaskCompleted,
			TaskTransitionEvidence{VerificationSucceeded: true},
		)
	default:
		return current, fmt.Errorf("%w: %s", ErrUnsupportedFeatureState, current)
	}
}

func (workflow *FeatureWorkflow) transition(
	projectID, taskID int64,
	target domain.TaskStatus,
	evidence TaskTransitionEvidence,
) (domain.TaskStatus, error) {
	return workflow.controller.ApplyTaskTransition(
		projectID,
		taskID,
		target,
		evidence,
	)
}

func featureWorkflowTerminal(status domain.TaskStatus) bool {
	switch status {
	case domain.TaskWaitingInput,
		domain.TaskWaitingCI,
		domain.TaskBlocked,
		domain.TaskCompleted,
		domain.TaskCancelled,
		domain.TaskDeferred:
		return true
	default:
		return false
	}
}
