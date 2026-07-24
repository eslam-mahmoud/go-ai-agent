package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type WorkflowEventSource string

const (
	WorkflowSourceController WorkflowEventSource = "controller"
	WorkflowSourceWorkflow   WorkflowEventSource = "workflow"
	WorkflowSourceRecovery   WorkflowEventSource = "recovery"
	WorkflowSourceEngine     WorkflowEventSource = "engine"
	WorkflowSourceExternal   WorkflowEventSource = "external"
)

type WorkflowEventType string

const (
	WorkflowTaskTransitioned     WorkflowEventType = "task.transitioned"
	WorkflowProjectPaused        WorkflowEventType = "project.paused"
	WorkflowProjectResumed       WorkflowEventType = "project.resumed"
	WorkflowTaskCancelled        WorkflowEventType = "task.cancelled"
	WorkflowExecutionRetried     WorkflowEventType = "execution.retried"
	WorkflowExecutionInterrupted WorkflowEventType = "execution.interrupted"
	WorkflowRecoveryDecided      WorkflowEventType = "recovery.decided"
	WorkflowBacklogReordered     WorkflowEventType = "backlog.reordered"
	WorkflowTaskSelected         WorkflowEventType = "task.selected"
	WorkflowProjectPublished     WorkflowEventType = "project.published"
	WorkflowDiscoveriesRecorded  WorkflowEventType = "discoveries.recorded"
)

var ErrInvalidWorkflowEvent = errors.New("invalid workflow event")

// WorkflowEvent is one immutable, project-scoped audit fact. Sequence is
// monotonic within a project and assigned by the store.
type WorkflowEvent struct {
	ID             int64
	ProjectID      int64
	TaskID         *int64
	ExecutionID    *int64
	Sequence       int64
	Source         WorkflowEventSource
	Type           WorkflowEventType
	Message        string
	Data           json.RawMessage
	IdempotencyKey string
	CreatedAt      time.Time
}

func NewWorkflowEvent(
	projectID int64,
	source WorkflowEventSource,
	eventType WorkflowEventType,
	message string,
) *WorkflowEvent {
	return &WorkflowEvent{
		ProjectID: projectID,
		Source:    source,
		Type:      eventType,
		Message:   message,
		Data:      json.RawMessage(`{}`),
	}
}

func (source WorkflowEventSource) Valid() bool {
	switch source {
	case WorkflowSourceController,
		WorkflowSourceWorkflow,
		WorkflowSourceRecovery,
		WorkflowSourceEngine,
		WorkflowSourceExternal:
		return true
	default:
		return false
	}
}

func (event *WorkflowEvent) Validate() error {
	if event == nil {
		return fmt.Errorf("%w: event is nil", ErrInvalidWorkflowEvent)
	}
	switch {
	case event.ProjectID <= 0:
		return fmt.Errorf("%w: project ID must be positive", ErrInvalidWorkflowEvent)
	case event.TaskID != nil && *event.TaskID <= 0:
		return fmt.Errorf("%w: task ID must be positive", ErrInvalidWorkflowEvent)
	case event.ExecutionID != nil && *event.ExecutionID <= 0:
		return fmt.Errorf("%w: execution ID must be positive", ErrInvalidWorkflowEvent)
	case event.Sequence < 0:
		return fmt.Errorf("%w: sequence cannot be negative", ErrInvalidWorkflowEvent)
	case !event.Source.Valid():
		return fmt.Errorf("%w: unknown source %q", ErrInvalidWorkflowEvent, event.Source)
	case strings.TrimSpace(string(event.Type)) == "":
		return fmt.Errorf("%w: type is required", ErrInvalidWorkflowEvent)
	case len(event.Data) == 0 || !json.Valid(event.Data):
		return fmt.Errorf("%w: data must be valid JSON", ErrInvalidWorkflowEvent)
	case strings.TrimSpace(event.IdempotencyKey) != event.IdempotencyKey:
		return fmt.Errorf(
			"%w: idempotency key cannot have surrounding whitespace",
			ErrInvalidWorkflowEvent,
		)
	default:
		return nil
	}
}
