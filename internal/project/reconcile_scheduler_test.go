package project

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	githubclient "github.com/eslam-mahmoud/go-ai-agent/internal/github"
)

func TestReconcileOnceCoversEveryProject(t *testing.T) {
	t.Parallel()
	fixture := newInitFixture(t)
	fixture.reconcilableTask(t, "Filed work", 61, domain.TaskDeveloping, "")
	client := newReconcileClient()
	client.issues[61] = &githubclient.Issue{
		Number: 61, State: "open", Labels: []string{"madar:queued"},
	}
	scheduler := fixture.scheduler(t, client, 0)

	results, err := scheduler.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	if len(results) != 1 || len(results[0].Tasks) != 1 {
		t.Fatalf("results = %#v", results)
	}
	if !results[0].Tasks[0].LabelsUpdated {
		t.Fatal("scheduled pass did not converge labels")
	}
}

// One unreachable repository must not stop the others from converging.
func TestReconcileOnceSkipsAFailingProject(t *testing.T) {
	t.Parallel()
	fixture := newInitFixture(t)
	fixture.reconcilableTask(t, "Filed work", 62, domain.TaskDeveloping, "")
	client := newReconcileClient()
	client.getErr = errors.New("github is unavailable")
	scheduler := fixture.scheduler(t, client, 0)

	results, err := scheduler.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOnce returned an error instead of skipping: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("results = %#v", results)
	}
}

func TestRunReconcilesAtStartupThenOnTheInterval(t *testing.T) {
	t.Parallel()
	fixture := newInitFixture(t)
	fixture.reconcilableTask(t, "Filed work", 63, domain.TaskDeveloping, "")
	client := newReconcileClient()
	client.issues[63] = &githubclient.Issue{Number: 63, State: "open"}
	// Measure one pass, since a pass makes several reads.
	if _, err := fixture.scheduler(t, client, 0).ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	perPass := client.readCount()
	if perPass == 0 {
		t.Fatal("a pass performed no reads")
	}
	client.resetReads()

	// Wait for the second pass rather than racing a fixed deadline: a loaded
	// machine can make one pass take longer than a short timeout, which says
	// nothing about whether the scheduler ticks.
	scheduler := fixture.scheduler(t, client, 10*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- scheduler.Run(ctx) }()

	deadline := time.After(30 * time.Second)
	for client.readCount() < 2*perPass {
		select {
		case <-deadline:
			cancel()
			t.Fatalf("scheduler read %d times, want at least %d",
				client.readCount(), 2*perPass)
		case err := <-done:
			t.Fatalf("Run exited early: %v", err)
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v", err)
	}
}

func TestRunWithoutAnIntervalReconcilesOnlyOnce(t *testing.T) {
	t.Parallel()
	fixture := newInitFixture(t)
	fixture.reconcilableTask(t, "Filed work", 64, domain.TaskDeveloping, "")
	client := newReconcileClient()
	client.issues[64] = &githubclient.Issue{Number: 64, State: "open"}
	if _, err := fixture.scheduler(t, client, 0).ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	perPass := client.readCount()
	client.resetReads()

	scheduler := fixture.scheduler(t, client, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	if err := scheduler.Run(ctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run error = %v", err)
	}
	// The startup pass only: a zero interval schedules nothing further.
	if client.readCount() != perPass {
		t.Fatalf("disabled interval read %d times, want %d", client.reads, perPass)
	}
}

func TestNewReconcileSchedulerValidatesItsDependencies(t *testing.T) {
	t.Parallel()
	fixture := newInitFixture(t)
	reconciler := fixture.reconciler(t, newReconcileClient())
	if _, err := NewReconcileScheduler(nil, fixture.store, time.Minute, nil); err == nil {
		t.Error("missing reconciler accepted")
	}
	if _, err := NewReconcileScheduler(reconciler, nil, time.Minute, nil); err == nil {
		t.Error("missing project lister accepted")
	}
	if _, err := NewReconcileScheduler(
		reconciler, fixture.store, -time.Minute, nil,
	); err == nil {
		t.Error("negative interval accepted")
	}
	// A nil logger is allowed and must not panic.
	scheduler, err := NewReconcileScheduler(reconciler, fixture.store, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scheduler.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("ReconcileOnce with a default logger: %v", err)
	}
}

func (fixture *initFixture) scheduler(
	t *testing.T,
	client ReconcileClient,
	interval time.Duration,
) *ReconcileScheduler {
	t.Helper()
	scheduler, err := NewReconcileScheduler(
		fixture.reconciler(t, client),
		fixture.store,
		interval,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	if err != nil {
		t.Fatal(err)
	}
	return scheduler
}
