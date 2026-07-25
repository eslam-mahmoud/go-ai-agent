package project

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/eslam-mahmoud/go-ai-agent/internal/architecturedocs"
	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
)

// TestInitializeBootstrapsAProjectFromRepositoryAndGoal is the end-to-end
// check the Milestone 7 audit was missing: every component existed and passed
// in isolation while nothing could bootstrap a project.
func TestInitializeBootstrapsAProjectFromRepositoryAndGoal(t *testing.T) {
	t.Parallel()
	fixture := newBootstrapFixture(t)
	risk := fixture.architectureRisk(t, "Cross-cutting cache change")
	runner := &fakeArchitectRunner{output: bootstrapProposal()}
	initializer, client := fixture.initializer(t, runner)

	result, err := initializer.Initialize(context.Background(), fixture.projectID)
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if result.AlreadyOnPlan {
		t.Fatal("a fresh project reported it was already on plan")
	}

	// The repository was inspected.
	if result.Scan == nil || len(result.Scan.Ecosystems) != 1 ||
		result.Scan.Ecosystems[0] != "go" {
		t.Fatalf("scan = %#v", result.Scan)
	}

	// The architect ran over the risk that raised the obligation.
	if result.Architecture == nil || !result.Architecture.Required {
		t.Fatalf("architecture = %#v", result.Architecture)
	}
	if runner.calls != 1 || len(runner.lastIDs) != 1 || runner.lastIDs[0] != risk.ID {
		t.Fatalf("runner calls=%d ids=%v", runner.calls, runner.lastIDs)
	}

	// The architecture reached the filesystem.
	if result.Documents == nil || len(result.Documents.Written) == 0 {
		t.Fatalf("documents = %#v", result.Documents)
	}
	workspace := filepath.Join(fixture.workspaceRoot, "owner", "repo")
	for _, relative := range []string{
		architecturedocs.AgentsFile,
		architecturedocs.OverviewFile,
		architecturedocs.ComponentsFile,
	} {
		if _, err := os.Stat(filepath.Join(workspace, filepath.FromSlash(relative))); err != nil {
			t.Fatalf("%s was not written: %v", relative, err)
		}
	}

	// The architecture version now counts the change.
	projectRecord, err := fixture.store.GetProjectByID(fixture.projectID)
	if err != nil {
		t.Fatal(err)
	}
	if projectRecord.ArchitectureVersion != 1 {
		t.Fatalf("architecture version = %d, want 1", projectRecord.ArchitectureVersion)
	}

	// The backlog exists, ordered, with an issue per task.
	if result.Backlog == nil || len(result.Backlog.Tasks) != 2 {
		t.Fatalf("backlog = %#v", result.Backlog)
	}
	tasks, err := fixture.store.ListProjectTasks(fixture.projectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 {
		t.Fatalf("backlog has %d tasks", len(tasks))
	}
	for index, task := range tasks {
		if task.Sequence != index+1 || task.IssueNumber == 0 {
			t.Fatalf("task %d = %#v", index, task)
		}
	}
	if client.creates != 2 {
		t.Fatalf("client filed %d issues", client.creates)
	}
}

func TestInitializeIsIdempotentOnASettledProject(t *testing.T) {
	t.Parallel()
	fixture := newBootstrapFixture(t)
	fixture.architectureRisk(t, "Cross-cutting cache change")
	runner := &fakeArchitectRunner{output: bootstrapProposal()}
	initializer, client := fixture.initializer(t, runner)

	if _, err := initializer.Initialize(context.Background(), fixture.projectID); err != nil {
		t.Fatal(err)
	}
	// Resolve the risk so nothing is owed, then re-initialize.
	fixture.resolveRisk(t)
	second, err := initializer.Initialize(context.Background(), fixture.projectID)
	if err != nil {
		t.Fatalf("re-initialize: %v", err)
	}
	if !second.AlreadyOnPlan {
		t.Fatalf("settled project reported work: %#v", second)
	}
	if second.Architecture.Required || runner.calls != 1 {
		t.Fatalf("architect ran again: calls=%d", runner.calls)
	}
	if client.creates != 2 {
		t.Fatalf("client filed %d issues in total", client.creates)
	}
	projectRecord, _ := fixture.store.GetProjectByID(fixture.projectID)
	if projectRecord.ArchitectureVersion != 1 {
		t.Fatalf("architecture version drifted to %d", projectRecord.ArchitectureVersion)
	}
}

func TestInitializeRunsWithoutGitHubStages(t *testing.T) {
	t.Parallel()
	fixture := newBootstrapFixture(t)
	fixture.architectureRisk(t, "Cross-cutting cache change")
	writer, err := NewWorkspaceArchitectureWriter(fixture.store, fixture.workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	architecture, err := NewArchitectureControllerWithDocuments(
		fixture.store, &fakeArchitectRunner{output: bootstrapProposal()}, writer,
	)
	if err != nil {
		t.Fatal(err)
	}
	initializer, err := NewInitializer(
		fixture.store,
		architecture,
		WorkspaceRootResolver{Root: fixture.workspaceRoot},
		InitializerOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := initializer.Initialize(context.Background(), fixture.projectID)
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	// Documents are still written; only the GitHub stages are absent.
	if result.Documents == nil || len(result.Documents.Written) == 0 {
		t.Fatalf("documents = %#v", result.Documents)
	}
	if result.Backlog != nil || result.Publication != nil {
		t.Fatalf("GitHub stages ran: %#v %#v", result.Backlog, result.Publication)
	}
}

func TestInitializeRejectsUnusableInput(t *testing.T) {
	t.Parallel()
	fixture := newBootstrapFixture(t)
	initializer, _ := fixture.initializer(t, &fakeArchitectRunner{output: bootstrapProposal()})
	if _, err := initializer.Initialize(context.Background(), 0); !errors.Is(
		err, ErrInitialization,
	) {
		t.Fatalf("zero project error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := initializer.Initialize(ctx, fixture.projectID); !errors.Is(
		err, context.Canceled,
	) {
		t.Fatalf("cancelled error = %v", err)
	}

	architecture, _ := NewArchitectureController(fixture.store)
	resolver := WorkspaceRootResolver{Root: fixture.workspaceRoot}
	if _, err := NewInitializer(nil, architecture, resolver, InitializerOptions{}); err == nil {
		t.Error("missing store accepted")
	}
	if _, err := NewInitializer(fixture.store, nil, resolver, InitializerOptions{}); err == nil {
		t.Error("missing architecture controller accepted")
	}
	if _, err := NewInitializer(
		fixture.store, architecture, nil, InitializerOptions{},
	); err == nil {
		t.Error("missing workspace resolver accepted")
	}
}

func TestWorkspaceArchitectureWriterVersionsOnlyRealChanges(t *testing.T) {
	t.Parallel()
	fixture := newBootstrapFixture(t)
	writer, err := NewWorkspaceArchitectureWriter(fixture.store, fixture.workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteArchitectureDocuments(
		fixture.projectID, bootstrapProposal(),
	); err != nil {
		t.Fatalf("first write: %v", err)
	}
	first, _ := fixture.store.GetProjectByID(fixture.projectID)
	if first.ArchitectureVersion != 1 {
		t.Fatalf("architecture version = %d, want 1", first.ArchitectureVersion)
	}

	// Re-applying the same proposal writes nothing, so the version holds.
	result, err := writer.WriteArchitectureDocuments(fixture.projectID, bootstrapProposal())
	if err != nil {
		t.Fatalf("second write: %v", err)
	}
	if len(result.Written) != 0 {
		t.Fatalf("second write changed %v", result.Written)
	}
	second, _ := fixture.store.GetProjectByID(fixture.projectID)
	if second.ArchitectureVersion != 1 {
		t.Fatalf("architecture version drifted to %d", second.ArchitectureVersion)
	}

	if _, err := writer.WriteArchitectureDocuments(0, bootstrapProposal()); !errors.Is(
		err, ErrArchitectureDocuments,
	) {
		t.Fatalf("zero project error = %v", err)
	}
	if _, err := writer.WriteArchitectureDocuments(
		fixture.projectID, json.RawMessage(`{"status":"needs_input"}`),
	); !errors.Is(err, ErrArchitectureDocuments) {
		t.Fatalf("non-completed proposal error = %v", err)
	}
	if _, err := NewWorkspaceArchitectureWriter(nil, fixture.workspaceRoot); err == nil {
		t.Error("missing store accepted")
	}
	if _, err := NewWorkspaceArchitectureWriter(fixture.store, "  "); err == nil {
		t.Error("missing workspace root accepted")
	}
}

type bootstrapFixture struct {
	store         *store.Store
	projectID     int64
	workspaceRoot string
}

func newBootstrapFixture(t *testing.T) *bootstrapFixture {
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
	workspaceRoot := t.TempDir()
	workspace := filepath.Join(workspaceRoot, "owner", "repo")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(workspace, "go.mod"), []byte("module example.com/repo"), 0o644,
	); err != nil {
		t.Fatal(err)
	}
	return &bootstrapFixture{
		store:         projectStore,
		projectID:     projectRecord.ID,
		workspaceRoot: workspaceRoot,
	}
}

func (fixture *bootstrapFixture) initializer(
	t *testing.T,
	runner ArchitectRunner,
) (*Initializer, *fakeDiscoveryIssueClient) {
	t.Helper()
	writer, err := NewWorkspaceArchitectureWriter(fixture.store, fixture.workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	architecture, err := NewArchitectureControllerWithDocuments(
		fixture.store, runner, writer,
	)
	if err != nil {
		t.Fatal(err)
	}
	client := &fakeDiscoveryIssueClient{nextNumber: 800}
	backlog, err := NewInitialBacklogController(fixture.store, client)
	if err != nil {
		t.Fatal(err)
	}
	initializer, err := NewInitializer(
		fixture.store,
		architecture,
		WorkspaceRootResolver{Root: fixture.workspaceRoot},
		InitializerOptions{Backlog: backlog},
	)
	if err != nil {
		t.Fatal(err)
	}
	return initializer, client
}

func (fixture *bootstrapFixture) architectureRisk(
	t *testing.T,
	title string,
) *domain.Discovery {
	t.Helper()
	discovery := domain.NewDiscovery(
		fixture.projectID, 0, 0, title,
		domain.DiscoveryArchitecture, domain.SeverityHigh,
	)
	discovery.ArchitectureRisk = true
	batch, err := fixture.store.CreateDiscoveries(
		fixture.projectID, []*domain.Discovery{discovery}, "",
	)
	if err != nil {
		t.Fatal(err)
	}
	return batch.Created[0]
}

func (fixture *bootstrapFixture) resolveRisk(t *testing.T) {
	t.Helper()
	pending, err := fixture.store.ListUnevaluatedDiscoveries(fixture.projectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) == 0 {
		return
	}
	review := domain.NewManagerReview(fixture.projectID)
	review.ReleaseReadiness = "not-ready"
	review.OwnerUpdate = "Risk resolved."
	created, err := fixture.store.CreateManagerReview(review)
	if err != nil {
		t.Fatal(err)
	}
	decisions := make([]store.DiscoveryDecisionRecord, 0, len(pending))
	for _, discovery := range pending {
		decisions = append(decisions, store.DiscoveryDecisionRecord{
			DiscoveryID: discovery.ID,
			Decision:    domain.DecisionRejectOutOfScope,
			Status:      domain.DiscoveryRejected,
			Reason:      "Handled by the architecture run",
		})
	}
	if _, err := fixture.store.ApplyDiscoveryDecisions(store.DiscoveryDecisionUpdate{
		ProjectID:       fixture.projectID,
		ManagerReviewID: created.ID,
		Decisions:       decisions,
	}); err != nil {
		t.Fatal(err)
	}
}

func bootstrapProposal() json.RawMessage {
	return json.RawMessage(`{
		"status": "completed",
		"architecture_summary": "One binary, one writer.",
		"components": [
			{"name":"store","responsibility":"Own durable state."},
			{"name":"workflow","responsibility":"Sequence delivery modes."}
		],
		"decisions": [
			{"title":"Keep SQLite single-writer","decision":"Serialize writes.","rationale":"Avoids contention."}
		],
		"dependencies": [],
		"risks": [],
		"recommended_tasks": [
			{"title":"Extract the engine interface","goal":"Define the provider boundary","reason":"Enables Codex"},
			{"title":"Split the store","goal":"Separate legacy and v2 tables","reason":"Reduces coupling"}
		],
		"addressed_discovery_ids": []
	}`)
}
