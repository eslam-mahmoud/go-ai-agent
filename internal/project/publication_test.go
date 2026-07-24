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
	"github.com/eslam-mahmoud/go-ai-agent/internal/projectfiles"
	"github.com/eslam-mahmoud/go-ai-agent/internal/projectissue"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
)

func TestPublisherCreatesParentIssueAndProjectFiles(t *testing.T) {
	t.Parallel()
	projectStore, projectRecord, workspaceRoot := publicationFixture(t)
	review := createPublicationReview(t, projectStore, projectRecord.ID)
	client := &fakeIssueClient{nextNumber: 501}
	publisher := newTestPublisher(t, projectStore, client, workspaceRoot)

	result, err := publisher.PublishManagerReview(
		context.Background(),
		projectRecord.ID,
		review.ID,
	)
	if err != nil {
		t.Fatalf("PublishManagerReview: %v", err)
	}
	if !result.FilesChanged ||
		!result.IssueCreated ||
		result.IssueUnchanged ||
		!result.Recorded ||
		result.ParentIssueNumber != 501 {
		t.Fatalf("result = %#v", result)
	}
	if client.creates != 1 || client.updates != 0 {
		t.Fatalf("client calls = %d creates, %d updates", client.creates, client.updates)
	}
	if !strings.Contains(client.lastBody, projectissue.StartMarker) {
		t.Fatal("created body has no managed section")
	}

	stored, err := projectStore.GetProjectByID(projectRecord.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ParentIssueNumber != 501 {
		t.Fatalf("persisted parent issue = %d", stored.ParentIssueNumber)
	}
	projectPath := filepath.Join(
		workspaceRoot, "owner", "repo",
		projectfiles.DirectoryName, projectfiles.ProjectFileName,
	)
	planPath := filepath.Join(
		workspaceRoot, "owner", "repo",
		projectfiles.DirectoryName, projectfiles.PlanFileName,
	)
	for _, path := range []string{projectPath, planPath} {
		if info, err := os.Stat(path); err != nil || info.Size() == 0 {
			t.Fatalf("project file %s: err=%v", path, err)
		}
	}

	events, err := projectStore.ListWorkflowEvents(projectRecord.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != domain.WorkflowProjectPublished {
		t.Fatalf("events = %#v", events)
	}
	var evidence struct {
		ManagerReviewID   int64 `json:"manager_review_id"`
		ParentIssueNumber int   `json:"parent_issue_number"`
		IssueCreated      bool  `json:"issue_created"`
		FilesChanged      bool  `json:"files_changed"`
	}
	if err := json.Unmarshal(events[0].Data, &evidence); err != nil {
		t.Fatal(err)
	}
	if evidence.ManagerReviewID != review.ID ||
		evidence.ParentIssueNumber != 501 ||
		!evidence.IssueCreated ||
		!evidence.FilesChanged {
		t.Fatalf("evidence = %#v", evidence)
	}
}

func TestPublisherRepublishesWithoutTouchingEitherSurface(t *testing.T) {
	t.Parallel()
	projectStore, projectRecord, workspaceRoot := publicationFixture(t)
	review := createPublicationReview(t, projectStore, projectRecord.ID)
	client := &fakeIssueClient{nextNumber: 77}
	publisher := newTestPublisher(t, projectStore, client, workspaceRoot)

	if _, err := publisher.PublishManagerReview(
		context.Background(), projectRecord.ID, review.ID,
	); err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(
		workspaceRoot, "owner", "repo",
		projectfiles.DirectoryName, projectfiles.PlanFileName,
	)
	first, err := os.Stat(planPath)
	if err != nil {
		t.Fatal(err)
	}

	result, err := publisher.PublishManagerReview(
		context.Background(), projectRecord.ID, review.ID,
	)
	if err != nil {
		t.Fatalf("republish: %v", err)
	}
	if result.FilesChanged || result.IssueCreated || !result.IssueUnchanged || result.Recorded {
		t.Fatalf("republished result = %#v", result)
	}
	if client.updates != 0 {
		t.Fatalf("republish issued %d body updates", client.updates)
	}
	second, err := os.Stat(planPath)
	if err != nil {
		t.Fatal(err)
	}
	if !second.ModTime().Equal(first.ModTime()) {
		t.Fatal("republish rewrote an unchanged project file")
	}
	events, _ := projectStore.ListWorkflowEvents(projectRecord.ID, 0, 100)
	if len(events) != 1 {
		t.Fatalf("republish emitted %d events", len(events))
	}
}

func TestPublisherUpdatesOnlyTheManagedSection(t *testing.T) {
	t.Parallel()
	projectStore, projectRecord, workspaceRoot := publicationFixture(t)
	projectRecord.ParentIssueNumber = 42
	if _, err := projectStore.UpdateProject(projectRecord); err != nil {
		t.Fatal(err)
	}
	review := createPublicationReview(t, projectStore, projectRecord.ID)
	human := "Written by a human.\n\nPlease keep this text.\n"
	client := &fakeIssueClient{
		issues: map[int]*githubclient.Issue{
			42: {Number: 42, Title: "[Madar] Madar", Body: human},
		},
	}
	publisher := newTestPublisher(t, projectStore, client, workspaceRoot)

	result, err := publisher.PublishManagerReview(
		context.Background(), projectRecord.ID, review.ID,
	)
	if err != nil {
		t.Fatalf("PublishManagerReview: %v", err)
	}
	if result.IssueCreated || result.IssueUnchanged || result.ParentIssueNumber != 42 {
		t.Fatalf("result = %#v", result)
	}
	if client.creates != 0 || client.updates != 1 {
		t.Fatalf("client calls = %d creates, %d updates", client.creates, client.updates)
	}
	if !strings.HasPrefix(client.lastBody, human) ||
		!strings.Contains(client.lastBody, projectissue.StartMarker) {
		t.Fatalf("updated body = %q", client.lastBody)
	}
}

func TestPublisherKeepsProjectFilesWhenParentIssueFails(t *testing.T) {
	t.Parallel()
	projectStore, projectRecord, workspaceRoot := publicationFixture(t)
	review := createPublicationReview(t, projectStore, projectRecord.ID)
	client := &fakeIssueClient{createErr: errors.New("github is unavailable")}
	publisher := newTestPublisher(t, projectStore, client, workspaceRoot)

	if _, err := publisher.PublishManagerReview(
		context.Background(), projectRecord.ID, review.ID,
	); !errors.Is(err, ErrParentIssueSync) {
		t.Fatalf("PublishManagerReview error = %v", err)
	}
	projectPath := filepath.Join(
		workspaceRoot, "owner", "repo",
		projectfiles.DirectoryName, projectfiles.ProjectFileName,
	)
	if _, err := os.Stat(projectPath); err != nil {
		t.Fatalf("project files were rolled back: %v", err)
	}
	events, _ := projectStore.ListWorkflowEvents(projectRecord.ID, 0, 100)
	if len(events) != 0 {
		t.Fatalf("failed publication recorded %#v", events)
	}
	stored, _ := projectStore.GetProjectByID(projectRecord.ID)
	if stored.ParentIssueNumber != 0 {
		t.Fatalf("parent issue number = %d", stored.ParentIssueNumber)
	}

	// The caller may retry once GitHub recovers.
	client.createErr = nil
	client.nextNumber = 99
	result, err := publisher.PublishManagerReview(
		context.Background(), projectRecord.ID, review.ID,
	)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	// The retry converges the files onto the newly created issue number.
	if !result.IssueCreated || !result.FilesChanged || !result.Recorded {
		t.Fatalf("retry result = %#v", result)
	}
	stored, _ = projectStore.GetProjectByID(projectRecord.ID)
	if stored.ParentIssueNumber != 99 {
		t.Fatalf("retry parent issue number = %d", stored.ParentIssueNumber)
	}
}

func TestPublisherRejectsStaleReviewAndBadInputBeforeWriting(t *testing.T) {
	t.Parallel()
	t.Run("stale review", func(t *testing.T) {
		t.Parallel()
		projectStore, projectRecord, workspaceRoot := publicationFixture(t)
		stale := createPublicationReview(t, projectStore, projectRecord.ID)
		createPublicationReview(t, projectStore, projectRecord.ID)
		client := &fakeIssueClient{nextNumber: 5}
		publisher := newTestPublisher(t, projectStore, client, workspaceRoot)
		if _, err := publisher.PublishManagerReview(
			context.Background(), projectRecord.ID, stale.ID,
		); !errors.Is(err, ErrStaleManagerReview) {
			t.Fatalf("error = %v", err)
		}
		assertNoProjectFiles(t, workspaceRoot)
		if client.creates != 0 {
			t.Fatal("stale review reached GitHub")
		}
	})

	t.Run("invalid identifiers", func(t *testing.T) {
		t.Parallel()
		projectStore, projectRecord, workspaceRoot := publicationFixture(t)
		review := createPublicationReview(t, projectStore, projectRecord.ID)
		client := &fakeIssueClient{nextNumber: 5}
		publisher := newTestPublisher(t, projectStore, client, workspaceRoot)
		for _, ids := range [][2]int64{{0, review.ID}, {projectRecord.ID, 0}} {
			if _, err := publisher.PublishManagerReview(
				context.Background(), ids[0], ids[1],
			); !errors.Is(err, ErrInvalidPublication) {
				t.Fatalf("ids %v error = %v", ids, err)
			}
		}
		assertNoProjectFiles(t, workspaceRoot)
	})

	t.Run("cancelled context", func(t *testing.T) {
		t.Parallel()
		projectStore, projectRecord, workspaceRoot := publicationFixture(t)
		review := createPublicationReview(t, projectStore, projectRecord.ID)
		client := &fakeIssueClient{nextNumber: 5}
		publisher := newTestPublisher(t, projectStore, client, workspaceRoot)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := publisher.PublishManagerReview(
			ctx, projectRecord.ID, review.ID,
		); !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v", err)
		}
		assertNoProjectFiles(t, workspaceRoot)
	})
}

func TestNewPublisherRequiresDependencies(t *testing.T) {
	t.Parallel()
	projectStore, _, workspaceRoot := publicationFixture(t)
	client := &fakeIssueClient{}
	if _, err := NewPublisher(nil, client, PublisherOptions{WorkspaceRoot: workspaceRoot}); err == nil {
		t.Fatal("missing store accepted")
	}
	if _, err := NewPublisher(projectStore, nil, PublisherOptions{WorkspaceRoot: workspaceRoot}); err == nil {
		t.Fatal("missing client accepted")
	}
	if _, err := NewPublisher(projectStore, client, PublisherOptions{WorkspaceRoot: "  "}); err == nil {
		t.Fatal("missing workspace root accepted")
	}
}

func TestSplitRepositoryRejectsMalformedIdentities(t *testing.T) {
	t.Parallel()
	for _, repo := range []string{"", "owner", "owner/", "/repo", "owner/repo/extra", "  /  "} {
		if _, _, err := splitRepository(repo); !errors.Is(err, ErrInvalidPublication) {
			t.Errorf("splitRepository(%q) error = %v", repo, err)
		}
	}
	owner, name, err := splitRepository(" owner/repo ")
	if err != nil || owner != "owner" || name != "repo" {
		t.Fatalf("splitRepository = %q, %q, %v", owner, name, err)
	}
}

type fakeIssueClient struct {
	issues     map[int]*githubclient.Issue
	nextNumber int
	creates    int
	updates    int
	lastBody   string
	createErr  error
	updateErr  error
	getErr     error
}

func (fake *fakeIssueClient) GetIssue(
	_ context.Context,
	_, _ string,
	number int,
) (*githubclient.Issue, error) {
	if fake.getErr != nil {
		return nil, fake.getErr
	}
	issue, ok := fake.issues[number]
	if !ok {
		return nil, errors.New("issue not found")
	}
	copied := *issue
	return &copied, nil
}

func (fake *fakeIssueClient) CreateIssue(
	_ context.Context,
	_, _, title, body string,
	_ []string,
) (*githubclient.Issue, error) {
	if fake.createErr != nil {
		return nil, fake.createErr
	}
	fake.creates++
	fake.lastBody = body
	issue := &githubclient.Issue{Number: fake.nextNumber, Title: title, Body: body}
	if fake.issues == nil {
		fake.issues = make(map[int]*githubclient.Issue)
	}
	fake.issues[issue.Number] = issue
	return issue, nil
}

func (fake *fakeIssueClient) UpdateIssueBody(
	_ context.Context,
	_, _ string,
	number int,
	body string,
) (*githubclient.Issue, error) {
	if fake.updateErr != nil {
		return nil, fake.updateErr
	}
	fake.updates++
	fake.lastBody = body
	issue, ok := fake.issues[number]
	if !ok {
		return nil, errors.New("issue not found")
	}
	issue.Body = body
	copied := *issue
	return &copied, nil
}

func publicationFixture(t *testing.T) (*store.Store, *domain.Project, string) {
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
	for _, title := range []string{"First", "Second"} {
		task := domain.NewTask(projectRecord.ID, title, title+" goal")
		task.Status = domain.TaskQueued
		if _, err := projectStore.CreateProjectTask(task); err != nil {
			t.Fatal(err)
		}
	}
	workspaceRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspaceRoot, "owner", "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	return projectStore, projectRecord, workspaceRoot
}

func newTestPublisher(
	t *testing.T,
	projectStore *store.Store,
	client projectissue.Client,
	workspaceRoot string,
) *Publisher {
	t.Helper()
	publisher, err := NewPublisher(
		projectStore,
		client,
		PublisherOptions{WorkspaceRoot: workspaceRoot},
	)
	if err != nil {
		t.Fatal(err)
	}
	return publisher
}

func createPublicationReview(
	t *testing.T,
	projectStore *store.Store,
	projectID int64,
) *domain.ManagerReview {
	t.Helper()
	review := domain.NewManagerReview(projectID)
	review.ProgressEstimate = 40
	review.ReleaseReadiness = "not-ready"
	review.OwnerUpdate = "Project remains on track."
	created, err := projectStore.CreateManagerReview(review)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func assertNoProjectFiles(t *testing.T, workspaceRoot string) {
	t.Helper()
	path := filepath.Join(workspaceRoot, "owner", "repo", projectfiles.DirectoryName)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("project files were written: err=%v", err)
	}
}
