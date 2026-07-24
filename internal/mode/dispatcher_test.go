package mode

import (
	"context"
	"errors"
	"testing"

	"github.com/eslam-mahmoud/go-ai-agent/internal/workflow"
)

func TestDispatcherRunsRegisteredModeValidatesOutputAndMapsOutcome(t *testing.T) {
	reviewer := &registryTestMode{
		definition: mustBuiltinDefinition(t, workflow.ModeReviewer),
		output:     validReviewerOutput(OutputCompleted, true),
	}
	registry, err := NewRegistry(reviewer)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := NewDispatcher(registry)
	if err != nil {
		t.Fatal(err)
	}
	request := workflow.ModeRequest{
		ProjectID: 1,
		TaskID:    2,
		Mode:      workflow.ModeReviewer,
	}
	outcome, err := dispatcher.RunMode(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Status != workflow.ModeCompleted ||
		!outcome.BlockingFindings ||
		outcome.Summary != "Review complete." {
		t.Fatalf("outcome = %#v", outcome)
	}
	reviewer.mu.Lock()
	defer reviewer.mu.Unlock()
	if len(reviewer.requests) != 1 || reviewer.requests[0] != request {
		t.Fatalf("requests = %#v", reviewer.requests)
	}
}

func TestDispatcherRejectsUnknownInvalidFailedAndCancelledRuns(t *testing.T) {
	if dispatcher, err := NewDispatcher(nil); dispatcher != nil || err == nil {
		t.Fatalf("nil dispatcher=%#v error=%v", dispatcher, err)
	}
	planner := &registryTestMode{
		definition: mustBuiltinDefinition(t, workflow.ModePlanner),
		output:     validPlannerOutput(OutputCompleted),
	}
	registry, _ := NewRegistry(planner)
	dispatcher, _ := NewDispatcher(registry)
	if _, err := dispatcher.RunMode(context.Background(), workflow.ModeRequest{
		Mode: "missing",
	}); !errors.Is(err, ErrModeNotFound) {
		t.Fatalf("missing mode error = %v", err)
	}

	planner.output = []byte(`{}`)
	if _, err := dispatcher.RunMode(context.Background(), workflow.ModeRequest{
		Mode: workflow.ModePlanner,
	}); !errors.Is(err, ErrInvalidModeOutput) {
		t.Fatalf("invalid output error = %v", err)
	}
	runErr := errors.New("provider failed")
	planner.err = runErr
	if _, err := dispatcher.RunMode(context.Background(), workflow.ModeRequest{
		Mode: workflow.ModePlanner,
	}); !errors.Is(err, runErr) {
		t.Fatalf("run error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	planner.err = nil
	if _, err := dispatcher.RunMode(ctx, workflow.ModeRequest{
		Mode: workflow.ModePlanner,
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
}
