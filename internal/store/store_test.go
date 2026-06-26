package store

import (
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestUpsertAndGet(t *testing.T) {
	s := openTestStore(t)

	task, err := s.UpsertTask("owner/repo", 42, StateInProgress, "session-abc")
	if err != nil {
		t.Fatalf("UpsertTask: %v", err)
	}
	if task == nil {
		t.Fatal("UpsertTask returned nil task")
	}
	if task.Repo != "owner/repo" {
		t.Errorf("Repo = %q", task.Repo)
	}
	if task.IssueNumber != 42 {
		t.Errorf("IssueNumber = %d", task.IssueNumber)
	}
	if task.SessionID != "session-abc" {
		t.Errorf("SessionID = %q", task.SessionID)
	}
	if task.State != StateInProgress {
		t.Errorf("State = %q", task.State)
	}

	// Get should return same task.
	got, err := s.GetTask("owner/repo", 42)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got == nil || got.IssueNumber != 42 {
		t.Fatal("GetTask returned wrong task")
	}
}

func TestGetTask_missing(t *testing.T) {
	s := openTestStore(t)
	got, err := s.GetTask("owner/repo", 99)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got != nil {
		t.Error("expected nil for missing task")
	}
}

func TestUpsert_updates(t *testing.T) {
	s := openTestStore(t)

	_, err := s.UpsertTask("owner/repo", 1, StateInProgress, "sess-1")
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	_, err = s.UpsertTask("owner/repo", 1, StateAwaitingFeedback, "")
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	got, _ := s.GetTask("owner/repo", 1)
	if got.State != StateAwaitingFeedback {
		t.Errorf("State = %q after update, want awaiting-feedback", got.State)
	}
	// Session ID should be preserved when empty string passed.
	if got.SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want sess-1", got.SessionID)
	}
}

func TestSetSessionID(t *testing.T) {
	s := openTestStore(t)
	_, _ = s.UpsertTask("owner/repo", 5, StateInProgress, "old-session")
	if err := s.SetSessionID("owner/repo", 5, "new-session"); err != nil {
		t.Fatalf("SetSessionID: %v", err)
	}
	got, _ := s.GetTask("owner/repo", 5)
	if got.SessionID != "new-session" {
		t.Errorf("SessionID = %q, want new-session", got.SessionID)
	}
}

func TestSetClarificationTime(t *testing.T) {
	s := openTestStore(t)
	_, _ = s.UpsertTask("owner/repo", 7, StateAwaitingFeedback, "s")
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.SetClarificationTime("owner/repo", 7, now); err != nil {
		t.Fatalf("SetClarificationTime: %v", err)
	}
	got, _ := s.GetTask("owner/repo", 7)
	if got.LastClarificationAt == nil {
		t.Fatal("LastClarificationAt is nil")
	}
	diff := got.LastClarificationAt.Sub(now)
	if diff < 0 {
		diff = -diff
	}
	if diff > time.Second {
		t.Errorf("LastClarificationAt diff %v too large", diff)
	}
}

func TestListByState(t *testing.T) {
	s := openTestStore(t)
	_, _ = s.UpsertTask("r1", 1, StateInProgress, "s1")
	_, _ = s.UpsertTask("r1", 2, StateAwaitingFeedback, "s2")
	_, _ = s.UpsertTask("r1", 3, StateInProgress, "s3")
	_, _ = s.UpsertTask("r1", 4, StateDone, "s4")

	inProg, err := s.ListByState(StateInProgress)
	if err != nil {
		t.Fatalf("ListByState: %v", err)
	}
	if len(inProg) != 2 {
		t.Errorf("in-progress count = %d, want 2", len(inProg))
	}

	done, _ := s.ListByState(StateDone)
	if len(done) != 1 {
		t.Errorf("done count = %d, want 1", len(done))
	}
}

func TestCountActive(t *testing.T) {
	s := openTestStore(t)
	_, _ = s.UpsertTask("r", 1, StateInProgress, "s")
	_, _ = s.UpsertTask("r", 2, StateAwaitingFeedback, "s") // parked — not counted
	_, _ = s.UpsertTask("r", 3, StateDone, "s")
	_, _ = s.UpsertTask("r", 4, StateReady, "s")
	_, _ = s.UpsertTask("r", 5, StateInProgress, "s") // second active Claude run

	count, err := s.CountActive()
	if err != nil {
		t.Fatalf("CountActive: %v", err)
	}
	// Only in-progress tasks count; awaiting-feedback is excluded.
	if count != 2 {
		t.Errorf("CountActive = %d, want 2 (awaiting-feedback excluded)", count)
	}
}

func TestLog(t *testing.T) {
	s := openTestStore(t)
	_, _ = s.UpsertTask("r", 10, StateInProgress, "s")
	if err := s.Log("r", 10, "claimed", "session=abc"); err != nil {
		t.Fatalf("Log: %v", err)
	}
	if err := s.Log("r", 10, "done", ""); err != nil {
		t.Fatalf("Log: %v", err)
	}

	entries, err := s.GetAuditLog("r", 10)
	if err != nil {
		t.Fatalf("GetAuditLog: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("audit log len = %d, want 2", len(entries))
	}
	if entries[0].Event != "claimed" {
		t.Errorf("first event = %q", entries[0].Event)
	}
	if entries[1].Event != "done" {
		t.Errorf("second event = %q", entries[1].Event)
	}
}
