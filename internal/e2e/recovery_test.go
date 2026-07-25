package e2e

import (
	"context"
	"testing"

	"github.com/eslam-mahmoud/go-ai-agent/internal/config"
	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	githubclient "github.com/eslam-mahmoud/go-ai-agent/internal/github"
	"github.com/eslam-mahmoud/go-ai-agent/internal/projectloop"
)

func projectModeConfig(workspaceRoot string) *config.Config {
	cfg := &config.Config{WorkspaceDir: workspaceRoot}
	cfg.Project.Repo = "owner/repo"
	return cfg
}

// A restart must repair v2 state before anything reads it. The daemon ran v1
// recovery and v2 reconciliation but never v2 recovery, so an in-flight task
// resumed on top of a provider process that no longer existed.
func TestStartupRecoveryInterruptsRunningExecutions(t *testing.T) {
	harness := newHarness(t)
	task := harness.queueTask("Was running when the process died", 501)
	harness.selectTask(task, "In the lane when the crash happened")

	running := domain.NewExecution(harness.projectID, task.ID, "developer", "claude", "opus", 1)
	running.Status = domain.ExecutionRunning
	if _, err := harness.store.CreateExecution(running); err != nil {
		t.Fatal(err)
	}

	if err := projectloop.Recover(
		projectModeConfig(harness.workspaceRoot), harness.store, nil,
	); err != nil {
		t.Fatalf("recovery: %v", err)
	}

	executions, err := harness.store.ListTaskExecutions(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, execution := range executions {
		if execution.Status == domain.ExecutionRunning {
			t.Fatalf("execution %d is still marked running after recovery", execution.ID)
		}
	}
}

// Recovery is a no-op when project mode is off, so the daemon can call it
// unconditionally without touching a v1-only installation.
func TestRecoveryIsANoOpWhenProjectModeIsOff(t *testing.T) {
	harness := newHarness(t)
	if err := projectloop.Recover(
		&config.Config{WorkspaceDir: harness.workspaceRoot}, harness.store, nil,
	); err != nil {
		t.Fatal(err)
	}
}

// Running recovery twice must be safe: a daemon that crash-loops would
// otherwise corrupt state a little more on each restart.
func TestRecoveryIsIdempotent(t *testing.T) {
	harness := newHarness(t)
	task := harness.queueTask("Selected before the crash", 502)
	harness.selectTask(task, "In the lane")

	cfg := projectModeConfig(harness.workspaceRoot)
	for attempt := 0; attempt < 3; attempt++ {
		if err := projectloop.Recover(cfg, harness.store, nil); err != nil {
			t.Fatalf("recovery attempt %d: %v", attempt+1, err)
		}
	}
}

// The pull request a task's own branch opened must be attached to it, so the
// verifier sees a PR number rather than zero.
func TestDeliveryCycleAttachesThePullRequest(t *testing.T) {
	harness := newHarness(t)
	task := harness.queueTask("Opens a pull request", 503)
	harness.selectTask(task, "First in the backlog")

	// Nothing assigns a branch yet, so the test sets the state the delivery
	// modes will eventually produce and exercises discovery from there.
	const branch = "madar/issue-503"
	selected := harness.task(task.ID)
	selected.BranchName = branch
	if _, err := harness.store.UpdateProjectTask(selected); err != nil {
		t.Fatal(err)
	}
	harness.github.pulls[branch] = []*githubclient.PullRequest{
		{Number: 77, HeadBranch: branch, State: "open"},
	}

	loop := harness.newDaemonLoop(
		t, newScript(), &scriptedManager{output: managerOutput(nil)},
	)
	if _, err := loop.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := harness.task(task.ID).PRNumber; got != 77 {
		t.Fatalf("PR number = %d, want 77", got)
	}
}
