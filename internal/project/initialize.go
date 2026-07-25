package project

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/eslam-mahmoud/go-ai-agent/internal/architecturedocs"
	"github.com/eslam-mahmoud/go-ai-agent/internal/reposcan"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
)

var ErrInitialization = errors.New("project initialization failed")

// InitializationResult reports what each stage of a bootstrap actually did, so
// a caller can tell a fresh initialization from a resumed or repeated one.
type InitializationResult struct {
	Scan          *reposcan.Report
	Architecture  *ArchitectureAssessment
	Proposal      json.RawMessage
	Documents     *architecturedocs.Result
	Backlog       *InitialBacklogResult
	Publication   *PublicationResult
	AlreadyOnPlan bool
}

type InitializerStore interface {
	LoadProjectAggregate(projectID int64) (*store.ProjectAggregate, error)
}

// InitializerOptions carries the stages that are optional because they need
// GitHub credentials or are not configured in every deployment.
type InitializerOptions struct {
	// Backlog creates the first ordered backlog and files its issues.
	Backlog *InitialBacklogController
	// Publisher synchronizes the parent dashboard issue and project files.
	Publisher *Publisher
}

// Initializer bootstraps a managed project: scan the repository, run the
// architect, write the architecture it proposes, create the first backlog,
// and publish the result. Every stage is idempotent, so a partially
// initialized project is repaired by running it again.
type Initializer struct {
	store        InitializerStore
	architecture *ArchitectureController
	workspace    WorkspaceResolver
	backlog      *InitialBacklogController
	publisher    *Publisher
}

// WorkspaceResolver reports where a project's repository is checked out.
type WorkspaceResolver interface {
	ProjectWorkspace(repo string) (string, error)
}

// WorkspaceRootResolver resolves <root>/<owner>/<repo>, the layout the rest of
// Madar already uses.
type WorkspaceRootResolver struct {
	Root string
}

func (resolver WorkspaceRootResolver) ProjectWorkspace(repo string) (string, error) {
	owner, name, err := splitRepository(repo)
	if err != nil {
		return "", err
	}
	return joinWorkspace(resolver.Root, owner, name), nil
}

func NewInitializer(
	initializerStore InitializerStore,
	architecture *ArchitectureController,
	workspace WorkspaceResolver,
	options InitializerOptions,
) (*Initializer, error) {
	switch {
	case initializerStore == nil:
		return nil, errors.New("initializer store is required")
	case architecture == nil:
		return nil, errors.New("initializer architecture controller is required")
	case workspace == nil:
		return nil, errors.New("initializer workspace resolver is required")
	}
	return &Initializer{
		store:        initializerStore,
		architecture: architecture,
		workspace:    workspace,
		backlog:      options.Backlog,
		publisher:    options.Publisher,
	}, nil
}

// Initialize bootstraps the project. It is safe to call repeatedly: a project
// that is already on plan reports that and writes nothing new.
func (initializer *Initializer) Initialize(
	ctx context.Context,
	projectID int64,
) (*InitializationResult, error) {
	if projectID <= 0 {
		return nil, fmt.Errorf("%w: project ID must be positive", ErrInitialization)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	aggregate, err := initializer.store.LoadProjectAggregate(projectID)
	if err != nil {
		return nil, err
	}
	if aggregate == nil || aggregate.Project == nil {
		return nil, fmt.Errorf("%w: project aggregate is nil", ErrInconsistentState)
	}
	workspace, err := initializer.workspace.ProjectWorkspace(aggregate.Project.Repo)
	if err != nil {
		return nil, err
	}
	scan, err := reposcan.Scan(workspace)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInitialization, err)
	}
	result := &InitializationResult{Scan: scan}

	// The architect runs only when something is owed: an initialized project
	// with no outstanding risk needs no new architecture.
	assessment, proposal, err := initializer.architecture.RunArchitect(ctx, projectID)
	if err != nil {
		return nil, err
	}
	result.Architecture = assessment
	result.Proposal = proposal
	if assessment != nil {
		result.Documents = assessment.Documents
	}

	if initializer.backlog != nil {
		backlog, err := initializer.backlog.Initialize(ctx, projectID, proposal)
		if err != nil {
			return nil, err
		}
		result.Backlog = backlog
	}
	if err := initializer.publish(ctx, projectID, result); err != nil {
		return nil, err
	}
	result.AlreadyOnPlan = initializationUnchanged(result)
	return result, nil
}

// publish synchronizes the parent issue only when the project has a manager
// review to publish; before the first review there is nothing to report.
func (initializer *Initializer) publish(
	ctx context.Context,
	projectID int64,
	result *InitializationResult,
) error {
	if initializer.publisher == nil {
		return nil
	}
	aggregate, err := initializer.store.LoadProjectAggregate(projectID)
	if err != nil {
		return err
	}
	if aggregate == nil || aggregate.LatestManagerReview == nil {
		return nil
	}
	publication, err := initializer.publisher.PublishManagerReview(
		ctx, projectID, aggregate.LatestManagerReview.ID,
	)
	if err != nil {
		return err
	}
	result.Publication = publication
	return nil
}

// initializationUnchanged reports whether this run wrote nothing anywhere,
// which is what a repeated initialization of a settled project looks like.
func initializationUnchanged(result *InitializationResult) bool {
	if result.Documents != nil && len(result.Documents.Written) > 0 {
		return false
	}
	if result.Backlog != nil &&
		(!result.Backlog.AlreadyExisted ||
			len(result.Backlog.FiledIssues) > 0 ||
			len(result.Backlog.ReusedIssues) > 0) {
		return false
	}
	if result.Publication != nil &&
		(result.Publication.FilesChanged ||
			result.Publication.IssueCreated ||
			!result.Publication.IssueUnchanged) {
		return false
	}
	return true
}

var _ InitializerStore = (*store.Store)(nil)

// joinWorkspace keeps the <root>/<owner>/<repo> layout in one place.
func joinWorkspace(root, owner, repo string) string {
	return filepath.Join(root, owner, repo)
}
