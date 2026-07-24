package domain

import (
	"errors"
	"math"
	"testing"
	"time"
)

func TestNewExecutionDefaultsAndStatuses(t *testing.T) {
	execution := NewExecution(1, 2, "developer", "codex", "gpt-test", 1)
	if execution.Status != ExecutionPending {
		t.Errorf("status = %q", execution.Status)
	}
	if err := execution.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, status := range []ExecutionStatus{
		ExecutionPending,
		ExecutionRunning,
		ExecutionCompleted,
		ExecutionFailed,
		ExecutionCancelled,
		ExecutionInterrupted,
	} {
		if !status.Valid() {
			t.Errorf("status %q is invalid", status)
		}
	}
	if ExecutionStatus("unknown").Valid() {
		t.Error("unknown status is valid")
	}
}

func TestExecutionValidation(t *testing.T) {
	valid := *NewExecution(1, 2, "developer", "codex", "", 1)
	started := time.Now().UTC()
	before := started.Add(-time.Second)
	zero := int64(0)
	cases := []struct {
		name   string
		mutate func(*Execution)
	}{
		{"project", func(e *Execution) { e.ProjectID = 0 }},
		{"task", func(e *Execution) { e.TaskID = 0 }},
		{"mode", func(e *Execution) { e.Mode = "" }},
		{"engine", func(e *Execution) { e.Engine = "" }},
		{"attempt", func(e *Execution) { e.Attempt = 0 }},
		{"status", func(e *Execution) { e.Status = "unknown" }},
		{"input artifact", func(e *Execution) { e.InputArtifactID = &zero }},
		{"output artifact", func(e *Execution) { e.OutputArtifactID = &zero }},
		{"completion without start", func(e *Execution) { e.CompletedAt = &started }},
		{"completion before start", func(e *Execution) {
			e.StartedAt = &started
			e.CompletedAt = &before
		}},
		{"input tokens", func(e *Execution) { e.InputTokens = -1 }},
		{"output tokens", func(e *Execution) { e.OutputTokens = -1 }},
		{"negative cost", func(e *Execution) { e.EstimatedCost = -1 }},
		{"nonfinite cost", func(e *Execution) { e.EstimatedCost = math.Inf(1) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			execution := valid
			tc.mutate(&execution)
			if err := execution.Validate(); !errors.Is(err, ErrInvalidExecution) {
				t.Errorf("Validate error = %v", err)
			}
		})
	}
	if err := (*Execution)(nil).Validate(); !errors.Is(err, ErrInvalidExecution) {
		t.Errorf("nil Validate error = %v", err)
	}
}
