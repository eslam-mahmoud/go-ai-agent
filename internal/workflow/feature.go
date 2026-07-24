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
	ModeManager   ModeName = "manager"
	ModeArchitect ModeName = "architect"
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
	ErrInvalidReviewFixCount   = errors.New("invalid persisted review/fix cycle count")
)

const DefaultMaxReviewFixCycles = 2

type ModeRequest struct {
	ProjectID    int64
	TaskID       int64
	Mode         ModeName
	Status       domain.TaskStatus
	FixCycle     int
	MaxFixCycles int
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

// ReviewFixCycleCounter lets production controllers restore the repair budget
// from durable execution history. Controllers without this optional interface
// begin at zero for compatibility with isolated workflow uses.
type ReviewFixCycleCounter interface {
	ReviewFixCycleCount(projectID, taskID int64) (int, error)
}

type FeatureOptions struct {
	CIRequired         bool
	MaxSteps           int
	MaxReviewFixCycles int
}

type FeatureResult struct {
	FinalStatus           domain.TaskStatus
	ModesRun              []ModeName
	ReviewFixCycles       int
	MaxReviewFixCycles    int
	ReviewFixLimitReached bool
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
	if options.MaxReviewFixCycles < 0 {
		return nil, errors.New("feature workflow max review/fix cycles cannot be negative")
	}
	if options.MaxSteps == 0 {
		options.MaxSteps = 100
	}
	if options.MaxReviewFixCycles == 0 {
		options.MaxReviewFixCycles = DefaultMaxReviewFixCycles
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
	result := &FeatureResult{MaxReviewFixCycles: workflow.options.MaxReviewFixCycles}
	if counter, ok := workflow.controller.(ReviewFixCycleCounter); ok {
		result.ReviewFixCycles, err = counter.ReviewFixCycleCount(projectID, taskID)
		if err != nil {
			return nil, fmt.Errorf("load review/fix cycle count: %w", err)
		}
		if result.ReviewFixCycles < 0 {
			return nil, fmt.Errorf(
				"%w: %d",
				ErrInvalidReviewFixCount,
				result.ReviewFixCycles,
			)
		}
	}
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
			if workflow.fixLimitReached(result) {
				status, err = workflow.blockForFixLimit(projectID, taskID, result)
			} else {
				status, err = workflow.runMode(
					ctx, projectID, taskID, status, ModeFixer, result,
				)
			}
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
	fixCycle := result.ReviewFixCycles
	if mode == ModeFixer {
		fixCycle++
	}
	outcome, err := workflow.runner.RunMode(ctx, ModeRequest{
		ProjectID:    projectID,
		TaskID:       taskID,
		Mode:         mode,
		Status:       current,
		FixCycle:     fixCycle,
		MaxFixCycles: workflow.options.MaxReviewFixCycles,
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
			return workflow.routeToFix(
				projectID, taskID, result,
				TaskTransitionEvidence{BlockingReviewFindings: true},
			)
		case domain.TaskVerifying:
			return workflow.routeToFix(
				projectID, taskID, result,
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
			return workflow.routeToFix(
				projectID, taskID, result,
				TaskTransitionEvidence{BlockingReviewFindings: true},
			)
		}
		return workflow.transition(
			projectID, taskID, domain.TaskVerifying, TaskTransitionEvidence{},
		)
	case domain.TaskFixing:
		result.ReviewFixCycles = fixCycle
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

func (workflow *FeatureWorkflow) routeToFix(
	projectID, taskID int64,
	result *FeatureResult,
	evidence TaskTransitionEvidence,
) (domain.TaskStatus, error) {
	if workflow.fixLimitReached(result) {
		return workflow.blockForFixLimit(projectID, taskID, result)
	}
	return workflow.transition(projectID, taskID, domain.TaskFixing, evidence)
}

func (workflow *FeatureWorkflow) fixLimitReached(result *FeatureResult) bool {
	return result.ReviewFixCycles >= workflow.options.MaxReviewFixCycles
}

func (workflow *FeatureWorkflow) blockForFixLimit(
	projectID, taskID int64,
	result *FeatureResult,
) (domain.TaskStatus, error) {
	result.ReviewFixLimitReached = true
	return workflow.transition(
		projectID,
		taskID,
		domain.TaskBlocked,
		TaskTransitionEvidence{ReviewFixLimitReached: true},
	)
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
