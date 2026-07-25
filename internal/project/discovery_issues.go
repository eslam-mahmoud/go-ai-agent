package project

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	githubclient "github.com/eslam-mahmoud/go-ai-agent/internal/github"
	"github.com/eslam-mahmoud/go-ai-agent/internal/githubops"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
)

var ErrDiscoveryIssuePublish = errors.New("discovery issue publication failed")

// DiscoveryIssueClient is the narrow GitHub boundary needed to publish
// discoveries. It keeps API details inside internal/github.
type DiscoveryIssueClient interface {
	ListOpenIssues(ctx context.Context, owner, repo string) ([]*githubclient.Issue, error)
	CreateIssue(
		ctx context.Context,
		owner, repo, title, body string,
		labels []string,
	) (*githubclient.Issue, error)
	PostComment(
		ctx context.Context,
		owner, repo string,
		number int,
		body string,
	) (*githubclient.Comment, error)
	// GetComments lets source-context comments be posted idempotently, so a
	// retry after a partial failure does not repeat them.
	GetComments(
		ctx context.Context,
		owner, repo string,
		number int,
		since *time.Time,
	) ([]*githubclient.Comment, error)
	EnsureLabels(ctx context.Context, owner, repo string, labels map[string]string) error
}

type DiscoveryIssueStore interface {
	LoadProjectAggregate(projectID int64) (*store.ProjectAggregate, error)
	ListDiscoveries(projectID int64) ([]*domain.Discovery, error)
	RecordDiscoveryIssue(update store.DiscoveryIssueUpdate) (*domain.Discovery, error)
}

type DiscoveryIssueResult struct {
	Created []*domain.Discovery
	Reused  []*domain.Discovery
}

// DiscoveryIssuePublisher turns accepted discoveries into GitHub issues,
// reusing an existing open issue rather than filing a duplicate.
type DiscoveryIssuePublisher struct {
	store  DiscoveryIssueStore
	client DiscoveryIssueClient
}

func NewDiscoveryIssuePublisher(
	issueStore DiscoveryIssueStore,
	client DiscoveryIssueClient,
) (*DiscoveryIssuePublisher, error) {
	if issueStore == nil {
		return nil, errors.New("discovery issue store is required")
	}
	if client == nil {
		return nil, errors.New("discovery issue client is required")
	}
	return &DiscoveryIssuePublisher{store: issueStore, client: client}, nil
}

// PublishAcceptedDiscoveries files an issue for every discovery whose verdict
// creates new work and that has no issue yet. It is safe to re-run.
func (publisher *DiscoveryIssuePublisher) PublishAcceptedDiscoveries(
	ctx context.Context,
	projectID int64,
) (*DiscoveryIssueResult, error) {
	if projectID <= 0 {
		return nil, fmt.Errorf("%w: project ID must be positive", ErrDiscoveryIssuePublish)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	aggregate, err := publisher.store.LoadProjectAggregate(projectID)
	if err != nil {
		return nil, err
	}
	if aggregate == nil || aggregate.Project == nil {
		return nil, fmt.Errorf("%w: project aggregate is nil", ErrInconsistentState)
	}
	owner, repo, err := splitRepository(aggregate.Project.Repo)
	if err != nil {
		return nil, err
	}
	discoveries, err := publisher.store.ListDiscoveries(projectID)
	if err != nil {
		return nil, err
	}
	publishable := make([]*domain.Discovery, 0, len(discoveries))
	for _, discovery := range discoveries {
		if publishableDiscovery(discovery) {
			publishable = append(publishable, discovery)
		}
	}
	result := &DiscoveryIssueResult{}
	if len(publishable) == 0 {
		return result, nil
	}

	openIssues, err := publisher.client.ListOpenIssues(ctx, owner, repo)
	if err != nil {
		return nil, fmt.Errorf("%w: list open issues: %v", ErrDiscoveryIssuePublish, err)
	}
	if err := publisher.client.EnsureLabels(
		ctx, owner, repo, discoveryLabelColors(publishable),
	); err != nil {
		return nil, fmt.Errorf("%w: ensure labels: %v", ErrDiscoveryIssuePublish, err)
	}

	for _, discovery := range publishable {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		existing := matchOpenIssue(openIssues, discovery.Title)
		if existing != nil {
			// Keyed on the discovery, so a retry between the comment and the
			// recorded issue number does not post it twice.
			if _, err := githubops.EnsureComment(
				ctx,
				publisher.client,
				owner,
				repo,
				existing.Number,
				fmt.Sprintf("discovery:%d:source", discovery.ID),
				discoverySourceComment(discovery),
			); err != nil {
				return nil, fmt.Errorf(
					"%w: comment on issue #%d: %v",
					ErrDiscoveryIssuePublish,
					existing.Number,
					err,
				)
			}
			stored, err := publisher.store.RecordDiscoveryIssue(store.DiscoveryIssueUpdate{
				ProjectID:   projectID,
				DiscoveryID: discovery.ID,
				IssueNumber: existing.Number,
				Reused:      true,
			})
			if err != nil {
				return nil, err
			}
			result.Reused = append(result.Reused, stored)
			continue
		}
		issue, err := publisher.client.CreateIssue(
			ctx,
			owner,
			repo,
			discovery.Title,
			discoveryIssueBody(discovery),
			discoveryLabels(discovery),
		)
		if err != nil {
			return nil, fmt.Errorf("%w: create issue: %v", ErrDiscoveryIssuePublish, err)
		}
		if issue == nil || issue.Number <= 0 {
			return nil, fmt.Errorf("%w: created issue has no number", ErrDiscoveryIssuePublish)
		}
		stored, err := publisher.store.RecordDiscoveryIssue(store.DiscoveryIssueUpdate{
			ProjectID:   projectID,
			DiscoveryID: discovery.ID,
			IssueNumber: issue.Number,
		})
		if err != nil {
			return nil, err
		}
		result.Created = append(result.Created, stored)
		openIssues = append(openIssues, issue)
	}
	return result, nil
}

// publishableDiscovery reports whether a discovery needs an issue: the manager
// accepted it as new work and nothing has been filed yet.
func publishableDiscovery(discovery *domain.Discovery) bool {
	return discovery != nil &&
		discovery.Status == domain.DiscoveryAccepted &&
		discovery.Decision.CreatesTask() &&
		discovery.CreatedIssueNumber == 0
}

func matchOpenIssue(
	issues []*githubclient.Issue,
	title string,
) *githubclient.Issue {
	normalized := domain.NormalizeDiscoveryTitle(title)
	if normalized == "" {
		return nil
	}
	for _, issue := range issues {
		if issue == nil {
			continue
		}
		if domain.NormalizeDiscoveryTitle(issue.Title) == normalized {
			return issue
		}
	}
	return nil
}

func discoveryIssueBody(discovery *domain.Discovery) string {
	var body strings.Builder
	if description := strings.TrimSpace(discovery.Description); description != "" {
		body.WriteString(description)
		body.WriteString("\n\n")
	}
	body.WriteString("### Discovery\n\n")
	fmt.Fprintf(&body, "- **Category:** %s\n", discovery.Category)
	fmt.Fprintf(&body, "- **Severity:** %s\n", discovery.Severity)
	fmt.Fprintf(&body, "- **Times observed:** %d\n", discovery.Occurrences)
	fmt.Fprintf(&body, "- **Recommended action:** %s\n", textOrDash(discovery.SuggestedAction))
	if discovery.SourceTaskID > 0 {
		fmt.Fprintf(&body, "- **Source task:** %d\n", discovery.SourceTaskID)
	}
	if discovery.SourceExecutionID > 0 {
		fmt.Fprintf(&body, "- **Source execution:** %d\n", discovery.SourceExecutionID)
	}
	if discovery.BlocksCurrent {
		body.WriteString("- **Blocks current work:** yes\n")
	}
	if discovery.ArchitectureRisk {
		body.WriteString("- **Architecture risk:** yes\n")
	}
	body.WriteString("\n### Manager decision\n\n")
	fmt.Fprintf(&body, "- **Decision:** %s\n", discovery.Decision)
	fmt.Fprintf(&body, "- **Reason:** %s\n", textOrDash(discovery.DecisionReason))
	fmt.Fprintf(&body, "\n_Opened by Madar from discovery `%s`._\n", discovery.ExternalID)
	return body.String()
}

// discoverySourceComment adds context to an issue that already covers this
// work, which the plan prefers over opening a duplicate.
func discoverySourceComment(discovery *domain.Discovery) string {
	var comment strings.Builder
	comment.WriteString("Madar observed this again while working the project.\n\n")
	fmt.Fprintf(&comment, "- **Discovery:** `%s`\n", discovery.ExternalID)
	fmt.Fprintf(&comment, "- **Category / severity:** %s / %s\n",
		discovery.Category, discovery.Severity)
	fmt.Fprintf(&comment, "- **Times observed:** %d\n", discovery.Occurrences)
	if discovery.SourceTaskID > 0 {
		fmt.Fprintf(&comment, "- **Source task:** %d\n", discovery.SourceTaskID)
	}
	fmt.Fprintf(&comment, "- **Manager decision:** %s — %s\n",
		discovery.Decision, textOrDash(discovery.DecisionReason))
	return comment.String()
}

func discoveryLabels(discovery *domain.Discovery) []string {
	labels := []string{"type:discovery", discoveryPriorityLabel(discovery.Severity)}
	if discovery.Decision == domain.DecisionCreateReleaseBlocker {
		labels = append(labels, "release:blocker")
	}
	if discovery.ArchitectureRisk {
		labels = append(labels, "architecture:review-required")
	}
	return labels
}

// discoveryPriorityLabel maps severity by rank, not by name, so the mapping
// stays correct if severities are ever renamed.
func discoveryPriorityLabel(severity domain.DiscoverySeverity) string {
	switch {
	case severity.AtLeast(domain.SeverityCritical):
		return "priority:critical"
	case severity.AtLeast(domain.SeverityHigh):
		return "priority:high"
	case severity.AtLeast(domain.SeverityMedium):
		return "priority:normal"
	default:
		return "priority:low"
	}
}

func discoveryLabelColors(discoveries []*domain.Discovery) map[string]string {
	colors := map[string]string{
		"type:discovery":               "5319e7",
		"priority:critical":            "b60205",
		"priority:high":                "d93f0b",
		"priority:normal":              "fbca04",
		"priority:low":                 "0e8a16",
		"release:blocker":              "b60205",
		"architecture:review-required": "1d76db",
	}
	used := make(map[string]string, len(colors))
	for _, discovery := range discoveries {
		for _, label := range discoveryLabels(discovery) {
			if color, known := colors[label]; known {
				used[label] = color
			}
		}
	}
	return used
}

var _ DiscoveryIssueStore = (*store.Store)(nil)

// textOrDash keeps rendered fields on one line and never blank.
func textOrDash(value string) string {
	collapsed := strings.Join(strings.Fields(value), " ")
	if collapsed == "" {
		return "-"
	}
	return collapsed
}
