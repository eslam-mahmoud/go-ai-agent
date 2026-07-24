package store

import (
	"database/sql"
	"errors"
	"os"
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

func TestPruneCompletedTasks(t *testing.T) {
	s := openTestStore(t)
	_, _ = s.UpsertTask("r", 10, StateDone, "s1")
	_, _ = s.UpsertTask("r", 11, StateDone, "s2")
	_, _ = s.UpsertTask("r", 12, StateInProgress, "s3") // active — must not be deleted

	// Zero retention — all done tasks qualify.
	n, err := s.PruneCompletedTasks(0)
	if err != nil {
		t.Fatalf("PruneCompletedTasks: %v", err)
	}
	if n != 2 {
		t.Errorf("deleted %d rows, want 2", n)
	}

	// Active task must survive.
	task, _ := s.GetTask("r", 12)
	if task == nil || task.State != StateInProgress {
		t.Error("active task should not have been pruned")
	}
}

func TestPruneCompletedTasks_keepsRecent(t *testing.T) {
	s := openTestStore(t)
	_, _ = s.UpsertTask("r", 20, StateDone, "s")

	// 90-day retention — just-completed task should survive.
	n, err := s.PruneCompletedTasks(90 * 24 * time.Hour)
	if err != nil {
		t.Fatalf("PruneCompletedTasks: %v", err)
	}
	if n != 0 {
		t.Errorf("deleted %d rows, want 0 (task is recent)", n)
	}
}

func TestPruneAuditLog(t *testing.T) {
	s := openTestStore(t)
	_, _ = s.UpsertTask("r", 1, StateInProgress, "s")

	// Write two entries — we'll prune entries older than 1h.
	if err := s.Log("r", 1, "old_event", "details"); err != nil {
		t.Fatal(err)
	}
	if err := s.Log("r", 1, "new_event", "details"); err != nil {
		t.Fatal(err)
	}

	// Prune with a very short retention — both entries are older than 0ns so
	// both should be deleted.
	n, err := s.PruneAuditLog(0)
	if err != nil {
		t.Fatalf("PruneAuditLog: %v", err)
	}
	if n != 2 {
		t.Errorf("deleted %d rows, want 2", n)
	}

	// Verify they're gone.
	entries, _ := s.GetAuditLog("r", 1)
	if len(entries) != 0 {
		t.Errorf("expected 0 entries after prune, got %d", len(entries))
	}
}

func TestPruneAuditLog_keepsRecent(t *testing.T) {
	s := openTestStore(t)
	_, _ = s.UpsertTask("r", 2, StateInProgress, "s")
	if err := s.Log("r", 2, "event", "details"); err != nil {
		t.Fatal(err)
	}

	// Prune with 30-day retention — recent entries should survive.
	n, err := s.PruneAuditLog(30 * 24 * time.Hour)
	if err != nil {
		t.Fatalf("PruneAuditLog: %v", err)
	}
	if n != 0 {
		t.Errorf("deleted %d rows, want 0 (entries are recent)", n)
	}

	entries, _ := s.GetAuditLog("r", 2)
	if len(entries) != 1 {
		t.Errorf("expected 1 entry after prune of recent data, got %d", len(entries))
	}
}

func TestOpen_createsParentDirs(t *testing.T) {
	// Path inside a non-existent subdirectory — Open() must create it.
	path := filepath.Join(t.TempDir(), "new_subdir", "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open with non-existent parent dir: %v", err)
	}
	s.Close()
}

func TestOpenReadOnlyReadsExistingDatabaseWithoutWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	writer, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := writer.UpsertTask("owner/repo", 42, StateInProgress, "session"); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	defer writer.Close()

	reader, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer reader.Close()

	task, err := reader.GetTask("owner/repo", 42)
	if err != nil {
		t.Fatalf("GetTask through read-only store: %v", err)
	}
	if task == nil || task.SessionID != "session" {
		t.Errorf("read-only task = %#v", task)
	}
	if _, err := reader.UpsertTask("owner/repo", 43, StateReady, ""); err == nil {
		t.Fatal("write through read-only store unexpectedly succeeded")
	}
}

func TestOpenReadOnlyDoesNotCreateMissingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "test.db")
	if _, err := OpenReadOnly(path); err == nil {
		t.Fatal("OpenReadOnly created or opened a missing database")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("missing database exists after OpenReadOnly: %v", err)
	}
}

func TestMigration_setsVersion(t *testing.T) {
	s := openTestStore(t)
	v, err := s.SchemaVersion()
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	want := len(migrations)
	if v != want {
		t.Errorf("schema version = %d, want %d (one per migration)", v, want)
	}
}

func TestMigration_indexesExist(t *testing.T) {
	s := openTestStore(t)
	// Verify the indexes from v2 were created.
	indexes := []string{"idx_tasks_state", "idx_tasks_ci_state", "idx_audit_created"}
	for _, idx := range indexes {
		var name string
		err := s.db.QueryRow(
			`SELECT name FROM sqlite_master WHERE type='index' AND name=?`, idx,
		).Scan(&name)
		if err != nil {
			t.Errorf("index %q not found: %v", idx, err)
		}
	}
}

func TestMigration_idempotent(t *testing.T) {
	// Opening the same DB a second time should not error.
	dir := t.TempDir()
	path := dir + "/test.db"
	s1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	s1.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open (idempotent): %v", err)
	}
	defer s2.Close()

	v, _ := s2.SchemaVersion()
	if v != len(migrations) {
		t.Errorf("schema version after reopen = %d, want %d", v, len(migrations))
	}
}

func TestMigrationPreservesLegacyProviderSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		CREATE TABLE schema_migrations (
			version INTEGER PRIMARY KEY,
			applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)
	`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:2] {
		if _, err := db.Exec(migration.sql); err != nil {
			t.Fatalf("apply legacy migration v%d: %v", migration.version, err)
		}
		if _, err := db.Exec(`INSERT INTO schema_migrations (version) VALUES (?)`, migration.version); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`
		INSERT INTO tasks (repo, issue_number, session_id, state)
		VALUES ('owner/repo', 42, 'legacy-session', 'interrupted')
	`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("migrate legacy database: %v", err)
	}
	defer s.Close()
	task, err := s.GetTask("owner/repo", 42)
	if err != nil {
		t.Fatal(err)
	}
	if task.SessionID != "legacy-session" || task.Engine != "" || task.Model != "" {
		t.Fatalf("migrated task = %#v", task)
	}
	bound, err := s.BindTaskExecution("owner/repo", 42, StateRecovering, ExecutionBinding{
		Engine:            "claude",
		Model:             "sonnet",
		ProviderSessionID: "replacement-must-not-win",
	})
	if err != nil {
		t.Fatal(err)
	}
	if bound.SessionID != "legacy-session" || bound.Engine != "claude" || bound.Model != "sonnet" {
		t.Errorf("bound legacy task = %#v", bound)
	}
}

func TestBindTaskExecutionPinsIdentity(t *testing.T) {
	s := openTestStore(t)
	task, err := s.BindTaskExecution("owner/repo", 7, StateInProgress, ExecutionBinding{
		Engine:            "codex",
		Model:             "gpt-test",
		ProviderSessionID: "provisional",
	})
	if err != nil {
		t.Fatal(err)
	}
	if task.Engine != "codex" || task.Model != "gpt-test" || task.SessionID != "provisional" {
		t.Fatalf("initial binding = %#v", task)
	}

	if _, err := s.UpsertTask("owner/repo", 7, StateAwaitingFeedback, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSessionID("owner/repo", 7, "provider-session"); err != nil {
		t.Fatal(err)
	}
	task, _ = s.GetTask("owner/repo", 7)
	if task.Engine != "codex" || task.Model != "gpt-test" || task.SessionID != "provider-session" {
		t.Errorf("binding after transition/session update = %#v", task)
	}

	_, err = s.BindTaskExecution("owner/repo", 7, StateInProgress, ExecutionBinding{
		Engine:            "codex",
		Model:             "different-model",
		ProviderSessionID: "different-session",
	})
	if !errors.Is(err, ErrExecutionBindingConflict) {
		t.Errorf("conflicting binding error = %v", err)
	}
	task, _ = s.GetTask("owner/repo", 7)
	if task.Engine != "codex" || task.Model != "gpt-test" || task.SessionID != "provider-session" {
		t.Errorf("conflict changed binding = %#v", task)
	}
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
	_, _ = s.UpsertTask("r", 1, StateInProgress, "s")       // active — counted
	_, _ = s.UpsertTask("r", 2, StateAwaitingFeedback, "s") // parked — not counted
	_, _ = s.UpsertTask("r", 3, StateDone, "s")             // done — not counted
	_, _ = s.UpsertTask("r", 4, StateReady, "s")            // not started — not counted
	_, _ = s.UpsertTask("r", 5, StateInProgress, "s")       // active — counted
	_ = s.SetCIState("r", 5, CIStateWaiting)                // CI-watching — not counted
	_, _ = s.UpsertTask("r", 6, StateInProgress, "s")       // genuinely active — counted

	count, err := s.CountActive()
	if err != nil {
		t.Fatalf("CountActive: %v", err)
	}
	// Only in-progress tasks with ci_state='' count.
	// Issue 1: counted. Issue 5: CI-watching, not counted. Issue 6: counted.
	if count != 2 {
		t.Errorf("CountActive = %d, want 2", count)
	}
}

func TestRecoveryStatesRoundTrip(t *testing.T) {
	s := openTestStore(t)
	cases := []TaskState{StateInterrupted, StateRecovering}

	for i, state := range cases {
		issueNumber := 100 + i
		if _, err := s.UpsertTask("owner/repo", issueNumber, state, "session"); err != nil {
			t.Fatalf("UpsertTask(%q): %v", state, err)
		}
		task, err := s.GetTask("owner/repo", issueNumber)
		if err != nil {
			t.Fatalf("GetTask(%q): %v", state, err)
		}
		if task.State != state {
			t.Errorf("state round trip = %q, want %q", task.State, state)
		}
	}
}

func TestMarkActiveTasksInterrupted(t *testing.T) {
	s := openTestStore(t)
	_, _ = s.UpsertTask("r", 1, StateInProgress, "active")
	_, _ = s.UpsertTask("r", 2, StateRecovering, "recovering")
	_, _ = s.UpsertTask("r", 3, StateInProgress, "ci")
	_ = s.SetCIState("r", 3, CIStateWaiting)
	_, _ = s.UpsertTask("r", 4, StateAwaitingFeedback, "waiting")

	count, err := s.MarkActiveTasksInterrupted()
	if err != nil {
		t.Fatalf("MarkActiveTasksInterrupted: %v", err)
	}
	if count != 2 {
		t.Errorf("interrupted count = %d, want 2", count)
	}

	for _, issueNumber := range []int{1, 2} {
		task, _ := s.GetTask("r", issueNumber)
		if task.State != StateInterrupted {
			t.Errorf("issue %d state = %q, want interrupted", issueNumber, task.State)
		}
		entries, _ := s.GetAuditLog("r", issueNumber)
		if len(entries) != 1 || entries[0].Event != "interrupted" {
			t.Errorf("issue %d audit = %#v, want interrupted event", issueNumber, entries)
		}
	}

	ciTask, _ := s.GetTask("r", 3)
	if ciTask.State != StateInProgress {
		t.Errorf("CI-waiting task state = %q, want in-progress", ciTask.State)
	}
	waitingTask, _ := s.GetTask("r", 4)
	if waitingTask.State != StateAwaitingFeedback {
		t.Errorf("awaiting-feedback task state = %q", waitingTask.State)
	}
}

func TestCountActiveIncludesRecovering(t *testing.T) {
	s := openTestStore(t)
	_, _ = s.UpsertTask("r", 1, StateRecovering, "session")

	count, err := s.CountActive()
	if err != nil {
		t.Fatalf("CountActive: %v", err)
	}
	if count != 1 {
		t.Errorf("CountActive = %d, want 1 for recovering task", count)
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
