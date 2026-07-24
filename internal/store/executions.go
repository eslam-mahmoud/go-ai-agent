package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

var (
	ErrExecutionAlreadyExists = errors.New("execution already exists")
	ErrExecutionNotFound      = errors.New("execution not found")
	ErrRunningExecutionExists = errors.New("a running execution already exists")
)

func (s *Store) CreateExecution(execution *domain.Execution) (*domain.Execution, error) {
	if err := execution.Validate(); err != nil {
		return nil, fmt.Errorf("create execution: %w", err)
	}
	if execution.ID != 0 {
		return nil, fmt.Errorf("%w: new execution ID must be zero", domain.ErrInvalidExecution)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin create execution: %w", err)
	}
	defer tx.Rollback()
	if err := requireProjectTask(tx, execution.ProjectID, execution.TaskID); err != nil {
		return nil, err
	}
	if err := validateExecutionArtifacts(tx, execution); err != nil {
		return nil, err
	}
	var existing int64
	err = tx.QueryRow(`
		SELECT id FROM executions WHERE task_id = ? AND mode = ? AND attempt = ?
	`, execution.TaskID, execution.Mode, execution.Attempt).Scan(&existing)
	if err == nil {
		return nil, fmt.Errorf(
			"%w: task %d mode %q attempt %d",
			ErrExecutionAlreadyExists, execution.TaskID, execution.Mode, execution.Attempt,
		)
	}
	if err != sql.ErrNoRows {
		return nil, fmt.Errorf("check execution identity: %w", err)
	}
	result, err := tx.Exec(`
		INSERT INTO executions (
			project_id, task_id, mode, engine, model, provider_session_id,
			attempt, status, input_artifact_id, output_artifact_id,
			started_at, completed_at, error_class, error_message,
			input_tokens, output_tokens, estimated_cost
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		execution.ProjectID, execution.TaskID, execution.Mode, execution.Engine,
		execution.Model, execution.ProviderSessionID, execution.Attempt,
		string(execution.Status), nullableInt64(execution.InputArtifactID),
		nullableInt64(execution.OutputArtifactID), nullableTime(execution.StartedAt),
		nullableTime(execution.CompletedAt), execution.ErrorClass,
		execution.ErrorMessage, execution.InputTokens, execution.OutputTokens,
		execution.EstimatedCost,
	)
	if err != nil {
		return nil, fmt.Errorf("insert execution: %w", classifyRunningExecutionConstraint(err))
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("read execution ID: %w", err)
	}
	if err := touchProject(tx, execution.ProjectID, time.Now().UTC()); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit create execution: %w", err)
	}
	return s.GetExecutionByID(id)
}

func (s *Store) GetExecutionByID(id int64) (*domain.Execution, error) {
	return scanExecution(s.db.QueryRow(executionSelect+` WHERE id = ?`, id))
}

func (s *Store) UpdateExecution(execution *domain.Execution) (*domain.Execution, error) {
	if err := execution.Validate(); err != nil {
		return nil, fmt.Errorf("update execution: %w", err)
	}
	if execution.ID <= 0 {
		return nil, fmt.Errorf("%w: persisted execution ID must be positive", domain.ErrInvalidExecution)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin update execution: %w", err)
	}
	defer tx.Rollback()
	var projectID, taskID int64
	var mode, engineName, model string
	var attempt int
	err = tx.QueryRow(`
		SELECT project_id, task_id, mode, engine, model, attempt
		FROM executions WHERE id = ?
	`, execution.ID).Scan(&projectID, &taskID, &mode, &engineName, &model, &attempt)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%w: ID %d", ErrExecutionNotFound, execution.ID)
	}
	if err != nil {
		return nil, fmt.Errorf("read execution identity: %w", err)
	}
	if projectID != execution.ProjectID || taskID != execution.TaskID ||
		mode != execution.Mode || engineName != execution.Engine ||
		model != execution.Model || attempt != execution.Attempt {
		return nil, fmt.Errorf(
			"%w: project, task, mode, engine, model, and attempt are immutable",
			domain.ErrInvalidExecution,
		)
	}
	if err := validateExecutionArtifacts(tx, execution); err != nil {
		return nil, err
	}
	result, err := tx.Exec(`
		UPDATE executions SET
			engine = ?, model = ?, provider_session_id = ?, status = ?,
			input_artifact_id = ?, output_artifact_id = ?,
			started_at = ?, completed_at = ?, error_class = ?, error_message = ?,
			input_tokens = ?, output_tokens = ?, estimated_cost = ?
		WHERE id = ?
	`,
		execution.Engine, execution.Model, execution.ProviderSessionID,
		string(execution.Status), nullableInt64(execution.InputArtifactID),
		nullableInt64(execution.OutputArtifactID), nullableTime(execution.StartedAt),
		nullableTime(execution.CompletedAt), execution.ErrorClass,
		execution.ErrorMessage, execution.InputTokens, execution.OutputTokens,
		execution.EstimatedCost, execution.ID,
	)
	if err != nil {
		return nil, fmt.Errorf("update execution: %w", classifyRunningExecutionConstraint(err))
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("read execution update count: %w", err)
	}
	if updated != 1 {
		return nil, fmt.Errorf("%w: ID %d", ErrExecutionNotFound, execution.ID)
	}
	if err := touchProject(tx, execution.ProjectID, time.Now().UTC()); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit update execution: %w", err)
	}
	return s.GetExecutionByID(execution.ID)
}

func classifyRunningExecutionConstraint(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "idx_executions_single_running") {
		return fmt.Errorf("%w: %v", ErrRunningExecutionExists, err)
	}
	return err
}

func (s *Store) ListTaskExecutions(taskID int64) ([]*domain.Execution, error) {
	rows, err := s.db.Query(executionSelect+` WHERE task_id = ? ORDER BY id ASC`, taskID)
	if err != nil {
		return nil, fmt.Errorf("list task executions: %w", err)
	}
	defer rows.Close()
	var executions []*domain.Execution
	for rows.Next() {
		execution, err := scanExecution(rows)
		if err != nil {
			return nil, err
		}
		executions = append(executions, execution)
	}
	return executions, rows.Err()
}

const executionSelect = `
	SELECT
		id, project_id, task_id, mode, engine, model, provider_session_id,
		attempt, status, input_artifact_id, output_artifact_id,
		started_at, completed_at, error_class, error_message,
		input_tokens, output_tokens, estimated_cost
	FROM executions
`

func scanExecution(row scanner) (*domain.Execution, error) {
	var execution domain.Execution
	var status string
	var inputID, outputID sql.NullInt64
	var startedAt, completedAt sql.NullTime
	if err := row.Scan(
		&execution.ID, &execution.ProjectID, &execution.TaskID, &execution.Mode,
		&execution.Engine, &execution.Model, &execution.ProviderSessionID,
		&execution.Attempt, &status, &inputID, &outputID, &startedAt, &completedAt,
		&execution.ErrorClass, &execution.ErrorMessage, &execution.InputTokens,
		&execution.OutputTokens, &execution.EstimatedCost,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("scan execution: %w", err)
	}
	execution.Status = domain.ExecutionStatus(status)
	execution.InputArtifactID = nullInt64Pointer(inputID)
	execution.OutputArtifactID = nullInt64Pointer(outputID)
	execution.StartedAt = nullTimePointer(startedAt)
	execution.CompletedAt = nullTimePointer(completedAt)
	return &execution, nil
}

func requireProjectTask(tx *sql.Tx, projectID, taskID int64) error {
	var exists int
	err := tx.QueryRow(`
		SELECT 1 FROM project_tasks WHERE id = ? AND project_id = ?
	`, taskID, projectID).Scan(&exists)
	if err == sql.ErrNoRows {
		return fmt.Errorf("%w: task %d does not belong to project %d", ErrProjectTaskNotFound, taskID, projectID)
	}
	if err != nil {
		return fmt.Errorf("check project task ownership: %w", err)
	}
	return nil
}

func validateExecutionArtifacts(tx *sql.Tx, execution *domain.Execution) error {
	for label, id := range map[string]*int64{
		"input": execution.InputArtifactID, "output": execution.OutputArtifactID,
	} {
		if id == nil {
			continue
		}
		var projectID int64
		var taskID, artifactExecutionID sql.NullInt64
		err := tx.QueryRow(`
			SELECT project_id, task_id, execution_id FROM artifacts WHERE id = ?
		`, *id).Scan(&projectID, &taskID, &artifactExecutionID)
		if err == sql.ErrNoRows {
			return fmt.Errorf("%w: %s artifact %d", ErrArtifactNotFound, label, *id)
		}
		if err != nil {
			return fmt.Errorf("check %s artifact: %w", label, err)
		}
		if projectID != execution.ProjectID || (taskID.Valid && taskID.Int64 != execution.TaskID) {
			return fmt.Errorf(
				"%w: %s artifact %d belongs to another project or task",
				domain.ErrInvalidExecution, label, *id,
			)
		}
		if label == "output" && artifactExecutionID.Valid &&
			artifactExecutionID.Int64 != execution.ID {
			return fmt.Errorf(
				"%w: output artifact %d belongs to execution %d",
				domain.ErrInvalidExecution, *id, artifactExecutionID.Int64,
			)
		}
	}
	return nil
}

func nullInt64Pointer(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	copy := value.Int64
	return &copy
}

func nullTimePointer(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	copy := value.Time.UTC()
	return &copy
}
