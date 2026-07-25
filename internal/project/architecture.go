package project

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/eslam-mahmoud/go-ai-agent/internal/architecturedocs"
	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
)

var ErrInvalidArchitectureAssessment = errors.New("invalid architecture assessment")

type ArchitectureStore interface {
	LoadProjectAggregate(projectID int64) (*store.ProjectAggregate, error)
	ListArchitectureRiskDiscoveries(projectID int64) ([]*domain.Discovery, error)
	AppendWorkflowEvent(
		event *domain.WorkflowEvent,
	) (*domain.WorkflowEvent, bool, error)
}

// ArchitectureAssessment states whether architecture review is owed and why.
// Architect mode consumes Discoveries; the delivery lane consumes Required.
type ArchitectureAssessment struct {
	Required         bool
	ManagerRequested bool
	Discoveries      []*domain.Discovery
	Reason           string
	Recorded         bool
	// Documents reports what an Architect run wrote to the repository.
	Documents *architecturedocs.Result
}

// ArchitectRunner is the provider-neutral boundary to Architect mode. It
// returns output already validated against the architect output schema.
type ArchitectRunner interface {
	RunArchitect(
		ctx context.Context,
		projectID int64,
		outstandingDiscoveryIDs []int64,
	) (json.RawMessage, error)
}

// ArchitectureController raises the architecture-review obligation and, when
// an Architect runner is configured, satisfies it. It never edits
// architecture itself; the run proposes and later items apply.
// ArchitectureDocumentWriter turns a validated proposal into repository
// documents. It is optional: a deployment may run architecture assessment
// without generating files.
type ArchitectureDocumentWriter interface {
	WriteArchitectureDocuments(
		projectID int64,
		proposal json.RawMessage,
	) (*architecturedocs.Result, error)
}

type ArchitectureController struct {
	store     ArchitectureStore
	architect ArchitectRunner
	documents ArchitectureDocumentWriter
}

func NewArchitectureController(
	architectureStore ArchitectureStore,
) (*ArchitectureController, error) {
	return NewArchitectureControllerWithRunner(architectureStore, nil)
}

// NewArchitectureControllerWithRunner adds the Architect run. Without a runner
// the controller still raises and reports the obligation.
func NewArchitectureControllerWithRunner(
	architectureStore ArchitectureStore,
	architect ArchitectRunner,
) (*ArchitectureController, error) {
	return NewArchitectureControllerWithDocuments(architectureStore, architect, nil)
}

// NewArchitectureControllerWithDocuments also writes the proposal to the
// repository once a run completes.
func NewArchitectureControllerWithDocuments(
	architectureStore ArchitectureStore,
	architect ArchitectRunner,
	documents ArchitectureDocumentWriter,
) (*ArchitectureController, error) {
	if architectureStore == nil {
		return nil, errors.New("architecture controller store is required")
	}
	return &ArchitectureController{
		store:     architectureStore,
		architect: architect,
		documents: documents,
	}, nil
}

// RunArchitect satisfies an outstanding obligation by running Architect mode
// over the exact discoveries that raised it. It returns the assessment plus
// the raw architecture proposal for later items to apply.
func (controller *ArchitectureController) RunArchitect(
	ctx context.Context,
	projectID int64,
) (*ArchitectureAssessment, json.RawMessage, error) {
	assessment, err := controller.Assess(projectID)
	if err != nil {
		return nil, nil, err
	}
	if !assessment.Required || controller.architect == nil {
		return assessment, nil, nil
	}
	ids := make([]int64, 0, len(assessment.Discoveries))
	for _, discovery := range assessment.Discoveries {
		ids = append(ids, discovery.ID)
	}
	proposal, err := controller.architect.RunArchitect(ctx, projectID, ids)
	if err != nil {
		return nil, nil, fmt.Errorf("run architect: %w", err)
	}
	if controller.documents != nil && len(proposal) > 0 {
		written, err := controller.documents.WriteArchitectureDocuments(projectID, proposal)
		if err != nil {
			return nil, nil, fmt.Errorf("write architecture documents: %w", err)
		}
		assessment.Documents = written
	}
	return assessment, proposal, nil
}

// Assess reports the project's architecture-review obligation and records it
// once per outstanding set, so a polling caller creates no audit noise.
func (controller *ArchitectureController) Assess(
	projectID int64,
) (*ArchitectureAssessment, error) {
	if projectID <= 0 {
		return nil, fmt.Errorf(
			"%w: project ID must be positive",
			ErrInvalidArchitectureAssessment,
		)
	}
	aggregate, err := controller.store.LoadProjectAggregate(projectID)
	if err != nil {
		return nil, err
	}
	if aggregate == nil || aggregate.Project == nil {
		return nil, fmt.Errorf("%w: project aggregate is nil", ErrInconsistentState)
	}
	outstanding, err := controller.store.ListArchitectureRiskDiscoveries(projectID)
	if err != nil {
		return nil, err
	}
	assessment := &ArchitectureAssessment{
		Discoveries: outstanding,
	}
	if review := aggregate.LatestManagerReview; review != nil {
		assessment.ManagerRequested = review.ArchitectureReviewRequired
	}
	assessment.Required = assessment.ManagerRequested || len(outstanding) > 0
	if !assessment.Required {
		return assessment, nil
	}
	assessment.Reason = architectureReason(assessment)
	recorded, err := controller.recordObligation(projectID, assessment)
	if err != nil {
		return nil, err
	}
	assessment.Recorded = recorded
	return assessment, nil
}

func architectureReason(assessment *ArchitectureAssessment) string {
	reasons := make([]string, 0, 2)
	if assessment.ManagerRequested {
		reasons = append(reasons, "the Engineering Manager requested architecture review")
	}
	if count := len(assessment.Discoveries); count > 0 {
		reasons = append(reasons, fmt.Sprintf(
			"%d discovery/discoveries carry unresolved architecture risk",
			count,
		))
	}
	return strings.Join(reasons, "; ")
}

// recordObligation keys the audit fact on the exact outstanding set, so the
// obligation is recorded once but a changed set is recorded again.
func (controller *ArchitectureController) recordObligation(
	projectID int64,
	assessment *ArchitectureAssessment,
) (bool, error) {
	ids := make([]int64, 0, len(assessment.Discoveries))
	fingerprint := make([]string, 0, len(assessment.Discoveries)+1)
	for _, discovery := range assessment.Discoveries {
		ids = append(ids, discovery.ID)
		fingerprint = append(fingerprint, fmt.Sprintf("%d", discovery.ID))
	}
	if assessment.ManagerRequested {
		fingerprint = append(fingerprint, "manager")
	}
	event := domain.NewWorkflowEvent(
		projectID,
		domain.WorkflowSourceController,
		domain.WorkflowArchitectureReviewRequired,
		assessment.Reason,
	)
	data, err := json.Marshal(map[string]any{
		"manager_requested": assessment.ManagerRequested,
		"discovery_ids":     ids,
	})
	if err != nil {
		return false, fmt.Errorf("encode architecture assessment: %w", err)
	}
	event.Data = data
	event.IdempotencyKey = "architecture-review:" + strings.Join(fingerprint, ",")
	_, created, err := controller.store.AppendWorkflowEvent(event)
	if err != nil {
		return false, err
	}
	return created, nil
}

var _ ArchitectureStore = (*store.Store)(nil)
