package store

import (
	"fmt"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

// InterruptRunningExecutionsForRecovery atomically converts provider
// executions that could not survive a process restart into durable interrupted
// records and appends one audit fact per execution. Legacy v1 task rows are
// intentionally untouched.
func (s *Store) InterruptRunningExecutionsForRecovery() ([]*domain.Execution, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin workflow startup recovery: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.Query(
		executionSelect + ` WHERE status = 'running' ORDER BY id ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("list running executions for recovery: %w", err)
	}
	var executions []*domain.Execution
	for rows.Next() {
		execution, err := scanExecution(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		executions = append(executions, execution)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate running executions for recovery: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close running executions for recovery: %w", err)
	}

	now := time.Now().UTC()
	for _, execution := range executions {
		result, err := tx.Exec(`
			UPDATE executions
			SET status = 'interrupted', error_message = 'startup detected no provider process'
			WHERE id = ? AND status = 'running'
		`, execution.ID)
		if err != nil {
			return nil, fmt.Errorf("interrupt execution %d for recovery: %w", execution.ID, err)
		}
		if err := requireOneControlUpdate(result, execution.ProjectID); err != nil {
			return nil, err
		}
		taskID := execution.TaskID
		executionID := execution.ID
		if err := appendWorkflowFactTx(
			tx,
			execution.ProjectID,
			&taskID,
			&executionID,
			domain.WorkflowSourceRecovery,
			domain.WorkflowExecutionInterrupted,
			"Startup interrupted an orphaned provider execution.",
			map[string]any{
				"mode":                execution.Mode,
				"attempt":             execution.Attempt,
				"provider_session_id": execution.ProviderSessionID,
				"recovered_at":        now,
			},
			fmt.Sprintf("startup-interrupt:%d", execution.ID),
		); err != nil {
			return nil, err
		}
		execution.Status = domain.ExecutionInterrupted
		execution.ErrorMessage = "startup detected no provider process"
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit workflow startup recovery: %w", err)
	}
	return executions, nil
}
