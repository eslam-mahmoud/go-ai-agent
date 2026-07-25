package project

import (
	"context"
	"errors"
	"testing"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	githubclient "github.com/eslam-mahmoud/go-ai-agent/internal/github"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
)

func TestDiscoverBindsBranchesToTheirPullRequests(t *testing.T) {
	t.Parallel()
	fixture := newInitFixture(t)
	withBranch := fixture.taskWithBranch(t, "Has a branch", "madar/issue-1")
	fixture.taskWithBranch(t, "No pull request yet", "madar/issue-2")
	fixture.taskWithoutBranch(t, "Never started")

	client := &fakePullRequestLister{
		byBranch: map[string][]*githubclient.PullRequest{
			"madar/issue-1": {{Number: 31, State: "open", HeadBranch: "madar/issue-1"}},
		},
	}
	discoverer := fixture.discoverer(t, client)

	result, err := discoverer.Discover(context.Background(), fixture.projectID)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(result.Discovered) != 1 || result.Discovered[0].ID != withBranch.ID ||
		result.Discovered[0].PRNumber != 31 {
		t.Fatalf("discovered = %#v", result.Discovered)
	}
	if len(result.Unmatched) != 1 || len(result.Ambiguous) != 0 {
		t.Fatalf("unmatched = %d, ambiguous = %d",
			len(result.Unmatched), len(result.Ambiguous))
	}
	// A task with no branch is not a candidate at all.
	if client.calls != 2 {
		t.Fatalf("client was called %d times", client.calls)
	}

	// Re-running reports the binding as known and asks GitHub only about the
	// branch that is still unresolved.
	client.calls = 0
	again, err := discoverer.Discover(context.Background(), fixture.projectID)
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if len(again.Discovered) != 0 || len(again.AlreadyKnown) != 1 {
		t.Fatalf("re-run = %#v", again)
	}
	if client.calls != 1 {
		t.Fatalf("re-run called GitHub %d times", client.calls)
	}
}

func TestDiscoverReportsAmbiguousBranchesInsteadOfGuessing(t *testing.T) {
	t.Parallel()
	fixture := newInitFixture(t)
	task := fixture.taskWithBranch(t, "Contested branch", "madar/issue-1")
	client := &fakePullRequestLister{
		byBranch: map[string][]*githubclient.PullRequest{
			"madar/issue-1": {
				{Number: 31, State: "open", HeadBranch: "madar/issue-1"},
				{Number: 32, State: "open", HeadBranch: "madar/issue-1"},
			},
		},
	}
	discoverer := fixture.discoverer(t, client)

	result, err := discoverer.Discover(context.Background(), fixture.projectID)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(result.Discovered) != 0 || len(result.Ambiguous) != 1 {
		t.Fatalf("result = %#v", result)
	}
	ambiguous := result.Ambiguous[0]
	if ambiguous.TaskID != task.ID ||
		ambiguous.Branch != "madar/issue-1" ||
		len(ambiguous.Numbers) != 2 {
		t.Fatalf("ambiguous = %#v", ambiguous)
	}
	// Nothing was bound, so recovery can escalate rather than inherit a guess.
	stored, _ := fixture.store.GetProjectTaskByID(task.ID)
	if stored.PRNumber != 0 {
		t.Fatalf("task recorded pull request %d", stored.PRNumber)
	}
}

func TestRecordProjectTaskPullRequestRefusesToRebind(t *testing.T) {
	t.Parallel()
	fixture := newInitFixture(t)
	task := fixture.taskWithBranch(t, "Has a branch", "madar/issue-1")

	if _, err := fixture.store.RecordProjectTaskPullRequest(
		fixture.projectID, task.ID, 31,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.RecordProjectTaskPullRequest(
		fixture.projectID, task.ID, 31,
	); err != nil {
		t.Fatalf("idempotent rebind: %v", err)
	}
	if _, err := fixture.store.RecordProjectTaskPullRequest(
		fixture.projectID, task.ID, 32,
	); !errors.Is(err, store.ErrProjectTaskIssueConflict) {
		t.Fatalf("rebind error = %v", err)
	}
	events, _ := fixture.store.ListWorkflowEvents(fixture.projectID, 0, 100)
	found := 0
	for _, event := range events {
		if event.Type == domain.WorkflowTaskPullRequestFound {
			found++
		}
	}
	if found != 1 {
		t.Fatalf("emitted %d discovery events", found)
	}
}

func TestDiscoverRejectsUnusableInput(t *testing.T) {
	t.Parallel()
	fixture := newInitFixture(t)
	discoverer := fixture.discoverer(t, &fakePullRequestLister{})
	if _, err := discoverer.Discover(context.Background(), 0); !errors.Is(
		err, ErrPullRequestDiscovery,
	) {
		t.Fatalf("zero project error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := discoverer.Discover(ctx, fixture.projectID); !errors.Is(
		err, context.Canceled,
	) {
		t.Fatalf("cancelled error = %v", err)
	}
	if _, err := NewPullRequestDiscoverer(nil, &fakePullRequestLister{}); err == nil {
		t.Error("missing store accepted")
	}
	if _, err := NewPullRequestDiscoverer(fixture.store, nil); err == nil {
		t.Error("missing client accepted")
	}

	failure := errors.New("github is unavailable")
	fixture.taskWithBranch(t, "Has a branch", "madar/issue-1")
	failing := fixture.discoverer(t, &fakePullRequestLister{err: failure})
	if _, err := failing.Discover(
		context.Background(), fixture.projectID,
	); !errors.Is(err, ErrPullRequestDiscovery) {
		t.Fatalf("client failure = %v", err)
	}
}

func (fixture *initFixture) discoverer(
	t *testing.T,
	client *fakePullRequestLister,
) *PullRequestDiscoverer {
	t.Helper()
	discoverer, err := NewPullRequestDiscoverer(fixture.store, client)
	if err != nil {
		t.Fatal(err)
	}
	return discoverer
}

func (fixture *initFixture) taskWithBranch(
	t *testing.T,
	title, branch string,
) *domain.Task {
	t.Helper()
	task := domain.NewTask(fixture.projectID, title, title+" goal")
	task.Status = domain.TaskQueued
	task.BranchName = branch
	created, err := fixture.store.CreateProjectTask(task)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func (fixture *initFixture) taskWithoutBranch(t *testing.T, title string) *domain.Task {
	t.Helper()
	task := domain.NewTask(fixture.projectID, title, title+" goal")
	task.Status = domain.TaskQueued
	created, err := fixture.store.CreateProjectTask(task)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

type fakePullRequestLister struct {
	byBranch map[string][]*githubclient.PullRequest
	calls    int
	err      error
}

func (fake *fakePullRequestLister) ListPullRequestsForBranch(
	_ context.Context,
	_, _, branch string,
) ([]*githubclient.PullRequest, error) {
	fake.calls++
	if fake.err != nil {
		return nil, fake.err
	}
	return fake.byBranch[branch], nil
}
