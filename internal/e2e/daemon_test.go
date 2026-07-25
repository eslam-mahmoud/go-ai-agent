package e2e

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/config"
	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	"github.com/eslam-mahmoud/go-ai-agent/internal/projectloop"
)

// newDaemonLoop builds the loop exactly as the daemon does, with only the
// provider engine and GitHub faked. This is the standing guard against the
// failure this whole milestone was about: a v2 stack that is fully
// implemented, fully unit-tested, and never executed by a running daemon.
func (h *harness) newDaemonLoop(
	t *testing.T, script *modeScript, manager *scriptedManager,
) *projectloop.Loop {
	t.Helper()
	return h.newDaemonLoopWithArchitect(t, script, manager, &scriptedArchitect{
		output: architectOutput(),
	})
}

func (h *harness) newDaemonLoopWithArchitect(
	t *testing.T,
	script *modeScript,
	manager *scriptedManager,
	architect *scriptedArchitect,
) *projectloop.Loop {
	t.Helper()
	cfg := &config.Config{WorkspaceDir: h.workspaceRoot}
	loop, err := projectloop.BuildWithRunners(projectloop.Dependencies{
		Config:    cfg,
		Store:     h.store,
		GitHub:    h.github,
		ProjectID: h.projectID,
	}, script, manager, architect)
	if err != nil {
		t.Fatalf("building the daemon's wiring: %v", err)
	}
	return loop
}

func TestDaemonWiringSelectsDeliversAndReviewsATask(t *testing.T) {
	harness := newHarness(t)
	first := harness.queueTask("Add the delivery loop", 401)
	second := harness.queueTask("Document the loop", 402)

	script := newScript()
	manager := &scriptedManager{output: managerOutput(map[string]any{
		"next_task": map[string]any{
			"task_id":      first.ID,
			"issue_number": first.IssueNumber,
			"reason":       "First in the ordered backlog.",
		},
	})}
	loop := harness.newDaemonLoop(t, script, manager)
	ctx := context.Background()

	// Tick one: no task is in the lane, so the manager selects the first.
	selected, err := loop.Tick(ctx)
	if err != nil {
		t.Fatalf("selection tick: %v", err)
	}
	if selected.Action != projectloop.ActionSelect || selected.TaskID != first.ID {
		t.Fatalf("selection tick = %#v", selected)
	}
	if harness.task(first.ID).Status != domain.TaskSelected {
		t.Fatalf("task status = %q", harness.task(first.ID).Status)
	}

	// Tick two: the selected task runs the real sequential workflow.
	delivered, err := loop.Tick(ctx)
	if err != nil {
		t.Fatalf("delivery tick: %v", err)
	}
	if delivered.Action != projectloop.ActionDeliver {
		t.Fatalf("delivery tick = %#v", delivered)
	}
	if delivered.Status != domain.TaskCompleted {
		t.Fatalf("delivery reached %q, want completed", delivered.Status)
	}
	// Every delivery mode must have actually run, not just the first.
	for _, want := range []string{"planner", "developer", "reviewer", "verifier"} {
		found := false
		for _, ran := range script.ran {
			if string(ran) == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("mode %q never ran: %v", want, script.ran)
		}
	}

	// Tick three: the completed task is reviewed and the next one selected.
	manager.output = managerOutput(map[string]any{
		"next_task": map[string]any{
			"task_id":      second.ID,
			"issue_number": second.IssueNumber,
			"reason":       "Next in the ordered backlog.",
		},
	})
	reviewed, err := loop.Tick(ctx)
	if err != nil {
		t.Fatalf("review tick: %v", err)
	}
	if reviewed.Action != projectloop.ActionReview || reviewed.TaskID != first.ID {
		t.Fatalf("review tick = %#v", reviewed)
	}
	if manager.calls != 2 {
		t.Fatalf("manager ran %d times, want one selection and one review", manager.calls)
	}
	if harness.task(second.ID).Status != domain.TaskSelected {
		t.Fatalf("next task status = %q", harness.task(second.ID).Status)
	}
	harness.requireEvent(domain.WorkflowTaskSelected)
}

// A paused project must stay paused no matter how long the daemon runs.
func TestDaemonWiringHonoursAPausedProject(t *testing.T) {
	harness := newHarness(t)
	harness.queueTask("Should not start", 403)
	paused, err := harness.store.GetProjectByID(harness.projectID)
	if err != nil {
		t.Fatal(err)
	}
	paused.PausedFromState = paused.State
	paused.State = domain.ProjectPaused
	if _, err := harness.store.UpdateProject(paused); err != nil {
		t.Fatal(err)
	}

	manager := &scriptedManager{output: managerOutput(nil)}
	loop := harness.newDaemonLoop(t, newScript(), manager)

	outcome, err := loop.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Action != projectloop.ActionPaused {
		t.Fatalf("action = %q, want paused", outcome.Action)
	}
	if manager.calls != 0 {
		t.Fatal("a paused project must not consult the manager")
	}
}

// Run must survive cancellation cleanly: the daemon shuts down on a signal
// and a loop that ignored the context would block the whole process.
func TestDaemonLoopStopsWhenTheContextIsCancelled(t *testing.T) {
	harness := newHarness(t)
	harness.queueTask("Anything", 404)
	loop := harness.newDaemonLoop(
		t, newScript(), &scriptedManager{output: managerOutput(nil)},
	)

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- loop.Run(ctx, 10*time.Millisecond) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run returned without reporting cancellation")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run ignored context cancellation")
	}
}

// Without GitHub credentials the cycle must still decide, just not publish.
func TestDaemonWiringRunsWithoutGitHubCredentials(t *testing.T) {
	harness := newHarness(t)
	task := harness.queueTask("Runs without GitHub", 405)

	cfg := &config.Config{WorkspaceDir: filepath.Clean(harness.workspaceRoot)}
	loop, err := projectloop.BuildWithRunners(projectloop.Dependencies{
		Config:    cfg,
		Store:     harness.store,
		ProjectID: harness.projectID,
	}, newScript(), &scriptedManager{output: managerOutput(map[string]any{
		"next_task": map[string]any{
			"task_id":      task.ID,
			"issue_number": task.IssueNumber,
			"reason":       "Only task in the backlog.",
		},
	})}, &scriptedArchitect{output: architectOutput()})
	if err != nil {
		t.Fatalf("building without GitHub: %v", err)
	}
	outcome, err := loop.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick without GitHub: %v", err)
	}
	if outcome.Action != projectloop.ActionSelect || outcome.TaskID != task.ID {
		t.Fatalf("outcome = %#v", outcome)
	}
}

// A manager review that requires architecture review must actually run the
// Architect. Recording the obligation and stopping there is the failure this
// item existed to fix: the mode was complete and unreachable.
func TestDaemonWiringRunsTheArchitectWhenReviewRequiresIt(t *testing.T) {
	harness := newHarness(t)
	task := harness.queueTask("Needs an architecture decision", 406)

	architect := &scriptedArchitect{output: architectOutput()}
	manager := &scriptedManager{output: managerOutput(map[string]any{
		"architecture_review_required": true,
		"discoveries": []any{
			map[string]any{
				"title":       "The delivery loop has no back pressure",
				"description": "Ticks queue without bound when the provider is slow.",
				"category":    "architecture",
				"severity":    "high",
			},
		},
		"next_task": map[string]any{
			"task_id":      task.ID,
			"issue_number": task.IssueNumber,
			"reason":       "First in the ordered backlog.",
		},
	})}
	loop := harness.newDaemonLoopWithArchitect(t, newScript(), manager, architect)

	// Selection is refused while an architecture risk is unresolved, which is
	// the invariant doing its job. What matters here is that the architect
	// ran first rather than the obligation being recorded and abandoned.
	_, err := loop.Tick(context.Background())
	if err == nil {
		t.Fatal("selection must be refused while an architecture risk is open")
	}
	if !strings.Contains(err.Error(), "architecture risk") {
		t.Fatalf("err = %v, want the architecture-risk precondition", err)
	}
	if architect.calls != 1 {
		t.Fatalf("architect ran %d times, want 1", architect.calls)
	}
	harness.requireEvent(domain.WorkflowArchitectureReviewRequired)
}

// An architect failure must not lose the delivery decision the manager already
// made: the review is durable before the architecture run is attempted.
func TestArchitectFailureDoesNotDiscardTheReview(t *testing.T) {
	harness := newHarness(t)
	task := harness.queueTask("Still selectable", 407)

	architect := &scriptedArchitect{err: errAchitectUnavailable}
	manager := &scriptedManager{output: managerOutput(map[string]any{
		"architecture_review_required": true,
		"discoveries": []any{
			map[string]any{
				"title":       "Unbounded retry loop",
				"description": "Failures retry without a ceiling.",
				"category":    "architecture",
				"severity":    "high",
			},
		},
		"next_task": map[string]any{
			"task_id":      task.ID,
			"issue_number": task.IssueNumber,
			"reason":       "First in the ordered backlog.",
		},
	})}
	loop := harness.newDaemonLoopWithArchitect(t, newScript(), manager, architect)

	_, err := loop.Tick(context.Background())
	if err == nil {
		t.Fatal("an architect failure must surface rather than be swallowed")
	}
	reviews, listErr := harness.store.LatestManagerReview(harness.projectID)
	if listErr != nil {
		t.Fatal(listErr)
	}
	if reviews == nil {
		t.Fatal("the manager review must be durable before the architect runs")
	}
}

var errAchitectUnavailable = errors.New("architect engine unavailable")
