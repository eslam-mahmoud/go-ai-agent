package project

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
)

var ErrInvalidDiscoveryDecisions = errors.New("invalid manager discovery decisions")

type DiscoveryStore interface {
	LoadProjectAggregate(projectID int64) (*store.ProjectAggregate, error)
	ListUnevaluatedDiscoveries(projectID int64) ([]*domain.Discovery, error)
	ApplyDiscoveryDecisions(
		update store.DiscoveryDecisionUpdate,
	) ([]*domain.Discovery, error)
}

type DiscoveryDecisionResult struct {
	Decided []*domain.Discovery
}

// DiscoveryController applies one review's discovery verdicts. It owns the
// coverage invariant; the store owns atomicity.
type DiscoveryController struct {
	store DiscoveryStore
}

func NewDiscoveryController(discoveryStore DiscoveryStore) (*DiscoveryController, error) {
	if discoveryStore == nil {
		return nil, errors.New("discovery controller store is required")
	}
	return &DiscoveryController{store: discoveryStore}, nil
}

// ApplyManagerReview records every verdict in the review. The plan requires a
// review to evaluate all pending discoveries, so an uncovered one is an error
// rather than a silent carry-over.
func (controller *DiscoveryController) ApplyManagerReview(
	projectID, managerReviewID int64,
) (*DiscoveryDecisionResult, error) {
	if projectID <= 0 || managerReviewID <= 0 {
		return nil, fmt.Errorf(
			"%w: project and manager review IDs must be positive",
			ErrInvalidDiscoveryDecisions,
		)
	}
	aggregate, err := controller.store.LoadProjectAggregate(projectID)
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
	pending, err := controller.store.ListUnevaluatedDiscoveries(projectID)
	if err != nil {
		return nil, err
	}
	decisions, err := decodeDiscoveryDecisions(review.DiscoveryDecisions)
	if err != nil {
		return nil, err
	}
	if replayedDiscoveryDecisions(decisions, pending) {
		// Every named discovery was already decided and nothing is waiting:
		// re-applying the same review is a no-op, not a coverage failure.
		return &DiscoveryDecisionResult{}, nil
	}
	records, err := buildDiscoveryDecisionRecords(decisions, pending)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return &DiscoveryDecisionResult{}, nil
	}
	decided, err := controller.store.ApplyDiscoveryDecisions(store.DiscoveryDecisionUpdate{
		ProjectID:       projectID,
		ManagerReviewID: managerReviewID,
		Decisions:       records,
	})
	if err != nil {
		return nil, err
	}
	return &DiscoveryDecisionResult{Decided: decided}, nil
}

type managerDiscoveryDecision struct {
	DiscoveryID int64  `json:"discovery_id"`
	Decision    string `json:"decision"`
	TaskID      *int64 `json:"task_id"`
	Reason      string `json:"reason"`
}

func decodeDiscoveryDecisions(
	raw json.RawMessage,
) ([]managerDiscoveryDecision, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var decisions []managerDiscoveryDecision
	if err := decoder.Decode(&decisions); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidDiscoveryDecisions, err)
	}
	if decisions == nil {
		return nil, fmt.Errorf(
			"%w: decisions must be a JSON array",
			ErrInvalidDiscoveryDecisions,
		)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: trailing JSON", ErrInvalidDiscoveryDecisions)
	}
	return decisions, nil
}

// buildDiscoveryDecisionRecords validates the verdicts against the discoveries
// actually awaiting evaluation, and refuses to leave any of them behind.
func buildDiscoveryDecisionRecords(
	decisions []managerDiscoveryDecision,
	pending []*domain.Discovery,
) ([]store.DiscoveryDecisionRecord, error) {
	pendingByID := make(map[int64]*domain.Discovery, len(pending))
	for _, discovery := range pending {
		pendingByID[discovery.ID] = discovery
	}
	records := make([]store.DiscoveryDecisionRecord, 0, len(decisions))
	covered := make(map[int64]struct{}, len(decisions))
	for index, decision := range decisions {
		if decision.DiscoveryID <= 0 {
			return nil, fmt.Errorf(
				"%w: decision %d requires a discovery ID",
				ErrInvalidDiscoveryDecisions,
				index,
			)
		}
		if _, duplicate := covered[decision.DiscoveryID]; duplicate {
			return nil, fmt.Errorf(
				"%w: discovery %d has duplicate decisions",
				ErrInvalidDiscoveryDecisions,
				decision.DiscoveryID,
			)
		}
		if _, awaiting := pendingByID[decision.DiscoveryID]; !awaiting {
			return nil, fmt.Errorf(
				"%w: discovery %d is not awaiting evaluation in this project",
				ErrInvalidDiscoveryDecisions,
				decision.DiscoveryID,
			)
		}
		verdict := domain.DiscoveryDecision(strings.TrimSpace(decision.Decision))
		status, ok := verdict.Status()
		if !ok {
			return nil, fmt.Errorf(
				"%w: discovery %d has unknown decision %q",
				ErrInvalidDiscoveryDecisions,
				decision.DiscoveryID,
				decision.Decision,
			)
		}
		reason := strings.TrimSpace(decision.Reason)
		if reason == "" {
			return nil, fmt.Errorf(
				"%w: discovery %d requires a reason",
				ErrInvalidDiscoveryDecisions,
				decision.DiscoveryID,
			)
		}
		if verdict == domain.DecisionMergeIntoExisting &&
			(decision.TaskID == nil || *decision.TaskID <= 0) {
			return nil, fmt.Errorf(
				"%w: discovery %d must name the task it merges into",
				ErrInvalidDiscoveryDecisions,
				decision.DiscoveryID,
			)
		}
		covered[decision.DiscoveryID] = struct{}{}
		records = append(records, store.DiscoveryDecisionRecord{
			DiscoveryID: decision.DiscoveryID,
			Decision:    verdict,
			Status:      status,
			TaskID:      decision.TaskID,
			Reason:      reason,
		})
	}
	uncovered := make([]int64, 0)
	for _, discovery := range pending {
		if _, decided := covered[discovery.ID]; !decided {
			uncovered = append(uncovered, discovery.ID)
		}
	}
	if len(uncovered) > 0 {
		return nil, fmt.Errorf(
			"%w: discoveries %v were not evaluated",
			ErrInvalidDiscoveryDecisions,
			uncovered,
		)
	}
	return records, nil
}

var _ DiscoveryStore = (*store.Store)(nil)

// replayedDiscoveryDecisions reports whether this review's verdicts were
// already recorded: nothing awaits evaluation and every named discovery has
// left the pending set.
func replayedDiscoveryDecisions(
	decisions []managerDiscoveryDecision,
	pending []*domain.Discovery,
) bool {
	if len(decisions) == 0 || len(pending) > 0 {
		return false
	}
	return true
}
