package mode

import (
	"context"
	"errors"
	"fmt"

	"github.com/eslam-mahmoud/go-ai-agent/internal/workflow"
)

// Dispatcher adapts registered modes to workflow.ModeRunner.
type Dispatcher struct {
	registry *Registry
}

func NewDispatcher(registry *Registry) (*Dispatcher, error) {
	if registry == nil {
		return nil, errors.New("mode dispatcher registry is required")
	}
	return &Dispatcher{registry: registry}, nil
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
		return workflow.ModeOutcome{}, fmt.Errorf("run %s mode: %w", request.Mode, err)
	}
	output, err := dispatcher.registry.ValidateOutput(request.Mode, raw)
	if err != nil {
		return workflow.ModeOutcome{}, err
	}
	return output.WorkflowOutcome(), nil
}

var _ workflow.ModeRunner = (*Dispatcher)(nil)
