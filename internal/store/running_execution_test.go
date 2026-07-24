package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

func TestSingleRunningExecutionAcrossConcurrentWriters(t *testing.T) {
	s := openTestStore(t)
	firstProject, firstTask := createQueuedTask(t, s, "owner/one")
	secondProject, secondTask := createQueuedTask(t, s, "owner/two")
	first := mustCreateExecution(t, s, firstProject.ID, firstTask.ID, "planner", 1)
	second := mustCreateExecution(t, s, secondProject.ID, secondTask.ID, "developer", 1)

	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for _, execution := range []*domain.Execution{first, second} {
		wait.Add(1)
		go func(candidate *domain.Execution) {
			defer wait.Done()
			<-start
			candidate.Status = domain.ExecutionRunning
			_, err := s.UpdateExecution(candidate)
			results <- err
		}(execution)
	}
	close(start)
	wait.Wait()
	close(results)

	successes := 0
	conflicts := 0
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrRunningExecutionExists):
			conflicts++
		default:
			t.Errorf("unexpected execution activation error: %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d", successes, conflicts)
	}

	firstStored, _ := s.GetExecutionByID(first.ID)
	secondStored, _ := s.GetExecutionByID(second.ID)
	var running, pending *domain.Execution
	if firstStored.Status.OccupiesProviderLane() {
		running, pending = firstStored, secondStored
	} else {
		running, pending = secondStored, firstStored
	}
	if pending.Status != domain.ExecutionPending {
		t.Fatalf("losing execution status = %q, want pending", pending.Status)
	}

	running.Status = domain.ExecutionInterrupted
	if _, err := s.UpdateExecution(running); err != nil {
		t.Fatalf("release provider lane: %v", err)
	}
	pending.Status = domain.ExecutionRunning
	if _, err := s.UpdateExecution(pending); err != nil {
		t.Fatalf("reuse provider lane: %v", err)
	}

	var count int
	if err := s.db.QueryRow(`
		SELECT COUNT(*) FROM executions WHERE status = 'running'
	`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("running execution count = %d, want 1", count)
	}
}

func TestSingleRunningExecutionConstraintCoversCreateAndDirectSQL(t *testing.T) {
	s := openTestStore(t)
	firstProject, firstTask := createQueuedTask(t, s, "owner/one")
	secondProject, secondTask := createQueuedTask(t, s, "owner/two")

	first := domain.NewExecution(firstProject.ID, firstTask.ID, "planner", "codex", "", 1)
	first.Status = domain.ExecutionRunning
	if _, err := s.CreateExecution(first); err != nil {
		t.Fatal(err)
	}

	second := domain.NewExecution(secondProject.ID, secondTask.ID, "developer", "claude", "", 1)
	second.Status = domain.ExecutionRunning
	if _, err := s.CreateExecution(second); !errors.Is(err, ErrRunningExecutionExists) {
		t.Fatalf("running create error = %v", err)
	}

	second.Status = domain.ExecutionPending
	second, err := s.CreateExecution(second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`
		UPDATE executions SET status = 'running' WHERE id = ?
	`, second.ID); err == nil {
		t.Fatal("direct SQL bypassed the one-running-execution constraint")
	}
	stored, err := s.GetExecutionByID(second.ID)
	if err != nil || stored.Status != domain.ExecutionPending {
		t.Fatalf("failed direct update changed execution: %#v error=%v", stored, err)
	}
}

func TestLegacyTasksRemainIndependentFromV2ExecutionLane(t *testing.T) {
	s := openTestStore(t)
	project, task := createQueuedTask(t, s, "owner/project")
	execution := domain.NewExecution(project.ID, task.ID, "developer", "codex", "", 1)
	execution.Status = domain.ExecutionRunning
	if _, err := s.CreateExecution(execution); err != nil {
		t.Fatal(err)
	}

	for _, repo := range []string{"legacy/one", "legacy/two"} {
		if _, err := s.UpsertTask(repo, 1, StateInProgress, "session"); err != nil {
			t.Fatalf("legacy task %s: %v", repo, err)
		}
	}
}

func TestLegacyMigrationRollsBackRunningExecutionConflict(t *testing.T) {
	s := openTestStore(t)
	project, task := createQueuedTask(t, s, "owner/existing")
	execution := domain.NewExecution(project.ID, task.ID, "developer", "codex", "", 1)
	execution.Status = domain.ExecutionRunning
	if _, err := s.CreateExecution(execution); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertTask("owner/legacy", 1, StateInProgress, "session"); err != nil {
		t.Fatal(err)
	}

	_, err := s.MigrateLegacyProject(LegacyProjectMigrationOptions{
		Repo: "owner/legacy",
		Name: "Legacy",
		Goal: "Goal",
	})
	if !errors.Is(err, ErrRunningExecutionExists) {
		t.Fatalf("migration error = %v", err)
	}
	migrated, getErr := s.GetProjectByRepo("owner/legacy")
	if getErr != nil || migrated != nil {
		t.Fatalf("migration partially created project: %#v error=%v", migrated, getErr)
	}
	legacy, getErr := s.GetTask("owner/legacy", 1)
	if getErr != nil || legacy == nil || legacy.State != StateInProgress {
		t.Fatalf("legacy source changed: %#v error=%v", legacy, getErr)
	}
}

func TestRunningExecutionMigrationRejectsExistingConflictAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v9.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		PRAGMA foreign_keys=ON;
		CREATE TABLE schema_migrations (
			version INTEGER PRIMARY KEY,
			applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
	`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:9] {
		if _, err := db.Exec(migration.sql); err != nil {
			t.Fatalf("apply migration v%d: %v", migration.version, err)
		}
		if _, err := db.Exec(
			`INSERT INTO schema_migrations (version) VALUES (?)`,
			migration.version,
		); err != nil {
			t.Fatal(err)
		}
	}
	result, err := db.Exec(`
		INSERT INTO projects (
			repo, name, goal, state, health, created_at, updated_at
		) VALUES ('owner/v9', 'V9', 'Goal', 'initializing', 'on-track',
			CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
	`)
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	result, err = db.Exec(`
		INSERT INTO project_tasks (
			project_id, title, goal, status, sequence, created_at, updated_at
		) VALUES (?, 'First', 'Goal', 'queued', 1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
	`, projectID)
	if err != nil {
		t.Fatal(err)
	}
	firstTaskID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	result, err = db.Exec(`
		INSERT INTO project_tasks (
			project_id, title, goal, status, sequence, created_at, updated_at
		) VALUES (?, 'Second', 'Goal', 'queued', 2, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
	`, projectID)
	if err != nil {
		t.Fatal(err)
	}
	secondTaskID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []struct {
		taskID int64
		mode   string
	}{
		{firstTaskID, "planner"},
		{secondTaskID, "developer"},
	} {
		if _, err := db.Exec(`
			INSERT INTO executions (
				project_id, task_id, mode, engine, attempt, status
			) VALUES (?, ?, ?, 'codex', 1, 'running')
		`, projectID, candidate.taskID, candidate.mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(path); !errors.Is(err, ErrRunningExecutionExists) {
		t.Fatalf("upgrade error = %v", err)
	}

	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err := db.QueryRow(`
		SELECT COALESCE(MAX(version), 0) FROM schema_migrations
	`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 9 {
		t.Fatalf("schema version after failed upgrade = %d, want 9", version)
	}
	var indexCount int
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'index' AND name = 'idx_executions_single_running'
	`).Scan(&indexCount); err != nil {
		t.Fatal(err)
	}
	if indexCount != 0 {
		t.Fatal("failed migration left the running-execution index behind")
	}
}

func TestSingleRunningExecutionIndexExists(t *testing.T) {
	s := openTestStore(t)
	var name string
	if err := s.db.QueryRow(`
		SELECT name FROM sqlite_master
		WHERE type = 'index' AND name = 'idx_executions_single_running'
	`).Scan(&name); err != nil {
		t.Fatal(err)
	}
}

func mustCreateExecution(
	t *testing.T,
	s *Store,
	projectID, taskID int64,
	mode string,
	attempt int,
) *domain.Execution {
	t.Helper()
	execution, err := s.CreateExecution(
		domain.NewExecution(projectID, taskID, mode, "codex", "", attempt),
	)
	if err != nil {
		t.Fatal(err)
	}
	return execution
}
