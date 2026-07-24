package workflow

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

func TestFeatureWorkflowSuccessfulPaths(t *testing.T) {
	t.Run("without CI", func(t *testing.T) {
		controller := &fakeFeatureController{status: domain.TaskSelected}
		runner := &fakeModeRunner{outcomes: []ModeOutcome{
			{Status: ModeCompleted},
			{Status: ModeCompleted},
			{Status: ModeCompleted},
			{Status: ModeCompleted},
		}}
		workflow, err := NewFeatureWorkflow(controller, runner, FeatureOptions{})
		if err != nil {
			t.Fatal(err)
		}
		result, err := workflow.Run(context.Background(), 1, 2)
		if err != nil {
			t.Fatal(err)
		}
		if result.FinalStatus != domain.TaskCompleted {
			t.Fatalf("result = %#v", result)
		}
		wantModes := []ModeName{ModePlanner, ModeDeveloper, ModeReviewer, ModeVerifier}
		if !reflect.DeepEqual(result.ModesRun, wantModes) {
			t.Errorf("modes = %v, want %v", result.ModesRun, wantModes)
		}
		wantTransitions := []domain.TaskStatus{
			domain.TaskPlanning,
			domain.TaskDeveloping,
			domain.TaskReviewing,
			domain.TaskVerifying,
			domain.TaskCompleted,
		}
		if !reflect.DeepEqual(controller.transitions, wantTransitions) {
			t.Errorf("transitions = %v, want %v", controller.transitions, wantTransitions)
		}
	})

	t.Run("with CI", func(t *testing.T) {
		controller := &fakeFeatureController{status: domain.TaskVerifying}
		runner := &fakeModeRunner{outcomes: []ModeOutcome{{Status: ModeCompleted}}}
		workflow, _ := NewFeatureWorkflow(
			controller,
			runner,
			FeatureOptions{CIRequired: true},
		)
		result, err := workflow.Run(context.Background(), 1, 2)
		if err != nil {
			t.Fatal(err)
		}
		if result.FinalStatus != domain.TaskWaitingCI ||
			!reflect.DeepEqual(result.ModesRun, []ModeName{ModeVerifier}) {
			t.Fatalf("result = %#v", result)
		}
	})
}

func TestFeatureWorkflowReviewAndVerificationFixCycles(t *testing.T) {
	t.Run("blocking review", func(t *testing.T) {
		controller := &fakeFeatureController{status: domain.TaskReviewing}
		runner := &fakeModeRunner{outcomes: []ModeOutcome{
			{Status: ModeCompleted, BlockingFindings: true},
			{Status: ModeCompleted},
			{Status: ModeCompleted},
			{Status: ModeCompleted},
		}}
		workflow, _ := NewFeatureWorkflow(controller, runner, FeatureOptions{})
		result, err := workflow.Run(context.Background(), 1, 2)
		if err != nil {
			t.Fatal(err)
		}
		want := []ModeName{ModeReviewer, ModeFixer, ModeReviewer, ModeVerifier}
		if result.FinalStatus != domain.TaskCompleted || !reflect.DeepEqual(result.ModesRun, want) {
			t.Fatalf("result = %#v", result)
		}
	})

	t.Run("verification failure", func(t *testing.T) {
		controller := &fakeFeatureController{status: domain.TaskVerifying}
		runner := &fakeModeRunner{outcomes: []ModeOutcome{
			{Status: ModeFailed},
			{Status: ModeCompleted},
			{Status: ModeCompleted},
			{Status: ModeCompleted},
		}}
		workflow, _ := NewFeatureWorkflow(controller, runner, FeatureOptions{})
		result, err := workflow.Run(context.Background(), 1, 2)
		if err != nil {
			t.Fatal(err)
		}
		want := []ModeName{ModeVerifier, ModeFixer, ModeReviewer, ModeVerifier}
		if result.FinalStatus != domain.TaskCompleted || !reflect.DeepEqual(result.ModesRun, want) {
			t.Fatalf("result = %#v", result)
		}
	})
}

func TestFeatureWorkflowDurableStops(t *testing.T) {
	tests := []struct {
		name    string
		start   domain.TaskStatus
		outcome ModeOutcome
		final   domain.TaskStatus
	}{
		{"planner input", domain.TaskPlanning, ModeOutcome{Status: ModeNeedsInput}, domain.TaskWaitingInput},
		{"developer input", domain.TaskDeveloping, ModeOutcome{Status: ModeNeedsInput}, domain.TaskWaitingInput},
		{"developer blocked", domain.TaskDeveloping, ModeOutcome{Status: ModeBlocked}, domain.TaskBlocked},
		{"planner failed", domain.TaskPlanning, ModeOutcome{Status: ModeFailed}, domain.TaskBlocked},
		{"reviewer input blocks", domain.TaskReviewing, ModeOutcome{Status: ModeNeedsInput}, domain.TaskBlocked},
		{"cancelled", domain.TaskDeveloping, ModeOutcome{Status: ModeCancelled}, domain.TaskCancelled},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			controller := &fakeFeatureController{status: tc.start}
			runner := &fakeModeRunner{outcomes: []ModeOutcome{tc.outcome}}
			workflow, _ := NewFeatureWorkflow(controller, runner, FeatureOptions{})
			result, err := workflow.Run(context.Background(), 1, 2)
			if err != nil {
				t.Fatal(err)
			}
			if result.FinalStatus != tc.final || len(result.ModesRun) != 1 {
				t.Fatalf("result = %#v", result)
			}
		})
	}

	for _, status := range []domain.TaskStatus{
		domain.TaskWaitingInput,
		domain.TaskWaitingCI,
		domain.TaskBlocked,
		domain.TaskCompleted,
		domain.TaskCancelled,
		domain.TaskDeferred,
	} {
		t.Run("already "+string(status), func(t *testing.T) {
			controller := &fakeFeatureController{status: status}
			runner := &fakeModeRunner{}
			workflow, _ := NewFeatureWorkflow(controller, runner, FeatureOptions{})
			result, err := workflow.Run(context.Background(), 1, 2)
			if err != nil || result.FinalStatus != status || len(runner.requests) != 0 {
				t.Fatalf("result=%#v error=%v requests=%v", result, err, runner.requests)
			}
		})
	}
}

func TestFeatureWorkflowErrorsAndCancellation(t *testing.T) {
	t.Run("runner error preserves state", func(t *testing.T) {
		controller := &fakeFeatureController{status: domain.TaskPlanning}
		runnerErr := errors.New("runner failed")
		runner := &fakeModeRunner{err: runnerErr}
		workflow, _ := NewFeatureWorkflow(controller, runner, FeatureOptions{})
		_, err := workflow.Run(context.Background(), 1, 2)
		if !errors.Is(err, runnerErr) || controller.status != domain.TaskPlanning {
			t.Fatalf("error=%v status=%s", err, controller.status)
		}
	})

	t.Run("cancelled context", func(t *testing.T) {
		controller := &fakeFeatureController{status: domain.TaskPlanning}
		runner := &fakeModeRunner{}
		workflow, _ := NewFeatureWorkflow(controller, runner, FeatureOptions{})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := workflow.Run(ctx, 1, 2)
		if !errors.Is(err, context.Canceled) || len(runner.requests) != 0 {
			t.Fatalf("error=%v requests=%v", err, runner.requests)
		}
	})

	t.Run("unsupported start", func(t *testing.T) {
		controller := &fakeFeatureController{status: domain.TaskQueued}
		workflow, _ := NewFeatureWorkflow(controller, &fakeModeRunner{}, FeatureOptions{})
		_, err := workflow.Run(context.Background(), 1, 2)
		if !errors.Is(err, ErrUnsupportedFeatureState) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("invalid outcome", func(t *testing.T) {
		controller := &fakeFeatureController{status: domain.TaskPlanning}
		runner := &fakeModeRunner{outcomes: []ModeOutcome{{Status: "unknown"}}}
		workflow, _ := NewFeatureWorkflow(controller, runner, FeatureOptions{})
		_, err := workflow.Run(context.Background(), 1, 2)
		if !errors.Is(err, ErrInvalidModeOutcome) {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestFeatureWorkflowDefensiveStepLimit(t *testing.T) {
	controller := &fakeFeatureController{status: domain.TaskReviewing}
	runner := &fakeModeRunner{repeat: ModeOutcome{
		Status:           ModeCompleted,
		BlockingFindings: true,
	}}
	workflow, err := NewFeatureWorkflow(controller, runner, FeatureOptions{MaxSteps: 3})
	if err != nil {
		t.Fatal(err)
	}
	_, err = workflow.Run(context.Background(), 1, 2)
	if !errors.Is(err, ErrFeatureStepLimit) {
		t.Fatalf("error = %v", err)
	}
	if len(runner.requests) != 3 {
		t.Fatalf("mode runs = %d, want 3", len(runner.requests))
	}
}

type fakeFeatureController struct {
	status      domain.TaskStatus
	transitions []domain.TaskStatus
}

func (controller *fakeFeatureController) TaskStatus(int64, int64) (domain.TaskStatus, error) {
	return controller.status, nil
}

func (controller *fakeFeatureController) ApplyTaskTransition(
	_, _ int64,
	target domain.TaskStatus,
	evidence TaskTransitionEvidence,
) (domain.TaskStatus, error) {
	if err := ValidateTaskTransition(TaskTransition{
		From:     controller.status,
		To:       target,
		Evidence: evidence,
	}); err != nil {
		return "", err
	}
	controller.status = target
	controller.transitions = append(controller.transitions, target)
	return target, nil
}

type fakeModeRunner struct {
	outcomes []ModeOutcome
	repeat   ModeOutcome
	err      error
	requests []ModeRequest
}

func (runner *fakeModeRunner) RunMode(
	_ context.Context,
	request ModeRequest,
) (ModeOutcome, error) {
	runner.requests = append(runner.requests, request)
	if runner.err != nil {
		return ModeOutcome{}, runner.err
	}
	if len(runner.outcomes) > 0 {
		outcome := runner.outcomes[0]
		runner.outcomes = runner.outcomes[1:]
		return outcome, nil
	}
	return runner.repeat, nil
}
