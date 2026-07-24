package project

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
	"github.com/eslam-mahmoud/go-ai-agent/internal/workflow"
)

func TestSelectionControllerSelectsNextTaskWithReasonAndAuditsOnce(t *testing.T) {
	t.Parallel()
	projectStore, projectID, tasks := selectionFixture(t)
	review := createSelectionReview(t, projectStore, projectID, func(review *domain.ManagerReview) {
		review.NextTaskID = &tasks[1].ID
		review.NextTaskReason = "Next dependency for the MVP"
	})
	controller, err := NewSelectionController(projectStore)
	if err != nil {
		t.Fatal(err)
	}
	result, err := controller.SelectNextTask(projectID, review.ID)
	if err != nil {
		t.Fatalf("SelectNextTask: %v", err)
	}
	if !result.Applied ||
		result.Task.ID != tasks[1].ID ||
		result.Task.Status != domain.TaskSelected ||
		result.Task.SelectedReason != "Next dependency for the MVP" ||
		result.Reason != "Next dependency for the MVP" {
		t.Fatalf("result = %#v", result)
	}
	projectRecord, err := projectStore.GetProjectByID(projectID)
	if err != nil {
		t.Fatal(err)
	}
	if projectRecord.State != domain.ProjectExecuting ||
		projectRecord.CurrentTaskID == nil ||
		*projectRecord.CurrentTaskID != tasks[1].ID {
		t.Fatalf("project = %#v", projectRecord)
	}
	events, err := projectStore.ListWorkflowEvents(projectID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != domain.WorkflowTaskSelected {
		t.Fatalf("events = %#v", events)
	}
	var evidence struct {
		ManagerReviewID int64             `json:"manager_review_id"`
		FromStatus      domain.TaskStatus `json:"from_status"`
		ToStatus        domain.TaskStatus `json:"to_status"`
		Reason          string            `json:"reason"`
	}
	if err := json.Unmarshal(events[0].Data, &evidence); err != nil {
		t.Fatal(err)
	}
	if evidence.ManagerReviewID != review.ID ||
		evidence.FromStatus != domain.TaskQueued ||
		evidence.ToStatus != domain.TaskSelected ||
		evidence.Reason != "Next dependency for the MVP" {
		t.Fatalf("evidence = %#v", evidence)
	}

	again, err := controller.SelectNextTask(projectID, review.ID)
	if err != nil {
		t.Fatalf("idempotent SelectNextTask: %v", err)
	}
	if again.Applied {
		t.Fatal("replayed selection reported a change")
	}
	events, _ = projectStore.ListWorkflowEvents(projectID, 0, 100)
	if len(events) != 1 {
		t.Fatalf("replay emitted %d events", len(events))
	}
}

func TestSelectionControllerResolvesNextTaskByIssueNumber(t *testing.T) {
	t.Parallel()
	projectStore, projectID, tasks := selectionFixture(t)
	tasks[2].IssueNumber = 43
	if _, err := projectStore.UpdateProjectTask(tasks[2]); err != nil {
		t.Fatal(err)
	}
	review := createSelectionReview(t, projectStore, projectID, func(review *domain.ManagerReview) {
		review.NextTaskIssueNumber = 43
		review.NextTaskReason = "Release blocker"
	})
	controller, _ := NewSelectionController(projectStore)
	result, err := controller.SelectNextTask(projectID, review.ID)
	if err != nil {
		t.Fatalf("SelectNextTask: %v", err)
	}
	if !result.Applied || result.Task.ID != tasks[2].ID {
		t.Fatalf("result = %#v", result)
	}
}

func TestSelectionControllerRejectsIneligibleSelectionsWithoutMutation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		arrange func(*testing.T, *store.Store, int64, []*domain.Task, *domain.ManagerReview)
		want    error
	}{
		{
			name: "no next task",
			arrange: func(_ *testing.T, _ *store.Store, _ int64, _ []*domain.Task, review *domain.ManagerReview) {
				review.NextTaskID = nil
				review.NextTaskReason = ""
			},
			want: ErrNoNextTaskSelected,
		},
		{
			name: "human approval required",
			arrange: func(_ *testing.T, _ *store.Store, _ int64, tasks []*domain.Task, review *domain.ManagerReview) {
				review.NextTaskID = &tasks[1].ID
				review.HumanApprovalRequired = true
			},
			want: ErrNextTaskNotEligible,
		},
		{
			name: "unresolved dependencies",
			arrange: func(t *testing.T, projectStore *store.Store, _ int64, tasks []*domain.Task, review *domain.ManagerReview) {
				tasks[1].DependencyState = "blocked-by:12"
				if _, err := projectStore.UpdateProjectTask(tasks[1]); err != nil {
					t.Fatal(err)
				}
				review.NextTaskID = &tasks[1].ID
			},
			want: ErrNextTaskNotEligible,
		},
		{
			name: "architecture review pending",
			arrange: func(_ *testing.T, _ *store.Store, _ int64, tasks []*domain.Task, review *domain.ManagerReview) {
				review.NextTaskID = &tasks[1].ID
				review.ArchitectureReviewRequired = true
			},
			want: workflow.ErrTransitionPrecondition,
		},
		{
			name: "unknown task",
			arrange: func(_ *testing.T, _ *store.Store, _ int64, tasks []*domain.Task, review *domain.ManagerReview) {
				review.NextTaskIssueNumber = 4041
			},
			want: ErrTaskNotFound,
		},
		{
			name: "paused project",
			arrange: func(t *testing.T, projectStore *store.Store, projectID int64, tasks []*domain.Task, review *domain.ManagerReview) {
				review.NextTaskID = &tasks[1].ID
			},
			want: ErrInvalidControl,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			projectStore, projectID, tasks := selectionFixture(t)
			review := createSelectionReview(t, projectStore, projectID, func(review *domain.ManagerReview) {
				review.NextTaskReason = "Next dependency"
				test.arrange(t, projectStore, projectID, tasks, review)
			})
			if test.name == "paused project" {
				projectRecord, err := projectStore.GetProjectByID(projectID)
				if err != nil {
					t.Fatal(err)
				}
				if err := projectStore.PauseProject(projectID, projectRecord.State); err != nil {
					t.Fatal(err)
				}
			}
			controller, _ := NewSelectionController(projectStore)
			if _, err := controller.SelectNextTask(projectID, review.ID); !errors.Is(err, test.want) {
				t.Fatalf("SelectNextTask error = %v, want %v", err, test.want)
			}
			assertNoTaskSelected(t, projectStore, projectID)
		})
	}
}

func TestSelectionControllerRejectsIneligibleSourceStatuses(t *testing.T) {
	t.Parallel()
	for _, status := range []domain.TaskStatus{
		domain.TaskProposed,
		domain.TaskDeferred,
		domain.TaskBlocked,
		domain.TaskCompleted,
		domain.TaskCancelled,
	} {
		status := status
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			projectStore, projectID, tasks := selectionFixture(t)
			tasks[1].Status = status
			if _, err := projectStore.UpdateProjectTask(tasks[1]); err != nil {
				t.Fatal(err)
			}
			review := createSelectionReview(t, projectStore, projectID, func(review *domain.ManagerReview) {
				review.NextTaskID = &tasks[1].ID
				review.NextTaskReason = "Next dependency"
			})
			controller, _ := NewSelectionController(projectStore)
			_, err := controller.SelectNextTask(projectID, review.ID)
			if !errors.Is(err, workflow.ErrInvalidTaskTransition) &&
				!errors.Is(err, workflow.ErrTransitionPrecondition) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestSelectionControllerPreservesSingleActiveTask(t *testing.T) {
	t.Parallel()
	projectStore, projectID, tasks := selectionFixture(t)
	tasks[0].Status = domain.TaskDeveloping
	if _, err := projectStore.UpdateProjectTask(tasks[0]); err != nil {
		t.Fatal(err)
	}
	review := createSelectionReview(t, projectStore, projectID, func(review *domain.ManagerReview) {
		review.NextTaskID = &tasks[1].ID
		review.NextTaskReason = "Next dependency"
	})
	controller, _ := NewSelectionController(projectStore)
	if _, err := controller.SelectNextTask(projectID, review.ID); !errors.Is(
		err,
		store.ErrActiveProjectTaskExists,
	) {
		t.Fatalf("SelectNextTask error = %v", err)
	}
	current, _ := projectStore.GetProjectTaskByID(tasks[1].ID)
	if current.Status != domain.TaskQueued {
		t.Fatalf("task status = %q", current.Status)
	}
	events, _ := projectStore.ListWorkflowEvents(projectID, 0, 100)
	if len(events) != 0 {
		t.Fatalf("rejected selection emitted events: %#v", events)
	}
}

func TestSelectionControllerRejectsStaleManagerReview(t *testing.T) {
	t.Parallel()
	projectStore, projectID, tasks := selectionFixture(t)
	stale := createSelectionReview(t, projectStore, projectID, func(review *domain.ManagerReview) {
		review.NextTaskID = &tasks[1].ID
		review.NextTaskReason = "First choice"
	})
	createSelectionReview(t, projectStore, projectID, func(review *domain.ManagerReview) {
		review.NextTaskID = &tasks[2].ID
		review.NextTaskReason = "Second choice"
	})
	controller, _ := NewSelectionController(projectStore)
	if _, err := controller.SelectNextTask(projectID, stale.ID); !errors.Is(err, ErrStaleManagerReview) {
		t.Fatalf("SelectNextTask error = %v", err)
	}
	assertNoTaskSelected(t, projectStore, projectID)
}

func TestSelectionControllerConcurrentSelectionAppliesOnce(t *testing.T) {
	projectStore, projectID, tasks := selectionFixture(t)
	review := createSelectionReview(t, projectStore, projectID, func(review *domain.ManagerReview) {
		review.NextTaskID = &tasks[1].ID
		review.NextTaskReason = "Next dependency"
	})
	controller, _ := NewSelectionController(projectStore)
	var group sync.WaitGroup
	applications := make(chan bool, 4)
	failures := make(chan error, 4)
	for range 4 {
		group.Add(1)
		go func() {
			defer group.Done()
			result, err := controller.SelectNextTask(projectID, review.ID)
			if err != nil {
				if !errors.Is(err, store.ErrProjectTaskSelectionConflict) &&
					!errors.Is(err, store.ErrActiveProjectTaskExists) {
					failures <- err
				}
				return
			}
			applications <- result.Applied
		}()
	}
	group.Wait()
	close(applications)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	applied := 0
	for value := range applications {
		if value {
			applied++
		}
	}
	if applied != 1 {
		t.Fatalf("applied %d times, want 1", applied)
	}
	events, _ := projectStore.ListWorkflowEvents(projectID, 0, 100)
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
}

// A persisted review can never carry a blank reason, so this guard is proven
// against a store that hands back a corrupted aggregate.
func TestSelectionControllerRejectsCorruptedReviewWithoutReason(t *testing.T) {
	t.Parallel()
	task := &domain.Task{ID: 7, ProjectID: 3, Status: domain.TaskQueued}
	nextTaskID := task.ID
	fake := &fakeSelectionStore{
		aggregate: &store.ProjectAggregate{
			Project: &domain.Project{ID: 3, State: domain.ProjectPlanning},
			Tasks:   []*domain.Task{task},
			LatestManagerReview: &domain.ManagerReview{
				ID:             11,
				ProjectID:      3,
				NextTaskID:     &nextTaskID,
				NextTaskReason: "  ",
			},
		},
	}
	controller, _ := NewSelectionController(fake)
	if _, err := controller.SelectNextTask(3, 11); !errors.Is(err, ErrNextTaskNotEligible) {
		t.Fatalf("SelectNextTask error = %v", err)
	}
	if fake.calls != 0 {
		t.Fatalf("store was called %d times", fake.calls)
	}
}

type fakeSelectionStore struct {
	aggregate *store.ProjectAggregate
	calls     int
}

func (fake *fakeSelectionStore) LoadProjectAggregate(int64) (*store.ProjectAggregate, error) {
	return fake.aggregate, nil
}

func (fake *fakeSelectionStore) ListArchitectureRiskDiscoveries(
	int64,
) ([]*domain.Discovery, error) {
	return nil, nil
}

func (fake *fakeSelectionStore) SelectProjectNextTask(
	store.ProjectNextTaskSelection,
) (*domain.Task, bool, error) {
	fake.calls++
	return nil, false, errors.New("unexpected selection write")
}

func selectionFixture(t *testing.T) (*store.Store, int64, []*domain.Task) {
	t.Helper()
	projectStore, err := store.Open(filepath.Join(t.TempDir(), "madar.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { projectStore.Close() })
	projectRecord, err := projectStore.CreateProject(
		domain.NewProject("owner/repo", "Madar", "Ship v2", "Scope"),
	)
	if err != nil {
		t.Fatal(err)
	}
	var tasks []*domain.Task
	for _, title := range []string{"First", "Second", "Third"} {
		task := domain.NewTask(projectRecord.ID, title, title+" goal")
		task.Status = domain.TaskQueued
		created, err := projectStore.CreateProjectTask(task)
		if err != nil {
			t.Fatal(err)
		}
		tasks = append(tasks, created)
	}
	return projectStore, projectRecord.ID, tasks
}

func createSelectionReview(
	t *testing.T,
	projectStore *store.Store,
	projectID int64,
	arrange func(*domain.ManagerReview),
) *domain.ManagerReview {
	t.Helper()
	review := domain.NewManagerReview(projectID)
	review.ReleaseReadiness = "not-ready"
	review.OwnerUpdate = "Next task chosen."
	arrange(review)
	created, err := projectStore.CreateManagerReview(review)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func assertNoTaskSelected(t *testing.T, projectStore *store.Store, projectID int64) {
	t.Helper()
	tasks, err := projectStore.ListProjectTasks(projectID)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if task.Status == domain.TaskSelected {
			t.Fatalf("task %d was selected", task.ID)
		}
	}
	events, err := projectStore.ListWorkflowEvents(projectID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == domain.WorkflowTaskSelected {
			t.Fatalf("rejected selection emitted %#v", event)
		}
	}
}
