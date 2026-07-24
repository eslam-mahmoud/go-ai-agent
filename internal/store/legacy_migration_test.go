package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

func TestMigrateLegacyProjectCopiesStateAndIsIdempotent(t *testing.T) {
	s := openTestStore(t)
	ready, err := s.UpsertTask("owner/repo", 10, StateReady, "")
	if err != nil {
		t.Fatal(err)
	}
	done, err := s.BindTaskExecution(
		"owner/repo",
		20,
		StateDone,
		ExecutionBinding{
			Engine:            "codex",
			Model:             "gpt-test",
			ProviderSessionID: "codex-session",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetPRNumber("owner/repo", 20, 220); err != nil {
		t.Fatal(err)
	}
	done, err = s.GetTask("owner/repo", 20)
	if err != nil {
		t.Fatal(err)
	}
	active, err := s.BindTaskExecution(
		"owner/repo",
		30,
		StateInProgress,
		ExecutionBinding{
			Engine:            "claude",
			Model:             "sonnet-test",
			ProviderSessionID: "claude-session",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	waiting, err := s.BindTaskExecution(
		"owner/repo",
		40,
		StateDone,
		ExecutionBinding{
			Engine:            "claude",
			ProviderSessionID: "waiting-session",
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	report, err := s.MigrateLegacyProject(LegacyProjectMigrationOptions{
		Repo:              "owner/repo",
		Name:              "Madar",
		Goal:              "Ship v2",
		Scope:             "Legacy compatibility",
		ReleaseTarget:     "v2.0.0",
		ParentIssueNumber: 99,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.ProjectCreated ||
		report.LegacyTasks != 4 ||
		report.MigratedTasks != 4 ||
		report.AlreadyMigrated != 0 ||
		report.ProjectTasksCreated != 4 ||
		report.ExecutionsCreated != 3 {
		t.Fatalf("migration report = %#v", report)
	}
	project := report.Project
	if project.State != domain.ProjectExecuting ||
		project.CurrentTaskID == nil ||
		project.ReleaseTarget != "v2.0.0" ||
		project.ParentIssueNumber != 99 {
		t.Fatalf("migrated project = %#v", project)
	}
	tasks, err := s.ListProjectTasks(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 4 {
		t.Fatalf("task count = %d", len(tasks))
	}
	wantStatuses := []domain.TaskStatus{
		domain.TaskQueued,
		domain.TaskCompleted,
		domain.TaskDeveloping,
		domain.TaskCompleted,
	}
	legacyRows := []*Task{ready, done, active, waiting}
	for index, task := range tasks {
		if task.Sequence != index+1 ||
			task.IssueNumber != legacyRows[index].IssueNumber ||
			task.Status != wantStatuses[index] ||
			task.Source != "legacy" ||
			task.TaskType != "legacy-issue" ||
			task.BranchName != "madar/issue-"+itoa(legacyRows[index].IssueNumber) {
			t.Errorf("task %d = %#v", index, task)
		}
		if !task.CreatedAt.Equal(legacyRows[index].CreatedAt) ||
			!task.UpdatedAt.Equal(legacyRows[index].UpdatedAt) {
			t.Errorf("task %d timestamps changed: %#v legacy=%#v", index, task, legacyRows[index])
		}
	}
	if tasks[1].PRNumber != 220 {
		t.Errorf("migrated PR number = %d, want 220", tasks[1].PRNumber)
	}
	if *project.CurrentTaskID != tasks[2].ID {
		t.Errorf("current task = %d, want %d", *project.CurrentTaskID, tasks[2].ID)
	}

	assertMigratedExecution(
		t,
		s,
		tasks[1].ID,
		domain.ExecutionCompleted,
		"codex",
		"gpt-test",
		"codex-session",
		true,
	)
	assertMigratedExecution(
		t,
		s,
		tasks[2].ID,
		domain.ExecutionRunning,
		"claude",
		"sonnet-test",
		"claude-session",
		false,
	)
	assertMigratedExecution(
		t,
		s,
		tasks[3].ID,
		domain.ExecutionCompleted,
		"claude",
		"",
		"waiting-session",
		true,
	)
	if executions, err := s.ListTaskExecutions(tasks[0].ID); err != nil || len(executions) != 0 {
		t.Fatalf("ready task executions = %#v, error=%v", executions, err)
	}

	var mappings int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM legacy_project_migrations`).Scan(&mappings); err != nil {
		t.Fatal(err)
	}
	if mappings != 4 {
		t.Errorf("migration mappings = %d, want 4", mappings)
	}
	for _, legacy := range legacyRows {
		stored, err := s.GetTask("owner/repo", legacy.IssueNumber)
		if err != nil || stored == nil || stored.ID != legacy.ID {
			t.Errorf("legacy task %d was changed or removed: %#v, error=%v", legacy.ID, stored, err)
		}
		audit, err := s.GetAuditLog("owner/repo", legacy.IssueNumber)
		if err != nil || len(audit) != 1 || audit[0].Event != "migrated-to-project" {
			t.Errorf("legacy task %d audit = %#v, error=%v", legacy.ID, audit, err)
		}
	}

	again, err := s.MigrateLegacyProject(LegacyProjectMigrationOptions{
		Repo:  "owner/repo",
		Name:  "Ignored",
		Goal:  "Ignored",
		Scope: "Ignored",
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.ProjectCreated ||
		again.MigratedTasks != 0 ||
		again.AlreadyMigrated != 4 ||
		again.ProjectTasksCreated != 0 ||
		again.ExecutionsCreated != 0 ||
		again.Project.ID != project.ID ||
		again.Project.Name != "Madar" {
		t.Fatalf("idempotent report = %#v", again)
	}
}

func TestMigrateLegacyProjectReusesExistingProjectTask(t *testing.T) {
	s := openTestStore(t)
	legacy, err := s.BindTaskExecution(
		"owner/repo",
		7,
		StateInProgress,
		ExecutionBinding{Engine: "claude", ProviderSessionID: "session"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertTask("owner/repo", 8, StateReady, ""); err != nil {
		t.Fatal(err)
	}
	project, err := s.CreateProject(domain.NewProject(
		"owner/repo",
		"Existing",
		"Existing goal",
		"",
	))
	if err != nil {
		t.Fatal(err)
	}
	existingTask := domain.NewTask(project.ID, "Human title", "Human goal")
	existingTask.IssueNumber = 7
	existingTask, err = s.CreateProjectTask(existingTask)
	if err != nil {
		t.Fatal(err)
	}

	report, err := s.MigrateLegacyProject(LegacyProjectMigrationOptions{
		Repo: "owner/repo",
		Name: "Ignored",
		Goal: "Ignored",
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.ProjectCreated ||
		report.ProjectTasksCreated != 1 ||
		report.MigratedTasks != 2 ||
		report.ExecutionsCreated != 1 {
		t.Fatalf("migration report = %#v", report)
	}
	tasks, err := s.ListProjectTasks(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 ||
		tasks[0].ID != existingTask.ID ||
		tasks[0].Title != "Human title" ||
		tasks[1].IssueNumber != 8 {
		t.Fatalf("reused tasks = %#v", tasks)
	}
	executions, err := s.ListTaskExecutions(existingTask.ID)
	if err != nil || len(executions) != 1 || executions[0].ProviderSessionID != legacy.SessionID {
		t.Fatalf("reused task execution = %#v, error=%v", executions, err)
	}
	storedProject, err := s.GetProjectByID(project.ID)
	if err != nil || storedProject.State != domain.ProjectInitializing {
		t.Fatalf("existing project state changed: %#v, error=%v", storedProject, err)
	}
}

func TestMigrateLegacyProjectRollsBackInvalidOrMissingInput(t *testing.T) {
	t.Run("missing repository tasks", func(t *testing.T) {
		s := openTestStore(t)
		_, err := s.MigrateLegacyProject(LegacyProjectMigrationOptions{
			Repo: "owner/missing",
			Name: "Missing",
			Goal: "Goal",
		})
		if !errors.Is(err, ErrNoLegacyTasks) {
			t.Fatalf("error = %v, want ErrNoLegacyTasks", err)
		}
		project, getErr := s.GetProjectByRepo("owner/missing")
		if getErr != nil || project != nil {
			t.Fatalf("partial project = %#v, error=%v", project, getErr)
		}
	})

	t.Run("invalid legacy row", func(t *testing.T) {
		s := openTestStore(t)
		if _, err := s.UpsertTask("owner/repo", 1, StateReady, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := s.UpsertTask("owner/repo", 0, StateReady, ""); err != nil {
			t.Fatal(err)
		}
		_, err := s.MigrateLegacyProject(LegacyProjectMigrationOptions{
			Repo: "owner/repo",
			Name: "Madar",
			Goal: "Ship",
		})
		if !errors.Is(err, ErrLegacyMigrationConflict) {
			t.Fatalf("error = %v, want ErrLegacyMigrationConflict", err)
		}
		project, getErr := s.GetProjectByRepo("owner/repo")
		if getErr != nil || project != nil {
			t.Fatalf("partial project = %#v, error=%v", project, getErr)
		}
		var mappings int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM legacy_project_migrations`).Scan(&mappings); err != nil {
			t.Fatal(err)
		}
		if mappings != 0 {
			t.Fatalf("partial mappings = %d", mappings)
		}
	})
}

func TestLegacyMigrationPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "madar.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.UpsertTask("owner/repo", 1, StateReady, ""); err != nil {
		t.Fatal(err)
	}
	initial, err := first.MigrateLegacyProject(LegacyProjectMigrationOptions{
		Repo: "owner/repo",
		Name: "Madar",
		Goal: "Ship",
	})
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
	again, err := reopened.MigrateLegacyProject(LegacyProjectMigrationOptions{
		Repo: "owner/repo",
		Name: "Madar",
		Goal: "Ship",
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.Project.ID != initial.Project.ID ||
		again.MigratedTasks != 0 ||
		again.AlreadyMigrated != 1 {
		t.Fatalf("reopened migration report = %#v", again)
	}
	legacy, err := reopened.GetTask("owner/repo", 1)
	if err != nil || legacy == nil {
		t.Fatalf("legacy row after reopen = %#v, error=%v", legacy, err)
	}
}

func TestMigratedTaskStatusMappings(t *testing.T) {
	tests := []struct {
		name   string
		state  TaskState
		ci     CIState
		status domain.TaskStatus
	}{
		{"ready", StateReady, CIStateNone, domain.TaskQueued},
		{"active", StateInProgress, CIStateNone, domain.TaskDeveloping},
		{"interrupted", StateInterrupted, CIStateNone, domain.TaskDeveloping},
		{"recovering", StateRecovering, CIStateNone, domain.TaskDeveloping},
		{"waiting input", StateAwaitingFeedback, CIStateNone, domain.TaskWaitingInput},
		{"done", StateDone, CIStateNone, domain.TaskCompleted},
		{"ci waiting", StateInProgress, CIStateWaiting, domain.TaskWaitingCI},
		{"ci failed", StateInProgress, CIStateFailed, domain.TaskWaitingCI},
		{"ci gave up", StateInProgress, CIStateGaveUp, domain.TaskBlocked},
		{"ci passed", StateInProgress, CIStatePassed, domain.TaskVerifying},
		{"done ci passed", StateDone, CIStatePassed, domain.TaskCompleted},
	}
	for index, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, err := migratedTaskStatus(&Task{
				ID:          int64(index + 1),
				IssueNumber: index + 1,
				State:       tc.state,
				CIState:     tc.ci,
			})
			if err != nil || status != tc.status {
				t.Fatalf("status = %q, error=%v, want %q", status, err, tc.status)
			}
		})
	}
}

func assertMigratedExecution(
	t *testing.T,
	s *Store,
	taskID int64,
	status domain.ExecutionStatus,
	engineName, model, session string,
	wantCompleted bool,
) {
	t.Helper()
	executions, err := s.ListTaskExecutions(taskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(executions) != 1 {
		t.Fatalf("task %d executions = %#v", taskID, executions)
	}
	execution := executions[0]
	if execution.Mode != "legacy-developer" ||
		execution.Status != status ||
		execution.Engine != engineName ||
		execution.Model != model ||
		execution.ProviderSessionID != session ||
		execution.StartedAt == nil ||
		(execution.CompletedAt != nil) != wantCompleted {
		t.Errorf("task %d execution = %#v", taskID, execution)
	}
}

func itoa(value int) string {
	return fmt.Sprintf("%d", value)
}
