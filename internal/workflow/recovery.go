package workflow

import (
	"errors"
	"fmt"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

type RecoveryAction string

const (
	RecoveryIdle           RecoveryAction = "idle"
	RecoveryHoldPaused     RecoveryAction = "hold-paused"
	RecoveryContinue       RecoveryAction = "continue-workflow"
	RecoveryWaitInput      RecoveryAction = "wait-input"
	RecoveryWaitCI         RecoveryAction = "wait-ci"
	RecoveryWaitBlocked    RecoveryAction = "wait-blocked"
	RecoveryManagerReview  RecoveryAction = "manager-review"
	RecoveryQueueRetry     RecoveryAction = "queue-retry"
	RecoveryPauseAmbiguous RecoveryAction = "pause-ambiguous"
)

var ErrInvalidRecoveryState = errors.New("invalid workflow recovery state")

type RecoveryInput struct {
	Project         *domain.Project
	CurrentTask     *domain.Task
	LatestExecution *domain.Execution
}

type RecoveryDecision struct {
	Action RecoveryAction
	Reason string
}

// PlanRecovery is a pure deterministic decision over durable state. It never
// assumes a provider process survived restart and never reruns completed work
// without resumable session or input-artifact evidence.
func PlanRecovery(input RecoveryInput) (RecoveryDecision, error) {
	if input.Project == nil {
		return RecoveryDecision{}, fmt.Errorf("%w: project is nil", ErrInvalidRecoveryState)
	}
	if input.Project.State == domain.ProjectPaused {
		return recoveryDecision(RecoveryHoldPaused, "project is explicitly paused"), nil
	}
	if input.CurrentTask == nil {
		if input.Project.CurrentTaskID != nil {
			return RecoveryDecision{}, fmt.Errorf(
				"%w: current task %d is missing",
				ErrInvalidRecoveryState,
				*input.Project.CurrentTaskID,
			)
		}
		return recoveryDecision(RecoveryIdle, "project has no current task"), nil
	}
	task := input.CurrentTask
	if task.ProjectID != input.Project.ID ||
		input.Project.CurrentTaskID == nil ||
		*input.Project.CurrentTaskID != task.ID {
		return RecoveryDecision{}, fmt.Errorf(
			"%w: task %d is not project %d's current task",
			ErrInvalidRecoveryState,
			task.ID,
			input.Project.ID,
		)
	}
	execution := input.LatestExecution
	if execution != nil &&
		(execution.ProjectID != input.Project.ID || execution.TaskID != task.ID) {
		return RecoveryDecision{}, fmt.Errorf(
			"%w: execution %d does not belong to current task %d",
			ErrInvalidRecoveryState,
			execution.ID,
			task.ID,
		)
	}

	switch task.Status {
	case domain.TaskWaitingInput:
		return recoveryDecision(RecoveryWaitInput, "task is waiting for human input"), nil
	case domain.TaskWaitingCI:
		return recoveryDecision(RecoveryWaitCI, "task is waiting for CI reconciliation"), nil
	case domain.TaskBlocked:
		return recoveryDecision(RecoveryWaitBlocked, "task has a durable blocker"), nil
	case domain.TaskCompleted:
		return recoveryDecision(
			RecoveryManagerReview,
			"completed task requires engineering-manager review",
		), nil
	case domain.TaskSelected:
		if execution == nil {
			return recoveryDecision(
				RecoveryContinue,
				"selected task has not started a provider execution",
			), nil
		}
		return recoveryDecision(
			RecoveryPauseAmbiguous,
			"selected task unexpectedly has execution history",
		), nil
	case domain.TaskPlanning,
		domain.TaskDeveloping,
		domain.TaskReviewing,
		domain.TaskFixing,
		domain.TaskVerifying:
		return planActivePhaseRecovery(task, execution), nil
	case domain.TaskProposed,
		domain.TaskQueued,
		domain.TaskCancelled,
		domain.TaskDeferred:
		return recoveryDecision(RecoveryIdle, "task status does not require active recovery"), nil
	default:
		return RecoveryDecision{}, fmt.Errorf(
			"%w: unknown task status %q",
			ErrInvalidRecoveryState,
			task.Status,
		)
	}
}

func planActivePhaseRecovery(
	task *domain.Task,
	execution *domain.Execution,
) RecoveryDecision {
	if execution == nil {
		return recoveryDecision(
			RecoveryPauseAmbiguous,
			"active task has no durable execution record",
		)
	}
	expected, ok := recoveryModeTaskStatus(execution.Mode)
	if !ok || expected != task.Status {
		return recoveryDecision(
			RecoveryPauseAmbiguous,
			fmt.Sprintf(
				"latest mode %q does not match task phase %q",
				execution.Mode,
				task.Status,
			),
		)
	}
	switch execution.Status {
	case domain.ExecutionPending:
		return recoveryDecision(
			RecoveryContinue,
			"latest execution is durably pending",
		)
	case domain.ExecutionInterrupted:
		if execution.ProviderSessionID != "" {
			return recoveryDecision(
				RecoveryQueueRetry,
				"interrupted execution has a resumable provider session",
			)
		}
		if execution.InputArtifactID != nil {
			return recoveryDecision(
				RecoveryQueueRetry,
				"interrupted execution has a durable input artifact",
			)
		}
		return recoveryDecision(
			RecoveryPauseAmbiguous,
			"interrupted execution lacks resumable session and input artifact",
		)
	case domain.ExecutionRunning:
		return recoveryDecision(
			RecoveryPauseAmbiguous,
			"running execution was not normalized during startup recovery",
		)
	default:
		return recoveryDecision(
			RecoveryPauseAmbiguous,
			fmt.Sprintf("active task has terminal execution status %q", execution.Status),
		)
	}
}

func recoveryModeTaskStatus(mode string) (domain.TaskStatus, bool) {
	switch ModeName(mode) {
	case ModePlanner:
		return domain.TaskPlanning, true
	case ModeDeveloper:
		return domain.TaskDeveloping, true
	case ModeReviewer:
		return domain.TaskReviewing, true
	case ModeFixer:
		return domain.TaskFixing, true
	case ModeVerifier:
		return domain.TaskVerifying, true
	default:
		if mode == "legacy-developer" {
			return domain.TaskDeveloping, true
		}
		return "", false
	}
}

func recoveryDecision(action RecoveryAction, reason string) RecoveryDecision {
	return RecoveryDecision{Action: action, Reason: reason}
}
