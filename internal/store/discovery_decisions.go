package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

var (
	ErrDiscoveryDecisionConflict = errors.New("discovery decision conflict")
	ErrDiscoveryUnevaluated      = errors.New("discoveries remain unevaluated")
)

type DiscoveryDecisionRecord struct {
	DiscoveryID int64                    `json:"discovery_id"`
	Decision    domain.DiscoveryDecision `json:"decision"`
	Status      domain.DiscoveryStatus   `json:"status"`
	TaskID      *int64                   `json:"task_id,omitempty"`
	Reason      string                   `json:"reason"`
}

type DiscoveryDecisionUpdate struct {
	ProjectID       int64
	ManagerReviewID int64
	Decisions       []DiscoveryDecisionRecord
}

// ApplyDiscoveryDecisions records the manager's verdicts in one transaction.
// A discovery that was already evaluated is a conflict, not a silent
// overwrite, so two reviews can never fight over the same finding.
func (s *Store) ApplyDiscoveryDecisions(
	update DiscoveryDecisionUpdate,
) ([]*domain.Discovery, error) {
	if update.ProjectID <= 0 || update.ManagerReviewID <= 0 {
		return nil, fmt.Errorf(
			"%w: project and manager review IDs must be positive",
			domain.ErrInvalidDiscovery,
		)
	}
	if len(update.Decisions) == 0 {
		return nil, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin discovery decisions: %w", err)
	}
	defer tx.Rollback()
	if err := requireProject(tx, update.ProjectID); err != nil {
		return nil, err
	}
	if err := requireLatestManagerReview(tx, update.ProjectID, update.ManagerReviewID); err != nil {
		return nil, err
	}
	if applied, err := discoveryDecisionsAlreadyApplied(tx, update); err != nil {
		return nil, err
	} else if applied {
		if err := tx.Rollback(); err != nil {
			return nil, fmt.Errorf("close replayed discovery decisions: %w", err)
		}
		return s.loadDiscoveries(decisionIDs(update.Decisions))
	}

	now := time.Now().UTC()
	for index, decision := range update.Decisions {
		if err := applyOneDiscoveryDecision(tx, update.ProjectID, decision, index, now); err != nil {
			return nil, err
		}
	}
	if err := touchProject(tx, update.ProjectID, now); err != nil {
		return nil, err
	}
	if err := appendWorkflowFactTx(
		tx,
		update.ProjectID,
		nil,
		nil,
		domain.WorkflowSourceController,
		domain.WorkflowDiscoveriesDecided,
		fmt.Sprintf("Engineering Manager decided %d discovery/discoveries.", len(update.Decisions)),
		map[string]any{
			"manager_review_id": update.ManagerReviewID,
			"decisions":         update.Decisions,
		},
		fmt.Sprintf("manager-review:%d:discovery-decisions", update.ManagerReviewID),
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit discovery decisions: %w", err)
	}
	return s.loadDiscoveries(decisionIDs(update.Decisions))
}

func applyOneDiscoveryDecision(
	tx *sql.Tx,
	projectID int64,
	decision DiscoveryDecisionRecord,
	index int,
	now time.Time,
) error {
	if decision.DiscoveryID <= 0 || !decision.Decision.Valid() || !decision.Status.Valid() {
		return fmt.Errorf(
			"%w: decision %d is incomplete",
			domain.ErrInvalidDiscovery,
			index,
		)
	}
	var storedProjectID int64
	var storedStatus string
	err := tx.QueryRow(`
		SELECT project_id, status FROM discoveries WHERE id = ?
	`, decision.DiscoveryID).Scan(&storedProjectID, &storedStatus)
	if err == sql.ErrNoRows {
		return fmt.Errorf("%w: ID %d", ErrDiscoveryNotFound, decision.DiscoveryID)
	}
	if err != nil {
		return fmt.Errorf("read discovery for decision: %w", err)
	}
	if storedProjectID != projectID {
		return fmt.Errorf(
			"%w: discovery %d belongs to project %d, not %d",
			ErrDiscoveryOwnership,
			decision.DiscoveryID,
			storedProjectID,
			projectID,
		)
	}
	if domain.DiscoveryStatus(storedStatus).Evaluated() {
		return fmt.Errorf(
			"%w: discovery %d is already %q",
			ErrDiscoveryDecisionConflict,
			decision.DiscoveryID,
			storedStatus,
		)
	}
	if decision.TaskID != nil {
		if err := requireNonTerminalTask(tx, projectID, *decision.TaskID); err != nil {
			return err
		}
	}
	result, err := tx.Exec(`
		UPDATE discoveries
		SET status = ?, decision = ?, decision_reason = ?,
			linked_task_id = COALESCE(?, linked_task_id), updated_at = ?
		WHERE id = ? AND project_id = ? AND status = 'unevaluated'
	`,
		string(decision.Status),
		string(decision.Decision),
		decision.Reason,
		nullableInt64(decision.TaskID),
		now,
		decision.DiscoveryID,
		projectID,
	)
	if err != nil {
		return fmt.Errorf("apply discovery decision: %w", classifyDiscoveryConstraint(err))
	}
	if changed, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("read discovery decision count: %w", err)
	} else if changed != 1 {
		return fmt.Errorf(
			"%w: discovery %d changed while being decided",
			ErrDiscoveryDecisionConflict,
			decision.DiscoveryID,
		)
	}
	return nil
}

// requireNonTerminalTask guards merge decisions: work cannot be merged into a
// task that will never run again.
func requireNonTerminalTask(tx *sql.Tx, projectID, taskID int64) error {
	var status string
	err := tx.QueryRow(`
		SELECT status FROM project_tasks WHERE id = ? AND project_id = ?
	`, taskID, projectID).Scan(&status)
	if err == sql.ErrNoRows {
		return fmt.Errorf(
			"%w: task %d is not in project %d",
			ErrProjectTaskNotFound,
			taskID,
			projectID,
		)
	}
	if err != nil {
		return fmt.Errorf("read discovery decision task: %w", err)
	}
	switch domain.TaskStatus(status) {
	case domain.TaskCompleted, domain.TaskCancelled, domain.TaskDeferred:
		return fmt.Errorf(
			"%w: task %d is %q and cannot absorb a discovery",
			ErrDiscoveryDecisionConflict,
			taskID,
			status,
		)
	}
	return nil
}

func requireLatestManagerReview(tx *sql.Tx, projectID, reviewID int64) error {
	var latest int64
	err := tx.QueryRow(`
		SELECT id FROM manager_reviews
		WHERE project_id = ? ORDER BY id DESC LIMIT 1
	`, projectID).Scan(&latest)
	if err == sql.ErrNoRows || (err == nil && latest != reviewID) {
		return fmt.Errorf(
			"%w: manager review %d is not latest",
			ErrDiscoveryDecisionConflict,
			reviewID,
		)
	}
	if err != nil {
		return fmt.Errorf("read latest manager review: %w", err)
	}
	return nil
}

func discoveryDecisionsAlreadyApplied(
	tx *sql.Tx,
	update DiscoveryDecisionUpdate,
) (bool, error) {
	var eventID int64
	err := tx.QueryRow(`
		SELECT id FROM workflow_events
		WHERE project_id = ? AND idempotency_key = ?
	`, update.ProjectID, fmt.Sprintf(
		"manager-review:%d:discovery-decisions", update.ManagerReviewID,
	)).Scan(&eventID)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("find recorded discovery decisions: %w", err)
	}
	return true, nil
}

// RequireEvaluatedDiscoveries enforces the plan's invariant that a manager
// review cannot finish while discoveries remain unevaluated.
func (s *Store) RequireEvaluatedDiscoveries(projectID int64) error {
	pending, err := s.ListUnevaluatedDiscoveries(projectID)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(pending))
	for _, discovery := range pending {
		ids = append(ids, discovery.ID)
	}
	return fmt.Errorf("%w: %v", ErrDiscoveryUnevaluated, ids)
}

func (s *Store) loadDiscoveries(ids []int64) ([]*domain.Discovery, error) {
	discoveries := make([]*domain.Discovery, 0, len(ids))
	for _, id := range ids {
		discovery, err := s.GetDiscoveryByID(id)
		if err != nil {
			return nil, err
		}
		discoveries = append(discoveries, discovery)
	}
	return discoveries, nil
}

func decisionIDs(decisions []DiscoveryDecisionRecord) []int64 {
	ids := make([]int64, 0, len(decisions))
	for _, decision := range decisions {
		ids = append(ids, decision.DiscoveryID)
	}
	return ids
}
