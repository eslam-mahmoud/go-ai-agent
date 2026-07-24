package store

import (
	"errors"
	"sync"
	"testing"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

func TestSelectProjectNextTaskAppliesReasonStateAndAuditOnce(t *testing.T) {
	s := openTestStore(t)
	project, task := createQueuedTask(t, s, "owner/select")
	review := createSelectionReview(t, s, project.ID, task.ID, 0, "Next dependency for the MVP")

	selected, applied, err := s.SelectProjectNextTask(ProjectNextTaskSelection{
		ProjectID:       project.ID,
		ManagerReviewID: review.ID,
		TaskID:          task.ID,
		ExpectedStatus:  domain.TaskQueued,
		ProjectState:    domain.ProjectExecuting,
		Reason:          "  Next dependency for the MVP  ",
	})
	if err != nil {
		t.Fatalf("SelectProjectNextTask: %v", err)
	}
	if !applied {
		t.Fatal("first selection reported no change")
	}
	if selected.Status != domain.TaskSelected ||
		selected.SelectedReason != "Next dependency for the MVP" {
		t.Fatalf("selected = %#v", selected)
	}
	stored, err := s.GetProjectByID(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != domain.ProjectExecuting ||
		stored.CurrentTaskID == nil ||
		*stored.CurrentTaskID != task.ID {
		t.Fatalf("project = %#v", stored)
	}
	events, err := s.ListWorkflowEvents(project.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 ||
		events[0].Type != domain.WorkflowTaskSelected ||
		events[0].Source != domain.WorkflowSourceController ||
		events[0].TaskID == nil || *events[0].TaskID != task.ID {
		t.Fatalf("events = %#v", events)
	}

	again, applied, err := s.SelectProjectNextTask(ProjectNextTaskSelection{
		ProjectID:       project.ID,
		ManagerReviewID: review.ID,
		TaskID:          task.ID,
		ExpectedStatus:  domain.TaskQueued,
		ProjectState:    domain.ProjectExecuting,
		Reason:          "Next dependency for the MVP",
	})
	if err != nil {
		t.Fatalf("idempotent SelectProjectNextTask: %v", err)
	}
	if applied {
		t.Fatal("replayed selection reported a change")
	}
	if again.UpdatedAt != selected.UpdatedAt {
		t.Fatal("replayed selection rewrote the task")
	}
	events, _ = s.ListWorkflowEvents(project.ID, 0, 100)
	if len(events) != 1 {
		t.Fatalf("replay emitted %d events", len(events))
	}
}

func TestSelectProjectNextTaskAuthorizesByIssueNumber(t *testing.T) {
	s := openTestStore(t)
	project, err := s.CreateProject(domain.NewProject("owner/issue", "Madar", "Goal", ""))
	if err != nil {
		t.Fatal(err)
	}
	task := domain.NewTask(project.ID, "Task", "Goal")
	task.Status = domain.TaskQueued
	task.IssueNumber = 43
	task, err = s.CreateProjectTask(task)
	if err != nil {
		t.Fatal(err)
	}
	review := createSelectionReview(t, s, project.ID, 0, 43, "Unblocks the release")

	if _, applied, err := s.SelectProjectNextTask(ProjectNextTaskSelection{
		ProjectID:       project.ID,
		ManagerReviewID: review.ID,
		TaskID:          task.ID,
		ExpectedStatus:  domain.TaskQueued,
		ProjectState:    domain.ProjectExecuting,
		Reason:          "Unblocks the release",
	}); err != nil || !applied {
		t.Fatalf("SelectProjectNextTask applied=%v err=%v", applied, err)
	}
}

func TestSelectProjectNextTaskRejectsUnauthorizedAndStaleWrites(t *testing.T) {
	t.Run("stale review", func(t *testing.T) {
		s := openTestStore(t)
		project, task := createQueuedTask(t, s, "owner/stale")
		stale := createSelectionReview(t, s, project.ID, task.ID, 0, "First choice")
		createSelectionReview(t, s, project.ID, task.ID, 0, "Second choice")
		_, _, err := s.SelectProjectNextTask(ProjectNextTaskSelection{
			ProjectID:       project.ID,
			ManagerReviewID: stale.ID,
			TaskID:          task.ID,
			ExpectedStatus:  domain.TaskQueued,
			ProjectState:    domain.ProjectExecuting,
			Reason:          "First choice",
		})
		if !errors.Is(err, ErrProjectTaskSelectionConflict) {
			t.Fatalf("error = %v", err)
		}
		assertTaskUnchanged(t, s, task.ID, domain.TaskQueued)
	})

	t.Run("other task", func(t *testing.T) {
		s := openTestStore(t)
		project, task := createQueuedTask(t, s, "owner/other")
		other := domain.NewTask(project.ID, "Other", "Goal")
		other.Status = domain.TaskQueued
		other, err := s.CreateProjectTask(other)
		if err != nil {
			t.Fatal(err)
		}
		review := createSelectionReview(t, s, project.ID, task.ID, 0, "Chosen")
		_, _, err = s.SelectProjectNextTask(ProjectNextTaskSelection{
			ProjectID:       project.ID,
			ManagerReviewID: review.ID,
			TaskID:          other.ID,
			ExpectedStatus:  domain.TaskQueued,
			ProjectState:    domain.ProjectExecuting,
			Reason:          "Chosen",
		})
		if !errors.Is(err, ErrProjectTaskSelectionConflict) {
			t.Fatalf("error = %v", err)
		}
		assertTaskUnchanged(t, s, other.ID, domain.TaskQueued)
	})

	t.Run("stale expected status", func(t *testing.T) {
		s := openTestStore(t)
		project, task := createQueuedTask(t, s, "owner/status")
		review := createSelectionReview(t, s, project.ID, task.ID, 0, "Chosen")
		task.Status = domain.TaskDeferred
		if _, err := s.UpdateProjectTask(task); err != nil {
			t.Fatal(err)
		}
		_, _, err := s.SelectProjectNextTask(ProjectNextTaskSelection{
			ProjectID:       project.ID,
			ManagerReviewID: review.ID,
			TaskID:          task.ID,
			ExpectedStatus:  domain.TaskQueued,
			ProjectState:    domain.ProjectExecuting,
			Reason:          "Chosen",
		})
		if !errors.Is(err, ErrProjectTaskSelectionConflict) {
			t.Fatalf("error = %v", err)
		}
		assertTaskUnchanged(t, s, task.ID, domain.TaskDeferred)
	})

	t.Run("occupied delivery lane", func(t *testing.T) {
		s := openTestStore(t)
		busyProject, busyTask := createQueuedTask(t, s, "owner/busy")
		busyTask.Status = domain.TaskDeveloping
		if _, err := s.UpdateProjectTask(busyTask); err != nil {
			t.Fatal(err)
		}
		_ = busyProject
		project, task := createQueuedTask(t, s, "owner/waiting")
		review := createSelectionReview(t, s, project.ID, task.ID, 0, "Chosen")
		_, _, err := s.SelectProjectNextTask(ProjectNextTaskSelection{
			ProjectID:       project.ID,
			ManagerReviewID: review.ID,
			TaskID:          task.ID,
			ExpectedStatus:  domain.TaskQueued,
			ProjectState:    domain.ProjectExecuting,
			Reason:          "Chosen",
		})
		if !errors.Is(err, ErrActiveProjectTaskExists) {
			t.Fatalf("error = %v", err)
		}
		assertTaskUnchanged(t, s, task.ID, domain.TaskQueued)
	})

	t.Run("paused project", func(t *testing.T) {
		s := openTestStore(t)
		project, task := createQueuedTask(t, s, "owner/paused")
		review := createSelectionReview(t, s, project.ID, task.ID, 0, "Chosen")
		if err := s.PauseProject(project.ID, project.State); err != nil {
			t.Fatal(err)
		}
		_, _, err := s.SelectProjectNextTask(ProjectNextTaskSelection{
			ProjectID:       project.ID,
			ManagerReviewID: review.ID,
			TaskID:          task.ID,
			ExpectedStatus:  domain.TaskQueued,
			ProjectState:    domain.ProjectExecuting,
			Reason:          "Chosen",
		})
		if !errors.Is(err, ErrProjectPaused) {
			t.Fatalf("error = %v", err)
		}
		assertTaskUnchanged(t, s, task.ID, domain.TaskQueued)
	})

	t.Run("foreign task", func(t *testing.T) {
		s := openTestStore(t)
		project, task := createQueuedTask(t, s, "owner/home")
		_, foreign := createQueuedTask(t, s, "owner/foreign")
		review := createSelectionReview(t, s, project.ID, task.ID, 0, "Chosen")
		_, _, err := s.SelectProjectNextTask(ProjectNextTaskSelection{
			ProjectID:       project.ID,
			ManagerReviewID: review.ID,
			TaskID:          foreign.ID,
			ExpectedStatus:  domain.TaskQueued,
			ProjectState:    domain.ProjectExecuting,
			Reason:          "Chosen",
		})
		if !errors.Is(err, domain.ErrInvalidTask) {
			t.Fatalf("error = %v", err)
		}
		assertTaskUnchanged(t, s, foreign.ID, domain.TaskQueued)
	})

	t.Run("conflicting reason on selected task", func(t *testing.T) {
		s := openTestStore(t)
		project, task := createQueuedTask(t, s, "owner/reason")
		review := createSelectionReview(t, s, project.ID, task.ID, 0, "Chosen")
		if _, _, err := s.SelectProjectNextTask(ProjectNextTaskSelection{
			ProjectID:       project.ID,
			ManagerReviewID: review.ID,
			TaskID:          task.ID,
			ExpectedStatus:  domain.TaskQueued,
			ProjectState:    domain.ProjectExecuting,
			Reason:          "Chosen",
		}); err != nil {
			t.Fatal(err)
		}
		_, _, err := s.SelectProjectNextTask(ProjectNextTaskSelection{
			ProjectID:       project.ID,
			ManagerReviewID: review.ID,
			TaskID:          task.ID,
			ExpectedStatus:  domain.TaskQueued,
			ProjectState:    domain.ProjectExecuting,
			Reason:          "A different reason",
		})
		if !errors.Is(err, ErrProjectTaskSelectionConflict) {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestSelectProjectNextTaskRejectsInvalidInput(t *testing.T) {
	s := openTestStore(t)
	project, task := createQueuedTask(t, s, "owner/input")
	review := createSelectionReview(t, s, project.ID, task.ID, 0, "Chosen")
	base := ProjectNextTaskSelection{
		ProjectID:       project.ID,
		ManagerReviewID: review.ID,
		TaskID:          task.ID,
		ExpectedStatus:  domain.TaskQueued,
		ProjectState:    domain.ProjectExecuting,
		Reason:          "Chosen",
	}
	tests := []struct {
		name   string
		mutate func(*ProjectNextTaskSelection)
		want   error
	}{
		{"missing project", func(s *ProjectNextTaskSelection) { s.ProjectID = 0 }, domain.ErrInvalidTask},
		{"missing review", func(s *ProjectNextTaskSelection) { s.ManagerReviewID = 0 }, domain.ErrInvalidTask},
		{"missing task", func(s *ProjectNextTaskSelection) { s.TaskID = 0 }, domain.ErrInvalidTask},
		{"bad status", func(s *ProjectNextTaskSelection) { s.ExpectedStatus = "nonsense" }, domain.ErrInvalidTask},
		{"blank reason", func(s *ProjectNextTaskSelection) { s.Reason = "   " }, domain.ErrInvalidTask},
		{"bad project state", func(s *ProjectNextTaskSelection) { s.ProjectState = "nonsense" }, domain.ErrInvalidProject},
		{"unknown project", func(s *ProjectNextTaskSelection) { s.ProjectID = 9999 }, ErrProjectNotFound},
		{"unknown task", func(s *ProjectNextTaskSelection) { s.TaskID = 9999 }, ErrProjectTaskNotFound},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			selection := base
			test.mutate(&selection)
			if _, _, err := s.SelectProjectNextTask(selection); !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			assertTaskUnchanged(t, s, task.ID, domain.TaskQueued)
		})
	}
}

func TestSelectProjectNextTaskConcurrentWritersSelectOnce(t *testing.T) {
	s := openTestStore(t)
	project, task := createQueuedTask(t, s, "owner/race")
	review := createSelectionReview(t, s, project.ID, task.ID, 0, "Chosen")
	selection := ProjectNextTaskSelection{
		ProjectID:       project.ID,
		ManagerReviewID: review.ID,
		TaskID:          task.ID,
		ExpectedStatus:  domain.TaskQueued,
		ProjectState:    domain.ProjectExecuting,
		Reason:          "Chosen",
	}
	start := make(chan struct{})
	results := make(chan bool, 4)
	failures := make(chan error, 4)
	var wait sync.WaitGroup
	for range 4 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, applied, err := s.SelectProjectNextTask(selection)
			if err != nil {
				failures <- err
				return
			}
			results <- applied
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	close(failures)
	for err := range failures {
		t.Fatalf("concurrent selection: %v", err)
	}
	applications := 0
	for applied := range results {
		if applied {
			applications++
		}
	}
	if applications != 1 {
		t.Fatalf("applied %d times, want 1", applications)
	}
	events, _ := s.ListWorkflowEvents(project.ID, 0, 100)
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
}

func createSelectionReview(
	t *testing.T,
	s *Store,
	projectID, nextTaskID int64,
	nextIssueNumber int,
	reason string,
) *domain.ManagerReview {
	t.Helper()
	review := domain.NewManagerReview(projectID)
	if nextTaskID > 0 {
		review.NextTaskID = &nextTaskID
	}
	review.NextTaskIssueNumber = nextIssueNumber
	review.NextTaskReason = reason
	review.ReleaseReadiness = "not-ready"
	review.OwnerUpdate = "Next task chosen."
	created, err := s.CreateManagerReview(review)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func assertTaskUnchanged(t *testing.T, s *Store, taskID int64, want domain.TaskStatus) {
	t.Helper()
	task, err := s.GetProjectTaskByID(taskID)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != want {
		t.Fatalf("task %d status = %q, want %q", taskID, task.Status, want)
	}
}
