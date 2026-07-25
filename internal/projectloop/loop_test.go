package projectloop

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	"github.com/eslam-mahmoud/go-ai-agent/internal/project"
	"github.com/eslam-mahmoud/go-ai-agent/internal/workflow"
)

type fakeController struct {
	snapshot *project.Snapshot
	err      error
}

func (fake *fakeController) Snapshot(int64) (*project.Snapshot, error) {
	return fake.snapshot, fake.err
}

type fakeDelivery struct {
	result *workflow.FeatureResult
	err    error
	calls  int
}

func (fake *fakeDelivery) Run(
	context.Context, int64, int64,
) (*workflow.FeatureResult, error) {
	fake.calls++
	return fake.result, fake.err
}

type fakeReviewer struct {
	afterTask   *project.ReviewResult
	projectWide *project.ReviewResult
	err         error
	afterCalls  int
	projectCall int
}

func (fake *fakeReviewer) ReviewAfterTask(
	context.Context, int64, int64,
) (*project.ReviewResult, error) {
	fake.afterCalls++
	return fake.afterTask, fake.err
}

func (fake *fakeReviewer) ReviewProject(
	context.Context, int64,
) (*project.ReviewResult, error) {
	fake.projectCall++
	return fake.projectWide, fake.err
}

type fakeInitializer struct {
	result *project.InitializationResult
	err    error
	calls  int
}

func (fake *fakeInitializer) Initialize(
	context.Context, int64,
) (*project.InitializationResult, error) {
	fake.calls++
	return fake.result, fake.err
}

// workflowResultCompleted is the ordinary outcome of a delivered task.
var workflowResultCompleted = workflow.FeatureResult{FinalStatus: domain.TaskCompleted}

func snapshotWith(state domain.ProjectState, current *domain.Task, tasks ...*domain.Task) *project.Snapshot {
	return &project.Snapshot{
		Project:     &domain.Project{ID: 1, State: state},
		Tasks:       tasks,
		CurrentTask: current,
	}
}

func newLoop(
	t *testing.T,
	controller Controller,
	delivery Delivery,
	reviewer Reviewer,
	options Options,
) *Loop {
	t.Helper()
	loop, err := New(1, controller, delivery, reviewer, options)
	if err != nil {
		t.Fatal(err)
	}
	return loop
}

func TestNewRejectsMissingCollaborators(t *testing.T) {
	controller := &fakeController{}
	delivery := &fakeDelivery{}
	reviewer := &fakeReviewer{}
	tests := []struct {
		name       string
		projectID  int64
		controller Controller
		delivery   Delivery
		reviewer   Reviewer
	}{
		{"no project", 0, controller, delivery, reviewer},
		{"no controller", 1, nil, delivery, reviewer},
		{"no delivery", 1, controller, nil, reviewer},
		{"no reviewer", 1, controller, delivery, nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := New(
				test.projectID, test.controller, test.delivery, test.reviewer, Options{},
			)
			if err == nil {
				t.Fatal("expected construction to fail")
			}
		})
	}
}

func TestTickDoesNothingWhileTheProjectIsPaused(t *testing.T) {
	delivery := &fakeDelivery{}
	reviewer := &fakeReviewer{}
	loop := newLoop(t,
		&fakeController{snapshot: snapshotWith(
			domain.ProjectPaused,
			&domain.Task{ID: 5, Status: domain.TaskSelected},
			&domain.Task{ID: 5},
		)},
		delivery, reviewer, Options{})

	outcome, err := loop.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Action != ActionPaused {
		t.Fatalf("action = %q", outcome.Action)
	}
	if delivery.calls != 0 || reviewer.afterCalls != 0 || reviewer.projectCall != 0 {
		t.Fatal("a paused project must not advance")
	}
}

func TestTickInitializesAnEmptyProject(t *testing.T) {
	initializer := &fakeInitializer{result: &project.InitializationResult{}}
	loop := newLoop(t,
		&fakeController{snapshot: snapshotWith(domain.ProjectExecuting, nil)},
		&fakeDelivery{}, &fakeReviewer{},
		Options{Initializer: initializer})

	outcome, err := loop.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Action != ActionInitialize || initializer.calls != 1 {
		t.Fatalf("outcome = %#v, calls = %d", outcome, initializer.calls)
	}
}

// Without an initializer an empty project must idle rather than error: a
// backlog created by hand is a supported way to start.
func TestEmptyProjectIdlesWithoutAnInitializer(t *testing.T) {
	loop := newLoop(t,
		&fakeController{snapshot: snapshotWith(domain.ProjectExecuting, nil)},
		&fakeDelivery{}, &fakeReviewer{}, Options{})

	outcome, err := loop.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Action != ActionIdle {
		t.Fatalf("action = %q", outcome.Action)
	}
}

// Bootstrapping depends on this: the first task of a project has no
// predecessor, so only a taskless review can select it.
func TestTickSelectsTheFirstTaskThroughAProjectReview(t *testing.T) {
	queued := &domain.Task{ID: 9, Status: domain.TaskQueued, Title: "First"}
	reviewer := &fakeReviewer{projectWide: &project.ReviewResult{
		Selection: &project.SelectionResult{
			Applied: true,
			Task:    &domain.Task{ID: 9, Status: domain.TaskSelected, Title: "First"},
		},
	}}
	delivery := &fakeDelivery{}
	loop := newLoop(t,
		&fakeController{snapshot: snapshotWith(domain.ProjectExecuting, nil, queued)},
		delivery, reviewer, Options{})

	outcome, err := loop.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Action != ActionSelect || outcome.TaskID != 9 {
		t.Fatalf("outcome = %#v", outcome)
	}
	if reviewer.projectCall != 1 || reviewer.afterCalls != 0 || delivery.calls != 0 {
		t.Fatal("a project with no current task must ask for a selection")
	}
}

func TestTickDeliversAnInFlightTask(t *testing.T) {
	current := &domain.Task{ID: 4, Status: domain.TaskSelected}
	delivery := &fakeDelivery{result: &workflow.FeatureResult{
		FinalStatus: domain.TaskCompleted,
	}}
	reviewer := &fakeReviewer{}
	loop := newLoop(t,
		&fakeController{snapshot: snapshotWith(domain.ProjectExecuting, current, current)},
		delivery, reviewer, Options{})

	outcome, err := loop.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Action != ActionDeliver || outcome.Status != domain.TaskCompleted {
		t.Fatalf("outcome = %#v", outcome)
	}
	if delivery.calls != 1 || reviewer.afterCalls != 0 {
		t.Fatal("an in-flight task must be delivered, not reviewed")
	}
}

func TestTickReviewsATaskAwaitingManagerReview(t *testing.T) {
	current := &domain.Task{ID: 4, Status: domain.TaskCompleted}
	if !workflow.ManagerReviewRequired(current.Status) {
		t.Skip("completed no longer requires manager review")
	}
	reviewer := &fakeReviewer{afterTask: &project.ReviewResult{
		Selection: &project.SelectionResult{
			Applied: true,
			Task:    &domain.Task{ID: 6, Status: domain.TaskSelected, Title: "Next"},
		},
	}}
	delivery := &fakeDelivery{}
	loop := newLoop(t,
		&fakeController{snapshot: snapshotWith(domain.ProjectExecuting, current, current)},
		delivery, reviewer, Options{})

	outcome, err := loop.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Action != ActionReview || outcome.TaskID != 4 {
		t.Fatalf("outcome = %#v", outcome)
	}
	if reviewer.afterCalls != 1 || delivery.calls != 0 {
		t.Fatal("a terminal task must be reviewed, not re-delivered")
	}
}

func TestTickReportsIdleWhenTheManagerSelectsNothing(t *testing.T) {
	queued := &domain.Task{ID: 9, Status: domain.TaskQueued}
	loop := newLoop(t,
		&fakeController{snapshot: snapshotWith(domain.ProjectExecuting, nil, queued)},
		&fakeDelivery{},
		&fakeReviewer{projectWide: &project.ReviewResult{NoNextTask: true}},
		Options{})

	outcome, err := loop.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Action != ActionIdle {
		t.Fatalf("action = %q, detail = %q", outcome.Action, outcome.Detail)
	}
}

func TestTickPropagatesFailures(t *testing.T) {
	current := &domain.Task{ID: 4, Status: domain.TaskSelected}
	loop := newLoop(t,
		&fakeController{snapshot: snapshotWith(domain.ProjectExecuting, current, current)},
		&fakeDelivery{err: errors.New("engine unavailable")},
		&fakeReviewer{}, Options{})

	if _, err := loop.Tick(context.Background()); err == nil {
		t.Fatal("expected the delivery failure to surface")
	}
}

// One bad cycle must not stop the agent: Run logs and retries.
func TestRunKeepsTickingAfterAFailedCycle(t *testing.T) {
	current := &domain.Task{ID: 4, Status: domain.TaskSelected}
	delivery := &fakeDelivery{err: errors.New("engine unavailable")}
	loop := newLoop(t,
		&fakeController{snapshot: snapshotWith(domain.ProjectExecuting, current, current)},
		delivery, &fakeReviewer{}, Options{})

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	if err := loop.Run(ctx, 10*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if delivery.calls < 2 {
		t.Fatalf("ticks = %d, want the loop to retry after a failure", delivery.calls)
	}
}

func TestRunRejectsANonPositiveInterval(t *testing.T) {
	loop := newLoop(t,
		&fakeController{snapshot: snapshotWith(domain.ProjectExecuting, nil)},
		&fakeDelivery{}, &fakeReviewer{}, Options{})
	if err := loop.Run(context.Background(), 0); !errors.Is(err, ErrInvalidLoop) {
		t.Fatalf("err = %v", err)
	}
}
