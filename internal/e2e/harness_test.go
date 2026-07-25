// Package e2e drives Madar's real components end to end with only the
// provider engine and GitHub faked.
//
// Every milestone audit in this project found components that passed their own
// unit tests while the capability did not exist end to end. These fixtures are
// the standing defence against that: they fail when a stage is skipped, not
// only when a stage is wrong.
package e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	githubclient "github.com/eslam-mahmoud/go-ai-agent/internal/github"
	"github.com/eslam-mahmoud/go-ai-agent/internal/project"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
	"github.com/eslam-mahmoud/go-ai-agent/internal/workflow"
)

// harness wires the real store, controllers, and workflow together.
type harness struct {
	t             *testing.T
	store         *store.Store
	projectID     int64
	controller    *project.Controller
	github        *fakeGitHub
	workspaceRoot string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	directory := t.TempDir()
	projectStore, err := store.Open(filepath.Join(directory, "madar.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { projectStore.Close() })

	projectRecord, err := projectStore.CreateProject(
		domain.NewProject("owner/repo", "Madar", "Ship v2", "Sequential delivery"),
	)
	if err != nil {
		t.Fatal(err)
	}
	controller, err := project.NewController(projectStore)
	if err != nil {
		t.Fatal(err)
	}
	workspaceRoot := filepath.Join(directory, "workspaces")
	if err := os.MkdirAll(
		filepath.Join(workspaceRoot, "owner", "repo"), 0o755,
	); err != nil {
		t.Fatal(err)
	}
	return &harness{
		t:             t,
		store:         projectStore,
		projectID:     projectRecord.ID,
		controller:    controller,
		github:        newFakeGitHub(),
		workspaceRoot: workspaceRoot,
	}
}

// queueTask appends a queued backlog task, the state a manager selects from.
func (h *harness) queueTask(title string, issueNumber int) *domain.Task {
	h.t.Helper()
	task := domain.NewTask(h.projectID, title, title+" goal")
	task.Status = domain.TaskQueued
	task.IssueNumber = issueNumber
	created, err := h.store.CreateProjectTask(task)
	if err != nil {
		h.t.Fatal(err)
	}
	return created
}

// selectTask drives a queued task into the delivery lane through the real
// selection controller, so the manager-review gate is genuinely exercised.
func (h *harness) selectTask(task *domain.Task, reason string) {
	h.t.Helper()
	review := h.recordReview(func(record *domain.ManagerReview) {
		record.NextTaskID = &task.ID
		record.NextTaskIssueNumber = task.IssueNumber
		record.NextTaskReason = reason
	})
	selector, err := project.NewSelectionController(h.store)
	if err != nil {
		h.t.Fatal(err)
	}
	result, err := selector.SelectNextTask(h.projectID, review.ID)
	if err != nil {
		h.t.Fatalf("selecting %q: %v", task.Title, err)
	}
	if !result.Applied || result.Task.Status != domain.TaskSelected {
		h.t.Fatalf("selection result = %#v", result)
	}
}

func (h *harness) recordReview(
	arrange func(*domain.ManagerReview),
) *domain.ManagerReview {
	h.t.Helper()
	review := domain.NewManagerReview(h.projectID)
	review.ReleaseReadiness = "not-ready"
	review.OwnerUpdate = "Project remains on track."
	if arrange != nil {
		arrange(review)
	}
	created, err := h.store.CreateManagerReview(review)
	if err != nil {
		h.t.Fatal(err)
	}
	return created
}

// runDelivery runs the real feature workflow with scripted mode outcomes.
func (h *harness) runDelivery(
	task *domain.Task,
	script *modeScript,
	options workflow.FeatureOptions,
) *workflow.FeatureResult {
	h.t.Helper()
	feature, err := workflow.NewFeatureWorkflow(h.controller, script, options)
	if err != nil {
		h.t.Fatal(err)
	}
	result, err := feature.Run(context.Background(), h.projectID, task.ID)
	if err != nil {
		h.t.Fatalf("delivery of %q: %v", task.Title, err)
	}
	return result
}

func (h *harness) task(id int64) *domain.Task {
	h.t.Helper()
	task, err := h.store.GetProjectTaskByID(id)
	if err != nil || task == nil {
		h.t.Fatalf("task %d = %#v, err = %v", id, task, err)
	}
	return task
}

func (h *harness) tasks() []*domain.Task {
	h.t.Helper()
	tasks, err := h.store.ListProjectTasks(h.projectID)
	if err != nil {
		h.t.Fatal(err)
	}
	return tasks
}

// eventTypes returns the recorded audit trail, which is what proves a stage
// actually ran rather than being skipped.
func (h *harness) eventTypes() []domain.WorkflowEventType {
	h.t.Helper()
	events, err := h.store.ListWorkflowEvents(h.projectID, 0, 1000)
	if err != nil {
		h.t.Fatal(err)
	}
	types := make([]domain.WorkflowEventType, 0, len(events))
	for _, event := range events {
		types = append(types, event.Type)
	}
	return types
}

func (h *harness) requireEvent(want domain.WorkflowEventType) {
	h.t.Helper()
	for _, recorded := range h.eventTypes() {
		if recorded == want {
			return
		}
	}
	h.t.Fatalf("audit trail is missing %q: %v", want, h.eventTypes())
}

// modeScript returns scripted outcomes per mode, and records the order modes
// ran in so a skipped stage is visible.
type modeScript struct {
	outcomes map[workflow.ModeName][]workflow.ModeOutcome
	ran      []workflow.ModeName
}

func newScript() *modeScript {
	return &modeScript{outcomes: map[workflow.ModeName][]workflow.ModeOutcome{}}
}

// on queues one outcome for a mode. Repeated calls queue successive runs, so
// a review/fix cycle can be scripted explicitly.
func (script *modeScript) on(
	mode workflow.ModeName,
	outcome workflow.ModeOutcome,
) *modeScript {
	script.outcomes[mode] = append(script.outcomes[mode], outcome)
	return script
}

func (script *modeScript) RunMode(
	_ context.Context,
	request workflow.ModeRequest,
) (workflow.ModeOutcome, error) {
	script.ran = append(script.ran, request.Mode)
	queued := script.outcomes[request.Mode]
	if len(queued) == 0 {
		return workflow.ModeOutcome{Status: workflow.ModeCompleted}, nil
	}
	next := queued[0]
	if len(queued) > 1 {
		script.outcomes[request.Mode] = queued[1:]
	}
	return next, nil
}

func completed() workflow.ModeOutcome {
	return workflow.ModeOutcome{Status: workflow.ModeCompleted}
}

// fakeGitHub is a deterministic stand-in with no network.
type fakeGitHub struct {
	issues      map[int]*githubclient.Issue
	pulls       map[string][]*githubclient.PullRequest
	nextIssue   int
	creates     int
	comments    int
	labelWrites int
	closes      int
	lastLabels  []string
}

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{
		issues:    map[int]*githubclient.Issue{},
		pulls:     map[string][]*githubclient.PullRequest{},
		nextIssue: 900,
	}
}

func (fake *fakeGitHub) GetIssue(
	_ context.Context, _, _ string, number int,
) (*githubclient.Issue, error) {
	return fake.issues[number], nil
}

func (fake *fakeGitHub) ListOpenIssues(
	_ context.Context, _, _ string,
) ([]*githubclient.Issue, error) {
	open := make([]*githubclient.Issue, 0, len(fake.issues))
	for _, issue := range fake.issues {
		if issue.State != "closed" {
			copied := *issue
			open = append(open, &copied)
		}
	}
	return open, nil
}

func (fake *fakeGitHub) CreateIssue(
	_ context.Context, _, _, title, body string, labels []string,
) (*githubclient.Issue, error) {
	fake.creates++
	issue := &githubclient.Issue{
		Number: fake.nextIssue, Title: title, Body: body,
		Labels: labels, State: "open",
	}
	fake.nextIssue++
	fake.issues[issue.Number] = issue
	return issue, nil
}

func (fake *fakeGitHub) PostComment(
	_ context.Context, _, _ string, _ int, body string,
) (*githubclient.Comment, error) {
	fake.comments++
	return &githubclient.Comment{ID: int64(fake.comments), Body: body}, nil
}

func (fake *fakeGitHub) GetComments(
	_ context.Context, _, _ string, _ int, _ *time.Time,
) ([]*githubclient.Comment, error) {
	return nil, nil
}

func (fake *fakeGitHub) EnsureLabels(
	_ context.Context, _, _ string, _ map[string]string,
) error {
	return nil
}

func (fake *fakeGitHub) ReplaceLabels(
	_ context.Context, _, _ string, number int, labels []string,
) error {
	fake.labelWrites++
	fake.lastLabels = labels
	if issue, ok := fake.issues[number]; ok {
		issue.Labels = labels
	}
	return nil
}

func (fake *fakeGitHub) CloseIssue(
	_ context.Context, _, _ string, number int,
) error {
	fake.closes++
	if issue, ok := fake.issues[number]; ok {
		issue.State = "closed"
	}
	return nil
}

func (fake *fakeGitHub) ListPullRequestsForBranch(
	_ context.Context, _, _, branch string,
) ([]*githubclient.PullRequest, error) {
	return fake.pulls[branch], nil
}

// scriptedManager returns fixed manager output, standing in for the engine.
type scriptedManager struct {
	output json.RawMessage
	calls  int
}

func (manager *scriptedManager) RunManagerReview(
	_ context.Context,
	_, _ int64,
) (json.RawMessage, error) {
	manager.calls++
	return manager.output, nil
}

// managerOutput builds schema-shaped manager output with overrides.
func managerOutput(overrides map[string]any) json.RawMessage {
	output := map[string]any{
		"status":                       "completed",
		"summary":                      "Reviewed the delivered task.",
		"question":                     nil,
		"discoveries":                  []any{},
		"risks":                        []any{},
		"recommended_next_action":      "Continue delivery.",
		"project_health":               "on-track",
		"progress_estimate":            50,
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
		panic(err)
	}
	return raw
}
