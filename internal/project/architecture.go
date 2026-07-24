package project

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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
}

// ArchitectureController raises the architecture-review obligation. It does not
// run Architect mode; it decides when one is owed and blocks work until then.
type ArchitectureController struct {
	store ArchitectureStore
}

func NewArchitectureController(
	architectureStore ArchitectureStore,
) (*ArchitectureController, error) {
	if architectureStore == nil {
		return nil, errors.New("architecture controller store is required")
	}
	return &ArchitectureController{store: architectureStore}, nil
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
