package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

type TaskStatus string

const (
	TaskProposed     TaskStatus = "proposed"
	TaskQueued       TaskStatus = "queued"
	TaskSelected     TaskStatus = "selected"
	TaskPlanning     TaskStatus = "planning"
	TaskWaitingInput TaskStatus = "waiting-input"
	TaskDeveloping   TaskStatus = "developing"
	TaskReviewing    TaskStatus = "reviewing"
	TaskFixing       TaskStatus = "fixing"
	TaskVerifying    TaskStatus = "verifying"
	TaskWaitingCI    TaskStatus = "waiting-ci"
	TaskBlocked      TaskStatus = "blocked"
	TaskCompleted    TaskStatus = "completed"
	TaskCancelled    TaskStatus = "cancelled"
	TaskDeferred     TaskStatus = "deferred"
)

var ErrInvalidTask = errors.New("invalid project task")

// Task is one ordered unit of work in a Project backlog. It is intentionally
// separate from store.Task, the legacy issue-execution compatibility record.
type Task struct {
	ID                int64
	ProjectID         int64
	IssueNumber       int
	Title             string
	Goal              string
	Status            TaskStatus
	Priority          int
	Sequence          int
	TaskType          string
	Source            string
	SourceDiscoveryID *int64
	BlocksRelease     bool
	SelectedReason    string
	BranchName        string
	PRNumber          int
	DependencyState   string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// NewTask constructs a proposed task. Sequence zero asks the store to append
// it atomically to the project's current backlog.
func NewTask(projectID int64, title, goal string) *Task {
	return &Task{
		ProjectID: projectID,
		Title:     title,
		Goal:      goal,
		Status:    TaskProposed,
	}
}

func (status TaskStatus) Valid() bool {
	switch status {
	case TaskProposed,
		TaskQueued,
		TaskSelected,
		TaskPlanning,
		TaskWaitingInput,
		TaskDeveloping,
		TaskReviewing,
		TaskFixing,
		TaskVerifying,
		TaskWaitingCI,
		TaskBlocked,
		TaskCompleted,
		TaskCancelled,
		TaskDeferred:
		return true
	default:
		return false
	}
}

// Active reports whether a task owns Madar's single delivery lane.
func (status TaskStatus) Active() bool {
	switch status {
	case TaskSelected,
		TaskPlanning,
		TaskWaitingInput,
		TaskDeveloping,
		TaskReviewing,
		TaskFixing,
		TaskVerifying,
		TaskWaitingCI,
		TaskBlocked:
		return true
	default:
		return false
	}
}

// DependenciesSatisfied reports whether the recorded dependency state allows a
// task to start. Selection fails closed: an unrecognized state counts as
// unresolved so unknown vocabulary can never promote ineligible work.
func (task *Task) DependenciesSatisfied() bool {
	if task == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(task.DependencyState)) {
	case "", "none", "met", "resolved", "satisfied":
		return true
	default:
		return false
	}
}

// Validate checks record invariants, not lifecycle transitions.
func (task *Task) Validate() error {
	if task == nil {
		return fmt.Errorf("%w: task is nil", ErrInvalidTask)
	}
	switch {
	case task.ProjectID <= 0:
		return fmt.Errorf("%w: project ID must be positive", ErrInvalidTask)
	case task.IssueNumber < 0:
		return fmt.Errorf("%w: issue number cannot be negative", ErrInvalidTask)
	case strings.TrimSpace(task.Title) == "":
		return fmt.Errorf("%w: title is required", ErrInvalidTask)
	case strings.TrimSpace(task.Goal) == "":
		return fmt.Errorf("%w: goal is required", ErrInvalidTask)
	case !task.Status.Valid():
		return fmt.Errorf("%w: unknown status %q", ErrInvalidTask, task.Status)
	case task.Priority < 0:
		return fmt.Errorf("%w: priority cannot be negative", ErrInvalidTask)
	case task.Sequence < 0:
		return fmt.Errorf("%w: sequence cannot be negative", ErrInvalidTask)
	case task.SourceDiscoveryID != nil && *task.SourceDiscoveryID <= 0:
		return fmt.Errorf("%w: source discovery ID must be positive", ErrInvalidTask)
	case task.PRNumber < 0:
		return fmt.Errorf("%w: PR number cannot be negative", ErrInvalidTask)
	default:
		return nil
	}
}
