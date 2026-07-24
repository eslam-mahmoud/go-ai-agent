package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

var ErrDiscoveryTaskExists = errors.New("discovery already produced a task")

type DiscoveryTaskInsert struct {
	ProjectID   int64
	DiscoveryID int64
	Task        *domain.Task
	// Front places the task at the first position it may safely occupy;
	// otherwise it is appended after the existing backlog.
	Front bool
}

// InsertDiscoveryTask adds one accepted discovery to the ordered backlog and
// renumbers the tasks it displaced, in a single transaction.
func (s *Store) InsertDiscoveryTask(
	insert DiscoveryTaskInsert,
) (*domain.Task, error) {
	if insert.ProjectID <= 0 || insert.DiscoveryID <= 0 {
		return nil, fmt.Errorf(
			"%w: project and discovery IDs must be positive",
			domain.ErrInvalidDiscovery,
		)
	}
	if insert.Task == nil {
		return nil, fmt.Errorf("%w: task is required", domain.ErrInvalidTask)
	}
	task := *insert.Task
	task.ProjectID = insert.ProjectID
	task.SourceDiscoveryID = &insert.DiscoveryID
	if err := task.Validate(); err != nil {
		return nil, fmt.Errorf("insert discovery task: %w", err)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin insert discovery task: %w", err)
	}
	defer tx.Rollback()
	if err := requireProject(tx, insert.ProjectID); err != nil {
		return nil, err
	}
	if err := requireDiscoveryWithoutTask(tx, insert.ProjectID, insert.DiscoveryID); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	sequence, err := reserveBacklogPosition(tx, insert.ProjectID, insert.Front, now)
	if err != nil {
		return nil, err
	}
	task.Sequence = sequence
	id, err := insertBacklogTask(tx, &task, now)
	if err != nil {
		return nil, err
	}
	if err := touchProject(tx, insert.ProjectID, now); err != nil {
		return nil, err
	}
	if err := appendWorkflowFactTx(
		tx,
		insert.ProjectID,
		&id,
		nil,
		domain.WorkflowSourceController,
		domain.WorkflowDiscoveryQueued,
		fmt.Sprintf("Discovery %d entered the backlog at position %d.", insert.DiscoveryID, sequence),
		map[string]any{
			"discovery_id": insert.DiscoveryID,
			"task_id":      id,
			"issue_number": task.IssueNumber,
			"sequence":     sequence,
			"front":        insert.Front,
		},
		fmt.Sprintf("discovery:%d:task", insert.DiscoveryID),
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit insert discovery task: %w", err)
	}
	return s.GetProjectTaskByID(id)
}

// requireDiscoveryWithoutTask makes one-task-per-discovery a transactional
// guarantee rather than a check the caller has to remember.
func requireDiscoveryWithoutTask(tx *sql.Tx, projectID, discoveryID int64) error {
	var storedProjectID int64
	var status string
	err := tx.QueryRow(`
		SELECT project_id, status FROM discoveries WHERE id = ?
	`, discoveryID).Scan(&storedProjectID, &status)
	if err == sql.ErrNoRows {
		return fmt.Errorf("%w: ID %d", ErrDiscoveryNotFound, discoveryID)
	}
	if err != nil {
		return fmt.Errorf("read discovery for backlog insert: %w", err)
	}
	if storedProjectID != projectID {
		return fmt.Errorf(
			"%w: discovery %d belongs to project %d, not %d",
			ErrDiscoveryOwnership,
			discoveryID,
			storedProjectID,
			projectID,
		)
	}
	var existingTaskID int64
	err = tx.QueryRow(`
		SELECT id FROM project_tasks
		WHERE project_id = ? AND source_discovery_id = ?
	`, projectID, discoveryID).Scan(&existingTaskID)
	if err == nil {
		return fmt.Errorf(
			"%w: discovery %d is task %d",
			ErrDiscoveryTaskExists,
			discoveryID,
			existingTaskID,
		)
	}
	if err != sql.ErrNoRows {
		return fmt.Errorf("check discovery task: %w", err)
	}
	return nil
}

// reserveBacklogPosition opens a slot for a new task. Front insertion starts
// at the first position not held by a terminal or active task, since neither
// may be displaced; otherwise the task is appended.
func reserveBacklogPosition(
	tx *sql.Tx,
	projectID int64,
	front bool,
	now time.Time,
) (int, error) {
	rows, err := tx.Query(`
		SELECT id, sequence, status FROM project_tasks
		WHERE project_id = ?
		ORDER BY sequence ASC, id ASC
	`, projectID)
	if err != nil {
		return 0, fmt.Errorf("read backlog for insert: %w", err)
	}
	type backlogRow struct {
		id       int64
		sequence int
		status   domain.TaskStatus
	}
	var backlog []backlogRow
	for rows.Next() {
		var row backlogRow
		if err := rows.Scan(&row.id, &row.sequence, &row.status); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan backlog for insert: %w", err)
		}
		backlog = append(backlog, row)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close backlog for insert: %w", err)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate backlog for insert: %w", err)
	}

	target := len(backlog) + 1
	if front {
		target = 1
		for _, row := range backlog {
			if row.status == domain.TaskProposed || row.status == domain.TaskQueued {
				break
			}
			target++
		}
	}
	// Shift displaced tasks down, highest sequence first so no two rows ever
	// hold the same position mid-transaction.
	for index := len(backlog) - 1; index >= 0; index-- {
		row := backlog[index]
		if row.sequence < target {
			continue
		}
		if _, err := tx.Exec(`
			UPDATE project_tasks SET sequence = ?, updated_at = ?
			WHERE id = ? AND project_id = ?
		`, row.sequence+1, now, row.id, projectID); err != nil {
			return 0, fmt.Errorf("shift backlog for insert: %w", err)
		}
	}
	return target, nil
}

func insertBacklogTask(tx *sql.Tx, task *domain.Task, now time.Time) (int64, error) {
	result, err := tx.Exec(`
		INSERT INTO project_tasks (
			project_id, issue_number, title, goal, status, priority, sequence,
			task_type, source, source_discovery_id, blocks_release,
			selected_reason, branch_name, pr_number, dependency_state,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		task.ProjectID,
		task.IssueNumber,
		task.Title,
		task.Goal,
		string(task.Status),
		task.Priority,
		task.Sequence,
		task.TaskType,
		task.Source,
		nullableInt64(task.SourceDiscoveryID),
		task.BlocksRelease,
		task.SelectedReason,
		task.BranchName,
		task.PRNumber,
		task.DependencyState,
		now,
		now,
	)
	if err != nil {
		return 0, fmt.Errorf("insert discovery task: %w", classifyActiveTaskConstraint(err))
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("read discovery task ID: %w", err)
	}
	return id, nil
}
