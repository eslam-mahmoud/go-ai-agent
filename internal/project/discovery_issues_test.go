package project

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	githubclient "github.com/eslam-mahmoud/go-ai-agent/internal/github"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
)

func TestDiscoveryIssuePublisherCreatesIssuesForAcceptedWork(t *testing.T) {
	t.Parallel()
	fixture := newReviewFixture(t)
	accepted := fixture.decidedDiscovery(t,
		"Retry budget is unbounded", domain.DecisionCreateReleaseBlocker, domain.SeverityCritical)
	client := &fakeDiscoveryIssueClient{nextNumber: 900}
	publisher := fixture.issuePublisher(t, client)

	result, err := publisher.PublishAcceptedDiscoveries(context.Background(), fixture.projectID)
	if err != nil {
		t.Fatalf("PublishAcceptedDiscoveries: %v", err)
	}
	if len(result.Created) != 1 || len(result.Reused) != 0 {
		t.Fatalf("result = %d created, %d reused", len(result.Created), len(result.Reused))
	}
	if result.Created[0].CreatedIssueNumber != 900 {
		t.Fatalf("recorded issue = %d", result.Created[0].CreatedIssueNumber)
	}
	if client.createdTitle != accepted.Title {
		t.Fatalf("issue title = %q", client.createdTitle)
	}
	for _, fragment := range []string{
		"**Category:** bug",
		"**Severity:** critical",
		"**Times observed:** 1",
		"**Decision:** create-release-blocker",
		accepted.ExternalID,
	} {
		if !strings.Contains(client.createdBody, fragment) {
			t.Fatalf("issue body missing %q:\n%s", fragment, client.createdBody)
		}
	}
	wantLabels := map[string]bool{
		"type:discovery":    true,
		"priority:critical": true,
		"release:blocker":   true,
	}
	if len(client.createdLabels) != len(wantLabels) {
		t.Fatalf("labels = %v", client.createdLabels)
	}
	for _, label := range client.createdLabels {
		if !wantLabels[label] {
			t.Fatalf("unexpected label %q in %v", label, client.createdLabels)
		}
	}
	if len(client.ensuredLabels) == 0 {
		t.Fatal("labels were not ensured before use")
	}

	// Re-running must not file the issue again.
	again, err := publisher.PublishAcceptedDiscoveries(context.Background(), fixture.projectID)
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if len(again.Created) != 0 || client.creates != 1 {
		t.Fatalf("re-run created %d issues (client calls %d)", len(again.Created), client.creates)
	}
}

func TestDiscoveryIssuePublisherReusesMatchingOpenIssue(t *testing.T) {
	t.Parallel()
	fixture := newReviewFixture(t)
	fixture.decidedDiscovery(t,
		"Retry budget is unbounded", domain.DecisionAddToBacklog, domain.SeverityMedium)
	client := &fakeDiscoveryIssueClient{
		nextNumber: 900,
		open: []*githubclient.Issue{
			{Number: 41, Title: "  RETRY budget, is unbounded!  "},
		},
	}
	publisher := fixture.issuePublisher(t, client)

	result, err := publisher.PublishAcceptedDiscoveries(context.Background(), fixture.projectID)
	if err != nil {
		t.Fatalf("PublishAcceptedDiscoveries: %v", err)
	}
	if len(result.Reused) != 1 || len(result.Created) != 0 {
		t.Fatalf("result = %d created, %d reused", len(result.Created), len(result.Reused))
	}
	if result.Reused[0].CreatedIssueNumber != 41 {
		t.Fatalf("recorded issue = %d", result.Reused[0].CreatedIssueNumber)
	}
	if client.creates != 0 || client.comments != 1 || client.commentIssue != 41 {
		t.Fatalf("client: creates=%d comments=%d on #%d",
			client.creates, client.comments, client.commentIssue)
	}
	if !strings.Contains(client.commentBody, "observed this again") {
		t.Fatalf("comment = %q", client.commentBody)
	}
}

func TestDiscoveryIssuePublisherSkipsVerdictsThatCreateNoWork(t *testing.T) {
	t.Parallel()
	for _, decision := range []domain.DiscoveryDecision{
		domain.DecisionFixInCurrentTask,
		domain.DecisionDefer,
		domain.DecisionMergeIntoExisting,
		domain.DecisionRejectOutOfScope,
		domain.DecisionRequestArchitecture,
		domain.DecisionRequestHuman,
	} {
		decision := decision
		t.Run(string(decision), func(t *testing.T) {
			t.Parallel()
			fixture := newReviewFixture(t)
			fixture.decidedDiscovery(t, "Some finding", decision, domain.SeverityHigh)
			client := &fakeDiscoveryIssueClient{nextNumber: 900}
			publisher := fixture.issuePublisher(t, client)
			result, err := publisher.PublishAcceptedDiscoveries(
				context.Background(), fixture.projectID,
			)
			if err != nil {
				t.Fatalf("PublishAcceptedDiscoveries: %v", err)
			}
			if len(result.Created) != 0 || len(result.Reused) != 0 || client.creates != 0 {
				t.Fatalf("verdict %q published work", decision)
			}
			if client.listed != 0 {
				t.Fatal("no publishable discovery should not reach GitHub")
			}
		})
	}
}

func TestDiscoveryIssuePublisherLeavesDiscoveryUnpublishedOnGitHubFailure(t *testing.T) {
	t.Parallel()
	fixture := newReviewFixture(t)
	discovery := fixture.decidedDiscovery(t,
		"Retry budget is unbounded", domain.DecisionCreateNextTask, domain.SeverityHigh)
	client := &fakeDiscoveryIssueClient{
		nextNumber: 900,
		createErr:  errors.New("github is unavailable"),
	}
	publisher := fixture.issuePublisher(t, client)

	if _, err := publisher.PublishAcceptedDiscoveries(
		context.Background(), fixture.projectID,
	); !errors.Is(err, ErrDiscoveryIssuePublish) {
		t.Fatalf("error = %v", err)
	}
	stored, err := fixture.store.GetDiscoveryByID(discovery.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.CreatedIssueNumber != 0 {
		t.Fatalf("failed publication recorded issue %d", stored.CreatedIssueNumber)
	}

	// Retry once GitHub recovers.
	client.createErr = nil
	result, err := publisher.PublishAcceptedDiscoveries(context.Background(), fixture.projectID)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(result.Created) != 1 || result.Created[0].CreatedIssueNumber != 900 {
		t.Fatalf("retry result = %#v", result)
	}
}

func TestRecordDiscoveryIssueRefusesToRebind(t *testing.T) {
	t.Parallel()
	fixture := newReviewFixture(t)
	discovery := fixture.decidedDiscovery(t,
		"Finding", domain.DecisionAddToBacklog, domain.SeverityLow)
	if _, err := fixture.store.RecordDiscoveryIssue(store.DiscoveryIssueUpdate{
		ProjectID:   fixture.projectID,
		DiscoveryID: discovery.ID,
		IssueNumber: 7,
	}); err != nil {
		t.Fatal(err)
	}
	// Binding the same issue again is a no-op.
	if _, err := fixture.store.RecordDiscoveryIssue(store.DiscoveryIssueUpdate{
		ProjectID:   fixture.projectID,
		DiscoveryID: discovery.ID,
		IssueNumber: 7,
	}); err != nil {
		t.Fatalf("idempotent rebind: %v", err)
	}
	// Binding a different issue is a conflict, not an overwrite.
	if _, err := fixture.store.RecordDiscoveryIssue(store.DiscoveryIssueUpdate{
		ProjectID:   fixture.projectID,
		DiscoveryID: discovery.ID,
		IssueNumber: 8,
	}); !errors.Is(err, store.ErrDiscoveryIssueConflict) {
		t.Fatalf("rebind error = %v", err)
	}
	events, _ := fixture.store.ListWorkflowEvents(fixture.projectID, 0, 100)
	published := 0
	for _, event := range events {
		if event.Type == domain.WorkflowDiscoveryPublished {
			published++
		}
	}
	if published != 1 {
		t.Fatalf("emitted %d publication events", published)
	}
}

func TestDiscoveryPriorityLabelMapsBySeverityRank(t *testing.T) {
	t.Parallel()
	want := map[domain.DiscoverySeverity]string{
		domain.SeverityCritical: "priority:critical",
		domain.SeverityHigh:     "priority:high",
		domain.SeverityMedium:   "priority:normal",
		domain.SeverityLow:      "priority:low",
	}
	for severity, label := range want {
		if got := discoveryPriorityLabel(severity); got != label {
			t.Errorf("discoveryPriorityLabel(%q) = %q, want %q", severity, got, label)
		}
	}
}

func TestNewDiscoveryIssuePublisherRequiresDependencies(t *testing.T) {
	t.Parallel()
	fixture := newReviewFixture(t)
	if _, err := NewDiscoveryIssuePublisher(nil, &fakeDiscoveryIssueClient{}); err == nil {
		t.Error("missing store accepted")
	}
	if _, err := NewDiscoveryIssuePublisher(fixture.store, nil); err == nil {
		t.Error("missing client accepted")
	}
}

type fakeDiscoveryIssueClient struct {
	open           []*githubclient.Issue
	nextNumber     int
	creates        int
	comments       int
	listed         int
	createdTitle   string
	createdBody    string
	createdLabels  []string
	commentIssue   int
	commentBody    string
	postedComments []*githubclient.Comment
	// recordErr fails the caller's store write after the comment is posted,
	// which is the window a duplicate comment would appear in.
	recordErr     error
	ensuredLabels map[string]string
	createErr     error
	listErr       error
	// failAfter makes CreateIssue fail once this many issues exist, which
	// models GitHub dying partway through a batch.
	failAfter int
}

func (fake *fakeDiscoveryIssueClient) ListOpenIssues(
	_ context.Context,
	_, _ string,
) ([]*githubclient.Issue, error) {
	fake.listed++
	if fake.listErr != nil {
		return nil, fake.listErr
	}
	return fake.open, nil
}

func (fake *fakeDiscoveryIssueClient) CreateIssue(
	_ context.Context,
	_, _, title, body string,
	labels []string,
) (*githubclient.Issue, error) {
	if fake.createErr != nil {
		return nil, fake.createErr
	}
	if fake.failAfter > 0 && fake.creates >= fake.failAfter {
		return nil, errors.New("github is unavailable")
	}
	fake.creates++
	fake.createdTitle = title
	fake.createdBody = body
	fake.createdLabels = labels
	issue := &githubclient.Issue{Number: fake.nextNumber, Title: title, Body: body}
	fake.nextNumber++
	return issue, nil
}

func (fake *fakeDiscoveryIssueClient) PostComment(
	_ context.Context,
	_, _ string,
	number int,
	body string,
) (*githubclient.Comment, error) {
	fake.comments++
	fake.commentIssue = number
	fake.commentBody = body
	comment := &githubclient.Comment{ID: int64(fake.comments), Body: body}
	fake.postedComments = append(fake.postedComments, comment)
	return comment, nil
}

func (fake *fakeDiscoveryIssueClient) GetComments(
	context.Context,
	string, string,
	int,
	*time.Time,
) ([]*githubclient.Comment, error) {
	return fake.postedComments, nil
}

func (fake *fakeDiscoveryIssueClient) EnsureLabels(
	_ context.Context,
	_, _ string,
	labels map[string]string,
) error {
	fake.ensuredLabels = labels
	return nil
}

func (fixture *reviewFixture) issuePublisher(
	t *testing.T,
	client DiscoveryIssueClient,
) *DiscoveryIssuePublisher {
	t.Helper()
	var issueStore DiscoveryIssueStore = fixture.store
	if fake, ok := client.(*fakeDiscoveryIssueClient); ok {
		issueStore = &failingRecordStore{DiscoveryIssueStore: fixture.store, client: fake}
	}
	publisher, err := NewDiscoveryIssuePublisher(issueStore, client)
	if err != nil {
		t.Fatal(err)
	}
	return publisher
}

// decidedDiscovery records a discovery and applies one manager verdict to it,
// which is the state the issue publisher consumes.
func (fixture *reviewFixture) decidedDiscovery(
	t *testing.T,
	title string,
	decision domain.DiscoveryDecision,
	severity domain.DiscoverySeverity,
) *domain.Discovery {
	t.Helper()
	batch, err := fixture.store.CreateDiscoveries(fixture.projectID, []*domain.Discovery{
		domain.NewDiscovery(
			fixture.projectID, fixture.tasks[0].ID, 0,
			title, domain.DiscoveryBug, severity,
		),
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	discovery := batch.Created[0]
	status, ok := decision.Status()
	if !ok {
		t.Fatalf("decision %q has no status", decision)
	}
	var taskID *int64
	if decision == domain.DecisionMergeIntoExisting {
		taskID = &fixture.tasks[1].ID
	}
	review := fixture.review(t, []map[string]any{
		{
			"discovery_id": discovery.ID,
			"decision":     string(decision),
			"reason":       "Recorded by the fixture",
		},
	})
	decided, err := fixture.store.ApplyDiscoveryDecisions(store.DiscoveryDecisionUpdate{
		ProjectID:       fixture.projectID,
		ManagerReviewID: review.ID,
		Decisions: []store.DiscoveryDecisionRecord{{
			DiscoveryID: discovery.ID,
			Decision:    decision,
			Status:      status,
			TaskID:      taskID,
			Reason:      "Recorded by the fixture",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return decided[0]
}

// A retry between the source-context comment and the recorded issue number
// must not post the comment twice.
func TestDiscoverySourceCommentIsPostedOnlyOnce(t *testing.T) {
	t.Parallel()
	fixture := newReviewFixture(t)
	fixture.decidedDiscovery(t,
		"Retry budget is unbounded", domain.DecisionAddToBacklog, domain.SeverityMedium)
	client := &fakeDiscoveryIssueClient{
		nextNumber: 900,
		open:       []*githubclient.Issue{{Number: 41, Title: "Retry budget is unbounded"}},
		recordErr:  errors.New("database is unavailable"),
	}
	publisher := fixture.issuePublisher(t, client)

	// The first attempt comments, then fails before recording the number.
	if _, err := publisher.PublishAcceptedDiscoveries(
		context.Background(), fixture.projectID,
	); err == nil {
		t.Fatal("expected the recording failure to surface")
	}
	if client.comments != 1 {
		t.Fatalf("first attempt posted %d comments", client.comments)
	}

	client.recordErr = nil
	result, err := publisher.PublishAcceptedDiscoveries(context.Background(), fixture.projectID)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(result.Reused) != 1 {
		t.Fatalf("retry result = %#v", result)
	}
	if client.comments != 1 {
		t.Fatalf("retry posted a duplicate comment: %d total", client.comments)
	}
}

// failingRecordStore lets a test fail the issue-number write while leaving
// every other store operation real.
type failingRecordStore struct {
	DiscoveryIssueStore
	client *fakeDiscoveryIssueClient
}

func (wrapper *failingRecordStore) RecordDiscoveryIssue(
	update store.DiscoveryIssueUpdate,
) (*domain.Discovery, error) {
	if wrapper.client.recordErr != nil {
		return nil, wrapper.client.recordErr
	}
	return wrapper.DiscoveryIssueStore.RecordDiscoveryIssue(update)
}
