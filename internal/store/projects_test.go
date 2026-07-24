package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

func TestProjectCRUDAndNullableFields(t *testing.T) {
	s := openTestStore(t)
	project := domain.NewProject("owner/repo", "Madar", "Ship v2", "Sequential manager")

	created, err := s.CreateProject(project)
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if created.ID <= 0 || created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() {
		t.Fatalf("created project identity/timestamps = %#v", created)
	}
	if created.CurrentTaskID != nil || created.LastManagerReviewAt != nil {
		t.Errorf("new project nullable fields = %#v", created)
	}

	byID, err := s.GetProjectByID(created.ID)
	if err != nil || byID == nil {
		t.Fatalf("GetProjectByID: project=%#v error=%v", byID, err)
	}
	byRepo, err := s.GetProjectByRepo("owner/repo")
	if err != nil || byRepo == nil || byRepo.ID != created.ID {
		t.Fatalf("GetProjectByRepo: project=%#v error=%v", byRepo, err)
	}

	projectTask, err := s.CreateProjectTask(domain.NewTask(
		created.ID,
		"Current project task",
		"Exercise the current task relationship",
	))
	if err != nil {
		t.Fatalf("CreateProjectTask: %v", err)
	}
	taskID := projectTask.ID
	reviewedAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	byID.ParentIssueNumber = 42
	byID.Name = "Madar v2"
	byID.Goal = "Ship the autonomous manager"
	byID.Scope = "All v2 milestones"
	byID.State = domain.ProjectExecuting
	byID.Health = domain.HealthAtRisk
	byID.CurrentTaskID = &taskID
	byID.CurrentPlanVersion = 3
	byID.ArchitectureVersion = 2
	byID.ReleaseTarget = "v2.0.0"
	byID.ReleaseReadiness = "tests pending"
	byID.LastManagerReviewAt = &reviewedAt

	updated, err := s.UpdateProject(byID)
	if err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}
	if updated.CreatedAt != created.CreatedAt {
		t.Errorf("CreatedAt changed: before=%v after=%v", created.CreatedAt, updated.CreatedAt)
	}
	if updated.ParentIssueNumber != 42 ||
		updated.State != domain.ProjectExecuting ||
		updated.Health != domain.HealthAtRisk ||
		updated.CurrentTaskID == nil ||
		*updated.CurrentTaskID != taskID ||
		updated.CurrentPlanVersion != 3 ||
		updated.ArchitectureVersion != 2 ||
		updated.ReleaseTarget != "v2.0.0" ||
		updated.ReleaseReadiness != "tests pending" ||
		updated.LastManagerReviewAt == nil ||
		!updated.LastManagerReviewAt.Equal(reviewedAt) {
		t.Errorf("updated project = %#v", updated)
	}

	updated.CurrentTaskID = nil
	updated.LastManagerReviewAt = nil
	updated, err = s.UpdateProject(updated)
	if err != nil {
		t.Fatal(err)
	}
	if updated.CurrentTaskID != nil || updated.LastManagerReviewAt != nil {
		t.Errorf("cleared nullable fields = %#v", updated)
	}
}

func TestProjectCreateAndUpdateErrors(t *testing.T) {
	s := openTestStore(t)
	first, err := s.CreateProject(domain.NewProject("owner/one", "One", "Goal one", ""))
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateProject(domain.NewProject("owner/two", "Two", "Goal two", ""))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.CreateProject(domain.NewProject("owner/one", "Duplicate", "Goal", "")); !errors.Is(err, ErrProjectAlreadyExists) {
		t.Errorf("duplicate create error = %v", err)
	}
	second.Repo = first.Repo
	if _, err := s.UpdateProject(second); !errors.Is(err, ErrProjectAlreadyExists) {
		t.Errorf("duplicate update error = %v", err)
	}

	invalid := domain.NewProject("", "Invalid", "Goal", "")
	if _, err := s.CreateProject(invalid); !errors.Is(err, domain.ErrInvalidProject) {
		t.Errorf("invalid create error = %v", err)
	}
	missing := domain.NewProject("owner/missing", "Missing", "Goal", "")
	missing.ID = 9999
	if _, err := s.UpdateProject(missing); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("missing update error = %v", err)
	}
}

func TestListProjectsIsDeterministic(t *testing.T) {
	s := openTestStore(t)
	for _, repo := range []string{"owner/zeta", "owner/alpha", "owner/middle"} {
		if _, err := s.CreateProject(domain.NewProject(repo, repo, "Goal", "")); err != nil {
			t.Fatal(err)
		}
	}
	projects, err := s.ListProjects()
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 3 {
		t.Fatalf("project count = %d", len(projects))
	}
	for i := 1; i < len(projects); i++ {
		if projects[i-1].ID >= projects[i].ID {
			t.Errorf("projects not ordered by ID: %#v", projects)
		}
	}
}

func TestProjectDatabaseConstraintsAndForeignKeys(t *testing.T) {
	s := openTestStore(t)
	var foreignKeys int
	if err := s.db.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if foreignKeys != 1 {
		t.Errorf("PRAGMA foreign_keys = %d, want 1", foreignKeys)
	}

	cases := []struct {
		name   string
		repo   string
		state  string
		health string
		plan   int
	}{
		{"invalid state", "owner/state", "invalid", "on-track", 0},
		{"invalid health", "owner/health", "initializing", "invalid", 0},
		{"negative plan version", "owner/version", "initializing", "on-track", -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.db.Exec(`
				INSERT INTO projects (
					repo, name, goal, state, health, current_plan_version,
					created_at, updated_at
				) VALUES (?, 'name', 'goal', ?, ?, ?, ?, ?)
			`, tc.repo, tc.state, tc.health, tc.plan, time.Now().UTC(), time.Now().UTC())
			if err == nil {
				t.Error("constraint accepted invalid project")
			}
		})
	}
}

func TestProjectMigrationReopenPreservesLegacyTask(t *testing.T) {
	path := filepath.Join(t.TempDir(), "madar.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.UpsertTask("owner/repo", 7, StateInProgress, "legacy-session"); err != nil {
		t.Fatal(err)
	}
	project, err := first.CreateProject(domain.NewProject("owner/repo", "Madar", "Ship v2", ""))
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	task, err := reopened.GetTask("owner/repo", 7)
	if err != nil || task == nil || task.SessionID != "legacy-session" {
		t.Fatalf("legacy task after reopen = %#v, error=%v", task, err)
	}
	got, err := reopened.GetProjectByID(project.ID)
	if err != nil || got == nil || got.Repo != "owner/repo" {
		t.Fatalf("project after reopen = %#v, error=%v", got, err)
	}
}

func TestGetProjectMissing(t *testing.T) {
	s := openTestStore(t)
	byID, err := s.GetProjectByID(404)
	if err != nil || byID != nil {
		t.Errorf("GetProjectByID missing = %#v, error=%v", byID, err)
	}
	byRepo, err := s.GetProjectByRepo("owner/missing")
	if err != nil || byRepo != nil {
		t.Errorf("GetProjectByRepo missing = %#v, error=%v", byRepo, err)
	}
}
