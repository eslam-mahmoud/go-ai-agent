package domain

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestWorkflowEventDefaultsSourcesAndValidation(t *testing.T) {
	event := NewWorkflowEvent(
		1,
		WorkflowSourceController,
		WorkflowTaskTransitioned,
		"Task selected.",
	)
	if string(event.Data) != "{}" {
		t.Fatalf("default data = %s", event.Data)
	}
	if err := event.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, source := range []WorkflowEventSource{
		WorkflowSourceController,
		WorkflowSourceWorkflow,
		WorkflowSourceRecovery,
		WorkflowSourceEngine,
		WorkflowSourceExternal,
	} {
		if !source.Valid() {
			t.Errorf("source %q is invalid", source)
		}
	}
	if WorkflowEventSource("unknown").Valid() {
		t.Fatal("unknown source is valid")
	}
}

func TestWorkflowEventValidationRejectsInvalidRecords(t *testing.T) {
	valid := *NewWorkflowEvent(
		1,
		WorkflowSourceWorkflow,
		WorkflowEventType("mode.started"),
		"",
	)
	zero := int64(0)
	cases := []struct {
		name   string
		mutate func(*WorkflowEvent)
	}{
		{"project", func(event *WorkflowEvent) { event.ProjectID = 0 }},
		{"task", func(event *WorkflowEvent) { event.TaskID = &zero }},
		{"execution", func(event *WorkflowEvent) { event.ExecutionID = &zero }},
		{"sequence", func(event *WorkflowEvent) { event.Sequence = -1 }},
		{"source", func(event *WorkflowEvent) { event.Source = "unknown" }},
		{"type", func(event *WorkflowEvent) { event.Type = " " }},
		{"empty data", func(event *WorkflowEvent) { event.Data = nil }},
		{"invalid JSON", func(event *WorkflowEvent) {
			event.Data = json.RawMessage(`{"broken"`)
		}},
		{"idempotency whitespace", func(event *WorkflowEvent) {
			event.IdempotencyKey = " duplicate "
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			event := valid
			test.mutate(&event)
			if err := event.Validate(); !errors.Is(err, ErrInvalidWorkflowEvent) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	persisted := valid
	persisted.ID = 1
	persisted.Sequence = 1
	persisted.CreatedAt = time.Now().UTC()
	if err := persisted.Validate(); err != nil {
		t.Fatalf("persisted event: %v", err)
	}
	if err := (*WorkflowEvent)(nil).Validate(); !errors.Is(err, ErrInvalidWorkflowEvent) {
		t.Fatalf("nil error = %v", err)
	}
}
