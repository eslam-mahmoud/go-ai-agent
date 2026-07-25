package mode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/eslam-mahmoud/go-ai-agent/internal/workflow"
)

// ExecutionRecorder closes the execution a mode opened while building its
// context. It is optional so tests and simple deployments can dispatch modes
// without a durable execution history.
type ExecutionRecorder interface {
	CompleteRunning(taskID int64, mode string, output json.RawMessage) error
	FailRunning(taskID int64, mode, errorClass, message string) error
}

// Dispatcher adapts registered modes to workflow.ModeRunner.
type Dispatcher struct {
	registry *Registry
	recorder ExecutionRecorder
}

func NewDispatcher(registry *Registry) (*Dispatcher, error) {
	if registry == nil {
		return nil, errors.New("mode dispatcher registry is required")
	}
	return &Dispatcher{registry: registry}, nil
}

// NewRecordingDispatcher dispatches modes and records every run, so a later
// mode in the chain can read what an earlier one produced.
func NewRecordingDispatcher(
	registry *Registry, recorder ExecutionRecorder,
) (*Dispatcher, error) {
	dispatcher, err := NewDispatcher(registry)
	if err != nil {
		return nil, err
	}
	if isNilDependency(recorder) {
		return nil, errors.New("recording dispatcher requires an execution recorder")
	}
	dispatcher.recorder = recorder
	return dispatcher, nil
}

func (dispatcher *Dispatcher) RunMode(
	ctx context.Context,
	request workflow.ModeRequest,
) (workflow.ModeOutcome, error) {
	if err := ctx.Err(); err != nil {
		return workflow.ModeOutcome{}, err
	}
	deliveryMode, err := dispatcher.registry.Resolve(request.Mode)
	if err != nil {
		return workflow.ModeOutcome{}, err
	}
	raw, err := deliveryMode.Run(ctx, request)
	if err != nil {
		dispatcher.recordFailure(request, "mode-run", err)
		return workflow.ModeOutcome{}, fmt.Errorf("run %s mode: %w", request.Mode, err)
	}
	output, err := dispatcher.registry.ValidateOutput(request.Mode, raw)
	if err != nil {
		// Output that fails its schema is not recorded as usable: a later mode
		// reading it would inherit the malformed shape.
		dispatcher.recordFailure(request, "invalid-output", err)
		return workflow.ModeOutcome{}, err
	}
	if dispatcher.recorder != nil {
		if err := dispatcher.recorder.CompleteRunning(
			request.TaskID, string(request.Mode), raw,
		); err != nil {
			return workflow.ModeOutcome{}, fmt.Errorf(
				"record %s output: %w", request.Mode, err,
			)
		}
	}
	return output.WorkflowOutcome(), nil
}

func (dispatcher *Dispatcher) recordFailure(
	request workflow.ModeRequest, class string, cause error,
) {
	if dispatcher.recorder == nil {
		return
	}
	// A failure to record a failure must not mask the original error.
	_ = dispatcher.recorder.FailRunning(
		request.TaskID, string(request.Mode), class, cause.Error(),
	)
}

var _ workflow.ModeRunner = (*Dispatcher)(nil)
