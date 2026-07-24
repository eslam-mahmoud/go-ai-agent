package workflow

import (
	"errors"
	"reflect"
	"testing"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

func TestTaskTransitionGraphExhaustively(t *testing.T) {
	expected := map[domain.TaskStatus][]domain.TaskStatus{
		domain.TaskProposed: {
			domain.TaskQueued, domain.TaskBlocked, domain.TaskCancelled, domain.TaskDeferred,
		},
		domain.TaskQueued: {
			domain.TaskSelected, domain.TaskBlocked, domain.TaskCancelled, domain.TaskDeferred,
		},
		domain.TaskSelected: {
			domain.TaskPlanning, domain.TaskBlocked, domain.TaskCancelled, domain.TaskDeferred,
		},
		domain.TaskPlanning: {
			domain.TaskWaitingInput, domain.TaskDeveloping, domain.TaskBlocked,
			domain.TaskCancelled, domain.TaskDeferred,
		},
		domain.TaskWaitingInput: {
			domain.TaskPlanning, domain.TaskDeveloping, domain.TaskBlocked,
			domain.TaskCancelled, domain.TaskDeferred,
		},
		domain.TaskDeveloping: {
			domain.TaskWaitingInput, domain.TaskReviewing, domain.TaskBlocked,
			domain.TaskCancelled, domain.TaskDeferred,
		},
		domain.TaskReviewing: {
			domain.TaskFixing, domain.TaskVerifying, domain.TaskBlocked,
			domain.TaskCancelled, domain.TaskDeferred,
		},
		domain.TaskFixing: {
			domain.TaskReviewing, domain.TaskBlocked, domain.TaskCancelled, domain.TaskDeferred,
		},
		domain.TaskVerifying: {
			domain.TaskFixing, domain.TaskWaitingCI, domain.TaskCompleted,
			domain.TaskBlocked, domain.TaskCancelled, domain.TaskDeferred,
		},
		domain.TaskWaitingCI: {
			domain.TaskFixing, domain.TaskCompleted, domain.TaskBlocked,
			domain.TaskCancelled, domain.TaskDeferred,
		},
		domain.TaskBlocked: {
			domain.TaskQueued, domain.TaskSelected, domain.TaskPlanning,
			domain.TaskWaitingInput, domain.TaskDeveloping, domain.TaskReviewing,
			domain.TaskFixing, domain.TaskVerifying, domain.TaskWaitingCI,
			domain.TaskCancelled, domain.TaskDeferred,
		},
		domain.TaskCompleted: {},
		domain.TaskCancelled: {},
		domain.TaskDeferred: {
			domain.TaskQueued, domain.TaskCancelled,
		},
	}

	for _, from := range taskStatusOrder {
		gotTargets, err := AllowedTaskTargets(from)
		if err != nil {
			t.Fatalf("AllowedTaskTargets(%q): %v", from, err)
		}
		if !reflect.DeepEqual(gotTargets, expected[from]) {
			t.Errorf("targets from %q = %v, want %v", from, gotTargets, expected[from])
		}
		expectedSet := make(map[domain.TaskStatus]bool)
		for _, target := range expected[from] {
			expectedSet[target] = true
		}
		for _, to := range taskStatusOrder {
			err := ValidateTaskTransition(TaskTransition{
				From:     from,
				To:       to,
				Evidence: passingEvidence(from, to),
			})
			if expectedSet[to] {
				if err != nil {
					t.Errorf("%s -> %s unexpectedly failed: %v", from, to, err)
				}
			} else if !errors.Is(err, ErrInvalidTaskTransition) {
				t.Errorf("%s -> %s error = %v, want ErrInvalidTaskTransition", from, to, err)
			}
		}
	}
}

func TestTaskTransitionEvidenceGates(t *testing.T) {
	tests := []struct {
		name        string
		transition  TaskTransition
		requirement string
	}{
		{
			name: "selection requires manager review",
			transition: TaskTransition{
				From: domain.TaskQueued,
				To:   domain.TaskSelected,
			},
			requirement: "a completed manager review",
		},
		{
			name: "selection rejects architecture risk",
			transition: TaskTransition{
				From: domain.TaskQueued,
				To:   domain.TaskSelected,
				Evidence: TaskTransitionEvidence{
					ManagerReviewCompleted:  true,
					ArchitectureRiskPending: true,
				},
			},
			requirement: "resolved architecture risk",
		},
		{
			name: "development requires plan",
			transition: TaskTransition{
				From: domain.TaskPlanning,
				To:   domain.TaskDeveloping,
			},
			requirement: "a completed plan or explicit planning bypass",
		},
		{
			name: "waiting input requires answer",
			transition: TaskTransition{
				From: domain.TaskWaitingInput,
				To:   domain.TaskPlanning,
			},
			requirement: "provided human input",
		},
		{
			name: "review fix requires blocking finding",
			transition: TaskTransition{
				From: domain.TaskReviewing,
				To:   domain.TaskFixing,
			},
			requirement: "blocking review findings",
		},
		{
			name: "verification fix requires failure",
			transition: TaskTransition{
				From: domain.TaskVerifying,
				To:   domain.TaskFixing,
			},
			requirement: "failed verification evidence",
		},
		{
			name: "CI fix requires failure",
			transition: TaskTransition{
				From: domain.TaskWaitingCI,
				To:   domain.TaskFixing,
			},
			requirement: "failed CI evidence",
		},
		{
			name: "verification rejects blocking review findings",
			transition: TaskTransition{
				From: domain.TaskReviewing,
				To:   domain.TaskVerifying,
				Evidence: TaskTransitionEvidence{
					BlockingReviewFindings: true,
				},
			},
			requirement: "no blocking review findings",
		},
		{
			name: "waiting CI requires verification",
			transition: TaskTransition{
				From: domain.TaskVerifying,
				To:   domain.TaskWaitingCI,
				Evidence: TaskTransitionEvidence{
					CIRequired: true,
				},
			},
			requirement: "successful verification evidence",
		},
		{
			name: "waiting CI requires configured CI",
			transition: TaskTransition{
				From: domain.TaskVerifying,
				To:   domain.TaskWaitingCI,
				Evidence: TaskTransitionEvidence{
					VerificationSucceeded: true,
				},
			},
			requirement: "configured CI verification",
		},
		{
			name: "direct completion requires verification",
			transition: TaskTransition{
				From: domain.TaskVerifying,
				To:   domain.TaskCompleted,
			},
			requirement: "successful verification evidence",
		},
		{
			name: "direct completion rejects required CI",
			transition: TaskTransition{
				From: domain.TaskVerifying,
				To:   domain.TaskCompleted,
				Evidence: TaskTransitionEvidence{
					VerificationSucceeded: true,
					CIRequired:            true,
				},
			},
			requirement: "CI to be disabled",
		},
		{
			name: "CI completion requires verification",
			transition: TaskTransition{
				From: domain.TaskWaitingCI,
				To:   domain.TaskCompleted,
				Evidence: TaskTransitionEvidence{
					CIPassed: true,
				},
			},
			requirement: "successful verification evidence",
		},
		{
			name: "CI completion requires pass",
			transition: TaskTransition{
				From: domain.TaskWaitingCI,
				To:   domain.TaskCompleted,
				Evidence: TaskTransitionEvidence{
					VerificationSucceeded: true,
				},
			},
			requirement: "passing CI evidence",
		},
		{
			name: "blocked resume requires resolution",
			transition: TaskTransition{
				From: domain.TaskBlocked,
				To:   domain.TaskPlanning,
			},
			requirement: "resolved blocker",
		},
		{
			name: "deferred resume requires reprioritization",
			transition: TaskTransition{
				From: domain.TaskDeferred,
				To:   domain.TaskQueued,
			},
			requirement: "explicit reprioritization",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateTaskTransition(tc.transition)
			if !errors.Is(err, ErrTransitionPrecondition) {
				t.Fatalf("error = %v, want ErrTransitionPrecondition", err)
			}
			var transitionErr *TaskTransitionError
			if !errors.As(err, &transitionErr) {
				t.Fatalf("error type = %T, want *TaskTransitionError", err)
			}
			if transitionErr.From != tc.transition.From ||
				transitionErr.To != tc.transition.To ||
				transitionErr.Requirement != tc.requirement {
				t.Errorf("transition error = %#v", transitionErr)
			}
		})
	}
}

func TestTaskTransitionEvidenceAllowsGuardedEdges(t *testing.T) {
	tests := []TaskTransition{
		{
			From: domain.TaskQueued,
			To:   domain.TaskSelected,
			Evidence: TaskTransitionEvidence{
				ManagerReviewCompleted: true,
			},
		},
		{
			From: domain.TaskPlanning,
			To:   domain.TaskDeveloping,
			Evidence: TaskTransitionEvidence{
				PlanCompleted: true,
			},
		},
		{
			From: domain.TaskPlanning,
			To:   domain.TaskDeveloping,
			Evidence: TaskTransitionEvidence{
				PlanningDisabled: true,
			},
		},
		{
			From: domain.TaskWaitingInput,
			To:   domain.TaskDeveloping,
			Evidence: TaskTransitionEvidence{
				InputProvided: true,
				PlanCompleted: true,
			},
		},
		{
			From: domain.TaskReviewing,
			To:   domain.TaskFixing,
			Evidence: TaskTransitionEvidence{
				BlockingReviewFindings: true,
			},
		},
		{
			From: domain.TaskVerifying,
			To:   domain.TaskFixing,
			Evidence: TaskTransitionEvidence{
				VerificationFailed: true,
			},
		},
		{
			From: domain.TaskWaitingCI,
			To:   domain.TaskFixing,
			Evidence: TaskTransitionEvidence{
				CIFailed: true,
			},
		},
		{
			From: domain.TaskReviewing,
			To:   domain.TaskVerifying,
		},
		{
			From: domain.TaskVerifying,
			To:   domain.TaskWaitingCI,
			Evidence: TaskTransitionEvidence{
				VerificationSucceeded: true,
				CIRequired:            true,
			},
		},
		{
			From: domain.TaskVerifying,
			To:   domain.TaskCompleted,
			Evidence: TaskTransitionEvidence{
				VerificationSucceeded: true,
			},
		},
		{
			From: domain.TaskWaitingCI,
			To:   domain.TaskCompleted,
			Evidence: TaskTransitionEvidence{
				VerificationSucceeded: true,
				CIPassed:              true,
			},
		},
		{
			From: domain.TaskBlocked,
			To:   domain.TaskPlanning,
			Evidence: TaskTransitionEvidence{
				BlockerResolved: true,
			},
		},
		{
			From: domain.TaskDeferred,
			To:   domain.TaskQueued,
			Evidence: TaskTransitionEvidence{
				Reprioritized: true,
			},
		},
	}
	for _, transition := range tests {
		if err := ValidateTaskTransition(transition); err != nil {
			t.Errorf("%s -> %s failed: %v", transition.From, transition.To, err)
		}
	}
}

func TestTaskTransitionRejectsUnknownSelfAndTerminalEdges(t *testing.T) {
	tests := []TaskTransition{
		{From: "unknown", To: domain.TaskQueued},
		{From: domain.TaskQueued, To: "unknown"},
		{From: domain.TaskQueued, To: domain.TaskQueued},
		{From: domain.TaskCompleted, To: domain.TaskQueued},
		{From: domain.TaskCancelled, To: domain.TaskQueued},
	}
	for _, transition := range tests {
		err := ValidateTaskTransition(transition)
		if !errors.Is(err, ErrInvalidTaskTransition) {
			t.Errorf("%s -> %s error = %v", transition.From, transition.To, err)
		}
		var transitionErr *TaskTransitionError
		if !errors.As(err, &transitionErr) ||
			transitionErr.From != transition.From ||
			transitionErr.To != transition.To {
			t.Errorf("transition error = %#v", transitionErr)
		}
	}
	if _, err := AllowedTaskTargets("unknown"); !errors.Is(err, ErrInvalidTaskTransition) {
		t.Errorf("unknown allowed-target error = %v", err)
	}
}

func TestAllowedTaskTargetsReturnsCopy(t *testing.T) {
	first, err := AllowedTaskTargets(domain.TaskQueued)
	if err != nil {
		t.Fatal(err)
	}
	first[0] = domain.TaskCompleted
	second, err := AllowedTaskTargets(domain.TaskQueued)
	if err != nil {
		t.Fatal(err)
	}
	if second[0] != domain.TaskSelected {
		t.Fatalf("caller mutated transition table: %v", second)
	}
}

func passingEvidence(from, to domain.TaskStatus) TaskTransitionEvidence {
	evidence := TaskTransitionEvidence{
		ManagerReviewCompleted: true,
		PlanCompleted:          true,
		InputProvided:          true,
		VerificationSucceeded:  true,
		VerificationFailed:     true,
		CIPassed:               true,
		CIFailed:               true,
		BlockerResolved:        true,
		Reprioritized:          true,
	}
	if from == domain.TaskReviewing && to == domain.TaskFixing {
		evidence.BlockingReviewFindings = true
	}
	if from == domain.TaskVerifying && to == domain.TaskWaitingCI {
		evidence.CIRequired = true
	}
	return evidence
}
