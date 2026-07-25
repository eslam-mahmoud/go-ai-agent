package project

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	githubclient "github.com/eslam-mahmoud/go-ai-agent/internal/github"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
)

func TestInitializeCreatesOrderedBacklogAndFilesIssues(t *testing.T) {
	t.Parallel()
	fixture := newInitFixture(t)
	client := &fakeDiscoveryIssueClient{nextNumber: 300}
	controller := fixture.controller(t, client)

	result, err := controller.Initialize(
		context.Background(), fixture.projectID, initialProposal(),
	)
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if result.AlreadyExisted {
		t.Fatal("empty project reported an existing backlog")
	}
	if len(result.Tasks) != 3 || len(result.FiledIssues) != 3 {
		t.Fatalf("tasks = %d, filed = %d", len(result.Tasks), len(result.FiledIssues))
	}
	// The architect's order is the backlog order.
	wantTitles := []string{"Extract the engine interface", "Split the store", "Document the flow"}
	for index, task := range result.Tasks {
		if task.Title != wantTitles[index] || task.Sequence != index+1 {
			t.Fatalf("task %d = %q at %d", index, task.Title, task.Sequence)
		}
		if task.Status != domain.TaskQueued || task.Source != "architect" {
			t.Fatalf("task %d = %#v", index, task)
		}
	}
	if !result.Tasks[1].BlocksRelease || result.Tasks[0].BlocksRelease {
		t.Fatal("release-blocking flag was not carried through")
	}
	for _, filed := range result.FiledIssues {
		if filed.IssueNumber == 0 {
			t.Fatalf("task %d has no issue", filed.ID)
		}
	}
	if client.creates != 3 {
		t.Fatalf("client created %d issues", client.creates)
	}
	if !strings.Contains(client.createdBody, "Backlog position:") {
		t.Fatalf("issue body = %q", client.createdBody)
	}

	// Re-running changes nothing.
	again, err := controller.Initialize(
		context.Background(), fixture.projectID, initialProposal(),
	)
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if !again.AlreadyExisted || len(again.FiledIssues) != 0 || client.creates != 3 {
		t.Fatalf("re-run = %#v, creates = %d", again, client.creates)
	}
}

func TestInitializeLeavesAnExistingBacklogAlone(t *testing.T) {
	t.Parallel()
	fixture := newInitFixture(t)
	existing := domain.NewTask(fixture.projectID, "Already planned", "Do the thing")
	existing.Status = domain.TaskQueued
	if _, err := fixture.store.CreateProjectTask(existing); err != nil {
		t.Fatal(err)
	}
	client := &fakeDiscoveryIssueClient{nextNumber: 400}
	controller := fixture.controller(t, client)

	result, err := controller.Initialize(
		context.Background(), fixture.projectID, initialProposal(),
	)
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if !result.AlreadyExisted || len(result.Tasks) != 1 {
		t.Fatalf("result = %#v", result)
	}
	// The existing task still gets an issue, since it had none.
	if len(result.FiledIssues) != 1 || result.FiledIssues[0].Title != "Already planned" {
		t.Fatalf("filed = %#v", result.FiledIssues)
	}
	tasks, _ := fixture.store.ListProjectTasks(fixture.projectID)
	if len(tasks) != 1 {
		t.Fatalf("backlog grew to %d tasks", len(tasks))
	}
}

func TestInitializeResumesAfterPartialIssueCreation(t *testing.T) {
	t.Parallel()
	fixture := newInitFixture(t)
	client := &fakeDiscoveryIssueClient{nextNumber: 500, failAfter: 2}
	controller := fixture.controller(t, client)

	if _, err := controller.Initialize(
		context.Background(), fixture.projectID, initialProposal(),
	); !errors.Is(err, ErrInitialBacklog) {
		t.Fatalf("error = %v", err)
	}
	tasks, _ := fixture.store.ListProjectTasks(fixture.projectID)
	filed := 0
	for _, task := range tasks {
		if task.IssueNumber != 0 {
			filed++
		}
	}
	if len(tasks) != 3 || filed != 2 {
		t.Fatalf("after failure: %d tasks, %d filed", len(tasks), filed)
	}

	// A later run files only what is missing and does not rebuild the backlog.
	client.failAfter = 0
	result, err := controller.Initialize(
		context.Background(), fixture.projectID, initialProposal(),
	)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !result.AlreadyExisted || len(result.FiledIssues) != 1 {
		t.Fatalf("resume result = %#v", result)
	}
	if client.creates != 3 {
		t.Fatalf("client created %d issues in total", client.creates)
	}
	tasks, _ = fixture.store.ListProjectTasks(fixture.projectID)
	for _, task := range tasks {
		if task.IssueNumber == 0 {
			t.Fatalf("task %d still has no issue", task.ID)
		}
	}
}

func TestInitializeReusesAMatchingOpenIssue(t *testing.T) {
	t.Parallel()
	fixture := newInitFixture(t)
	client := &fakeDiscoveryIssueClient{
		nextNumber: 600,
		open: []*githubclient.Issue{
			{Number: 21, Title: "  EXTRACT the engine interface!  "},
		},
	}
	controller := fixture.controller(t, client)
	result, err := controller.Initialize(
		context.Background(), fixture.projectID, initialProposal(),
	)
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if len(result.ReusedIssues) != 1 || result.ReusedIssues[0].IssueNumber != 21 {
		t.Fatalf("reused = %#v", result.ReusedIssues)
	}
	if client.creates != 2 {
		t.Fatalf("client created %d issues", client.creates)
	}
}

func TestInitializeRejectsUnusableProposals(t *testing.T) {
	t.Parallel()
	fixture := newInitFixture(t)
	controller := fixture.controller(t, &fakeDiscoveryIssueClient{nextNumber: 700})

	if _, err := controller.Initialize(
		context.Background(), 0, initialProposal(),
	); !errors.Is(err, ErrInitialBacklog) {
		t.Fatalf("zero project error = %v", err)
	}
	if _, err := controller.Initialize(
		context.Background(), fixture.projectID, json.RawMessage(`{`),
	); !errors.Is(err, ErrInitialBacklog) {
		t.Fatalf("malformed proposal error = %v", err)
	}
	untitled := json.RawMessage(`{"recommended_tasks":[{"title":"  ","goal":"g","reason":"r"}]}`)
	if _, err := controller.Initialize(
		context.Background(), fixture.projectID, untitled,
	); !errors.Is(err, ErrInitialBacklog) {
		t.Fatalf("untitled task error = %v", err)
	}
	// A proposal with no recommended work creates nothing.
	empty, err := controller.Initialize(
		context.Background(), fixture.projectID, json.RawMessage(`{"recommended_tasks":[]}`),
	)
	if err != nil {
		t.Fatalf("empty proposal: %v", err)
	}
	if len(empty.Tasks) != 0 {
		t.Fatalf("empty proposal created %d tasks", len(empty.Tasks))
	}
	tasks, _ := fixture.store.ListProjectTasks(fixture.projectID)
	if len(tasks) != 0 {
		t.Fatalf("rejected proposals left %d tasks", len(tasks))
	}

	if _, err := NewInitialBacklogController(nil, &fakeDiscoveryIssueClient{}); err == nil {
		t.Error("missing store accepted")
	}
	if _, err := NewInitialBacklogController(fixture.store, nil); err == nil {
		t.Error("missing client accepted")
	}
}

func TestRecordProjectTaskIssueRefusesToRebind(t *testing.T) {
	t.Parallel()
	fixture := newInitFixture(t)
	created, err := fixture.store.CreateInitialBacklog(fixture.projectID, []*domain.Task{
		func() *domain.Task {
			task := domain.NewTask(fixture.projectID, "Only", "Do it")
			task.Status = domain.TaskQueued
			return task
		}(),
	})
	if err != nil {
		t.Fatal(err)
	}
	task := created[0]
	if _, err := fixture.store.RecordProjectTaskIssue(
		fixture.projectID, task.ID, 11, false,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.RecordProjectTaskIssue(
		fixture.projectID, task.ID, 11, false,
	); err != nil {
		t.Fatalf("idempotent rebind: %v", err)
	}
	if _, err := fixture.store.RecordProjectTaskIssue(
		fixture.projectID, task.ID, 12, false,
	); !errors.Is(err, store.ErrProjectTaskIssueConflict) {
		t.Fatalf("rebind error = %v", err)
	}
	events, _ := fixture.store.ListWorkflowEvents(fixture.projectID, 0, 100)
	filed := 0
	for _, event := range events {
		if event.Type == domain.WorkflowTaskIssueFiled {
			filed++
		}
	}
	if filed != 1 {
		t.Fatalf("emitted %d filing events", filed)
	}
}

func TestCreateInitialBacklogRefusesASecondBacklog(t *testing.T) {
	t.Parallel()
	fixture := newInitFixture(t)
	first := []*domain.Task{
		func() *domain.Task {
			task := domain.NewTask(fixture.projectID, "First", "Do it")
			task.Status = domain.TaskQueued
			return task
		}(),
	}
	if _, err := fixture.store.CreateInitialBacklog(fixture.projectID, first); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.CreateInitialBacklog(fixture.projectID, first); !errors.Is(
		err, store.ErrProjectAlreadyInitialized,
	) {
		t.Fatalf("second backlog error = %v", err)
	}
	// An invalid batch writes nothing.
	fresh := newInitFixture(t)
	invalid := []*domain.Task{
		func() *domain.Task {
			task := domain.NewTask(fresh.projectID, "Valid", "Do it")
			task.Status = domain.TaskQueued
			return task
		}(),
		{ProjectID: fresh.projectID, Title: "", Goal: ""},
	}
	if _, err := fresh.store.CreateInitialBacklog(fresh.projectID, invalid); err == nil {
		t.Fatal("invalid batch accepted")
	}
	tasks, _ := fresh.store.ListProjectTasks(fresh.projectID)
	if len(tasks) != 0 {
		t.Fatalf("invalid batch wrote %d tasks", len(tasks))
	}
}

type initFixture struct {
	store     *store.Store
	projectID int64
}

func newInitFixture(t *testing.T) *initFixture {
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
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "owner", "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &initFixture{store: projectStore, projectID: projectRecord.ID}
}

func (fixture *initFixture) controller(
	t *testing.T,
	client DiscoveryIssueClient,
) *InitialBacklogController {
	t.Helper()
	controller, err := NewInitialBacklogController(fixture.store, client)
	if err != nil {
		t.Fatal(err)
	}
	return controller
}

func initialProposal() json.RawMessage {
	return json.RawMessage(`{
		"status": "completed",
		"recommended_tasks": [
			{"title":"Extract the engine interface","goal":"Define the provider boundary","reason":"Enables Codex"},
			{"title":"Split the store","goal":"Separate legacy and v2 tables","reason":"Reduces coupling","blocks_release":true},
			{"title":"Document the flow","goal":"Write the data-flow document","reason":"Onboarding"}
		]
	}`)
}
