package project

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
	"github.com/eslam-mahmoud/go-ai-agent/internal/workflow"
)

func TestPauseResumeInterruptsExecutionAndRejectsStaleWorkflowWrites(t *testing.T) {
	s := openControllerStore(t)
	project, task := createCurrentControllerTask(
		t,
		s,
		"owner/repo",
		domain.ProjectExecuting,
		domain.TaskDeveloping,
	)
	execution := domain.NewExecution(
		project.ID,
		task.ID,
		"developer",
		"codex",
		"gpt-test",
		1,
	)
	execution.Status = domain.ExecutionRunning
	execution.ProviderSessionID = "session-1"
	execution, err := s.CreateExecution(execution)
	if err != nil {
		t.Fatal(err)
	}
	controller, _ := NewController(s)

	paused, err := controller.Pause(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if paused.Project.State != domain.ProjectPaused ||
		paused.Project.PausedFromState != domain.ProjectExecuting ||
		paused.CurrentTask == nil ||
		paused.CurrentTask.Status != domain.TaskDeveloping {
		t.Fatalf("paused snapshot = %#v", paused)
	}
	interrupted, err := s.GetExecutionByID(execution.ID)
	if err != nil || interrupted.Status != domain.ExecutionInterrupted {
		t.Fatalf("interrupted execution = %#v error=%v", interrupted, err)
	}

	execution.Status = domain.ExecutionCompleted
	if _, err := s.UpdateExecution(execution); !errors.Is(err, store.ErrExecutionUpdateConflict) {
		t.Fatalf("stale execution completion error = %v", err)
	}
	if _, err := controller.TaskStatus(project.ID, task.ID); !errors.Is(err, store.ErrProjectPaused) {
		t.Fatalf("paused task status error = %v", err)
	}
	if _, err := controller.TransitionTask(
		project.ID,
		task.ID,
		domain.TaskReviewing,
		workflow.TaskTransitionEvidence{},
	); !errors.Is(err, store.ErrProjectPaused) {
		t.Fatalf("paused transition error = %v", err)
	}

	resumed, err := controller.Resume(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Project.State != domain.ProjectExecuting ||
		resumed.Project.PausedFromState != "" {
		t.Fatalf("resumed snapshot = %#v", resumed)
	}
	updated, retry, err := controller.Retry(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.CurrentTask.Status != domain.TaskDeveloping ||
		retry.Attempt != 2 ||
		retry.Status != domain.ExecutionPending ||
		retry.ProviderSessionID != "session-1" {
		t.Fatalf("retry snapshot=%#v execution=%#v", updated, retry)
	}
	original, _ := s.GetExecutionByID(execution.ID)
	if original.Status != domain.ExecutionInterrupted {
		t.Fatalf("retry mutated original execution: %#v", original)
	}
}

func TestCancelCurrentTaskAndExecutionAtomicallyReleasesLanes(t *testing.T) {
	s := openControllerStore(t)
	project, task := createCurrentControllerTask(
		t,
		s,
		"owner/one",
		domain.ProjectExecuting,
		domain.TaskReviewing,
	)
	started := time.Now().UTC().Add(-time.Minute)
	historical := domain.NewExecution(project.ID, task.ID, "reviewer", "claude", "", 1)
	historical, err := s.CreateExecution(historical)
	if err != nil {
		t.Fatal(err)
	}
	historical.Status = domain.ExecutionInterrupted
	historical, err = s.UpdateExecution(historical)
	if err != nil {
		t.Fatal(err)
	}
	execution := domain.NewExecution(project.ID, task.ID, "reviewer", "claude", "", 2)
	execution.Status = domain.ExecutionRunning
	execution.StartedAt = &started
	execution, err = s.CreateExecution(execution)
	if err != nil {
		t.Fatal(err)
	}
	controller, _ := NewController(s)

	cancelled, err := controller.Cancel(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Project.State != domain.ProjectPlanning ||
		cancelled.Project.CurrentTaskID != nil ||
		cancelled.CurrentTask != nil {
		t.Fatalf("cancelled snapshot = %#v", cancelled)
	}
	storedTask, _ := s.GetProjectTaskByID(task.ID)
	if storedTask.Status != domain.TaskCancelled {
		t.Fatalf("cancelled task = %#v", storedTask)
	}
	storedExecution, _ := s.GetExecutionByID(execution.ID)
	if storedExecution.Status != domain.ExecutionCancelled ||
		storedExecution.ErrorClass != "cancelled" ||
		storedExecution.CompletedAt == nil {
		t.Fatalf("cancelled execution = %#v", storedExecution)
	}
	storedHistorical, _ := s.GetExecutionByID(historical.ID)
	if storedHistorical.Status != domain.ExecutionInterrupted {
		t.Fatalf("cancellation rewrote execution history: %#v", storedHistorical)
	}
	if _, err := controller.Cancel(project.ID); !errors.Is(err, ErrNoCurrentTask) {
		t.Fatalf("second cancel error = %v", err)
	}

	otherProject, otherTask := createCurrentControllerTask(
		t,
		s,
		"owner/two",
		domain.ProjectExecuting,
		domain.TaskDeveloping,
	)
	next := domain.NewExecution(otherProject.ID, otherTask.ID, "developer", "codex", "", 1)
	next.Status = domain.ExecutionRunning
	if _, err := s.CreateExecution(next); err != nil {
		t.Fatalf("released lanes were not reusable: %v", err)
	}
}

func TestCancelWhilePausedKeepsProjectPausedForExplicitResume(t *testing.T) {
	s := openControllerStore(t)
	project, task := createCurrentControllerTask(
		t,
		s,
		"owner/repo",
		domain.ProjectExecuting,
		domain.TaskDeveloping,
	)
	execution := domain.NewExecution(project.ID, task.ID, "developer", "codex", "", 1)
	execution.Status = domain.ExecutionRunning
	execution, err := s.CreateExecution(execution)
	if err != nil {
		t.Fatal(err)
	}
	controller, _ := NewController(s)
	if _, err := controller.Pause(project.ID); err != nil {
		t.Fatal(err)
	}
	cancelled, err := controller.Cancel(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Project.State != domain.ProjectPaused ||
		cancelled.Project.PausedFromState != domain.ProjectPlanning ||
		cancelled.Project.CurrentTaskID != nil {
		t.Fatalf("paused cancellation snapshot = %#v", cancelled)
	}
	storedExecution, _ := s.GetExecutionByID(execution.ID)
	if storedExecution.Status != domain.ExecutionCancelled {
		t.Fatalf("paused cancellation execution = %#v", storedExecution)
	}
	resumed, err := controller.Resume(project.ID)
	if err != nil || resumed.Project.State != domain.ProjectPlanning {
		t.Fatalf("resume after cancellation snapshot=%#v error=%v", resumed, err)
	}
}

func TestRetryRestoresBlockedTaskPhaseForEveryRetryableStatus(t *testing.T) {
	for _, status := range []domain.ExecutionStatus{
		domain.ExecutionFailed,
		domain.ExecutionCancelled,
		domain.ExecutionInterrupted,
	} {
		t.Run(string(status), func(t *testing.T) {
			s := openControllerStore(t)
			project, task := createCurrentControllerTask(
				t,
				s,
				"owner/"+string(status),
				domain.ProjectBlocked,
				domain.TaskBlocked,
			)
			previous := domain.NewExecution(
				project.ID,
				task.ID,
				"developer",
				"codex",
				"gpt-test",
				1,
			)
			previous.ProviderSessionID = "resume-session"
			previous, err := s.CreateExecution(previous)
			if err != nil {
				t.Fatal(err)
			}
			previous.Status = status
			previous, err = s.UpdateExecution(previous)
			if err != nil {
				t.Fatal(err)
			}
			controller, _ := NewController(s)

			snapshot, retry, err := controller.Retry(project.ID)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.Project.State != domain.ProjectExecuting ||
				snapshot.CurrentTask.Status != domain.TaskDeveloping ||
				retry.Attempt != 2 ||
				retry.Status != domain.ExecutionPending ||
				retry.ProviderSessionID != "resume-session" {
				t.Fatalf("snapshot=%#v retry=%#v", snapshot, retry)
			}
			if _, _, err := controller.Retry(project.ID); !errors.Is(
				err,
				ErrExecutionNotRetryable,
			) {
				t.Fatalf("pending retry error = %v", err)
			}
		})
	}
}

func TestProjectControlRejectsInvalidStaleCrossProjectAndConcurrentRequests(t *testing.T) {
	t.Run("invalid states", func(t *testing.T) {
		s := openControllerStore(t)
		project, _ := createControllerProjectTask(
			t,
			s,
			"owner/completed",
			domain.TaskQueued,
		)
		project.State = domain.ProjectCompleted
		if _, err := s.UpdateProject(project); err != nil {
			t.Fatal(err)
		}
		controller, _ := NewController(s)
		if _, err := controller.Pause(project.ID); !errors.Is(err, ErrInvalidControl) {
			t.Fatalf("completed pause error = %v", err)
		}
		if _, err := controller.Resume(project.ID); !errors.Is(err, ErrInvalidControl) {
			t.Fatalf("active resume error = %v", err)
		}
	})

	t.Run("stale and cross-project", func(t *testing.T) {
		s := openControllerStore(t)
		first, firstTask := createCurrentControllerTask(
			t,
			s,
			"owner/first",
			domain.ProjectExecuting,
			domain.TaskDeveloping,
		)
		second, secondTask := createCurrentControllerTask(
			t,
			s,
			"owner/second",
			domain.ProjectExecuting,
			domain.TaskQueued,
		)
		execution := domain.NewExecution(
			second.ID,
			secondTask.ID,
			"developer",
			"codex",
			"",
			1,
		)
		execution.Status = domain.ExecutionFailed
		execution, err := s.CreateExecution(execution)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.CancelProjectTask(store.ProjectTaskCancellation{
			ProjectID:            first.ID,
			TaskID:               firstTask.ID,
			ExpectedProjectState: domain.ProjectBlocked,
			ExpectedTaskStatus:   domain.TaskDeveloping,
		}); !errors.Is(err, store.ErrProjectControlConflict) {
			t.Fatalf("stale cancellation error = %v", err)
		}
		if _, err := s.RetryProjectTaskExecution(store.ProjectExecutionRetry{
			ProjectID:            first.ID,
			TaskID:               firstTask.ID,
			ExecutionID:          execution.ID,
			ExpectedProjectState: domain.ProjectExecuting,
			ExpectedTaskStatus:   domain.TaskDeveloping,
			NewTaskStatus:        domain.TaskDeveloping,
		}); !errors.Is(err, store.ErrExecutionRetryConflict) {
			t.Fatalf("cross-project retry error = %v", err)
		}
	})

	t.Run("concurrent pause", func(t *testing.T) {
		s := openControllerStore(t)
		project, _ := createCurrentControllerTask(
			t,
			s,
			"owner/concurrent",
			domain.ProjectExecuting,
			domain.TaskDeveloping,
		)
		controller, _ := NewController(s)
		start := make(chan struct{})
		results := make(chan error, 2)
		var wait sync.WaitGroup
		for i := 0; i < 2; i++ {
			wait.Add(1)
			go func() {
				defer wait.Done()
				<-start
				_, err := controller.Pause(project.ID)
				results <- err
			}()
		}
		close(start)
		wait.Wait()
		close(results)
		successes := 0
		rejections := 0
		for err := range results {
			switch {
			case err == nil:
				successes++
			case errors.Is(err, ErrInvalidControl),
				errors.Is(err, store.ErrProjectControlConflict):
				rejections++
			default:
				t.Fatalf("unexpected concurrent pause error: %v", err)
			}
		}
		if successes != 1 || rejections != 1 {
			t.Fatalf("successes=%d rejections=%d", successes, rejections)
		}
	})
}

func createCurrentControllerTask(
	t *testing.T,
	s *store.Store,
	repo string,
	projectState domain.ProjectState,
	taskStatus domain.TaskStatus,
) (*domain.Project, *domain.Task) {
	t.Helper()
	project, task := createControllerProjectTask(t, s, repo, taskStatus)
	project.State = projectState
	project.CurrentTaskID = &task.ID
	project, err := s.UpdateProject(project)
	if err != nil {
		t.Fatal(err)
	}
	return project, task
}
