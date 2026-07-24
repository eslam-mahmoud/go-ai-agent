package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

var ErrProjectTaskSelectionConflict = errors.New("project task selection conflict")

type ProjectNextTaskSelection struct {
	ProjectID       int64
	ManagerReviewID int64
	TaskID          int64
	ExpectedStatus  domain.TaskStatus
	ProjectState    domain.ProjectState
	Reason          string
}

// SelectProjectNextTask promotes exactly one Manager-authorized task to
// `selected`, persisting its reason, the derived project state, and the audit
// fact in one transaction. Re-applying the same review mutates nothing and
// reports applied=false.
func (s *Store) SelectProjectNextTask(
	selection ProjectNextTaskSelection,
) (*domain.Task, bool, error) {
	reason := strings.TrimSpace(selection.Reason)
	switch {
	case selection.ProjectID <= 0 || selection.ManagerReviewID <= 0 || selection.TaskID <= 0:
		return nil, false, fmt.Errorf(
			"%w: project, manager review, and task IDs must be positive",
			domain.ErrInvalidTask,
		)
	case !selection.ExpectedStatus.Valid():
		return nil, false, fmt.Errorf("%w: expected status must be valid", domain.ErrInvalidTask)
	case !selection.ProjectState.Valid():
		return nil, false, fmt.Errorf("%w: project state must be valid", domain.ErrInvalidProject)
	case reason == "":
		return nil, false, fmt.Errorf("%w: selection reason is required", domain.ErrInvalidTask)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, false, fmt.Errorf("begin next task selection: %w", err)
	}
	defer tx.Rollback()

	var projectState string
	var currentTaskID sql.NullInt64
	err = tx.QueryRow(`
		SELECT state, current_task_id FROM projects WHERE id = ?
	`, selection.ProjectID).Scan(&projectState, &currentTaskID)
	if err == sql.ErrNoRows {
		return nil, false, fmt.Errorf("%w: ID %d", ErrProjectNotFound, selection.ProjectID)
	}
	if err != nil {
		return nil, false, fmt.Errorf("read project for next task selection: %w", err)
	}
	if domain.ProjectState(projectState) == domain.ProjectPaused {
		return nil, false, fmt.Errorf("%w: project %d", ErrProjectPaused, selection.ProjectID)
	}

	var storedProjectID int64
	var storedStatus, storedReason string
	var issueNumber int
	err = tx.QueryRow(`
		SELECT project_id, status, issue_number, selected_reason
		FROM project_tasks WHERE id = ?
	`, selection.TaskID).Scan(&storedProjectID, &storedStatus, &issueNumber, &storedReason)
	if err == sql.ErrNoRows {
		return nil, false, fmt.Errorf("%w: ID %d", ErrProjectTaskNotFound, selection.TaskID)
	}
	if err != nil {
		return nil, false, fmt.Errorf("read task for next task selection: %w", err)
	}
	if storedProjectID != selection.ProjectID {
		return nil, false, fmt.Errorf(
			"%w: task %d belongs to project %d, not %d",
			domain.ErrInvalidTask,
			selection.TaskID,
			storedProjectID,
			selection.ProjectID,
		)
	}

	if err := requireLatestSelectionAuthority(tx, selection, issueNumber); err != nil {
		return nil, false, err
	}

	idempotencyKey := fmt.Sprintf("manager-review:%d:next-task", selection.ManagerReviewID)
	if domain.TaskStatus(storedStatus) == domain.TaskSelected {
		replayed, err := selectionAlreadyApplied(
			tx,
			selection,
			idempotencyKey,
			currentTaskID,
			storedReason,
			reason,
		)
		if err != nil {
			return nil, false, err
		}
		if !replayed {
			return nil, false, fmt.Errorf(
				"%w: task %d is already selected by another decision",
				ErrProjectTaskSelectionConflict,
				selection.TaskID,
			)
		}
		// Release the single writer connection before reading back.
		if err := tx.Rollback(); err != nil {
			return nil, false, fmt.Errorf("close replayed next task selection: %w", err)
		}
		task, err := s.GetProjectTaskByID(selection.TaskID)
		return task, false, err
	}
	if domain.TaskStatus(storedStatus) != selection.ExpectedStatus {
		return nil, false, fmt.Errorf(
			"%w: task %d is %q, expected %q",
			ErrProjectTaskSelectionConflict,
			selection.TaskID,
			storedStatus,
			selection.ExpectedStatus,
		)
	}
	if err := requireIdleDeliveryLane(tx, selection.TaskID); err != nil {
		return nil, false, err
	}

	now := time.Now().UTC()
	result, err := tx.Exec(`
		UPDATE project_tasks
		SET status = ?, selected_reason = ?, updated_at = ?
		WHERE id = ? AND project_id = ? AND status = ?
	`,
		string(domain.TaskSelected),
		reason,
		now,
		selection.TaskID,
		selection.ProjectID,
		string(selection.ExpectedStatus),
	)
	if err != nil {
		return nil, false, fmt.Errorf("select next task: %w", classifyActiveTaskConstraint(err))
	}
	if changed, err := result.RowsAffected(); err != nil {
		return nil, false, fmt.Errorf("read selected task count: %w", err)
	} else if changed != 1 {
		return nil, false, fmt.Errorf(
			"%w: task %d changed while being selected",
			ErrProjectTaskSelectionConflict,
			selection.TaskID,
		)
	}

	result, err = tx.Exec(`
		UPDATE projects
		SET state = ?, current_task_id = ?, updated_at = ?
		WHERE id = ?
	`, string(selection.ProjectState), selection.TaskID, now, selection.ProjectID)
	if err != nil {
		return nil, false, fmt.Errorf("update project from next task selection: %w", err)
	}
	if changed, err := result.RowsAffected(); err != nil {
		return nil, false, fmt.Errorf("read selected project count: %w", err)
	} else if changed != 1 {
		return nil, false, fmt.Errorf("%w: ID %d", ErrProjectNotFound, selection.ProjectID)
	}

	taskID := selection.TaskID
	if err := appendWorkflowFactTx(
		tx,
		selection.ProjectID,
		&taskID,
		nil,
		domain.WorkflowSourceController,
		domain.WorkflowTaskSelected,
		"Engineering Manager selected the next task.",
		map[string]any{
			"manager_review_id": selection.ManagerReviewID,
			"from_status":       selection.ExpectedStatus,
			"to_status":         domain.TaskSelected,
			"issue_number":      issueNumber,
			"project_state":     selection.ProjectState,
			"reason":            reason,
		},
		idempotencyKey,
	); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("commit next task selection: %w", err)
	}
	task, err := s.GetProjectTaskByID(selection.TaskID)
	return task, true, err
}

// requireLatestSelectionAuthority rejects a decision that no longer reflects
// the project's newest Manager review, or that names a different task.
func requireLatestSelectionAuthority(
	tx *sql.Tx,
	selection ProjectNextTaskSelection,
	issueNumber int,
) error {
	var latestReviewID int64
	var nextTaskID sql.NullInt64
	var nextIssueNumber int
	err := tx.QueryRow(`
		SELECT id, next_task_id, next_task_issue_number
		FROM manager_reviews
		WHERE project_id = ?
		ORDER BY id DESC LIMIT 1
	`, selection.ProjectID).Scan(&latestReviewID, &nextTaskID, &nextIssueNumber)
	if err == sql.ErrNoRows || (err == nil && latestReviewID != selection.ManagerReviewID) {
		return fmt.Errorf(
			"%w: manager review %d is not latest",
			ErrProjectTaskSelectionConflict,
			selection.ManagerReviewID,
		)
	}
	if err != nil {
		return fmt.Errorf("read latest manager review: %w", err)
	}
	if nextTaskID.Valid {
		if nextTaskID.Int64 != selection.TaskID {
			return fmt.Errorf(
				"%w: manager review %d selected task %d, not %d",
				ErrProjectTaskSelectionConflict,
				selection.ManagerReviewID,
				nextTaskID.Int64,
				selection.TaskID,
			)
		}
		return nil
	}
	if nextIssueNumber <= 0 || nextIssueNumber != issueNumber {
		return fmt.Errorf(
			"%w: manager review %d does not select task %d",
			ErrProjectTaskSelectionConflict,
			selection.ManagerReviewID,
			selection.TaskID,
		)
	}
	return nil
}

// selectionAlreadyApplied reports whether an already-selected task is the
// durable result of this exact decision, which makes reapplying it a no-op.
func selectionAlreadyApplied(
	tx *sql.Tx,
	selection ProjectNextTaskSelection,
	idempotencyKey string,
	currentTaskID sql.NullInt64,
	storedReason, reason string,
) (bool, error) {
	if !currentTaskID.Valid ||
		currentTaskID.Int64 != selection.TaskID ||
		storedReason != reason {
		return false, nil
	}
	var eventID int64
	err := tx.QueryRow(`
		SELECT id FROM workflow_events
		WHERE project_id = ? AND idempotency_key = ?
	`, selection.ProjectID, idempotencyKey).Scan(&eventID)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("find recorded task selection: %w", err)
	}
	return true, nil
}

// requireIdleDeliveryLane enforces the single-active-task invariant before the
// unique index would reject the write with an opaque constraint error.
func requireIdleDeliveryLane(tx *sql.Tx, taskID int64) error {
	var activeID int64
	var activeStatus string
	err := tx.QueryRow(`
		SELECT id, status FROM project_tasks
		WHERE id <> ? AND status IN (
			'selected', 'planning', 'waiting-input', 'developing',
			'reviewing', 'fixing', 'verifying', 'waiting-ci', 'blocked'
		)
		ORDER BY id ASC LIMIT 1
	`, taskID).Scan(&activeID, &activeStatus)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check active delivery lane: %w", err)
	}
	return fmt.Errorf(
		"%w: task %d is %q",
		ErrActiveProjectTaskExists,
		activeID,
		activeStatus,
	)
}
