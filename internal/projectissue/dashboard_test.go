package projectissue

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	githubclient "github.com/eslam-mahmoud/go-ai-agent/internal/github"
)

func TestRenderExactEmptyDashboard(t *testing.T) {
	project := dashboardProject()
	got, err := Render(project, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := `<!-- madar:project-dashboard:start -->
## Madar Project Dashboard

### Goal

Ship v2

### Status

- **Health:** on-track
- **Current phase:** initializing
- **Active issue:** -
- **Progress estimate:** 0%
- **Release target:** -
- **Release readiness:** -

### Ordered Backlog

| Seq | Issue | Status | Priority | Task | Release blocker |
| ---: | ---: | --- | ---: | --- | :---: |
| - | - | - | - | _No tasks_ | - |

### Release Blockers

- None.

### Risks

- None recorded.

### Recent Discoveries

- None recorded.

### Last Manager Decision

- No manager review recorded.
<!-- madar:project-dashboard:end -->`
	if got != want {
		t.Errorf("dashboard mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestRenderUsesOrderedStateAndLatestReview(t *testing.T) {
	project := dashboardProject()
	project.Health = domain.HealthAtRisk
	project.State = domain.ProjectExecuting
	project.ReleaseTarget = "v2 | stable"
	project.ReleaseReadiness = "CI pending"
	currentTaskID := int64(11)
	project.CurrentTaskID = &currentTaskID

	first := domain.NewTask(project.ID, "First task", "Run first")
	first.ID = 11
	first.Sequence = 1
	first.IssueNumber = 42
	first.Status = domain.TaskDeveloping
	second := domain.NewTask(project.ID, "Blocked | task", "Unblock")
	second.ID = 12
	second.Sequence = 2
	second.IssueNumber = 43
	second.Status = domain.TaskBlocked
	second.Priority = 9
	second.BlocksRelease = true
	second.DependencyState = "waiting\non API"

	nextID := second.ID
	review := domain.NewManagerReview(project.ID)
	review.ProgressEstimate = 55
	review.CompletedTaskDecision = domain.TaskDecisionAccepted
	review.ArchitectureReviewRequired = true
	review.HumanApprovalRequired = true
	review.DiscoveryDecisions = json.RawMessage(`[{"id":"D-2","decision":"defer"}]`)
	review.NextTaskID = &nextID
	review.NextTaskIssueNumber = second.IssueNumber
	review.NextTaskReason = "Release risk"
	review.ReleaseReadiness = "CI pending"
	review.OwnerUpdate = "Feature complete.\nReviewing blockers."
	review.ReviewedAt = time.Date(2026, 7, 24, 1, 2, 3, 0, time.UTC)

	got, err := Render(project, []*domain.Task{second, first}, review)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Index(got, "First task") >= strings.Index(got, `Blocked \| task`) {
		t.Fatalf("backlog is not sequence ordered:\n%s", got)
	}
	for _, want := range []string{
		"- **Health:** at-risk",
		"- **Current phase:** executing",
		"- **Active issue:** #42 — First task",
		"- **Progress estimate:** 55%",
		"- **Release target:** v2 | stable",
		"| 2 | #43 | blocked | 9 | Blocked \\| task | yes |",
		"- #43 Blocked | task — blocked",
		"- Project health is **at-risk**.",
		"- #43 Blocked | task is blocked.",
		"dependency state: waiting on API.",
		"- Architecture review is required.",
		"- Human approval is required.",
		"- `{\"id\":\"D-2\",\"decision\":\"defer\"}`",
		"Feature complete.\nReviewing blockers.",
		"- **Completed task decision:** accepted",
		"- **Next task:** #43 — Release risk",
		"- **Reviewed at:** 2026-07-24T01:02:03Z",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("dashboard missing %q:\n%s", want, got)
		}
	}
}

func TestMergeManagedSectionPreservesHumanContent(t *testing.T) {
	section := StartMarker + "\nnew dashboard\n" + EndMarker

	t.Run("append", func(t *testing.T) {
		existing := "Human heading\n\nHuman notes  "
		got, err := MergeManagedSection(existing, section)
		if err != nil {
			t.Fatal(err)
		}
		want := existing + "\n\n" + section
		if got != want {
			t.Errorf("merged body = %q, want %q", got, want)
		}
	})

	t.Run("replace", func(t *testing.T) {
		prefix := "Human prefix\n\n"
		suffix := "\n\nHuman suffix\n"
		existing := prefix + StartMarker + "\nold\n" + EndMarker + suffix
		got, err := MergeManagedSection(existing, section)
		if err != nil {
			t.Fatal(err)
		}
		if got != prefix+section+suffix {
			t.Errorf("merged body = %q", got)
		}
	})
}

func TestMergeManagedSectionRejectsInvalidMarkers(t *testing.T) {
	validSection := StartMarker + "\nnew\n" + EndMarker
	tests := []struct {
		name     string
		existing string
		section  string
	}{
		{name: "missing end", existing: "human\n" + StartMarker, section: validSection},
		{name: "duplicate start", existing: StartMarker + StartMarker + EndMarker, section: validSection},
		{name: "end before start", existing: EndMarker + StartMarker, section: validSection},
		{name: "invalid replacement", existing: "human", section: "unmarked"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := MergeManagedSection(tc.existing, tc.section)
			if !errors.Is(err, ErrInvalidMarkers) {
				t.Fatalf("error = %v, want ErrInvalidMarkers", err)
			}
		})
	}
}

func TestRenderEscapesManagedMarkersFromProjectContent(t *testing.T) {
	project := dashboardProject()
	project.Goal = "Do work\n" + StartMarker
	task := domain.NewTask(project.ID, "Do not close "+EndMarker, "Safe rendering")
	task.ID = 1
	task.Sequence = 1

	rendered, err := Render(project, []*domain.Task{task}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(rendered, StartMarker) != 1 || strings.Count(rendered, EndMarker) != 1 {
		t.Fatalf("user content injected managed markers:\n%s", rendered)
	}
	for _, want := range []string{
		"&lt;!-- madar:project-dashboard:start --&gt;",
		"&lt;!-- madar:project-dashboard:end --&gt;",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered dashboard missing escaped marker %q:\n%s", want, rendered)
		}
	}
}

func TestSyncCreatesOrUpdatesParentIssue(t *testing.T) {
	project := dashboardProject()
	client := &fakeIssueClient{
		createdIssue: &githubclient.Issue{
			Number:  91,
			HTMLURL: "https://github.com/owner/repo/issues/91",
		},
	}
	created, err := Sync(context.Background(), client, "owner", "repo", project, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !created.Created || created.Issue.Number != 91 || client.createCalls != 1 {
		t.Fatalf("create result = %#v, calls=%d", created, client.createCalls)
	}
	if client.createOwner != "owner" ||
		client.createRepo != "repo" ||
		client.createTitle != "[Madar] Madar" ||
		!strings.Contains(client.createBody, StartMarker) {
		t.Errorf("create request = %#v", client)
	}

	project.ParentIssueNumber = 91
	// A stale managed section so the merge is a real change.
	stale := strings.Replace(client.createBody, "**Health:** on-track", "**Health:** at-risk", 1)
	if stale == client.createBody {
		t.Fatal("fixture did not produce a stale section")
	}
	client.existingIssue = &githubclient.Issue{
		Number: 91,
		Body:   "Human notes\n\n" + stale + "\n\nHuman footer",
	}
	updated, err := Sync(context.Background(), client, "owner", "repo", project, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Created || updated.Issue.Number != 91 || client.updateCalls != 1 {
		t.Fatalf("update result = %#v, calls=%d", updated, client.updateCalls)
	}
	if !strings.HasPrefix(client.updatedBody, "Human notes\n\n") ||
		!strings.HasSuffix(client.updatedBody, "\n\nHuman footer") ||
		strings.Count(client.updatedBody, StartMarker) != 1 {
		t.Errorf("updated body did not preserve human content:\n%s", client.updatedBody)
	}
}

func TestSyncSkipsUnchangedManagedSection(t *testing.T) {
	project := dashboardProject()
	project.ParentIssueNumber = 91
	section, err := Render(project, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &fakeIssueClient{
		existingIssue: &githubclient.Issue{
			Number: 91,
			Body:   "Human notes\n\n" + section + "\n\nHuman footer",
		},
	}
	result, err := Sync(context.Background(), client, "owner", "repo", project, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Unchanged || result.Created || result.Issue.Number != 91 {
		t.Fatalf("result = %#v", result)
	}
	if client.updateCalls != 0 {
		t.Fatalf("unchanged section issued %d updates", client.updateCalls)
	}
}

func TestSyncDoesNotUpdateMalformedExistingIssue(t *testing.T) {
	project := dashboardProject()
	project.ParentIssueNumber = 91
	client := &fakeIssueClient{
		existingIssue: &githubclient.Issue{
			Number: 91,
			Body:   "Human notes\n" + StartMarker + "\nunclosed",
		},
	}
	_, err := Sync(context.Background(), client, "owner", "repo", project, nil, nil)
	if !errors.Is(err, ErrInvalidMarkers) {
		t.Fatalf("error = %v, want ErrInvalidMarkers", err)
	}
	if client.updateCalls != 0 {
		t.Fatalf("update calls = %d, want 0", client.updateCalls)
	}
}

func dashboardProject() *domain.Project {
	project := domain.NewProject("owner/repo", "Madar", "Ship v2", "")
	project.ID = 7
	return project
}

type fakeIssueClient struct {
	createdIssue  *githubclient.Issue
	existingIssue *githubclient.Issue
	createCalls   int
	updateCalls   int
	createOwner   string
	createRepo    string
	createTitle   string
	createBody    string
	updatedBody   string
}

func (f *fakeIssueClient) GetIssue(
	context.Context,
	string,
	string,
	int,
) (*githubclient.Issue, error) {
	return f.existingIssue, nil
}

func (f *fakeIssueClient) CreateIssue(
	_ context.Context,
	owner, repo, title, body string,
	_ []string,
) (*githubclient.Issue, error) {
	f.createCalls++
	f.createOwner = owner
	f.createRepo = repo
	f.createTitle = title
	f.createBody = body
	return f.createdIssue, nil
}

func (f *fakeIssueClient) UpdateIssueBody(
	_ context.Context,
	_, _ string,
	number int,
	body string,
) (*githubclient.Issue, error) {
	f.updateCalls++
	f.updatedBody = body
	return &githubclient.Issue{
		Number:  number,
		Body:    body,
		HTMLURL: "https://github.com/owner/repo/issues/91",
	}, nil
}
