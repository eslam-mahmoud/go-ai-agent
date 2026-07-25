package project

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	githubclient "github.com/eslam-mahmoud/go-ai-agent/internal/github"
	"github.com/eslam-mahmoud/go-ai-agent/internal/workflow"
)

func TestReconcileConvergesLabelsWithoutDeletingForeignOnes(t *testing.T) {
	t.Parallel()
	fixture := newInitFixture(t)
	task := fixture.reconcilableTask(t, "Implement the thing", 41, domain.TaskDeveloping, "")
	client := newReconcileClient()
	// A stale Madar label plus labels a human added.
	client.issues[41] = &githubclient.Issue{
		Number: 41,
		State:  "open",
		Labels: []string{"madar:queued", "priority:high", "needs-design"},
	}
	reconciler := fixture.reconciler(t, client)

	result, err := reconciler.Reconcile(context.Background(), fixture.projectID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(result.Tasks) != 1 || !result.Tasks[0].LabelsUpdated {
		t.Fatalf("result = %#v", result)
	}
	written := client.lastLabels
	if !containsString(written, "madar:developing") {
		t.Fatalf("status label missing: %v", written)
	}
	for _, human := range []string{"priority:high", "needs-design"} {
		if !containsString(written, human) {
			t.Fatalf("reconciliation deleted %q: %v", human, written)
		}
	}
	if containsString(written, "madar:queued") {
		t.Fatalf("stale status label survived: %v", written)
	}
	_ = task

	// A second pass writes nothing.
	client.labelWrites = 0
	again, err := reconciler.Reconcile(context.Background(), fixture.projectID)
	if err != nil {
		t.Fatal(err)
	}
	if again.Tasks[0].LabelsUpdated || client.labelWrites != 0 {
		t.Fatalf("second pass wrote labels: %#v", again.Tasks[0])
	}
}

func TestReconcileClosesACompletedTaskIssueOnce(t *testing.T) {
	t.Parallel()
	fixture := newInitFixture(t)
	fixture.reconcilableTask(t, "Finished work", 42, domain.TaskCompleted, "")
	client := newReconcileClient()
	client.issues[42] = &githubclient.Issue{
		Number: 42, State: "open", Labels: []string{"madar:verifying"},
	}
	reconciler := fixture.reconciler(t, client)

	result, err := reconciler.Reconcile(context.Background(), fixture.projectID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !result.Tasks[0].IssueClosed || client.closes != 1 {
		t.Fatalf("result = %#v, closes = %d", result.Tasks[0], client.closes)
	}
	if !containsString(client.lastLabels, "madar:done") {
		t.Fatalf("completed label = %v", client.lastLabels)
	}

	again, err := reconciler.Reconcile(context.Background(), fixture.projectID)
	if err != nil {
		t.Fatal(err)
	}
	if again.Tasks[0].IssueClosed || client.closes != 1 {
		t.Fatalf("second pass closed again: closes = %d", client.closes)
	}
}

func TestReconcileReportsPullRequestDriftWithoutActingOnIt(t *testing.T) {
	t.Parallel()
	t.Run("merged while developing", func(t *testing.T) {
		t.Parallel()
		fixture := newInitFixture(t)
		fixture.reconcilableTask(t,
			"In progress", 43, domain.TaskDeveloping, "madar/issue-43")
		client := newReconcileClient()
		client.issues[43] = &githubclient.Issue{Number: 43, State: "open"}
		client.pulls["madar/issue-43"] = []*githubclient.PullRequest{
			{Number: 71, State: "closed", Merged: true, HeadBranch: "madar/issue-43"},
		}
		reconciler := fixture.reconciler(t, client)
		result, err := reconciler.Reconcile(context.Background(), fixture.projectID)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if !hasDrift(result.Tasks[0].Drift, "is merged while the task is") {
			t.Fatalf("drift = %v", result.Tasks[0].Drift)
		}
		// The reconciler binds the pull request but changes no task state.
		if result.Tasks[0].PullRequestBound != 71 {
			t.Fatalf("bound = %d", result.Tasks[0].PullRequestBound)
		}
		if client.closes != 0 {
			t.Fatal("drift caused a write")
		}
	})

	t.Run("open for a completed task", func(t *testing.T) {
		t.Parallel()
		fixture := newInitFixture(t)
		fixture.reconcilableTask(t,
			"Finished", 44, domain.TaskCompleted, "madar/issue-44")
		client := newReconcileClient()
		client.issues[44] = &githubclient.Issue{Number: 44, State: "closed"}
		client.pulls["madar/issue-44"] = []*githubclient.PullRequest{
			{Number: 72, State: "open", HeadBranch: "madar/issue-44"},
		}
		reconciler := fixture.reconciler(t, client)
		result, err := reconciler.Reconcile(context.Background(), fixture.projectID)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if !hasDrift(result.Tasks[0].Drift, "still open for a completed task") {
			t.Fatalf("drift = %v", result.Tasks[0].Drift)
		}
	})
}

func TestReconcileReportsAmbiguousBranchesAndMissingIssues(t *testing.T) {
	t.Parallel()
	t.Run("ambiguous branch", func(t *testing.T) {
		t.Parallel()
		fixture := newInitFixture(t)
		task := fixture.reconcilableTask(t,
			"Contested", 45, domain.TaskDeveloping, "madar/issue-45")
		client := newReconcileClient()
		client.issues[45] = &githubclient.Issue{Number: 45, State: "open"}
		client.pulls["madar/issue-45"] = []*githubclient.PullRequest{
			{Number: 81, State: "open", HeadBranch: "madar/issue-45"},
			{Number: 82, State: "open", HeadBranch: "madar/issue-45"},
		}
		reconciler := fixture.reconciler(t, client)
		result, err := reconciler.Reconcile(context.Background(), fixture.projectID)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if len(result.Ambiguous) != 1 || result.Ambiguous[0].TaskID != task.ID {
			t.Fatalf("ambiguous = %#v", result.Ambiguous)
		}
		if result.Tasks[0].PullRequestBound != 0 {
			t.Fatal("an ambiguous branch was bound anyway")
		}
	})

	t.Run("missing issue", func(t *testing.T) {
		t.Parallel()
		fixture := newInitFixture(t)
		fixture.reconcilableTask(t, "Vanished", 46, domain.TaskDeveloping, "")
		client := newReconcileClient()
		reconciler := fixture.reconciler(t, client)
		result, err := reconciler.Reconcile(context.Background(), fixture.projectID)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if !hasDrift(result.Tasks[0].Drift, "is missing on GitHub") {
			t.Fatalf("drift = %v", result.Tasks[0].Drift)
		}
		if client.labelWrites != 0 {
			t.Fatal("a missing issue was written to")
		}
	})
}

func TestReconcileSkipsTasksWithNothingOnGitHub(t *testing.T) {
	t.Parallel()
	fixture := newInitFixture(t)
	fixture.reconcilableTask(t, "Not filed yet", 0, domain.TaskQueued, "")
	client := newReconcileClient()
	reconciler := fixture.reconciler(t, client)

	result, err := reconciler.Reconcile(context.Background(), fixture.projectID)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(result.Tasks) != 0 {
		t.Fatalf("tasks = %#v", result.Tasks)
	}
	if client.labelWrites != 0 || client.closes != 0 {
		t.Fatal("an unfiled task reached GitHub")
	}
}

func TestReconcileRejectsUnusableInputAndPropagatesFailures(t *testing.T) {
	t.Parallel()
	fixture := newInitFixture(t)
	client := newReconcileClient()
	reconciler := fixture.reconciler(t, client)

	if _, err := reconciler.Reconcile(context.Background(), 0); !errors.Is(
		err, ErrReconciliation,
	) {
		t.Fatalf("zero project error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := reconciler.Reconcile(ctx, fixture.projectID); !errors.Is(
		err, context.Canceled,
	) {
		t.Fatalf("cancelled error = %v", err)
	}
	if _, err := NewReconciler(nil, client); err == nil {
		t.Error("missing store accepted")
	}
	if _, err := NewReconciler(fixture.store, nil); err == nil {
		t.Error("missing client accepted")
	}

	fixture.reconcilableTask(t, "Filed", 47, domain.TaskDeveloping, "")
	failure := errors.New("github is unavailable")
	failing := newReconcileClient()
	failing.getErr = failure
	if _, err := fixture.reconciler(t, failing).Reconcile(
		context.Background(), fixture.projectID,
	); !errors.Is(err, ErrReconciliation) {
		t.Fatalf("client failure = %v", err)
	}
}

func TestTaskStatusLabelCoversTheDeliveryLane(t *testing.T) {
	t.Parallel()
	published := map[domain.TaskStatus]string{
		domain.TaskProposed:     "madar:queued",
		domain.TaskQueued:       "madar:queued",
		domain.TaskSelected:     "madar:selected",
		domain.TaskPlanning:     "madar:planning",
		domain.TaskWaitingInput: "madar:waiting-input",
		domain.TaskDeveloping:   "madar:developing",
		domain.TaskReviewing:    "madar:reviewing",
		domain.TaskFixing:       "madar:fixing",
		domain.TaskVerifying:    "madar:verifying",
		domain.TaskWaitingCI:    "madar:waiting-ci",
		domain.TaskBlocked:      "madar:blocked",
		domain.TaskCompleted:    "madar:done",
	}
	for status, want := range published {
		got, ok := workflow.TaskStatusLabel(status)
		if !ok || got != want {
			t.Errorf("TaskStatusLabel(%q) = %q, %v", status, got, ok)
		}
	}
	// Work outside the lane carries no status label.
	for _, status := range []domain.TaskStatus{domain.TaskCancelled, domain.TaskDeferred} {
		if label, ok := workflow.TaskStatusLabel(status); ok {
			t.Errorf("TaskStatusLabel(%q) = %q, want none", status, label)
		}
	}
}

func (fixture *initFixture) reconciler(
	t *testing.T,
	client ReconcileClient,
) *Reconciler {
	t.Helper()
	reconciler, err := NewReconciler(fixture.store, client)
	if err != nil {
		t.Fatal(err)
	}
	return reconciler
}

func (fixture *initFixture) reconcilableTask(
	t *testing.T,
	title string,
	issueNumber int,
	status domain.TaskStatus,
	branch string,
) *domain.Task {
	t.Helper()
	task := domain.NewTask(fixture.projectID, title, title+" goal")
	task.Status = status
	task.IssueNumber = issueNumber
	task.BranchName = branch
	created, err := fixture.store.CreateProjectTask(task)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func hasDrift(drift []string, fragment string) bool {
	for _, entry := range drift {
		if strings.Contains(entry, fragment) {
			return true
		}
	}
	return false
}

type reconcileClient struct {
	issues      map[int]*githubclient.Issue
	pulls       map[string][]*githubclient.PullRequest
	labelWrites int
	closes      int
	lastLabels  []string
	getErr      error
}

func newReconcileClient() *reconcileClient {
	return &reconcileClient{
		issues: map[int]*githubclient.Issue{},
		pulls:  map[string][]*githubclient.PullRequest{},
	}
}

func (fake *reconcileClient) GetIssue(
	_ context.Context,
	_, _ string,
	number int,
) (*githubclient.Issue, error) {
	if fake.getErr != nil {
		return nil, fake.getErr
	}
	return fake.issues[number], nil
}

func (fake *reconcileClient) ReplaceLabels(
	_ context.Context,
	_, _ string,
	number int,
	labels []string,
) error {
	fake.labelWrites++
	fake.lastLabels = labels
	if issue, ok := fake.issues[number]; ok {
		issue.Labels = labels
	}
	return nil
}

func (fake *reconcileClient) CloseIssue(
	_ context.Context,
	_, _ string,
	number int,
) error {
	fake.closes++
	if issue, ok := fake.issues[number]; ok {
		issue.State = "closed"
	}
	return nil
}

func (fake *reconcileClient) ListPullRequestsForBranch(
	_ context.Context,
	_, _, branch string,
) ([]*githubclient.PullRequest, error) {
	if fake.getErr != nil {
		return nil, fake.getErr
	}
	return fake.pulls[branch], nil
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
