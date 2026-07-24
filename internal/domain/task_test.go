package domain

import (
	"errors"
	"testing"
)

func TestNewTaskDefaultsAndValidation(t *testing.T) {
	task := NewTask(7, "Build project domain", "Persist the project aggregate")
	if task.Status != TaskProposed || task.Sequence != 0 {
		t.Errorf("defaults = status %q sequence %d", task.Status, task.Sequence)
	}
	if err := task.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestTaskStatuses(t *testing.T) {
	statuses := []TaskStatus{
		TaskProposed,
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
		TaskDeferred,
	}
	for _, status := range statuses {
		if !status.Valid() {
			t.Errorf("status %q is invalid", status)
		}
	}
	if TaskStatus("unknown").Valid() {
		t.Error("unknown task status is valid")
	}
}

func TestActiveTaskStatuses(t *testing.T) {
	active := map[TaskStatus]bool{
		TaskSelected:     true,
		TaskPlanning:     true,
		TaskWaitingInput: true,
		TaskDeveloping:   true,
		TaskReviewing:    true,
		TaskFixing:       true,
		TaskVerifying:    true,
		TaskWaitingCI:    true,
		TaskBlocked:      true,
	}
	for _, status := range []TaskStatus{
		TaskProposed,
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
		TaskDeferred,
	} {
		if got := status.Active(); got != active[status] {
			t.Errorf("%q Active() = %t, want %t", status, got, active[status])
		}
	}
	if TaskStatus("unknown").Active() {
		t.Error("unknown status is active")
	}
}

func TestTaskValidationRejectsInvalidRecords(t *testing.T) {
	valid := *NewTask(7, "Task", "Goal")
	zero := int64(0)
	cases := []struct {
		name   string
		mutate func(*Task)
	}{
		{"invalid project", func(task *Task) { task.ProjectID = 0 }},
		{"negative issue", func(task *Task) { task.IssueNumber = -1 }},
		{"missing title", func(task *Task) { task.Title = " " }},
		{"missing goal", func(task *Task) { task.Goal = "" }},
		{"invalid status", func(task *Task) { task.Status = "unknown" }},
		{"negative priority", func(task *Task) { task.Priority = -1 }},
		{"negative sequence", func(task *Task) { task.Sequence = -1 }},
		{"invalid discovery", func(task *Task) { task.SourceDiscoveryID = &zero }},
		{"negative PR", func(task *Task) { task.PRNumber = -1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			task := valid
			tc.mutate(&task)
			if err := task.Validate(); !errors.Is(err, ErrInvalidTask) {
				t.Errorf("Validate error = %v", err)
			}
		})
	}
	if err := (*Task)(nil).Validate(); !errors.Is(err, ErrInvalidTask) {
		t.Errorf("nil Validate error = %v", err)
	}
}
