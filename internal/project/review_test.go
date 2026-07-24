package project

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
)

func TestReviewCoordinatorRunsFullManagerCycle(t *testing.T) {
	t.Parallel()
	fixture := newReviewFixture(t)
	completed := fixture.completeFirstTask(t)
	runner := &fakeManagerRunner{output: managerOutputJSON(map[string]any{
		// Position 2 is the first pending slot: the completed task holds
		// position 1 and terminal tasks may not be displaced.
		"backlog_changes": []map[string]any{
			{"action": "reorder", "task_id": fixture.tasks[2].ID, "position": 2, "reason": "Release blocker"},
		},
		"next_task": map[string]any{
			"issue_number": fixture.tasks[2].IssueNumber,
			"reason":       "Next dependency for the MVP",
		},
	})}
	coordinator := fixture.coordinator(t, runner)

	result, err := coordinator.ReviewAfterTask(context.Background(), fixture.projectID, completed.ID)
	if err != nil {
		t.Fatalf("ReviewAfterTask: %v", err)
	}
	if !result.Required || result.AlreadyDone || result.Review == nil {
		t.Fatalf("result = %#v", result)
	}
	if runner.calls != 1 || runner.lastCompletedTaskID != completed.ID {
		t.Fatalf("runner calls=%d completed=%d", runner.calls, runner.lastCompletedTaskID)
	}
	if result.Review.CompletedTaskID == nil || *result.Review.CompletedTaskID != completed.ID {
		t.Fatalf("review = %#v", result.Review)
	}
	if result.Backlog == nil || !result.Backlog.Changed {
		t.Fatalf("backlog = %#v", result.Backlog)
	}
	if result.Selection == nil ||
		result.Selection.Task.ID != fixture.tasks[2].ID ||
		result.Selection.Task.Status != domain.TaskSelected ||
		result.Selection.Task.SelectedReason != "Next dependency for the MVP" {
		t.Fatalf("selection = %#v", result.Selection)
	}
	if result.Publication == nil || result.Publication.ParentIssueNumber <= 0 {
		t.Fatalf("publication = %#v", result.Publication)
	}

	// Re-entering after a restart must not repeat the decision.
	again, err := coordinator.ReviewAfterTask(context.Background(), fixture.projectID, completed.ID)
	if err != nil {
		t.Fatalf("re-entrant ReviewAfterTask: %v", err)
	}
	if !again.AlreadyDone || runner.calls != 1 {
		t.Fatalf("re-entrant result = %#v, runner calls = %d", again, runner.calls)
	}
}

func TestReviewCoordinatorSkipsStatusesThatNeedNoReview(t *testing.T) {
	t.Parallel()
	for _, status := range []domain.TaskStatus{
		domain.TaskQueued,
		domain.TaskSelected,
		domain.TaskDeveloping,
		domain.TaskWaitingInput,
		domain.TaskWaitingCI,
		domain.TaskDeferred,
	} {
		status := status
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			fixture := newReviewFixture(t)
			task := fixture.tasks[0]
			task.Status = status
			if _, err := fixture.store.UpdateProjectTask(task); err != nil {
				t.Fatal(err)
			}
			runner := &fakeManagerRunner{output: managerOutputJSON(nil)}
			coordinator := fixture.coordinator(t, runner)
			result, err := coordinator.ReviewAfterTask(
				context.Background(), fixture.projectID, task.ID,
			)
			if err != nil {
				t.Fatalf("ReviewAfterTask: %v", err)
			}
			if result.Required || runner.calls != 0 {
				t.Fatalf("result = %#v, runner calls = %d", result, runner.calls)
			}
		})
	}
}

func TestReviewCoordinatorReviewsBlockedAndCancelledWithoutCompletedTask(t *testing.T) {
	t.Parallel()
	for _, status := range []domain.TaskStatus{domain.TaskBlocked, domain.TaskCancelled} {
		status := status
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			fixture := newReviewFixture(t)
			task := fixture.tasks[0]
			task.Status = status
			if _, err := fixture.store.UpdateProjectTask(task); err != nil {
				t.Fatal(err)
			}
			runner := &fakeManagerRunner{output: managerOutputJSON(map[string]any{
				"completed_task_decision": "not-applicable",
			})}
			coordinator := fixture.coordinator(t, runner)
			result, err := coordinator.ReviewAfterTask(
				context.Background(), fixture.projectID, task.ID,
			)
			if err != nil {
				t.Fatalf("ReviewAfterTask: %v", err)
			}
			if !result.Required || result.Review == nil || runner.calls != 1 {
				t.Fatalf("result = %#v", result)
			}
			if result.Review.CompletedTaskID != nil {
				t.Fatal("blocked/cancelled task was recorded as completed")
			}
			if runner.lastCompletedTaskID != 0 {
				t.Fatalf("manager received completed task %d", runner.lastCompletedTaskID)
			}
			if !result.NoNextTask || result.Selection != nil {
				t.Fatalf("selection = %#v, noNext = %v", result.Selection, result.NoNextTask)
			}
		})
	}
}

func TestReviewCoordinatorRejectsUnusableManagerOutput(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		output json.RawMessage
		want   error
	}{
		{"malformed", json.RawMessage(`{`), ErrInvalidManagerOutput},
		{"trailing json", json.RawMessage(`{"status":"completed"} {}`), ErrInvalidManagerOutput},
		{"missing status", json.RawMessage(`{"summary":"x"}`), ErrInvalidManagerOutput},
		{
			"needs input",
			managerOutputJSON(map[string]any{
				"status":   "needs_input",
				"question": "Which region should ship first?",
			}),
			ErrManagerNeedsInput,
		},
		{
			"needs input without question",
			managerOutputJSON(map[string]any{"status": "needs_input"}),
			ErrInvalidManagerOutput,
		},
		{
			"blocked",
			managerOutputJSON(map[string]any{"status": "blocked", "summary": "Credentials missing"}),
			ErrManagerNotCompleted,
		},
		{
			"failed",
			managerOutputJSON(map[string]any{"status": "failed"}),
			ErrManagerNotCompleted,
		},
		{
			"unknown next task",
			managerOutputJSON(map[string]any{
				"next_task": map[string]any{"issue_number": 9090, "reason": "why"},
			}),
			ErrInvalidManagerOutput,
		},
		{
			"invalid review record",
			managerOutputJSON(map[string]any{"owner_update": "   "}),
			ErrInvalidManagerOutput,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newReviewFixture(t)
			completed := fixture.completeFirstTask(t)
			runner := &fakeManagerRunner{output: test.output}
			coordinator := fixture.coordinator(t, runner)
			_, err := coordinator.ReviewAfterTask(
				context.Background(), fixture.projectID, completed.ID,
			)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			reviews, listErr := fixture.store.ListManagerReviews(fixture.projectID)
			if listErr != nil {
				t.Fatal(listErr)
			}
			if len(reviews) != 0 {
				t.Fatalf("unusable output persisted %d reviews", len(reviews))
			}
		})
	}
}

func TestReviewCoordinatorKeepsReviewWhenApplyFails(t *testing.T) {
	t.Parallel()
	fixture := newReviewFixture(t)
	completed := fixture.completeFirstTask(t)
	// Occupy the delivery lane so next-task selection cannot succeed.
	blocker := fixture.tasks[1]
	blocker.Status = domain.TaskDeveloping
	if _, err := fixture.store.UpdateProjectTask(blocker); err != nil {
		t.Fatal(err)
	}
	runner := &fakeManagerRunner{output: managerOutputJSON(map[string]any{
		"next_task": map[string]any{
			"issue_number": fixture.tasks[2].IssueNumber,
			"reason":       "Next dependency",
		},
	})}
	coordinator := fixture.coordinator(t, runner)

	if _, err := coordinator.ReviewAfterTask(
		context.Background(), fixture.projectID, completed.ID,
	); !errors.Is(err, store.ErrActiveProjectTaskExists) {
		t.Fatalf("error = %v", err)
	}
	reviews, err := fixture.store.ListManagerReviews(fixture.projectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reviews) != 1 {
		t.Fatalf("failed apply left %d reviews", len(reviews))
	}
}

func TestReviewCoordinatorRunsWithoutPublisher(t *testing.T) {
	t.Parallel()
	fixture := newReviewFixture(t)
	completed := fixture.completeFirstTask(t)
	runner := &fakeManagerRunner{output: managerOutputJSON(nil)}
	backlog, err := NewBacklogController(fixture.store)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := NewSelectionController(fixture.store)
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := NewReviewCoordinator(fixture.store, runner, backlog, selection, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := coordinator.ReviewAfterTask(
		context.Background(), fixture.projectID, completed.ID,
	)
	if err != nil {
		t.Fatalf("ReviewAfterTask: %v", err)
	}
	if result.Review == nil || result.Publication != nil || !result.NoNextTask {
		t.Fatalf("result = %#v", result)
	}
}

func TestNewReviewCoordinatorRequiresCollaborators(t *testing.T) {
	t.Parallel()
	fixture := newReviewFixture(t)
	runner := &fakeManagerRunner{}
	backlog, _ := NewBacklogController(fixture.store)
	selection, _ := NewSelectionController(fixture.store)
	cases := []struct {
		name      string
		store     ReviewStore
		runner    ManagerRunner
		backlog   *BacklogController
		selection *SelectionController
	}{
		{"store", nil, runner, backlog, selection},
		{"runner", fixture.store, nil, backlog, selection},
		{"backlog", fixture.store, runner, nil, selection},
		{"selection", fixture.store, runner, backlog, nil},
	}
	for _, test := range cases {
		if _, err := NewReviewCoordinator(
			test.store, test.runner, test.backlog, test.selection, nil,
		); err == nil {
			t.Errorf("missing %s accepted", test.name)
		}
	}
}

func TestReviewCoordinatorRejectsUnknownTaskAndBadInput(t *testing.T) {
	t.Parallel()
	fixture := newReviewFixture(t)
	coordinator := fixture.coordinator(t, &fakeManagerRunner{})
	if _, err := coordinator.ReviewAfterTask(
		context.Background(), fixture.projectID, 4040,
	); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("unknown task error = %v", err)
	}
	for _, ids := range [][2]int64{{0, 1}, {1, 0}} {
		if _, err := coordinator.ReviewAfterTask(
			context.Background(), ids[0], ids[1],
		); !errors.Is(err, ErrInvalidManagerOutput) {
			t.Fatalf("ids %v error = %v", ids, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := coordinator.ReviewAfterTask(
		ctx, fixture.projectID, fixture.tasks[0].ID,
	); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled error = %v", err)
	}
}

type fakeManagerRunner struct {
	output              json.RawMessage
	err                 error
	calls               int
	lastCompletedTaskID int64
}

func (fake *fakeManagerRunner) RunManagerReview(
	_ context.Context,
	_, completedTaskID int64,
) (json.RawMessage, error) {
	fake.calls++
	fake.lastCompletedTaskID = completedTaskID
	if fake.err != nil {
		return nil, fake.err
	}
	return fake.output, nil
}

type reviewFixture struct {
	store         *store.Store
	projectID     int64
	tasks         []*domain.Task
	workspaceRoot string
	client        *fakeIssueClient
}

func newReviewFixture(t *testing.T) *reviewFixture {
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
	for index, title := range []string{"First", "Second", "Third"} {
		task := domain.NewTask(projectRecord.ID, title, title+" goal")
		task.Status = domain.TaskQueued
		task.IssueNumber = 200 + index
		created, err := projectStore.CreateProjectTask(task)
		if err != nil {
			t.Fatal(err)
		}
		tasks = append(tasks, created)
	}
	workspaceRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspaceRoot, "owner", "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &reviewFixture{
		store:         projectStore,
		projectID:     projectRecord.ID,
		tasks:         tasks,
		workspaceRoot: workspaceRoot,
		client:        &fakeIssueClient{nextNumber: 300},
	}
}

// completeFirstTask drives the first task to completed so a manager review is
// required, mirroring the end of one delivery attempt.
func (fixture *reviewFixture) completeFirstTask(t *testing.T) *domain.Task {
	t.Helper()
	task := fixture.tasks[0]
	task.Status = domain.TaskCompleted
	completed, err := fixture.store.UpdateProjectTask(task)
	if err != nil {
		t.Fatal(err)
	}
	fixture.tasks[0] = completed
	return completed
}

func (fixture *reviewFixture) coordinator(
	t *testing.T,
	runner ManagerRunner,
) *ReviewCoordinator {
	t.Helper()
	backlog, err := NewBacklogController(fixture.store)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := NewSelectionController(fixture.store)
	if err != nil {
		t.Fatal(err)
	}
	publisher, err := NewPublisher(
		fixture.store,
		fixture.client,
		PublisherOptions{WorkspaceRoot: fixture.workspaceRoot},
	)
	if err != nil {
		t.Fatal(err)
	}
	coordinator, err := NewReviewCoordinator(
		fixture.store, runner, backlog, selection, publisher,
	)
	if err != nil {
		t.Fatal(err)
	}
	return coordinator
}

// managerOutputJSON builds schema-shaped manager output, letting each test
// override only the fields it cares about.
func managerOutputJSON(overrides map[string]any) json.RawMessage {
	output := map[string]any{
		"status":                       "completed",
		"summary":                      "Reviewed the delivered task.",
		"question":                     nil,
		"discoveries":                  []any{},
		"risks":                        []any{},
		"recommended_next_action":      "Continue delivery.",
		"project_health":               "on-track",
		"progress_estimate":            35,
		"completed_task_decision":      "accepted",
		"architecture_review_required": false,
		"human_approval_required":      false,
		"discovery_decisions":          []any{},
		"backlog_changes":              []any{},
		"next_task":                    nil,
		"release_readiness":            "not-ready",
		"owner_update":                 "Project remains on track.",
	}
	for key, value := range overrides {
		output[key] = value
	}
	raw, err := json.Marshal(output)
	if err != nil {
		panic(fmt.Sprintf("encode manager output: %v", err))
	}
	return raw
}
