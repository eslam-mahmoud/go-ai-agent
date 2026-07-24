package project

import (
	"errors"
	"sync"
	"testing"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
)

func TestDiscoveryBacklogInsertsNextWorkAheadOfThePreviousNextTask(t *testing.T) {
	t.Parallel()
	fixture := newReviewFixture(t)
	discovery := fixture.publishedDiscovery(t,
		"Retry budget is unbounded", domain.DecisionCreateNextTask, domain.SeverityCritical, 900)
	controller := fixture.backlogController(t)

	result, err := controller.InsertAcceptedDiscoveries(fixture.projectID)
	if err != nil {
		t.Fatalf("InsertAcceptedDiscoveries: %v", err)
	}
	if len(result.Inserted) != 1 || len(result.Skipped) != 0 {
		t.Fatalf("result = %d inserted, %d skipped",
			len(result.Inserted), len(result.Skipped))
	}
	inserted := result.Inserted[0]
	if inserted.Sequence != 1 {
		t.Fatalf("inserted at position %d, want 1", inserted.Sequence)
	}
	if inserted.IssueNumber != 900 ||
		inserted.Source != "discovery" ||
		inserted.TaskType != string(domain.DiscoveryBug) ||
		inserted.Status != domain.TaskQueued ||
		inserted.Priority != 1 ||
		inserted.BlocksRelease ||
		inserted.SourceDiscoveryID == nil ||
		*inserted.SourceDiscoveryID != discovery.ID {
		t.Fatalf("inserted task = %#v", inserted)
	}

	// The whole backlog stays contiguous and the previous work moves down.
	tasks, _ := fixture.store.ListProjectTasks(fixture.projectID)
	if len(tasks) != 4 {
		t.Fatalf("backlog has %d tasks", len(tasks))
	}
	for index, task := range tasks {
		if task.Sequence != index+1 {
			t.Fatalf("task %d sequence = %d", task.ID, task.Sequence)
		}
	}
	if tasks[0].ID != inserted.ID || tasks[1].ID != fixture.tasks[0].ID {
		t.Fatalf("order = %d, %d", tasks[0].ID, tasks[1].ID)
	}

	// Re-running must not queue the same discovery again.
	again, err := controller.InsertAcceptedDiscoveries(fixture.projectID)
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if len(again.Inserted) != 0 || len(again.Skipped) != 1 {
		t.Fatalf("re-run = %d inserted, %d skipped",
			len(again.Inserted), len(again.Skipped))
	}
}

func TestDiscoveryBacklogAppendsOrdinaryWorkAndFlagsReleaseBlockers(t *testing.T) {
	t.Parallel()
	fixture := newReviewFixture(t)
	fixture.publishedDiscovery(t,
		"Add integration coverage", domain.DecisionAddToBacklog, domain.SeverityLow, 910)
	fixture.publishedDiscovery(t,
		"Token is logged", domain.DecisionCreateReleaseBlocker, domain.SeverityHigh, 911)
	controller := fixture.backlogController(t)

	result, err := controller.InsertAcceptedDiscoveries(fixture.projectID)
	if err != nil {
		t.Fatalf("InsertAcceptedDiscoveries: %v", err)
	}
	if len(result.Inserted) != 2 {
		t.Fatalf("inserted %d tasks", len(result.Inserted))
	}
	appended := result.Inserted[0]
	blocker := result.Inserted[1]
	if appended.Sequence != 4 || appended.BlocksRelease || appended.Priority != 4 {
		t.Fatalf("appended = %#v", appended)
	}
	if blocker.Sequence != 1 || !blocker.BlocksRelease || blocker.Priority != 2 {
		t.Fatalf("blocker = %#v", blocker)
	}
	tasks, _ := fixture.store.ListProjectTasks(fixture.projectID)
	for index, task := range tasks {
		if task.Sequence != index+1 {
			t.Fatalf("task %d sequence = %d", task.ID, task.Sequence)
		}
	}
}

func TestDiscoveryBacklogNeverDisplacesTerminalOrActiveTasks(t *testing.T) {
	t.Parallel()
	fixture := newReviewFixture(t)
	// Position 1 completed, position 2 developing: the first safe slot is 3.
	fixture.tasks[0].Status = domain.TaskCompleted
	if _, err := fixture.store.UpdateProjectTask(fixture.tasks[0]); err != nil {
		t.Fatal(err)
	}
	fixture.tasks[1].Status = domain.TaskDeveloping
	if _, err := fixture.store.UpdateProjectTask(fixture.tasks[1]); err != nil {
		t.Fatal(err)
	}
	fixture.publishedDiscovery(t,
		"Urgent finding", domain.DecisionCreateNextTask, domain.SeverityCritical, 920)
	controller := fixture.backlogController(t)

	result, err := controller.InsertAcceptedDiscoveries(fixture.projectID)
	if err != nil {
		t.Fatalf("InsertAcceptedDiscoveries: %v", err)
	}
	if result.Inserted[0].Sequence != 3 {
		t.Fatalf("inserted at position %d, want 3", result.Inserted[0].Sequence)
	}
	tasks, _ := fixture.store.ListProjectTasks(fixture.projectID)
	if tasks[0].ID != fixture.tasks[0].ID || tasks[1].ID != fixture.tasks[1].ID {
		t.Fatal("a terminal or active task was displaced")
	}
	for index, task := range tasks {
		if task.Sequence != index+1 {
			t.Fatalf("task %d sequence = %d", task.ID, task.Sequence)
		}
	}
}

func TestDiscoveryBacklogSkipsWorkThatShouldNotBeQueued(t *testing.T) {
	t.Parallel()
	t.Run("verdict creates no work", func(t *testing.T) {
		t.Parallel()
		for _, decision := range []domain.DiscoveryDecision{
			domain.DecisionFixInCurrentTask,
			domain.DecisionDefer,
			domain.DecisionRejectOutOfScope,
			domain.DecisionRequestArchitecture,
			domain.DecisionRequestHuman,
		} {
			fixture := newReviewFixture(t)
			fixture.publishedDiscovery(t, "Finding", decision, domain.SeverityHigh, 930)
			controller := fixture.backlogController(t)
			result, err := controller.InsertAcceptedDiscoveries(fixture.projectID)
			if err != nil {
				t.Fatalf("decision %q: %v", decision, err)
			}
			if len(result.Inserted) != 0 {
				t.Fatalf("decision %q queued work", decision)
			}
		}
	})
	t.Run("no issue yet", func(t *testing.T) {
		t.Parallel()
		fixture := newReviewFixture(t)
		// Decided but never published: item 42 has not run yet.
		fixture.decidedDiscovery(t,
			"Unpublished", domain.DecisionCreateNextTask, domain.SeverityHigh)
		controller := fixture.backlogController(t)
		result, err := controller.InsertAcceptedDiscoveries(fixture.projectID)
		if err != nil {
			t.Fatalf("InsertAcceptedDiscoveries: %v", err)
		}
		if len(result.Inserted) != 0 {
			t.Fatal("queued a discovery with no issue")
		}
	})
	t.Run("bad input", func(t *testing.T) {
		t.Parallel()
		fixture := newReviewFixture(t)
		controller := fixture.backlogController(t)
		if _, err := controller.InsertAcceptedDiscoveries(0); !errors.Is(
			err, ErrDiscoveryBacklogInsert,
		) {
			t.Fatalf("error = %v", err)
		}
		if _, err := NewDiscoveryBacklogController(nil); err == nil {
			t.Fatal("nil store accepted")
		}
	})
}

func TestDiscoveryBacklogConcurrentInsertionCreatesOneTask(t *testing.T) {
	fixture := newReviewFixture(t)
	fixture.publishedDiscovery(t,
		"Racy finding", domain.DecisionCreateNextTask, domain.SeverityHigh, 940)
	controller := fixture.backlogController(t)

	var group sync.WaitGroup
	inserted := make(chan int, 4)
	failures := make(chan error, 4)
	for range 4 {
		group.Add(1)
		go func() {
			defer group.Done()
			result, err := controller.InsertAcceptedDiscoveries(fixture.projectID)
			if err != nil {
				failures <- err
				return
			}
			inserted <- len(result.Inserted)
		}()
	}
	group.Wait()
	close(inserted)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	total := 0
	for count := range inserted {
		total += count
	}
	if total != 1 {
		t.Fatalf("inserted %d tasks, want 1", total)
	}
	tasks, _ := fixture.store.ListProjectTasks(fixture.projectID)
	if len(tasks) != 4 {
		t.Fatalf("backlog has %d tasks", len(tasks))
	}
}

func (fixture *reviewFixture) backlogController(
	t *testing.T,
) *DiscoveryBacklogController {
	t.Helper()
	controller, err := NewDiscoveryBacklogController(fixture.store)
	if err != nil {
		t.Fatal(err)
	}
	return controller
}

// publishedDiscovery records a discovery, applies a verdict, and binds it to a
// GitHub issue — the state item 43 consumes.
func (fixture *reviewFixture) publishedDiscovery(
	t *testing.T,
	title string,
	decision domain.DiscoveryDecision,
	severity domain.DiscoverySeverity,
	issueNumber int,
) *domain.Discovery {
	t.Helper()
	discovery := fixture.decidedDiscovery(t, title, decision, severity)
	published, err := fixture.store.RecordDiscoveryIssue(store.DiscoveryIssueUpdate{
		ProjectID:   fixture.projectID,
		DiscoveryID: discovery.ID,
		IssueNumber: issueNumber,
	})
	if err != nil {
		t.Fatal(err)
	}
	return published
}
