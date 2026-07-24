package store

import (
	"testing"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

func TestStartupInterruptionAndAuditAreAtomic(t *testing.T) {
	s := openTestStore(t)
	project, task := createQueuedTask(t, s, "owner/repo")
	execution := domain.NewExecution(project.ID, task.ID, "planner", "codex", "", 1)
	execution.Status = domain.ExecutionRunning
	execution, err := s.CreateExecution(execution)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertTask("legacy/repo", 1, StateInProgress, "legacy"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DROP TABLE workflow_events`); err != nil {
		t.Fatal(err)
	}

	if _, err := s.InterruptRunningExecutionsForRecovery(); err == nil {
		t.Fatal("startup interruption succeeded without an audit event")
	}
	stored, err := s.GetExecutionByID(execution.ID)
	if err != nil || stored.Status != domain.ExecutionRunning {
		t.Fatalf("unaudited interruption was not rolled back: %#v error=%v", stored, err)
	}
	legacy, err := s.GetTask("legacy/repo", 1)
	if err != nil || legacy.State != StateInProgress {
		t.Fatalf("legacy task changed during v2 recovery: %#v error=%v", legacy, err)
	}
}
