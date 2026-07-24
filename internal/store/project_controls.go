package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

var (
	ErrProjectControlConflict = errors.New("project control conflict")
	ErrExecutionRetryConflict = errors.New("execution retry conflict")
)

type ProjectTaskCancellation struct {
	ProjectID            int64
	TaskID               int64
	ExpectedProjectState domain.ProjectState
	ExpectedTaskStatus   domain.TaskStatus
}

type ProjectExecutionRetry struct {
	ProjectID            int64
	TaskID               int64
	ExecutionID          int64
	ExpectedProjectState domain.ProjectState
	ExpectedTaskStatus   domain.TaskStatus
	NewTaskStatus        domain.TaskStatus
}

func (s *Store) PauseProject(projectID int64, expected domain.ProjectState) error {
	if projectID <= 0 || !expected.Valid() || expected == domain.ProjectPaused {
		return fmt.Errorf("%w: invalid pause request", ErrProjectControlConflict)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin project pause: %w", err)
	}
	defer tx.Rollback()

	if err := requireProjectState(tx, projectID, expected); err != nil {
		return err
	}
	if _, err := tx.Exec(`
		UPDATE executions
		SET status = 'interrupted', error_message = 'project paused'
		WHERE project_id = ? AND status = 'running'
	`, projectID); err != nil {
		return fmt.Errorf("interrupt execution for pause: %w", err)
	}
	result, err := tx.Exec(`
		UPDATE projects
		SET state = 'paused', paused_from_state = ?, updated_at = ?
		WHERE id = ? AND state = ?
	`, string(expected), time.Now().UTC(), projectID, string(expected))
	if err != nil {
		return fmt.Errorf("pause project: %w", err)
	}
	if err := requireOneControlUpdate(result, projectID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit project pause: %w", err)
	}
	return nil
}

func (s *Store) ResumeProject(projectID int64, target domain.ProjectState) error {
	if projectID <= 0 || !target.Valid() || target == domain.ProjectPaused {
		return fmt.Errorf("%w: invalid resume request", ErrProjectControlConflict)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin project resume: %w", err)
	}
	defer tx.Rollback()

	var state, pausedFrom string
	err = tx.QueryRow(`
		SELECT state, paused_from_state FROM projects WHERE id = ?
	`, projectID).Scan(&state, &pausedFrom)
	if err == sql.ErrNoRows {
		return fmt.Errorf("%w: ID %d", ErrProjectNotFound, projectID)
	}
	if err != nil {
		return fmt.Errorf("read project resume state: %w", err)
	}
	if domain.ProjectState(state) != domain.ProjectPaused ||
		domain.ProjectState(pausedFrom) != target {
		return fmt.Errorf(
			"%w: project %d is %q with resume state %q",
			ErrProjectControlConflict,
			projectID,
			state,
			pausedFrom,
		)
	}
	result, err := tx.Exec(`
		UPDATE projects
		SET state = ?, paused_from_state = '', updated_at = ?
		WHERE id = ? AND state = 'paused' AND paused_from_state = ?
	`, string(target), time.Now().UTC(), projectID, string(target))
	if err != nil {
		return fmt.Errorf("resume project: %w", err)
	}
	if err := requireOneControlUpdate(result, projectID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit project resume: %w", err)
	}
	return nil
}

func (s *Store) CancelProjectTask(update ProjectTaskCancellation) error {
	if update.ProjectID <= 0 || update.TaskID <= 0 ||
		!update.ExpectedProjectState.Valid() ||
		!update.ExpectedTaskStatus.Valid() {
		return fmt.Errorf("%w: invalid cancellation request", ErrProjectControlConflict)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin task cancellation: %w", err)
	}
	defer tx.Rollback()

	if err := requireCurrentProjectTask(
		tx,
		update.ProjectID,
		update.TaskID,
		update.ExpectedProjectState,
		update.ExpectedTaskStatus,
	); err != nil {
		return err
	}
	now := time.Now().UTC()
	if _, err := tx.Exec(`
		UPDATE executions
		SET
			status = 'cancelled',
			completed_at = CASE WHEN started_at IS NULL THEN NULL ELSE ? END,
			error_class = 'cancelled',
			error_message = 'task cancelled'
		WHERE id = (
			SELECT id FROM executions
			WHERE task_id = ?
			ORDER BY id DESC LIMIT 1
		)
		AND status IN ('pending', 'running', 'interrupted')
	`, now, update.TaskID); err != nil {
		return fmt.Errorf("cancel task executions: %w", err)
	}
	result, err := tx.Exec(`
		UPDATE project_tasks SET status = 'cancelled', updated_at = ?
		WHERE id = ? AND project_id = ? AND status = ?
	`, now, update.TaskID, update.ProjectID, string(update.ExpectedTaskStatus))
	if err != nil {
		return fmt.Errorf("cancel project task: %w", err)
	}
	if err := requireOneControlUpdate(result, update.ProjectID); err != nil {
		return err
	}

	projectState := domain.ProjectPlanning
	pausedFrom := domain.ProjectState("")
	if update.ExpectedProjectState == domain.ProjectPaused {
		projectState = domain.ProjectPaused
		pausedFrom = domain.ProjectPlanning
	}
	result, err = tx.Exec(`
		UPDATE projects
		SET state = ?, paused_from_state = ?, current_task_id = NULL, updated_at = ?
		WHERE id = ? AND state = ? AND current_task_id = ?
	`, string(projectState), string(pausedFrom), now, update.ProjectID,
		string(update.ExpectedProjectState), update.TaskID)
	if err != nil {
		return fmt.Errorf("update project after cancellation: %w", err)
	}
	if err := requireOneControlUpdate(result, update.ProjectID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit task cancellation: %w", err)
	}
	return nil
}

func (s *Store) RetryProjectTaskExecution(
	update ProjectExecutionRetry,
) (*domain.Execution, error) {
	if update.ProjectID <= 0 || update.TaskID <= 0 || update.ExecutionID <= 0 ||
		!update.ExpectedProjectState.Valid() ||
		update.ExpectedProjectState == domain.ProjectPaused ||
		!update.ExpectedTaskStatus.Valid() ||
		!update.NewTaskStatus.Valid() {
		return nil, fmt.Errorf("%w: invalid retry request", ErrExecutionRetryConflict)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin execution retry: %w", err)
	}
	defer tx.Rollback()

	if err := requireCurrentProjectTask(
		tx,
		update.ProjectID,
		update.TaskID,
		update.ExpectedProjectState,
		update.ExpectedTaskStatus,
	); err != nil {
		return nil, err
	}
	previous, err := scanExecution(tx.QueryRow(
		executionSelect+` WHERE id = ?`,
		update.ExecutionID,
	))
	if err != nil {
		return nil, err
	}
	if previous == nil {
		return nil, fmt.Errorf("%w: ID %d", ErrExecutionNotFound, update.ExecutionID)
	}
	if previous.ProjectID != update.ProjectID || previous.TaskID != update.TaskID {
		return nil, fmt.Errorf(
			"%w: execution %d belongs to project %d task %d",
			ErrExecutionRetryConflict,
			previous.ID,
			previous.ProjectID,
			previous.TaskID,
		)
	}
	if !executionRetryable(previous.Status) {
		return nil, fmt.Errorf(
			"%w: execution %d is %q",
			ErrExecutionRetryConflict,
			previous.ID,
			previous.Status,
		)
	}
	var latestID int64
	if err := tx.QueryRow(`
		SELECT id FROM executions WHERE task_id = ? ORDER BY id DESC LIMIT 1
	`, update.TaskID).Scan(&latestID); err != nil {
		return nil, fmt.Errorf("read latest execution for retry: %w", err)
	}
	if latestID != previous.ID {
		return nil, fmt.Errorf(
			"%w: execution %d is stale; latest is %d",
			ErrExecutionRetryConflict,
			previous.ID,
			latestID,
		)
	}

	now := time.Now().UTC()
	if update.ExpectedTaskStatus != update.NewTaskStatus {
		result, err := tx.Exec(`
			UPDATE project_tasks SET status = ?, updated_at = ?
			WHERE id = ? AND project_id = ? AND status = ?
		`, string(update.NewTaskStatus), now, update.TaskID, update.ProjectID,
			string(update.ExpectedTaskStatus))
		if err != nil {
			return nil, fmt.Errorf(
				"restore task phase for retry: %w",
				classifyActiveTaskConstraint(err),
			)
		}
		if err := requireOneControlUpdate(result, update.ProjectID); err != nil {
			return nil, err
		}
	}
	result, err := tx.Exec(`
		INSERT INTO executions (
			project_id, task_id, mode, engine, model, provider_session_id,
			attempt, status, input_artifact_id, output_artifact_id,
			started_at, completed_at, error_class, error_message,
			input_tokens, output_tokens, estimated_cost
		) VALUES (?, ?, ?, ?, ?, ?, ?, 'pending', ?, NULL, NULL, NULL, '', '', 0, 0, 0)
	`,
		previous.ProjectID,
		previous.TaskID,
		previous.Mode,
		previous.Engine,
		previous.Model,
		previous.ProviderSessionID,
		previous.Attempt+1,
		nullableInt64(previous.InputArtifactID),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: insert retry: %v", ErrExecutionRetryConflict, err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("read retry execution ID: %w", err)
	}
	result, err = tx.Exec(`
		UPDATE projects
		SET state = 'executing', paused_from_state = '', current_task_id = ?, updated_at = ?
		WHERE id = ? AND state = ? AND current_task_id = ?
	`, update.TaskID, now, update.ProjectID, string(update.ExpectedProjectState), update.TaskID)
	if err != nil {
		return nil, fmt.Errorf("update project for retry: %w", err)
	}
	if err := requireOneControlUpdate(result, update.ProjectID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit execution retry: %w", err)
	}
	return s.GetExecutionByID(id)
}

func executionRetryable(status domain.ExecutionStatus) bool {
	switch status {
	case domain.ExecutionFailed,
		domain.ExecutionCancelled,
		domain.ExecutionInterrupted:
		return true
	default:
		return false
	}
}

func requireProjectState(
	tx *sql.Tx,
	projectID int64,
	expected domain.ProjectState,
) error {
	var state string
	err := tx.QueryRow(`SELECT state FROM projects WHERE id = ?`, projectID).Scan(&state)
	if err == sql.ErrNoRows {
		return fmt.Errorf("%w: ID %d", ErrProjectNotFound, projectID)
	}
	if err != nil {
		return fmt.Errorf("read project control state: %w", err)
	}
	if domain.ProjectState(state) != expected {
		return fmt.Errorf(
			"%w: project %d is %q, expected %q",
			ErrProjectControlConflict,
			projectID,
			state,
			expected,
		)
	}
	return nil
}

func requireCurrentProjectTask(
	tx *sql.Tx,
	projectID, taskID int64,
	expectedProjectState domain.ProjectState,
	expectedTaskStatus domain.TaskStatus,
) error {
	var state, status string
	var currentTaskID sql.NullInt64
	err := tx.QueryRow(`
		SELECT project.state, project.current_task_id, task.status
		FROM projects project
		JOIN project_tasks task
			ON task.project_id = project.id AND task.id = ?
		WHERE project.id = ?
	`, taskID, projectID).Scan(&state, &currentTaskID, &status)
	if err == sql.ErrNoRows {
		return fmt.Errorf(
			"%w: project %d current task %d is missing",
			ErrProjectControlConflict,
			projectID,
			taskID,
		)
	}
	if err != nil {
		return fmt.Errorf("read project control task: %w", err)
	}
	if domain.ProjectState(state) != expectedProjectState ||
		!currentTaskID.Valid ||
		currentTaskID.Int64 != taskID ||
		domain.TaskStatus(status) != expectedTaskStatus {
		return fmt.Errorf(
			"%w: stale project %d task %d state",
			ErrProjectControlConflict,
			projectID,
			taskID,
		)
	}
	return nil
}

func requireOneControlUpdate(result sql.Result, projectID int64) error {
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read project control update count: %w", err)
	}
	if changed != 1 {
		return fmt.Errorf(
			"%w: project %d changed during control action",
			ErrProjectControlConflict,
			projectID,
		)
	}
	return nil
}
