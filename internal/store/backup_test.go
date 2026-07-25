package store

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

func TestBackupAndRestoreRoundTripsEveryTable(t *testing.T) {
	source := openTestStore(t)
	seeded := seedDurableState(t, source)

	backupPath := filepath.Join(t.TempDir(), "madar.backup")
	info, err := source.Backup(backupPath)
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if info.SchemaVersion != CurrentSchemaVersion() {
		t.Fatalf("backup schema = %d, want %d", info.SchemaVersion, CurrentSchemaVersion())
	}
	if info.Projects != 1 || info.Tasks != 2 || info.Executions != 1 || info.Bytes == 0 {
		t.Fatalf("backup info = %#v", info)
	}

	restorePath := filepath.Join(t.TempDir(), "restored.db")
	if _, err := Restore(backupPath, restorePath, RestoreOptions{}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	restored, err := Open(restorePath)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()

	project, err := restored.GetProjectByRepo(seeded.repo)
	if err != nil || project == nil {
		t.Fatalf("restored project = %#v, err = %v", project, err)
	}
	tasks, err := restored.ListProjectTasks(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 || tasks[0].Title != "First" || tasks[1].Title != "Second" {
		t.Fatalf("restored tasks = %#v", tasks)
	}
	executions, err := restored.ListProjectExecutions(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(executions) != 1 {
		t.Fatalf("restored executions = %d", len(executions))
	}
	discoveries, err := restored.ListDiscoveries(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(discoveries) != 1 || discoveries[0].Title != "Retry budget is unbounded" {
		t.Fatalf("restored discoveries = %#v", discoveries)
	}
	reviews, err := restored.ListManagerReviews(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reviews) != 1 || reviews[0].OwnerUpdate != "Project remains on track." {
		t.Fatalf("restored reviews = %#v", reviews)
	}
	events, err := restored.ListWorkflowEvents(project.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("restored database has no workflow events")
	}
}

// A restore must never silently destroy current state.
func TestRestoreRefusesToOverwriteWithoutPermission(t *testing.T) {
	source := openTestStore(t)
	seedDurableState(t, source)
	backupPath := filepath.Join(t.TempDir(), "madar.backup")
	if _, err := source.Backup(backupPath); err != nil {
		t.Fatal(err)
	}

	existingDir := t.TempDir()
	existing := filepath.Join(existingDir, "madar.db")
	occupant, err := Open(existing)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := occupant.CreateProject(
		domain.NewProject("owner/occupant", "Occupant", "Goal", ""),
	); err != nil {
		t.Fatal(err)
	}
	if err := occupant.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := Restore(backupPath, existing, RestoreOptions{}); !errors.Is(
		err, ErrBackupExists,
	) {
		t.Fatalf("restore error = %v", err)
	}
	// The occupant is untouched.
	unchanged, err := Open(existing)
	if err != nil {
		t.Fatal(err)
	}
	project, err := unchanged.GetProjectByRepo("owner/occupant")
	if err != nil || project == nil {
		t.Fatalf("refused restore damaged the target: %#v, %v", project, err)
	}
	if err := unchanged.Close(); err != nil {
		t.Fatal(err)
	}

	// With permission it replaces the database.
	if _, err := Restore(backupPath, existing, RestoreOptions{Overwrite: true}); err != nil {
		t.Fatalf("overwriting restore: %v", err)
	}
	replaced, err := Open(existing)
	if err != nil {
		t.Fatal(err)
	}
	defer replaced.Close()
	if gone, _ := replaced.GetProjectByRepo("owner/occupant"); gone != nil {
		t.Fatal("overwrite did not replace the database")
	}

	// Backing up over an existing file is refused for the same reason.
	if _, err := source.Backup(backupPath); !errors.Is(err, ErrBackupExists) {
		t.Fatalf("backup overwrite error = %v", err)
	}
}

func TestInspectBackupRejectsUnusableFiles(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	corrupt := filepath.Join(directory, "corrupt.db")
	if err := os.WriteFile(corrupt, []byte("this is not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectBackup(corrupt); !errors.Is(err, ErrBackupUnreadable) {
		t.Fatalf("corrupt file error = %v", err)
	}

	// A valid SQLite file that is not a Madar database is also rejected.
	foreign := filepath.Join(directory, "foreign.db")
	database, err := sql.Open("sqlite", foreign)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`CREATE TABLE unrelated (id INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectBackup(foreign); !errors.Is(err, ErrBackupUnreadable) {
		t.Fatalf("foreign database error = %v", err)
	}

	if _, err := InspectBackup(filepath.Join(directory, "absent.db")); !errors.Is(
		err, ErrBackupUnreadable,
	) {
		t.Fatalf("missing file error = %v", err)
	}
	if _, err := InspectBackup(""); !errors.Is(err, ErrInvalidBackupPath) {
		t.Fatalf("empty path error = %v", err)
	}
}

// State must survive an upgrade: an older database migrates forward on open
// and keeps its rows.
func TestOlderDatabaseMigratesForwardAndKeepsItsRows(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "old.db")
	buildLegacyDatabase(t, path)

	upgraded, err := Open(path)
	if err != nil {
		t.Fatalf("opening an older database: %v", err)
	}
	version, err := upgraded.SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if version != CurrentSchemaVersion() {
		t.Fatalf("schema after upgrade = %d, want %d", version, CurrentSchemaVersion())
	}
	// The legacy row survived the migration.
	task, err := upgraded.GetTask("owner/legacy", 7)
	if err != nil || task == nil {
		t.Fatalf("legacy task after upgrade = %#v, err = %v", task, err)
	}
	if task.SessionID != "session-legacy" {
		t.Fatalf("legacy task = %#v", task)
	}
	if err := upgraded.Close(); err != nil {
		t.Fatal(err)
	}
}

// A backup taken before an upgrade restores and migrates the same way.
func TestBackupFromAnOlderSchemaRestoresAndMigrates(t *testing.T) {
	directory := t.TempDir()
	oldPath := filepath.Join(directory, "old.db")
	buildLegacyDatabase(t, oldPath)

	backupPath := filepath.Join(directory, "old.backup")
	source, err := Open(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	// Opening migrated it, so take the backup from a copy of the raw file
	// instead, which is what an operator's pre-upgrade backup looks like.
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(directory, "legacy-copy.db")
	buildLegacyDatabase(t, legacyPath)
	content, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backupPath, content, 0o600); err != nil {
		t.Fatal(err)
	}

	info, err := InspectBackup(backupPath)
	if err != nil {
		t.Fatalf("InspectBackup: %v", err)
	}
	if info.SchemaVersion != 3 {
		t.Fatalf("backup schema = %d, want 3", info.SchemaVersion)
	}
	// A pre-v2 backup has no project tables yet; that is incomplete, not
	// unreadable.
	if info.Projects != 0 {
		t.Fatalf("legacy backup reported %d projects", info.Projects)
	}

	restorePath := filepath.Join(directory, "restored.db")
	restored, err := Restore(backupPath, restorePath, RestoreOptions{})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if restored.SchemaVersion != CurrentSchemaVersion() {
		t.Fatalf("restored schema = %d, want %d",
			restored.SchemaVersion, CurrentSchemaVersion())
	}
	store, err := Open(restorePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	task, err := store.GetTask("owner/legacy", 7)
	if err != nil || task == nil {
		t.Fatalf("legacy row lost in restore: %#v, %v", task, err)
	}
}

// A forward migration cannot be undone, so a newer backup is refused.
func TestRestoreRefusesABackupFromANewerBinary(t *testing.T) {
	directory := t.TempDir()
	source := openTestStore(t)
	backupPath := filepath.Join(directory, "future.backup")
	if _, err := source.Backup(backupPath); err != nil {
		t.Fatal(err)
	}
	// Claim a schema version beyond what this binary knows.
	future, err := sql.Open("sqlite", backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := future.Exec(
		`INSERT INTO schema_migrations (version) VALUES (?)`,
		CurrentSchemaVersion()+5,
	); err != nil {
		t.Fatal(err)
	}
	if err := future.Close(); err != nil {
		t.Fatal(err)
	}

	restorePath := filepath.Join(directory, "restored.db")
	if _, err := Restore(backupPath, restorePath, RestoreOptions{}); !errors.Is(
		err, ErrBackupTooNew,
	) {
		t.Fatalf("newer backup error = %v", err)
	}
	if _, err := os.Stat(restorePath); !os.IsNotExist(err) {
		t.Fatal("a refused restore still wrote the target")
	}
}

type seededState struct {
	repo string
}

func seedDurableState(t *testing.T, s *Store) seededState {
	t.Helper()
	repo := "owner/backup"
	project, err := s.CreateProject(domain.NewProject(repo, "Madar", "Ship v2", "Scope"))
	if err != nil {
		t.Fatal(err)
	}
	var tasks []*domain.Task
	for _, title := range []string{"First", "Second"} {
		task := domain.NewTask(project.ID, title, title+" goal")
		task.Status = domain.TaskQueued
		created, err := s.CreateProjectTask(task)
		if err != nil {
			t.Fatal(err)
		}
		tasks = append(tasks, created)
	}
	if _, err := s.CreateExecution(domain.NewExecution(
		project.ID, tasks[0].ID, "developer", "codex", "gpt-test", 1,
	)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateDiscoveries(project.ID, []*domain.Discovery{
		domain.NewDiscovery(
			project.ID, tasks[0].ID, 0,
			"Retry budget is unbounded", domain.DiscoveryBug, domain.SeverityHigh,
		),
	}, "backup-seed"); err != nil {
		t.Fatal(err)
	}
	review := domain.NewManagerReview(project.ID)
	review.ReleaseReadiness = "not-ready"
	review.OwnerUpdate = "Project remains on track."
	if _, err := s.CreateManagerReview(review); err != nil {
		t.Fatal(err)
	}
	return seededState{repo: repo}
}

// buildLegacyDatabase creates a database at schema v3, the last version before
// the v2 project tables, which is what an upgrading installation looks like.
func buildLegacyDatabase(t *testing.T, path string) {
	t.Helper()
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.Exec(`
		CREATE TABLE schema_migrations (
			version INTEGER PRIMARY KEY,
			applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
	`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations {
		if migration.version > 3 {
			break
		}
		if _, err := database.Exec(migration.sql); err != nil {
			t.Fatalf("legacy migration v%d: %v", migration.version, err)
		}
		if _, err := database.Exec(
			`INSERT INTO schema_migrations (version) VALUES (?)`, migration.version,
		); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := database.Exec(`
		INSERT INTO tasks (repo, issue_number, session_id, state)
		VALUES ('owner/legacy', 7, 'session-legacy', 'ready')
	`); err != nil {
		t.Fatal(err)
	}
}
