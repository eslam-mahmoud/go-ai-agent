package project

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
)

func TestBacklogControllerAppliesManagerOrderAndAuditsOnce(t *testing.T) {
	t.Parallel()
	projectStore, projectID, tasks := backlogFixture(t)
	review := createBacklogReview(t, projectStore, projectID, []map[string]any{
		{"action": "reorder", "task_id": tasks[2].ID, "position": 1, "reason": "Release blocker"},
		{"action": "reprioritize", "task_id": tasks[0].ID, "position": 3, "reason": "Lower delay cost"},
	})
	controller, err := NewBacklogController(projectStore)
	if err != nil {
		t.Fatal(err)
	}
	result, err := controller.ApplyManagerReview(projectID, review.ID)
	if err != nil {
		t.Fatalf("ApplyManagerReview: %v", err)
	}
	if !result.Changed || len(result.Moves) != 2 {
		t.Fatalf("result = %#v", result)
	}
	assertBacklogIDs(t, result.Tasks, []int64{tasks[2].ID, tasks[1].ID, tasks[0].ID})
	for index, task := range result.Tasks {
		if task.Sequence != index+1 {
			t.Fatalf("task %d sequence = %d", task.ID, task.Sequence)
		}
	}
	events, err := projectStore.ListWorkflowEvents(projectID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != domain.WorkflowBacklogReordered ||
		events[0].Source != domain.WorkflowSourceController {
		t.Fatalf("events = %#v", events)
	}
	var evidence struct {
		ManagerReviewID int                        `json:"manager_review_id"`
		OldOrder        []int64                    `json:"old_order"`
		NewOrder        []int64                    `json:"new_order"`
		Moves           []store.ProjectBacklogMove `json:"moves"`
	}
	if err := json.Unmarshal(events[0].Data, &evidence); err != nil {
		t.Fatal(err)
	}
	if evidence.ManagerReviewID != int(review.ID) ||
		len(evidence.OldOrder) != 3 ||
		len(evidence.NewOrder) != 3 ||
		len(evidence.Moves) != 2 {
		t.Fatalf("evidence = %#v", evidence)
	}

	again, err := controller.ApplyManagerReview(projectID, review.ID)
	if err != nil {
		t.Fatalf("idempotent ApplyManagerReview: %v", err)
	}
	if again.Changed {
		t.Fatal("idempotent application reported a change")
	}
	events, _ = projectStore.ListWorkflowEvents(projectID, 0, 100)
	if len(events) != 1 {
		t.Fatalf("idempotent application emitted %d events", len(events))
	}
}

func TestBacklogControllerRejectsInvalidManagerChangesWithoutMutation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		raw    json.RawMessage
		mutate func([]*domain.Task, *store.Store)
		want   error
	}{
		{"unknown field", json.RawMessage(`[{"action":"reorder","task_id":1,"position":1,"reason":"x","extra":true}]`), nil, ErrInvalidBacklogChanges},
		{"unsupported", backlogChanges(map[string]any{"action": "cancel", "task_id": int64(1), "position": 1, "reason": "x"}), nil, ErrInvalidBacklogChanges},
		{"missing task", backlogChanges(map[string]any{"action": "reorder", "task_id": nil, "position": 1, "reason": "x"}), nil, ErrInvalidBacklogChanges},
		{"bad position", backlogChanges(map[string]any{"action": "reorder", "task_id": int64(1), "position": 0, "reason": "x"}), nil, ErrInvalidBacklogChanges},
		{"missing reason", backlogChanges(map[string]any{"action": "reorder", "task_id": int64(1), "position": 1, "reason": ""}), nil, ErrInvalidBacklogChanges},
		{"unknown task", backlogChanges(map[string]any{"action": "reorder", "task_id": int64(404), "position": 1, "reason": "x"}), nil, ErrInvalidBacklogChanges},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			projectStore, projectID, tasks := backlogFixture(t)
			raw := test.raw
			if len(raw) > 0 && test.name != "unknown field" && test.name != "unknown task" {
				var changes []map[string]any
				_ = json.Unmarshal(raw, &changes)
				if id, ok := changes[0]["task_id"].(float64); ok && id == 1 {
					changes[0]["task_id"] = tasks[0].ID
				}
				raw, _ = json.Marshal(changes)
			}
			review := createBacklogReviewRaw(t, projectStore, projectID, raw)
			controller, _ := NewBacklogController(projectStore)
			if _, err := controller.ApplyManagerReview(projectID, review.ID); !errors.Is(err, test.want) {
				t.Fatalf("ApplyManagerReview error = %v", err)
			}
			current, _ := projectStore.ListProjectTasks(projectID)
			assertBacklogIDs(t, current, []int64{tasks[0].ID, tasks[1].ID, tasks[2].ID})
			events, _ := projectStore.ListWorkflowEvents(projectID, 0, 100)
			if len(events) != 0 {
				t.Fatalf("invalid change emitted events: %#v", events)
			}
		})
	}
}

func TestDecodeBacklogChangesRejectsInvalidJSON(t *testing.T) {
	t.Parallel()
	for _, raw := range []json.RawMessage{
		json.RawMessage(`[`),
		json.RawMessage(`null`),
		json.RawMessage(`[] {}`),
	} {
		if _, err := decodeBacklogChanges(raw); !errors.Is(err, ErrInvalidBacklogChanges) {
			t.Fatalf("decode %q error = %v", raw, err)
		}
	}
}

func TestBacklogControllerRejectsTerminalActiveDuplicateAndStaleMoves(t *testing.T) {
	t.Parallel()
	for _, status := range []domain.TaskStatus{domain.TaskCompleted, domain.TaskCancelled, domain.TaskDeferred, domain.TaskReviewing} {
		status := status
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			projectStore, projectID, tasks := backlogFixture(t)
			tasks[0].Status = status
			if _, err := projectStore.UpdateProjectTask(tasks[0]); err != nil {
				t.Fatal(err)
			}
			review := createBacklogReview(t, projectStore, projectID, []map[string]any{
				{"action": "reorder", "task_id": tasks[0].ID, "position": 2, "reason": "move"},
			})
			controller, _ := NewBacklogController(projectStore)
			if _, err := controller.ApplyManagerReview(projectID, review.ID); !errors.Is(err, ErrInvalidBacklogChanges) {
				t.Fatalf("error = %v", err)
			}
		})
	}
	t.Run("duplicate", func(t *testing.T) {
		t.Parallel()
		projectStore, projectID, tasks := backlogFixture(t)
		review := createBacklogReview(t, projectStore, projectID, []map[string]any{
			{"action": "reorder", "task_id": tasks[2].ID, "position": 1, "reason": "first"},
			{"action": "reorder", "task_id": tasks[2].ID, "position": 2, "reason": "again"},
		})
		controller, _ := NewBacklogController(projectStore)
		if _, err := controller.ApplyManagerReview(projectID, review.ID); !errors.Is(err, ErrInvalidBacklogChanges) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("stale review", func(t *testing.T) {
		t.Parallel()
		projectStore, projectID, tasks := backlogFixture(t)
		stale := createBacklogReview(t, projectStore, projectID, []map[string]any{
			{"action": "reorder", "task_id": tasks[2].ID, "position": 1, "reason": "move"},
		})
		createBacklogReview(t, projectStore, projectID, nil)
		controller, _ := NewBacklogController(projectStore)
		if _, err := controller.ApplyManagerReview(projectID, stale.ID); !errors.Is(err, ErrStaleManagerReview) {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestBacklogControllerConcurrentApplicationIsIdempotent(t *testing.T) {
	projectStore, projectID, tasks := backlogFixture(t)
	review := createBacklogReview(t, projectStore, projectID, []map[string]any{
		{"action": "reorder", "task_id": tasks[2].ID, "position": 1, "reason": "priority"},
	})
	controller, _ := NewBacklogController(projectStore)
	var group sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := controller.ApplyManagerReview(projectID, review.ID)
			if err != nil && !errors.Is(err, store.ErrProjectBacklogConflict) {
				errs <- err
			}
		}()
	}
	group.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	current, _ := projectStore.ListProjectTasks(projectID)
	assertBacklogIDs(t, current, []int64{tasks[2].ID, tasks[0].ID, tasks[1].ID})
	events, _ := projectStore.ListWorkflowEvents(projectID, 0, 100)
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
}

func backlogFixture(t *testing.T) (*store.Store, int64, []*domain.Task) {
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
		task, err := projectStore.CreateProjectTask(domain.NewTask(projectRecord.ID, title, title+" goal"))
		if err != nil {
			t.Fatal(err)
		}
		tasks = append(tasks, task)
	}
	return projectStore, projectRecord.ID, tasks
}

func createBacklogReview(
	t *testing.T,
	projectStore *store.Store,
	projectID int64,
	changes []map[string]any,
) *domain.ManagerReview {
	t.Helper()
	if changes == nil {
		changes = []map[string]any{}
	}
	raw, _ := json.Marshal(changes)
	return createBacklogReviewRaw(t, projectStore, projectID, raw)
}

func createBacklogReviewRaw(
	t *testing.T,
	projectStore *store.Store,
	projectID int64,
	raw json.RawMessage,
) *domain.ManagerReview {
	t.Helper()
	review := domain.NewManagerReview(projectID)
	review.BacklogChanges = raw
	review.ReleaseReadiness = "not-ready"
	review.OwnerUpdate = "Backlog assessed."
	created, err := projectStore.CreateManagerReview(review)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func backlogChanges(change map[string]any) json.RawMessage {
	raw, _ := json.Marshal([]map[string]any{change})
	return raw
}

func assertBacklogIDs(t *testing.T, tasks []*domain.Task, want []int64) {
	t.Helper()
	if len(tasks) != len(want) {
		t.Fatalf("tasks = %d, want %d", len(tasks), len(want))
	}
	for index, task := range tasks {
		if task.ID != want[index] {
			t.Fatalf("task %d = %d, want %d", index, task.ID, want[index])
		}
	}
}
