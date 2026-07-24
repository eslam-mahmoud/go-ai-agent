package domain

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

type ExecutionStatus string

const (
	ExecutionPending     ExecutionStatus = "pending"
	ExecutionRunning     ExecutionStatus = "running"
	ExecutionCompleted   ExecutionStatus = "completed"
	ExecutionFailed      ExecutionStatus = "failed"
	ExecutionCancelled   ExecutionStatus = "cancelled"
	ExecutionInterrupted ExecutionStatus = "interrupted"
)

var ErrInvalidExecution = errors.New("invalid execution")

// Execution is one sequential mode invocation for a project task.
type Execution struct {
	ID                int64
	ProjectID         int64
	TaskID            int64
	Mode              string
	Engine            string
	Model             string
	ProviderSessionID string
	Attempt           int
	Status            ExecutionStatus
	InputArtifactID   *int64
	OutputArtifactID  *int64
	StartedAt         *time.Time
	CompletedAt       *time.Time
	ErrorClass        string
	ErrorMessage      string
	InputTokens       int64
	OutputTokens      int64
	EstimatedCost     float64
}

func NewExecution(projectID, taskID int64, mode, engine, model string, attempt int) *Execution {
	return &Execution{
		ProjectID: projectID,
		TaskID:    taskID,
		Mode:      mode,
		Engine:    engine,
		Model:     model,
		Attempt:   attempt,
		Status:    ExecutionPending,
	}
}

func (status ExecutionStatus) Valid() bool {
	switch status {
	case ExecutionPending,
		ExecutionRunning,
		ExecutionCompleted,
		ExecutionFailed,
		ExecutionCancelled,
		ExecutionInterrupted:
		return true
	default:
		return false
	}
}

// OccupiesProviderLane reports whether the execution owns Madar's single
// provider-process lane. Pending and terminal/interrupted executions do not
// prevent another execution from starting.
func (status ExecutionStatus) OccupiesProviderLane() bool {
	return status == ExecutionRunning
}

// Validate checks record integrity but not execution lifecycle transitions.
func (execution *Execution) Validate() error {
	if execution == nil {
		return fmt.Errorf("%w: execution is nil", ErrInvalidExecution)
	}
	switch {
	case execution.ProjectID <= 0:
		return fmt.Errorf("%w: project ID must be positive", ErrInvalidExecution)
	case execution.TaskID <= 0:
		return fmt.Errorf("%w: task ID must be positive", ErrInvalidExecution)
	case strings.TrimSpace(execution.Mode) == "":
		return fmt.Errorf("%w: mode is required", ErrInvalidExecution)
	case strings.TrimSpace(execution.Engine) == "":
		return fmt.Errorf("%w: engine is required", ErrInvalidExecution)
	case execution.Attempt <= 0:
		return fmt.Errorf("%w: attempt must be positive", ErrInvalidExecution)
	case !execution.Status.Valid():
		return fmt.Errorf("%w: unknown status %q", ErrInvalidExecution, execution.Status)
	case execution.InputArtifactID != nil && *execution.InputArtifactID <= 0:
		return fmt.Errorf("%w: input artifact ID must be positive", ErrInvalidExecution)
	case execution.OutputArtifactID != nil && *execution.OutputArtifactID <= 0:
		return fmt.Errorf("%w: output artifact ID must be positive", ErrInvalidExecution)
	case execution.StartedAt == nil && execution.CompletedAt != nil:
		return fmt.Errorf("%w: completion time requires a start time", ErrInvalidExecution)
	case execution.StartedAt != nil && execution.CompletedAt != nil &&
		execution.CompletedAt.Before(*execution.StartedAt):
		return fmt.Errorf("%w: completion time precedes start time", ErrInvalidExecution)
	case execution.InputTokens < 0:
		return fmt.Errorf("%w: input tokens cannot be negative", ErrInvalidExecution)
	case execution.OutputTokens < 0:
		return fmt.Errorf("%w: output tokens cannot be negative", ErrInvalidExecution)
	case execution.EstimatedCost < 0 ||
		math.IsNaN(execution.EstimatedCost) ||
		math.IsInf(execution.EstimatedCost, 0):
		return fmt.Errorf("%w: estimated cost must be finite and non-negative", ErrInvalidExecution)
	default:
		return nil
	}
}
