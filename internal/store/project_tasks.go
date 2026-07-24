package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

var (
	ErrProjectTaskAlreadyExists = errors.New("project task already exists")
	ErrProjectTaskPositionTaken = errors.New("project task position is occupied")
	ErrProjectTaskNotFound      = errors.New("project task not found")
	ErrInvalidProjectTaskOrder  = errors.New("invalid project task order")
)

// CreateProjectTask persists a task. Sequence zero atomically appends it to
// the project's current backlog; a positive sequence must be unoccupied.
func (s *Store) CreateProjectTask(task *domain.Task) (*domain.Task, error) {
	if err := task.Validate(); err != nil {
		return nil, fmt.Errorf("create project task: %w", err)
	}
	if task.ID != 0 {
		return nil, fmt.Errorf("%w: new task ID must be zero", domain.ErrInvalidTask)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin create project task: %w", err)
	}
	defer tx.Rollback()
	if err := requireProject(tx, task.ProjectID); err != nil {
		return nil, err
	}

	sequence := task.Sequence
	if sequence == 0 {
		if err := tx.QueryRow(`
			SELECT COALESCE(MAX(sequence), 0) + 1
			FROM project_tasks
			WHERE project_id = ?
		`, task.ProjectID).Scan(&sequence); err != nil {
			return nil, fmt.Errorf("choose project task sequence: %w", err)
		}
	} else if occupied, err := taskPositionOccupied(tx, task.ProjectID, sequence, 0); err != nil {
		return nil, err
	} else if occupied {
		return nil, fmt.Errorf(
			"%w: project %d sequence %d",
			ErrProjectTaskPositionTaken,
			task.ProjectID,
			sequence,
		)
	}
	if task.IssueNumber > 0 {
		if duplicate, err := taskIssueExists(tx, task.ProjectID, task.IssueNumber, 0); err != nil {
			return nil, err
		} else if duplicate {
			return nil, fmt.Errorf(
				"%w: project %d issue %d",
				ErrProjectTaskAlreadyExists,
				task.ProjectID,
				task.IssueNumber,
			)
		}
	}

	now := time.Now().UTC()
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
		sequence,
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
		return nil, fmt.Errorf("insert project task: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("read project task ID: %w", err)
	}
	if err := touchProject(tx, task.ProjectID, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit create project task: %w", err)
	}
	return s.GetProjectTaskByID(id)
}

func (s *Store) GetProjectTaskByID(id int64) (*domain.Task, error) {
	return scanProjectTask(s.db.QueryRow(projectTaskSelect+` WHERE id = ?`, id))
}

func (s *Store) GetProjectTaskByIssue(projectID int64, issueNumber int) (*domain.Task, error) {
	if issueNumber <= 0 {
		return nil, nil
	}
	return scanProjectTask(s.db.QueryRow(
		projectTaskSelect+` WHERE project_id = ? AND issue_number = ?`,
		projectID,
		issueNumber,
	))
}

// UpdateProjectTask replaces all mutable fields. Moving a task between
// projects is not supported, and sequence zero is reserved for create/append.
func (s *Store) UpdateProjectTask(task *domain.Task) (*domain.Task, error) {
	if err := task.Validate(); err != nil {
		return nil, fmt.Errorf("update project task: %w", err)
	}
	if task.ID <= 0 {
		return nil, fmt.Errorf("%w: persisted task ID must be positive", domain.ErrInvalidTask)
	}
	if task.Sequence == 0 {
		return nil, fmt.Errorf("%w: persisted task sequence must be positive", domain.ErrInvalidTask)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin update project task: %w", err)
	}
	defer tx.Rollback()

	var storedProjectID int64
	err = tx.QueryRow(`SELECT project_id FROM project_tasks WHERE id = ?`, task.ID).Scan(&storedProjectID)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%w: ID %d", ErrProjectTaskNotFound, task.ID)
	}
	if err != nil {
		return nil, fmt.Errorf("read project task identity: %w", err)
	}
	if storedProjectID != task.ProjectID {
		return nil, fmt.Errorf(
			"%w: task %d belongs to project %d, not %d",
			domain.ErrInvalidTask,
			task.ID,
			storedProjectID,
			task.ProjectID,
		)
	}
	if occupied, err := taskPositionOccupied(tx, task.ProjectID, task.Sequence, task.ID); err != nil {
		return nil, err
	} else if occupied {
		return nil, fmt.Errorf(
			"%w: project %d sequence %d",
			ErrProjectTaskPositionTaken,
			task.ProjectID,
			task.Sequence,
		)
	}
	if task.IssueNumber > 0 {
		if duplicate, err := taskIssueExists(tx, task.ProjectID, task.IssueNumber, task.ID); err != nil {
			return nil, err
		} else if duplicate {
			return nil, fmt.Errorf(
				"%w: project %d issue %d",
				ErrProjectTaskAlreadyExists,
				task.ProjectID,
				task.IssueNumber,
			)
		}
	}

	now := time.Now().UTC()
	result, err := tx.Exec(`
		UPDATE project_tasks SET
			issue_number = ?,
			title = ?,
			goal = ?,
			status = ?,
			priority = ?,
			sequence = ?,
			task_type = ?,
			source = ?,
			source_discovery_id = ?,
			blocks_release = ?,
			selected_reason = ?,
			branch_name = ?,
			pr_number = ?,
			dependency_state = ?,
			updated_at = ?
		WHERE id = ? AND project_id = ?
	`,
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
		task.ID,
		task.ProjectID,
	)
	if err != nil {
		return nil, fmt.Errorf("update project task: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("read project task update count: %w", err)
	}
	if updated != 1 {
		return nil, fmt.Errorf("%w: ID %d", ErrProjectTaskNotFound, task.ID)
	}
	if err := touchProject(tx, task.ProjectID, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit update project task: %w", err)
	}
	return s.GetProjectTaskByID(task.ID)
}

// ListProjectTasks returns the project's backlog in sequence order.
func (s *Store) ListProjectTasks(projectID int64) ([]*domain.Task, error) {
	if project, err := s.GetProjectByID(projectID); err != nil {
		return nil, err
	} else if project == nil {
		return nil, fmt.Errorf("%w: ID %d", ErrProjectNotFound, projectID)
	}
	rows, err := s.db.Query(
		projectTaskSelect+` WHERE project_id = ? ORDER BY sequence ASC, id ASC`,
		projectID,
	)
	if err != nil {
		return nil, fmt.Errorf("list project tasks: %w", err)
	}
	defer rows.Close()

	var tasks []*domain.Task
	for rows.Next() {
		task, err := scanProjectTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate project tasks: %w", err)
	}
	return tasks, nil
}

// ReorderProjectTasks atomically replaces the complete backlog order. Every
// existing task must appear exactly once, and all IDs must belong to projectID.
func (s *Store) ReorderProjectTasks(projectID int64, orderedTaskIDs []int64) ([]*domain.Task, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin project task reorder: %w", err)
	}
	defer tx.Rollback()
	if err := requireProject(tx, projectID); err != nil {
		return nil, err
	}

	rows, err := tx.Query(`
		SELECT id, sequence
		FROM project_tasks
		WHERE project_id = ?
		ORDER BY sequence ASC, id ASC
	`, projectID)
	if err != nil {
		return nil, fmt.Errorf("read project task order: %w", err)
	}
	existing := make(map[int64]struct{})
	maxSequence := 0
	for rows.Next() {
		var id int64
		var sequence int
		if err := rows.Scan(&id, &sequence); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan project task order: %w", err)
		}
		existing[id] = struct{}{}
		if sequence > maxSequence {
			maxSequence = sequence
		}
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close project task order: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate project task order: %w", err)
	}

	if len(existing) != len(orderedTaskIDs) {
		return nil, fmt.Errorf(
			"%w: got %d IDs for %d tasks",
			ErrInvalidProjectTaskOrder,
			len(orderedTaskIDs),
			len(existing),
		)
	}
	seen := make(map[int64]struct{}, len(orderedTaskIDs))
	for _, id := range orderedTaskIDs {
		if _, duplicate := seen[id]; duplicate {
			return nil, fmt.Errorf("%w: duplicate task ID %d", ErrInvalidProjectTaskOrder, id)
		}
		if _, belongs := existing[id]; !belongs {
			return nil, fmt.Errorf(
				"%w: task ID %d does not belong to project %d",
				ErrInvalidProjectTaskOrder,
				id,
				projectID,
			)
		}
		seen[id] = struct{}{}
	}

	now := time.Now().UTC()
	temporaryBase := maxSequence + len(orderedTaskIDs) + 1
	for index, id := range orderedTaskIDs {
		if _, err := tx.Exec(`
			UPDATE project_tasks SET sequence = ?, updated_at = ?
			WHERE id = ? AND project_id = ?
		`, temporaryBase+index, now, id, projectID); err != nil {
			return nil, fmt.Errorf("stage project task reorder: %w", err)
		}
	}
	for index, id := range orderedTaskIDs {
		if _, err := tx.Exec(`
			UPDATE project_tasks SET sequence = ?, updated_at = ?
			WHERE id = ? AND project_id = ?
		`, index+1, now, id, projectID); err != nil {
			return nil, fmt.Errorf("apply project task reorder: %w", err)
		}
	}
	if err := touchProject(tx, projectID, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit project task reorder: %w", err)
	}
	return s.ListProjectTasks(projectID)
}

const projectTaskSelect = `
	SELECT
		id, project_id, issue_number, title, goal, status, priority, sequence,
		task_type, source, source_discovery_id, blocks_release,
		selected_reason, branch_name, pr_number, dependency_state,
		created_at, updated_at
	FROM project_tasks
`

func scanProjectTask(row scanner) (*domain.Task, error) {
	var (
		task              domain.Task
		status            string
		sourceDiscoveryID sql.NullInt64
		blocksRelease     int
	)
	if err := row.Scan(
		&task.ID,
		&task.ProjectID,
		&task.IssueNumber,
		&task.Title,
		&task.Goal,
		&status,
		&task.Priority,
		&task.Sequence,
		&task.TaskType,
		&task.Source,
		&sourceDiscoveryID,
		&blocksRelease,
		&task.SelectedReason,
		&task.BranchName,
		&task.PRNumber,
		&task.DependencyState,
		&task.CreatedAt,
		&task.UpdatedAt,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("scan project task: %w", err)
	}
	task.Status = domain.TaskStatus(status)
	task.BlocksRelease = blocksRelease != 0
	if sourceDiscoveryID.Valid {
		value := sourceDiscoveryID.Int64
		task.SourceDiscoveryID = &value
	}
	task.CreatedAt = task.CreatedAt.UTC()
	task.UpdatedAt = task.UpdatedAt.UTC()
	return &task, nil
}

func requireProject(tx *sql.Tx, projectID int64) error {
	var exists int
	err := tx.QueryRow(`SELECT 1 FROM projects WHERE id = ?`, projectID).Scan(&exists)
	if err == sql.ErrNoRows {
		return fmt.Errorf("%w: ID %d", ErrProjectNotFound, projectID)
	}
	if err != nil {
		return fmt.Errorf("check project: %w", err)
	}
	return nil
}

func taskPositionOccupied(tx *sql.Tx, projectID int64, sequence int, excludingID int64) (bool, error) {
	var exists int
	err := tx.QueryRow(`
		SELECT 1 FROM project_tasks
		WHERE project_id = ? AND sequence = ? AND id <> ?
	`, projectID, sequence, excludingID).Scan(&exists)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check project task sequence: %w", err)
	}
	return true, nil
}

func taskIssueExists(tx *sql.Tx, projectID int64, issueNumber int, excludingID int64) (bool, error) {
	var exists int
	err := tx.QueryRow(`
		SELECT 1 FROM project_tasks
		WHERE project_id = ? AND issue_number = ? AND id <> ?
	`, projectID, issueNumber, excludingID).Scan(&exists)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check project task issue: %w", err)
	}
	return true, nil
}

func touchProject(tx *sql.Tx, projectID int64, updatedAt time.Time) error {
	result, err := tx.Exec(`UPDATE projects SET updated_at = ? WHERE id = ?`, updatedAt, projectID)
	if err != nil {
		return fmt.Errorf("touch project: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read project touch count: %w", err)
	}
	if updated != 1 {
		return fmt.Errorf("%w: ID %d", ErrProjectNotFound, projectID)
	}
	return nil
}
