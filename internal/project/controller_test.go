package project

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
	"github.com/eslam-mahmoud/go-ai-agent/internal/workflow"
)

func TestControllerTransitionAppliesValidatedAggregateState(t *testing.T) {
	s := openControllerStore(t)
	project, task := createControllerProjectTask(t, s, "owner/repo", domain.TaskQueued)
	controller, err := NewController(s)
	if err != nil {
		t.Fatal(err)
	}

	snapshot, err := controller.TransitionTask(
		project.ID,
		task.ID,
		domain.TaskSelected,
		workflow.TaskTransitionEvidence{ManagerReviewCompleted: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.CurrentTask == nil ||
		snapshot.CurrentTask.ID != task.ID ||
		snapshot.CurrentTask.Status != domain.TaskSelected ||
		snapshot.Project.State != domain.ProjectExecuting ||
		snapshot.Project.CurrentTaskID == nil ||
		*snapshot.Project.CurrentTaskID != task.ID {
		t.Fatalf("selected snapshot = %#v", snapshot)
	}

	snapshot, err = controller.TransitionTask(
		project.ID,
		task.ID,
		domain.TaskPlanning,
		workflow.TaskTransitionEvidence{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.CurrentTask.Status != domain.TaskPlanning {
		t.Fatalf("planning snapshot = %#v", snapshot)
	}
}

func TestControllerRejectsEvidenceWithoutMutation(t *testing.T) {
	s := openControllerStore(t)
	project, task := createControllerProjectTask(t, s, "owner/repo", domain.TaskPlanning)
	controller, _ := NewController(s)
	_, err := controller.TransitionTask(
		project.ID,
		task.ID,
		domain.TaskDeveloping,
		workflow.TaskTransitionEvidence{},
	)
	if !errors.Is(err, workflow.ErrTransitionPrecondition) {
		t.Fatalf("error = %v, want precondition error", err)
	}
	stored, err := s.GetProjectTaskByID(task.ID)
	if err != nil || stored.Status != domain.TaskPlanning {
		t.Fatalf("stored task = %#v, error=%v", stored, err)
	}
}

func TestControllerDetectsStaleTransition(t *testing.T) {
	s := openControllerStore(t)
	project, task := createControllerProjectTask(t, s, "owner/repo", domain.TaskQueued)
	stale := &staleControllerStore{Store: s}
	controller, _ := NewController(stale)
	_, err := controller.TransitionTask(
		project.ID,
		task.ID,
		domain.TaskSelected,
		workflow.TaskTransitionEvidence{ManagerReviewCompleted: true},
	)
	if !errors.Is(err, store.ErrProjectTaskTransitionConflict) {
		t.Fatalf("error = %v, want transition conflict", err)
	}
	stored, _ := s.GetProjectTaskByID(task.ID)
	if stored.Status != domain.TaskDeferred {
		t.Fatalf("stale write overwrote task: %#v", stored)
	}
}

func TestControllerRejectsCrossProjectAndMissingTasks(t *testing.T) {
	s := openControllerStore(t)
	first, _ := createControllerProjectTask(t, s, "owner/one", domain.TaskQueued)
	_, otherTask := createControllerProjectTask(t, s, "owner/two", domain.TaskQueued)
	controller, _ := NewController(s)

	_, err := controller.TransitionTask(
		first.ID,
		otherTask.ID,
		domain.TaskSelected,
		workflow.TaskTransitionEvidence{ManagerReviewCompleted: true},
	)
	if !errors.Is(err, ErrTaskOwnership) {
		t.Fatalf("cross-project error = %v", err)
	}
	_, err = controller.TransitionTask(
		first.ID,
		9999,
		domain.TaskSelected,
		workflow.TaskTransitionEvidence{ManagerReviewCompleted: true},
	)
	if !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("missing-task error = %v", err)
	}
}

func TestControllerManagerReviewRequired(t *testing.T) {
	s := openControllerStore(t)
	project, task := createControllerProjectTask(t, s, "owner/repo", domain.TaskVerifying)
	project.CurrentTaskID = &task.ID
	project.State = domain.ProjectExecuting
	if _, err := s.UpdateProject(project); err != nil {
		t.Fatal(err)
	}
	controller, _ := NewController(s)
	snapshot, err := controller.TransitionTask(
		project.ID,
		task.ID,
		domain.TaskCompleted,
		workflow.TaskTransitionEvidence{VerificationSucceeded: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.ManagerReviewRequired ||
		snapshot.CurrentTask == nil ||
		snapshot.CurrentTask.Status != domain.TaskCompleted {
		t.Fatalf("completed snapshot = %#v", snapshot)
	}

	completedID := task.ID
	review := domain.NewManagerReview(project.ID)
	review.CompletedTaskID = &completedID
	review.ProgressEstimate = 50
	review.CompletedTaskDecision = domain.TaskDecisionAccepted
	review.ReleaseReadiness = "not ready"
	review.OwnerUpdate = "Task accepted."
	review.ReviewedAt = snapshot.CurrentTask.UpdatedAt.Add(time.Second)
	if _, err := s.CreateManagerReview(review); err != nil {
		t.Fatal(err)
	}
	snapshot, err = controller.Snapshot(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ManagerReviewRequired {
		t.Fatalf("manager review still required: %#v", snapshot)
	}
}

func TestControllerReviewFixCycleCountUsesCompletedExecutionHistory(t *testing.T) {
	s := openControllerStore(t)
	project, task := createControllerProjectTask(
		t,
		s,
		"owner/review-fix-count",
		domain.TaskReviewing,
	)
	for attempt, status := range []domain.ExecutionStatus{
		domain.ExecutionCompleted,
		domain.ExecutionCompleted,
		domain.ExecutionFailed,
	} {
		execution := domain.NewExecution(
			project.ID,
			task.ID,
			string(workflow.ModeFixer),
			"codex",
			"",
			attempt+1,
		)
		execution.Status = status
		if _, err := s.CreateExecution(execution); err != nil {
			t.Fatal(err)
		}
	}
	reviewer := domain.NewExecution(
		project.ID,
		task.ID,
		string(workflow.ModeReviewer),
		"claude",
		"",
		1,
	)
	reviewer.Status = domain.ExecutionCompleted
	if _, err := s.CreateExecution(reviewer); err != nil {
		t.Fatal(err)
	}

	controller, err := NewController(s)
	if err != nil {
		t.Fatal(err)
	}
	count, err := controller.ReviewFixCycleCount(project.ID, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("completed fix cycles = %d, want 2", count)
	}
	otherProject, _ := createControllerProjectTask(
		t,
		s,
		"owner/review-fix-other",
		domain.TaskQueued,
	)
	if _, err := controller.ReviewFixCycleCount(otherProject.ID, task.ID); !errors.Is(err, ErrTaskOwnership) {
		t.Fatalf("cross-project count error = %v", err)
	}
}

func TestDeriveProjectState(t *testing.T) {
	taskID := int64(9)
	project := domain.NewProject("owner/repo", "Project", "Goal", "")
	project.ID = 1
	task := domain.NewTask(project.ID, "Task", "Goal")
	task.ID = taskID

	tests := []struct {
		target     domain.TaskStatus
		state      domain.ProjectState
		setCurrent bool
		current    bool
	}{
		{domain.TaskSelected, domain.ProjectExecuting, true, true},
		{domain.TaskPlanning, domain.ProjectExecuting, true, true},
		{domain.TaskWaitingInput, domain.ProjectExecuting, true, true},
		{domain.TaskDeveloping, domain.ProjectExecuting, true, true},
		{domain.TaskReviewing, domain.ProjectExecuting, true, true},
		{domain.TaskFixing, domain.ProjectExecuting, true, true},
		{domain.TaskVerifying, domain.ProjectExecuting, true, true},
		{domain.TaskWaitingCI, domain.ProjectExecuting, true, true},
		{domain.TaskBlocked, domain.ProjectBlocked, true, true},
		{domain.TaskCompleted, domain.ProjectExecuting, true, true},
	}
	for _, tc := range tests {
		state, setCurrent, current := deriveProjectState(project, task, tc.target)
		if state != tc.state ||
			setCurrent != tc.setCurrent ||
			(current != nil) != tc.current ||
			(current != nil && *current != taskID) {
			t.Errorf("%s derived state = %s set=%t current=%v", tc.target, state, setCurrent, current)
		}
	}

	project.State = domain.ProjectBlocked
	project.CurrentTaskID = &taskID
	for _, target := range []domain.TaskStatus{
		domain.TaskQueued,
		domain.TaskCancelled,
		domain.TaskDeferred,
	} {
		state, setCurrent, current := deriveProjectState(project, task, target)
		if state != domain.ProjectPlanning || !setCurrent || current != nil {
			t.Errorf("%s clear derived state = %s set=%t current=%v", target, state, setCurrent, current)
		}
	}
}

type staleControllerStore struct {
	*store.Store
}

func (s *staleControllerStore) ApplyProjectTaskTransition(
	update store.ProjectTaskTransitionUpdate,
) error {
	task, err := s.GetProjectTaskByID(update.TaskID)
	if err != nil {
		return err
	}
	task.Status = domain.TaskDeferred
	if _, err := s.UpdateProjectTask(task); err != nil {
		return err
	}
	return s.Store.ApplyProjectTaskTransition(update)
}

func openControllerStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "madar.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func createControllerProjectTask(
	t *testing.T,
	s *store.Store,
	repo string,
	status domain.TaskStatus,
) (*domain.Project, *domain.Task) {
	t.Helper()
	project, err := s.CreateProject(domain.NewProject(repo, repo, "Goal", ""))
	if err != nil {
		t.Fatal(err)
	}
	task := domain.NewTask(project.ID, "Task", "Goal")
	task.Status = status
	task, err = s.CreateProjectTask(task)
	if err != nil {
		t.Fatal(err)
	}
	return project, task
}
