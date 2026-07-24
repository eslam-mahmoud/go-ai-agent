package engine

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

type fakeEngine struct {
	request RunRequest
}

var _ Engine = (*fakeEngine)(nil)

func (f *fakeEngine) Name() string {
	return "fake"
}

func (f *fakeEngine) Capabilities(context.Context) (CapabilitySet, error) {
	return CapabilitySet{
		Resume:           true,
		StructuredOutput: true,
		Streaming:        true,
		Usage:            true,
		Cancellation:     true,
		OutputSchema:     true,
	}, nil
}

func (f *fakeEngine) Run(
	_ context.Context,
	request RunRequest,
	emit func(Event) error,
) (*Result, error) {
	f.request = request
	if emit != nil {
		if err := emit(Event{Sequence: 1, Type: EventSessionStarted}); err != nil {
			return nil, err
		}
	}
	return &Result{Status: ResultCompleted}, nil
}

func (f *fakeEngine) Resume(
	ctx context.Context,
	request RunRequest,
	emit func(Event) error,
) (*Result, error) {
	return f.Run(ctx, request, emit)
}

func (f *fakeEngine) Cancel(context.Context, string) error {
	return nil
}

func TestEngineContractSupportsRunResumeEventsAndCancellation(t *testing.T) {
	provider := &fakeEngine{}
	request := RunRequest{
		ExecutionID:     42,
		WorkDir:         "/workspace",
		Prompt:          "implement task",
		Mode:            "developer",
		Model:           "model-1",
		SessionID:       "new-session",
		ResumeSessionID: "old-session",
		Timeout:         time.Minute,
		MaxTurns:        12,
		OutputSchema:    json.RawMessage(`{"type":"object"}`),
		Environment:     map[string]string{"SAFE": "value"},
		Policy: Policy{
			Sandbox:         "workspace-write",
			ApprovalPolicy:  "never",
			SkipPermissions: false,
		},
	}

	var events []Event
	result, err := provider.Run(context.Background(), request, func(event Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != ResultCompleted {
		t.Errorf("result status = %q, want %q", result.Status, ResultCompleted)
	}
	if provider.request.ExecutionID != 42 || provider.request.Mode != "developer" {
		t.Errorf("captured request = %#v", provider.request)
	}
	if len(events) != 1 || events[0].Type != EventSessionStarted {
		t.Errorf("events = %#v", events)
	}

	if _, err := provider.Resume(context.Background(), request, nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if err := provider.Cancel(context.Background(), "old-session"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	capabilities, err := provider.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if !capabilities.Resume || !capabilities.StructuredOutput ||
		!capabilities.Streaming || !capabilities.Usage ||
		!capabilities.Cancellation || !capabilities.OutputSchema {
		t.Errorf("capabilities = %#v, want all fake capabilities", capabilities)
	}
}

func TestNormalizedEventTypesHaveStableValues(t *testing.T) {
	cases := map[EventType]string{
		EventSessionStarted: "session.started",
		EventStepStarted:    "step.started",
		EventToolStarted:    "tool.started",
		EventToolCompleted:  "tool.completed",
		EventFileChanged:    "file.changed",
		EventProgress:       "progress",
		EventQuestion:       "question",
		EventUsage:          "usage",
		EventCheckpoint:     "checkpoint",
		EventCompleted:      "completed",
		EventFailed:         "failed",
	}
	for eventType, want := range cases {
		if got := string(eventType); got != want {
			t.Errorf("event type = %q, want %q", got, want)
		}
	}
}

func TestResultCarriesProviderNeutralExecutionEvidence(t *testing.T) {
	started := time.Now().UTC().Add(-time.Minute)
	completed := time.Now().UTC()
	result := Result{
		SessionID:   "session-1",
		Status:      ResultCompleted,
		OutputJSON:  json.RawMessage(`{"status":"completed"}`),
		OutputText:  "completed",
		ExitCode:    0,
		StartedAt:   started,
		CompletedAt: completed,
		Usage: Usage{
			InputTokens:       100,
			OutputTokens:      25,
			CachedInputTokens: 10,
			EstimatedCost:     0.5,
		},
	}

	if result.SessionID != "session-1" || result.ExitCode != 0 {
		t.Errorf("result = %#v", result)
	}
	if result.Usage.InputTokens != 100 || result.Usage.EstimatedCost != 0.5 {
		t.Errorf("usage = %#v", result.Usage)
	}
	if !result.CompletedAt.After(result.StartedAt) {
		t.Errorf("completion %v is not after start %v", result.CompletedAt, result.StartedAt)
	}
}

func TestResultStatusesHaveStableValues(t *testing.T) {
	cases := map[ResultStatus]string{
		ResultCompleted: "completed",
		ResultFailed:    "failed",
		ResultCancelled: "cancelled",
	}
	for status, want := range cases {
		if got := string(status); got != want {
			t.Errorf("result status = %q, want %q", got, want)
		}
	}
}
