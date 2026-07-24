package project

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	"github.com/eslam-mahmoud/go-ai-agent/internal/projectfiles"
	"github.com/eslam-mahmoud/go-ai-agent/internal/projectissue"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
)

var (
	ErrInvalidPublication = errors.New("invalid project publication")
	ErrParentIssueSync    = errors.New("parent project issue synchronization failed")
)

type PublicationStore interface {
	LoadProjectAggregate(projectID int64) (*store.ProjectAggregate, error)
	UpdateProject(project *domain.Project) (*domain.Project, error)
	AppendWorkflowEvent(
		event *domain.WorkflowEvent,
	) (*domain.WorkflowEvent, bool, error)
}

type PublicationResult struct {
	FilesChanged      bool
	ParentIssueNumber int
	IssueCreated      bool
	IssueUnchanged    bool
	Recorded          bool
}

type PublisherOptions struct {
	// WorkspaceRoot holds cloned repositories as <root>/<owner>/<repo>.
	WorkspaceRoot string
}

// Publisher mirrors one Manager review onto the two human-facing surfaces:
// the repository project files and the parent GitHub dashboard issue.
//
// Durable local files are written before the remote call, so a failed parent
// issue update is a retryable condition rather than lost work.
type Publisher struct {
	store         PublicationStore
	client        projectissue.Client
	workspaceRoot string
}

func NewPublisher(
	publicationStore PublicationStore,
	client projectissue.Client,
	options PublisherOptions,
) (*Publisher, error) {
	if publicationStore == nil {
		return nil, errors.New("publisher store is required")
	}
	if client == nil {
		return nil, errors.New("publisher issue client is required")
	}
	root := strings.TrimSpace(options.WorkspaceRoot)
	if root == "" {
		return nil, errors.New("publisher workspace root is required")
	}
	return &Publisher{
		store:         publicationStore,
		client:        client,
		workspaceRoot: root,
	}, nil
}

// PublishManagerReview writes the project files, synchronizes the parent
// issue, and records one audit fact. Republishing an unchanged review touches
// neither surface and appends no duplicate event.
func (publisher *Publisher) PublishManagerReview(
	ctx context.Context,
	projectID, managerReviewID int64,
) (*PublicationResult, error) {
	if projectID <= 0 || managerReviewID <= 0 {
		return nil, fmt.Errorf(
			"%w: project and manager review IDs must be positive",
			ErrInvalidPublication,
		)
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
	review := aggregate.LatestManagerReview
	if review == nil || review.ID != managerReviewID || review.ProjectID != projectID {
		return nil, fmt.Errorf(
			"%w: review %d is not the latest review for project %d",
			ErrStaleManagerReview,
			managerReviewID,
			projectID,
		)
	}
	owner, repo, err := splitRepository(aggregate.Project.Repo)
	if err != nil {
		return nil, err
	}

	changed, err := publisher.writeFiles(aggregate.Project, aggregate.Tasks, owner, repo)
	if err != nil {
		return nil, err
	}
	result := &PublicationResult{
		FilesChanged:      changed,
		ParentIssueNumber: aggregate.Project.ParentIssueNumber,
	}

	sync, err := projectissue.Sync(
		ctx,
		publisher.client,
		owner,
		repo,
		aggregate.Project,
		aggregate.Tasks,
		review,
	)
	if err != nil {
		// Project files are already durable; the caller may safely retry.
		return nil, fmt.Errorf("%w: %v", ErrParentIssueSync, err)
	}
	if sync == nil || sync.Issue == nil || sync.Issue.Number <= 0 {
		return nil, fmt.Errorf("%w: no parent issue number returned", ErrParentIssueSync)
	}
	result.ParentIssueNumber = sync.Issue.Number
	result.IssueCreated = sync.Created
	result.IssueUnchanged = sync.Unchanged
	if sync.Created {
		updated := *aggregate.Project
		updated.ParentIssueNumber = sync.Issue.Number
		persisted, err := publisher.store.UpdateProject(&updated)
		if err != nil {
			return nil, fmt.Errorf("persist parent project issue number: %w", err)
		}
		// The first write predates the issue number, so converge the files
		// now rather than leaving them stale until the next review.
		rewritten, err := publisher.writeFiles(persisted, aggregate.Tasks, owner, repo)
		if err != nil {
			return nil, err
		}
		result.FilesChanged = result.FilesChanged || rewritten
	}

	recorded, err := publisher.recordPublication(projectID, review, result)
	if err != nil {
		return nil, err
	}
	result.Recorded = recorded
	return result, nil
}

func (publisher *Publisher) writeFiles(
	projectRecord *domain.Project,
	tasks []*domain.Task,
	owner, repo string,
) (bool, error) {
	if projectRecord == nil {
		return false, fmt.Errorf("%w: project record is nil", ErrInconsistentState)
	}
	files, err := projectfiles.Render(projectRecord, tasks)
	if err != nil {
		return false, err
	}
	return projectfiles.WriteChanged(
		filepath.Join(publisher.workspaceRoot, owner, repo),
		files,
	)
}

func (publisher *Publisher) recordPublication(
	projectID int64,
	review *domain.ManagerReview,
	result *PublicationResult,
) (bool, error) {
	event := domain.NewWorkflowEvent(
		projectID,
		domain.WorkflowSourceController,
		domain.WorkflowProjectPublished,
		"Published the manager review to the parent issue and project files.",
	)
	data, err := json.Marshal(map[string]any{
		"manager_review_id":   review.ID,
		"parent_issue_number": result.ParentIssueNumber,
		"issue_created":       result.IssueCreated,
		"issue_unchanged":     result.IssueUnchanged,
		"files_changed":       result.FilesChanged,
	})
	if err != nil {
		return false, fmt.Errorf("encode publication evidence: %w", err)
	}
	event.Data = data
	event.IdempotencyKey = fmt.Sprintf("manager-review:%d:publication", review.ID)
	_, created, err := publisher.store.AppendWorkflowEvent(event)
	if err != nil {
		return false, err
	}
	return created, nil
}

func splitRepository(repo string) (string, string, error) {
	owner, name, found := strings.Cut(strings.TrimSpace(repo), "/")
	if !found ||
		strings.TrimSpace(owner) == "" ||
		strings.TrimSpace(name) == "" ||
		strings.Contains(name, "/") {
		return "", "", fmt.Errorf(
			"%w: repository %q must be owner/name",
			ErrInvalidPublication,
			repo,
		)
	}
	return owner, name, nil
}

var _ PublicationStore = (*store.Store)(nil)
