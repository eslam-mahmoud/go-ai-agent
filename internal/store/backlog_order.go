package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

var ErrProjectBacklogConflict = errors.New("project backlog changed concurrently")

type ProjectBacklogMove struct {
	TaskID   int64  `json:"task_id"`
	Position int    `json:"position"`
	Reason   string `json:"reason"`
}

type ProjectBacklogOrderUpdate struct {
	ProjectID       int64
	ManagerReviewID int64
	ExpectedTaskIDs []int64
	OrderedTaskIDs  []int64
	Moves           []ProjectBacklogMove
}

// ApplyProjectBacklogOrder atomically compare-and-sets the complete normalized
// task order and records the Manager decision that authorized it.
func (s *Store) ApplyProjectBacklogOrder(
	update ProjectBacklogOrderUpdate,
) ([]*domain.Task, error) {
	if update.ProjectID <= 0 || update.ManagerReviewID <= 0 {
		return nil, fmt.Errorf(
			"%w: project and manager review IDs must be positive",
			ErrInvalidProjectTaskOrder,
		)
	}
	if len(update.ExpectedTaskIDs) != len(update.OrderedTaskIDs) {
		return nil, fmt.Errorf("%w: order lengths differ", ErrInvalidProjectTaskOrder)
	}
	if equalTaskOrder(update.ExpectedTaskIDs, update.OrderedTaskIDs) {
		return nil, fmt.Errorf("%w: replacement order is unchanged", ErrInvalidProjectTaskOrder)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin manager backlog reorder: %w", err)
	}
	defer tx.Rollback()
	if err := requireProject(tx, update.ProjectID); err != nil {
		return nil, err
	}
	var latestReviewID int64
	err = tx.QueryRow(`
		SELECT id FROM manager_reviews
		WHERE project_id = ?
		ORDER BY id DESC LIMIT 1
	`, update.ProjectID).Scan(&latestReviewID)
	if err == sql.ErrNoRows || latestReviewID != update.ManagerReviewID {
		return nil, fmt.Errorf(
			"%w: manager review %d is not latest",
			ErrProjectBacklogConflict,
			update.ManagerReviewID,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("read latest manager review: %w", err)
	}
	current, statuses, maxSequence, err := readBacklogOrder(tx, update.ProjectID)
	if err != nil {
		return nil, err
	}
	if !equalTaskOrder(current, update.ExpectedTaskIDs) {
		return nil, fmt.Errorf("%w: expected order is stale", ErrProjectBacklogConflict)
	}
	if err := validateReplacementOrder(current, update.OrderedTaskIDs, statuses); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	temporaryBase := maxSequence + len(update.OrderedTaskIDs) + 1
	for index, id := range update.OrderedTaskIDs {
		if _, err := tx.Exec(`
			UPDATE project_tasks SET sequence = ?, updated_at = ?
			WHERE id = ? AND project_id = ?
		`, temporaryBase+index, now, id, update.ProjectID); err != nil {
			return nil, fmt.Errorf("stage manager backlog reorder: %w", err)
		}
	}
	for index, id := range update.OrderedTaskIDs {
		if _, err := tx.Exec(`
			UPDATE project_tasks SET sequence = ?, updated_at = ?
			WHERE id = ? AND project_id = ?
		`, index+1, now, id, update.ProjectID); err != nil {
			return nil, fmt.Errorf("apply manager backlog reorder: %w", err)
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
		domain.WorkflowBacklogReordered,
		"Engineering Manager reordered the project backlog.",
		map[string]any{
			"manager_review_id": update.ManagerReviewID,
			"old_order":         current,
			"new_order":         update.OrderedTaskIDs,
			"moves":             update.Moves,
		},
		fmt.Sprintf("manager-review:%d:backlog-order", update.ManagerReviewID),
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit manager backlog reorder: %w", err)
	}
	return s.ListProjectTasks(update.ProjectID)
}

func readBacklogOrder(
	tx *sql.Tx,
	projectID int64,
) ([]int64, map[int64]domain.TaskStatus, int, error) {
	rows, err := tx.Query(`
		SELECT id, sequence, status
		FROM project_tasks
		WHERE project_id = ?
		ORDER BY sequence ASC, id ASC
	`, projectID)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("read manager backlog order: %w", err)
	}
	defer rows.Close()
	var order []int64
	statuses := make(map[int64]domain.TaskStatus)
	maxSequence := 0
	for rows.Next() {
		var id int64
		var sequence int
		var status domain.TaskStatus
		if err := rows.Scan(&id, &sequence, &status); err != nil {
			return nil, nil, 0, fmt.Errorf("scan manager backlog order: %w", err)
		}
		order = append(order, id)
		statuses[id] = status
		if sequence > maxSequence {
			maxSequence = sequence
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, 0, fmt.Errorf("iterate manager backlog order: %w", err)
	}
	return order, statuses, maxSequence, nil
}

func validateReplacementOrder(
	current, replacement []int64,
	statuses map[int64]domain.TaskStatus,
) error {
	if len(current) != len(replacement) {
		return fmt.Errorf("%w: replacement is incomplete", ErrInvalidProjectTaskOrder)
	}
	seen := make(map[int64]struct{}, len(replacement))
	for index, id := range replacement {
		if _, duplicate := seen[id]; duplicate {
			return fmt.Errorf("%w: duplicate task %d", ErrInvalidProjectTaskOrder, id)
		}
		status, exists := statuses[id]
		if !exists {
			return fmt.Errorf("%w: unknown task %d", ErrInvalidProjectTaskOrder, id)
		}
		seen[id] = struct{}{}
		if status != domain.TaskProposed &&
			status != domain.TaskQueued &&
			current[index] != id {
			return fmt.Errorf(
				"%w: task %d in status %q cannot move",
				ErrInvalidProjectTaskOrder,
				id,
				status,
			)
		}
	}
	return nil
}

func equalTaskOrder(left, right []int64) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
