package e2e

import (
	"context"
	"testing"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	githubclient "github.com/eslam-mahmoud/go-ai-agent/internal/github"
	"github.com/eslam-mahmoud/go-ai-agent/internal/project"
	"github.com/eslam-mahmoud/go-ai-agent/internal/workflow"
)

// Scenario 1: the happy path, from manager selection through completion and
// on to the next task.
func TestHappyPathFromSelectionThroughCompletionToNextTask(t *testing.T) {
	t.Parallel()
	harness := newHarness(t)
	first := harness.queueTask("Extract the engine interface", 41)
	second := harness.queueTask("Split the store", 42)

	harness.selectTask(first, "Next dependency for the MVP")
	script := newScript()
	result := harness.runDelivery(first, script, workflow.FeatureOptions{})

	if result.FinalStatus != domain.TaskCompleted {
		t.Fatalf("final status = %q", result.FinalStatus)
	}
	// Every delivery mode ran, in order. A skipped stage shows up here.
	wantModes := []workflow.ModeName{
		workflow.ModePlanner,
		workflow.ModeDeveloper,
		workflow.ModeReviewer,
		workflow.ModeVerifier,
	}
	if len(script.ran) != len(wantModes) {
		t.Fatalf("modes ran = %v, want %v", script.ran, wantModes)
	}
	for index, mode := range wantModes {
		if script.ran[index] != mode {
			t.Fatalf("modes ran = %v, want %v", script.ran, wantModes)
		}
	}
	harness.requireEvent(domain.WorkflowTaskSelected)
	harness.requireEvent(domain.WorkflowTaskTransitioned)

	// The manager reviews the completed task and selects the next one.
	manager := &scriptedManager{output: managerOutput(map[string]any{
		"next_task": map[string]any{
			"issue_number": second.IssueNumber,
			"reason":       "Next dependency",
		},
	})}
	review := harness.reviewCycle(manager)
	cycle, err := review.ReviewAfterTask(context.Background(), harness.projectID, first.ID)
	if err != nil {
		t.Fatalf("ReviewAfterTask: %v", err)
	}
	if manager.calls != 1 || cycle.Review == nil {
		t.Fatalf("review cycle = %#v, manager calls = %d", cycle, manager.calls)
	}
	if cycle.Selection == nil || cycle.Selection.Task.ID != second.ID {
		t.Fatalf("selection = %#v", cycle.Selection)
	}
	if harness.task(second.ID).Status != domain.TaskSelected {
		t.Fatalf("second task = %q", harness.task(second.ID).Status)
	}
	// The delivery lane still holds exactly one active task.
	active := 0
	for _, task := range harness.tasks() {
		if task.Status.Active() {
			active++
		}
	}
	if active != 1 {
		t.Fatalf("%d tasks are active", active)
	}
}

// Scenario 2: a review finding causes a fix and a re-review.
func TestReviewFindingCausesFixAndReReview(t *testing.T) {
	t.Parallel()
	harness := newHarness(t)
	task := harness.queueTask("Add the Codex adapter", 43)
	harness.selectTask(task, "Blocking the MVP")

	script := newScript().
		on(workflow.ModeReviewer, workflow.ModeOutcome{
			Status: workflow.ModeCompleted, BlockingFindings: true,
		}).
		on(workflow.ModeReviewer, completed())
	result := harness.runDelivery(task, script, workflow.FeatureOptions{})

	if result.FinalStatus != domain.TaskCompleted {
		t.Fatalf("final status = %q", result.FinalStatus)
	}
	if result.ReviewFixCycles != 1 {
		t.Fatalf("review/fix cycles = %d, want 1", result.ReviewFixCycles)
	}
	fixed, reviewed := 0, 0
	for _, mode := range script.ran {
		switch mode {
		case workflow.ModeFixer:
			fixed++
		case workflow.ModeReviewer:
			reviewed++
		}
	}
	if fixed != 1 || reviewed != 2 {
		t.Fatalf("modes ran = %v", script.ran)
	}
}

// Scenario 3: a discovery becomes queued work ahead of the previous next task.
func TestDiscoveryBecomesQueuedWorkAheadOfTheNextTask(t *testing.T) {
	t.Parallel()
	harness := newHarness(t)
	active := harness.queueTask("Active work", 44)
	planned := harness.queueTask("Previously planned next", 45)
	harness.selectTask(active, "First")
	harness.runDelivery(active, newScript(), workflow.FeatureOptions{})

	// The developer found something while working.
	found, err := harness.store.CreateDiscoveries(harness.projectID, []*domain.Discovery{
		domain.NewDiscovery(
			harness.projectID, active.ID, 0,
			"Retry budget is unbounded", domain.DiscoveryBug, domain.SeverityCritical,
		),
	}, "e2e-discovery")
	if err != nil {
		t.Fatal(err)
	}
	discovery := found.Created[0]

	manager := &scriptedManager{output: managerOutput(map[string]any{
		"discovery_decisions": []map[string]any{{
			"discovery_id": discovery.ID,
			"decision":     "create-next-task",
			"reason":       "Blocks the MVP",
		}},
	})}
	cycle, err := harness.reviewCycle(manager).ReviewAfterTask(
		context.Background(), harness.projectID, active.ID,
	)
	if err != nil {
		t.Fatalf("ReviewAfterTask: %v", err)
	}
	if cycle.DiscoveryIssues == nil || len(cycle.DiscoveryIssues.Created) != 1 {
		t.Fatalf("discovery issues = %#v", cycle.DiscoveryIssues)
	}
	if cycle.DiscoveryBacklog == nil || len(cycle.DiscoveryBacklog.Inserted) != 1 {
		t.Fatalf("discovery backlog = %#v", cycle.DiscoveryBacklog)
	}

	// The new work sits ahead of the previously planned task.
	tasks := harness.tasks()
	inserted := cycle.DiscoveryBacklog.Inserted[0]
	var insertedPosition, plannedPosition int
	for _, task := range tasks {
		switch task.ID {
		case inserted.ID:
			insertedPosition = task.Sequence
		case planned.ID:
			plannedPosition = task.Sequence
		}
	}
	if insertedPosition == 0 || plannedPosition == 0 ||
		insertedPosition >= plannedPosition {
		t.Fatalf("discovery at %d, planned at %d", insertedPosition, plannedPosition)
	}
	if inserted.IssueNumber == 0 {
		t.Fatal("queued discovery work has no issue")
	}
	harness.requireEvent(domain.WorkflowDiscoveriesRecorded)
	harness.requireEvent(domain.WorkflowDiscoveryQueued)
}

// Scenario 6: a restart mid-delivery resumes safely rather than restarting
// the task or losing the lane.
func TestRestartDuringDeliveryResumesSafely(t *testing.T) {
	t.Parallel()
	harness := newHarness(t)
	task := harness.queueTask("Long running work", 46)
	harness.selectTask(task, "First")

	// Deliver until the task blocks, standing in for an interrupted run.
	blocking := newScript().
		on(workflow.ModeDeveloper, workflow.ModeOutcome{Status: workflow.ModeBlocked})
	result := harness.runDelivery(task, blocking, workflow.FeatureOptions{})
	if result.FinalStatus != domain.TaskBlocked {
		t.Fatalf("final status = %q", result.FinalStatus)
	}

	// A fresh controller, as after a restart, sees the same durable state.
	restarted, err := project.NewController(harness.store)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := restarted.Snapshot(harness.projectID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.CurrentTask == nil || snapshot.CurrentTask.ID != task.ID {
		t.Fatalf("restart lost the current task: %#v", snapshot.CurrentTask)
	}
	if snapshot.CurrentTask.Status != domain.TaskBlocked {
		t.Fatalf("restart changed the task status to %q", snapshot.CurrentTask.Status)
	}
	// The lane is still held by exactly that task, so nothing else can start.
	other := harness.queueTask("Should not start", 47)
	harness.recordReview(func(record *domain.ManagerReview) {
		record.NextTaskID = &other.ID
		record.NextTaskIssueNumber = other.IssueNumber
		record.NextTaskReason = "Should be refused"
	})
	selector, err := project.NewSelectionController(harness.store)
	if err != nil {
		t.Fatal(err)
	}
	latest, err := harness.store.LatestManagerReview(harness.projectID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := selector.SelectNextTask(harness.projectID, latest.ID); err == nil {
		t.Fatal("a second task started while one held the lane")
	}
}

// Scenario 9: GitHub drift is repaired by reconciliation.
func TestGitHubDriftIsRepairedByReconciliation(t *testing.T) {
	t.Parallel()
	harness := newHarness(t)
	task := harness.queueTask("Delivered work", 48)
	harness.selectTask(task, "First")
	harness.runDelivery(task, newScript(), workflow.FeatureOptions{})

	// GitHub still shows the issue open with a stale label, and a human label
	// that must survive.
	harness.github.issues[48] = &githubclient.Issue{
		Number: 48, State: "open",
		Labels: []string{"madar:developing", "needs-design"},
	}
	// The branch has a merged pull request the database never recorded.
	branch := "madar/issue-48"
	stored := harness.task(task.ID)
	stored.BranchName = branch
	if _, err := harness.store.UpdateProjectTask(stored); err != nil {
		t.Fatal(err)
	}
	harness.github.pulls[branch] = []*githubclient.PullRequest{
		{Number: 71, State: "closed", Merged: true, HeadBranch: branch},
	}

	reconciler, err := project.NewReconciler(harness.store, harness.github)
	if err != nil {
		t.Fatal(err)
	}
	result, err := reconciler.Reconcile(context.Background(), harness.projectID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(result.Tasks) != 1 {
		t.Fatalf("reconciled %d tasks", len(result.Tasks))
	}
	reconciliation := result.Tasks[0]
	if !reconciliation.LabelsUpdated || !reconciliation.IssueClosed {
		t.Fatalf("drift was not repaired: %#v", reconciliation)
	}
	if reconciliation.PullRequestBound != 71 {
		t.Fatalf("pull request was not bound: %#v", reconciliation)
	}
	// The human's label survived, and the status label is now correct.
	labels := harness.github.lastLabels
	foundHuman, foundStatus := false, false
	for _, label := range labels {
		switch label {
		case "needs-design":
			foundHuman = true
		case "madar:done":
			foundStatus = true
		}
	}
	if !foundHuman || !foundStatus {
		t.Fatalf("labels after reconciliation = %v", labels)
	}
	// A second pass changes nothing.
	harness.github.labelWrites = 0
	harness.github.closes = 0
	if _, err := reconciler.Reconcile(context.Background(), harness.projectID); err != nil {
		t.Fatal(err)
	}
	if harness.github.labelWrites != 0 || harness.github.closes != 0 {
		t.Fatalf("a converged project still wrote: %d labels, %d closes",
			harness.github.labelWrites, harness.github.closes)
	}
}

// reviewCycle wires the real review coordinator with every stage attached, so
// a missing stage fails the scenario rather than passing quietly.
func (h *harness) reviewCycle(manager project.ManagerRunner) *project.ReviewCoordinator {
	h.t.Helper()
	discovery, err := project.NewDiscoveryController(h.store)
	if err != nil {
		h.t.Fatal(err)
	}
	backlog, err := project.NewBacklogController(h.store)
	if err != nil {
		h.t.Fatal(err)
	}
	selection, err := project.NewSelectionController(h.store)
	if err != nil {
		h.t.Fatal(err)
	}
	issues, err := project.NewDiscoveryIssuePublisher(h.store, h.github)
	if err != nil {
		h.t.Fatal(err)
	}
	discoveryBacklog, err := project.NewDiscoveryBacklogController(h.store)
	if err != nil {
		h.t.Fatal(err)
	}
	architecture, err := project.NewArchitectureController(h.store)
	if err != nil {
		h.t.Fatal(err)
	}
	coordinator, err := project.NewReviewCoordinator(
		h.store, manager, discovery, backlog, selection,
		project.ReviewCoordinatorOptions{
			DiscoveryIssues:  issues,
			DiscoveryBacklog: discoveryBacklog,
			Architecture:     architecture,
		},
	)
	if err != nil {
		h.t.Fatal(err)
	}
	return coordinator
}
