package workflow

import (
	"errors"
	"strings"
	"testing"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

func TestPlanRecoveryDurableStopsAndSafeContinuation(t *testing.T) {
	tests := []struct {
		name   string
		status domain.TaskStatus
		action RecoveryAction
	}{
		{"waiting input", domain.TaskWaitingInput, RecoveryWaitInput},
		{"waiting CI", domain.TaskWaitingCI, RecoveryWaitCI},
		{"blocked", domain.TaskBlocked, RecoveryWaitBlocked},
		{"completed", domain.TaskCompleted, RecoveryManagerReview},
		{"selected", domain.TaskSelected, RecoveryContinue},
		{"queued", domain.TaskQueued, RecoveryIdle},
		{"cancelled", domain.TaskCancelled, RecoveryIdle},
		{"deferred", domain.TaskDeferred, RecoveryIdle},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := recoveryInput(test.status, nil)
			decision, err := PlanRecovery(input)
			if err != nil || decision.Action != test.action || decision.Reason == "" {
				t.Fatalf("decision=%#v error=%v", decision, err)
			}
		})
	}

	input := recoveryInput(domain.TaskSelected, nil)
	input.Project.State = domain.ProjectPaused
	input.Project.PausedFromState = domain.ProjectExecuting
	decision, err := PlanRecovery(input)
	if err != nil || decision.Action != RecoveryHoldPaused {
		t.Fatalf("paused decision=%#v error=%v", decision, err)
	}
	input = RecoveryInput{Project: &domain.Project{
		ID:    1,
		State: domain.ProjectPlanning,
	}}
	decision, err = PlanRecovery(input)
	if err != nil || decision.Action != RecoveryIdle {
		t.Fatalf("no-current decision=%#v error=%v", decision, err)
	}
}

func TestPlanRecoveryActivePhases(t *testing.T) {
	phases := []struct {
		status domain.TaskStatus
		mode   string
	}{
		{domain.TaskPlanning, "planner"},
		{domain.TaskDeveloping, "developer"},
		{domain.TaskReviewing, "reviewer"},
		{domain.TaskFixing, "fixer"},
		{domain.TaskVerifying, "verifier"},
	}
	for _, phase := range phases {
		t.Run(string(phase.status), func(t *testing.T) {
			pending := recoveryExecution(phase.mode, domain.ExecutionPending)
			input := recoveryInput(phase.status, pending)
			decision, err := PlanRecovery(input)
			if err != nil || decision.Action != RecoveryContinue {
				t.Fatalf("pending decision=%#v error=%v", decision, err)
			}

			session := recoveryExecution(phase.mode, domain.ExecutionInterrupted)
			session.ProviderSessionID = "session"
			input = recoveryInput(phase.status, session)
			decision, err = PlanRecovery(input)
			if err != nil || decision.Action != RecoveryQueueRetry ||
				!strings.Contains(decision.Reason, "session") {
				t.Fatalf("session decision=%#v error=%v", decision, err)
			}

			artifact := recoveryExecution(phase.mode, domain.ExecutionInterrupted)
			artifactID := int64(99)
			artifact.InputArtifactID = &artifactID
			input = recoveryInput(phase.status, artifact)
			decision, err = PlanRecovery(input)
			if err != nil || decision.Action != RecoveryQueueRetry ||
				!strings.Contains(decision.Reason, "artifact") {
				t.Fatalf("artifact decision=%#v error=%v", decision, err)
			}

			for _, ambiguous := range []*domain.Execution{
				nil,
				recoveryExecution(phase.mode, domain.ExecutionInterrupted),
				recoveryExecution(phase.mode, domain.ExecutionRunning),
				recoveryExecution(phase.mode, domain.ExecutionCompleted),
				recoveryExecution("wrong-mode", domain.ExecutionPending),
			} {
				input = recoveryInput(phase.status, ambiguous)
				decision, err = PlanRecovery(input)
				if err != nil || decision.Action != RecoveryPauseAmbiguous {
					t.Fatalf(
						"ambiguous execution=%#v decision=%#v error=%v",
						ambiguous,
						decision,
						err,
					)
				}
			}
		})
	}
}

func TestPlanRecoveryRejectsInconsistentOwnership(t *testing.T) {
	if _, err := PlanRecovery(RecoveryInput{}); !errors.Is(err, ErrInvalidRecoveryState) {
		t.Fatalf("nil project error = %v", err)
	}
	input := recoveryInput(domain.TaskDeveloping, nil)
	input.CurrentTask.ProjectID = 2
	if _, err := PlanRecovery(input); !errors.Is(err, ErrInvalidRecoveryState) {
		t.Fatalf("task ownership error = %v", err)
	}
	input = recoveryInput(
		domain.TaskDeveloping,
		recoveryExecution("developer", domain.ExecutionPending),
	)
	input.LatestExecution.ProjectID = 2
	if _, err := PlanRecovery(input); !errors.Is(err, ErrInvalidRecoveryState) {
		t.Fatalf("execution ownership error = %v", err)
	}
	input = recoveryInput(domain.TaskDeveloping, nil)
	input.Project.CurrentTaskID = nil
	if _, err := PlanRecovery(input); !errors.Is(err, ErrInvalidRecoveryState) {
		t.Fatalf("missing current pointer error = %v", err)
	}
}

func recoveryInput(
	status domain.TaskStatus,
	execution *domain.Execution,
) RecoveryInput {
	taskID := int64(2)
	project := &domain.Project{
		ID:            1,
		State:         domain.ProjectExecuting,
		CurrentTaskID: &taskID,
	}
	task := &domain.Task{ID: taskID, ProjectID: project.ID, Status: status}
	if execution != nil {
		execution.ID = 3
		execution.ProjectID = project.ID
		execution.TaskID = task.ID
	}
	return RecoveryInput{
		Project:         project,
		CurrentTask:     task,
		LatestExecution: execution,
	}
}

func recoveryExecution(
	mode string,
	status domain.ExecutionStatus,
) *domain.Execution {
	return &domain.Execution{Mode: mode, Status: status}
}
