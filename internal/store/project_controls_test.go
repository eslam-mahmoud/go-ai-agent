package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

func TestPausedProjectMigrationDerivesDeterministicResumeState(t *testing.T) {
	for _, test := range []struct {
		name       string
		taskStatus domain.TaskStatus
		want       domain.ProjectState
	}{
		{"without current task", "", domain.ProjectPlanning},
		{"with active task", domain.TaskDeveloping, domain.ProjectExecuting},
		{"with blocked task", domain.TaskBlocked, domain.ProjectBlocked},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "v10.db")
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
			for _, migration := range migrations[:10] {
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
				) VALUES (
					'owner/repo', 'Project', 'Goal', 'paused', 'on-track',
					CURRENT_TIMESTAMP, CURRENT_TIMESTAMP
				)
			`)
			if err != nil {
				t.Fatal(err)
			}
			projectID, err := result.LastInsertId()
			if err != nil {
				t.Fatal(err)
			}
			if test.taskStatus != "" {
				result, err = db.Exec(`
					INSERT INTO project_tasks (
						project_id, title, goal, status, sequence,
						created_at, updated_at
					) VALUES (
						?, 'Task', 'Goal', ?, 1,
						CURRENT_TIMESTAMP, CURRENT_TIMESTAMP
					)
				`, projectID, string(test.taskStatus))
				if err != nil {
					t.Fatal(err)
				}
				taskID, err := result.LastInsertId()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`
					UPDATE projects SET current_task_id = ? WHERE id = ?
				`, taskID, projectID); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}

			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			project, err := s.GetProjectByID(projectID)
			if err != nil {
				t.Fatal(err)
			}
			if project.State != domain.ProjectPaused ||
				project.PausedFromState != test.want {
				t.Fatalf("migrated project = %#v, want resume %q", project, test.want)
			}
			if err := project.Validate(); err != nil {
				t.Fatalf("migrated project is invalid: %v", err)
			}
		})
	}
}
