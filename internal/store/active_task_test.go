package store

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

func TestSingleActiveProjectTaskAcrossProjectsAndConcurrentWriters(t *testing.T) {
	s := openTestStore(t)
	firstProject, firstTask := createQueuedTask(t, s, "owner/one")
	secondProject, secondTask := createQueuedTask(t, s, "owner/two")

	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for _, candidate := range []struct {
		project *domain.Project
		task    *domain.Task
	}{
		{firstProject, firstTask},
		{secondProject, secondTask},
	} {
		wait.Add(1)
		go func(project *domain.Project, task *domain.Task) {
			defer wait.Done()
			<-start
			task.Status = domain.TaskSelected
			_, err := s.UpdateProjectTask(task)
			results <- err
		}(candidate.project, candidate.task)
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
		case errors.Is(err, ErrActiveProjectTaskExists):
			conflicts++
		default:
			t.Errorf("unexpected activation error: %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d", successes, conflicts)
	}

	firstStored, _ := s.GetProjectTaskByID(firstTask.ID)
	secondStored, _ := s.GetProjectTaskByID(secondTask.ID)
	var active, inactive *domain.Task
	if firstStored.Status.Active() {
		active, inactive = firstStored, secondStored
	} else {
		active, inactive = secondStored, firstStored
	}
	active.Status = domain.TaskCompleted
	if _, err := s.UpdateProjectTask(active); err != nil {
		t.Fatalf("release active lane: %v", err)
	}
	inactive.Status = domain.TaskSelected
	if _, err := s.UpdateProjectTask(inactive); err != nil {
		t.Fatalf("reuse active lane: %v", err)
	}
}

func TestSingleActiveTaskConstraintCoversCreateControllerAndDirectSQL(t *testing.T) {
	s := openTestStore(t)
	project, active := createQueuedTask(t, s, "owner/one")
	active.Status = domain.TaskDeveloping
	if _, err := s.UpdateProjectTask(active); err != nil {
		t.Fatal(err)
	}

	otherProject, err := s.CreateProject(domain.NewProject("owner/two", "Two", "Goal", ""))
	if err != nil {
		t.Fatal(err)
	}
	second := domain.NewTask(otherProject.ID, "Second", "Goal")
	second.Status = domain.TaskBlocked
	if _, err := s.CreateProjectTask(second); !errors.Is(err, ErrActiveProjectTaskExists) {
		t.Fatalf("active create error = %v", err)
	}

	queued := domain.NewTask(otherProject.ID, "Queued", "Goal")
	queued.Status = domain.TaskQueued
	queued, err = s.CreateProjectTask(queued)
	if err != nil {
		t.Fatal(err)
	}
	err = s.ApplyProjectTaskTransition(ProjectTaskTransitionUpdate{
		ProjectID:      otherProject.ID,
		TaskID:         queued.ID,
		ExpectedStatus: domain.TaskQueued,
		NewStatus:      domain.TaskSelected,
		ProjectState:   domain.ProjectExecuting,
		SetCurrentTask: true,
		CurrentTaskID:  &queued.ID,
	})
	if !errors.Is(err, ErrActiveProjectTaskExists) {
		t.Fatalf("controller persistence error = %v", err)
	}
	stored, _ := s.GetProjectTaskByID(queued.ID)
	if stored.Status != domain.TaskQueued {
		t.Fatalf("failed controller transition changed task: %#v", stored)
	}

	_, err = s.db.Exec(`
		INSERT INTO project_tasks (
			project_id, issue_number, title, goal, status, sequence,
			created_at, updated_at
		) VALUES (?, 99, 'Direct', 'Goal', 'reviewing', 99, ?, ?)
	`, project.ID, time.Now().UTC(), time.Now().UTC())
	if err == nil {
		t.Fatal("direct SQL bypassed the one-active-task constraint")
	}
}

func TestLegacyTasksRemainIndependentFromV2ActiveLane(t *testing.T) {
	s := openTestStore(t)
	_, active := createQueuedTask(t, s, "owner/project")
	active.Status = domain.TaskSelected
	if _, err := s.UpdateProjectTask(active); err != nil {
		t.Fatal(err)
	}
	for _, repo := range []string{"legacy/one", "legacy/two"} {
		if _, err := s.UpsertTask(repo, 1, StateInProgress, "session"); err != nil {
			t.Fatalf("legacy task %s: %v", repo, err)
		}
	}
}

func TestLegacyMigrationRollsBackMultipleActiveRowsWithTypedError(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.UpsertTask("owner/repo", 1, StateInProgress, "one"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertTask("owner/repo", 2, StateAwaitingFeedback, "two"); err != nil {
		t.Fatal(err)
	}
	_, err := s.MigrateLegacyProject(LegacyProjectMigrationOptions{
		Repo: "owner/repo",
		Name: "Project",
		Goal: "Goal",
	})
	if !errors.Is(err, ErrActiveProjectTaskExists) {
		t.Fatalf("migration error = %v", err)
	}
	project, getErr := s.GetProjectByRepo("owner/repo")
	if getErr != nil || project != nil {
		t.Fatalf("migration partially created project: %#v error=%v", project, getErr)
	}
}

func TestSingleActiveTaskIndexExists(t *testing.T) {
	s := openTestStore(t)
	var name string
	if err := s.db.QueryRow(`
		SELECT name FROM sqlite_master
		WHERE type = 'index' AND name = 'idx_project_tasks_single_active'
	`).Scan(&name); err != nil {
		t.Fatal(err)
	}
}

func createQueuedTask(
	t *testing.T,
	s *Store,
	repo string,
) (*domain.Project, *domain.Task) {
	t.Helper()
	project, err := s.CreateProject(domain.NewProject(repo, repo, "Goal", ""))
	if err != nil {
		t.Fatal(err)
	}
	task := domain.NewTask(project.ID, "Task", "Goal")
	task.Status = domain.TaskQueued
	task, err = s.CreateProjectTask(task)
	if err != nil {
		t.Fatal(err)
	}
	return project, task
}
