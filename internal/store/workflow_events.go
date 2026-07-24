package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

var (
	ErrWorkflowEventNotFound  = errors.New("workflow event not found")
	ErrWorkflowEventOwnership = errors.New("workflow event ownership conflict")
	ErrWorkflowEventConflict  = errors.New("workflow event conflict")
)

// AppendWorkflowEvent persists an immutable audit fact. A repeated non-empty
// idempotency key returns the original fact without allocating a new sequence.
func (s *Store) AppendWorkflowEvent(
	event *domain.WorkflowEvent,
) (*domain.WorkflowEvent, bool, error) {
	if err := event.Validate(); err != nil {
		return nil, false, fmt.Errorf("append workflow event: %w", err)
	}
	if event.ID != 0 || event.Sequence != 0 || !event.CreatedAt.IsZero() {
		return nil, false, fmt.Errorf(
			"%w: new event identity, sequence, and timestamp must be zero",
			domain.ErrInvalidWorkflowEvent,
		)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, false, fmt.Errorf("begin workflow event append: %w", err)
	}
	defer tx.Rollback()
	id, created, err := appendWorkflowEventTx(tx, event)
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("commit workflow event append: %w", err)
	}
	stored, err := s.GetWorkflowEventByID(id)
	return stored, created, err
}

func (s *Store) GetWorkflowEventByID(id int64) (*domain.WorkflowEvent, error) {
	if id <= 0 {
		return nil, fmt.Errorf("%w: ID %d", ErrWorkflowEventNotFound, id)
	}
	return scanWorkflowEvent(s.db.QueryRow(workflowEventSelect+` WHERE id = ?`, id))
}

// ListWorkflowEvents returns stable project order after the exclusive
// sequence cursor. Limit defaults to 100 and is capped at 1000.
func (s *Store) ListWorkflowEvents(
	projectID, afterSequence int64,
	limit int,
) ([]*domain.WorkflowEvent, error) {
	if projectID <= 0 || afterSequence < 0 {
		return nil, fmt.Errorf(
			"%w: project ID must be positive and cursor non-negative",
			domain.ErrInvalidWorkflowEvent,
		)
	}
	if limit < 0 || limit > 1000 {
		return nil, fmt.Errorf(
			"%w: limit must be between 0 and 1000",
			domain.ErrInvalidWorkflowEvent,
		)
	}
	if limit == 0 {
		limit = 100
	}
	if project, err := s.GetProjectByID(projectID); err != nil {
		return nil, err
	} else if project == nil {
		return nil, fmt.Errorf("%w: ID %d", ErrProjectNotFound, projectID)
	}
	rows, err := s.db.Query(
		workflowEventSelect+`
		WHERE project_id = ? AND sequence > ?
		ORDER BY sequence ASC
		LIMIT ?
	`, projectID, afterSequence, limit)
	if err != nil {
		return nil, fmt.Errorf("list workflow events: %w", err)
	}
	defer rows.Close()
	var events []*domain.WorkflowEvent
	for rows.Next() {
		event, err := scanWorkflowEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate workflow events: %w", err)
	}
	return events, nil
}

func appendWorkflowFactTx(
	tx *sql.Tx,
	projectID int64,
	taskID, executionID *int64,
	source domain.WorkflowEventSource,
	eventType domain.WorkflowEventType,
	message string,
	data any,
	idempotencyKey string,
) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("encode workflow event %q: %w", eventType, err)
	}
	event := domain.NewWorkflowEvent(projectID, source, eventType, message)
	event.TaskID = taskID
	event.ExecutionID = executionID
	event.Data = raw
	event.IdempotencyKey = idempotencyKey
	_, _, err = appendWorkflowEventTx(tx, event)
	return err
}

func appendWorkflowEventTx(
	tx *sql.Tx,
	event *domain.WorkflowEvent,
) (int64, bool, error) {
	if err := event.Validate(); err != nil {
		return 0, false, fmt.Errorf("append workflow event: %w", err)
	}
	if event.IdempotencyKey != "" {
		var existingID int64
		err := tx.QueryRow(`
			SELECT id FROM workflow_events
			WHERE project_id = ? AND idempotency_key = ?
		`, event.ProjectID, event.IdempotencyKey).Scan(&existingID)
		switch {
		case err == nil:
			return existingID, false, nil
		case err != sql.ErrNoRows:
			return 0, false, fmt.Errorf("find idempotent workflow event: %w", err)
		}
	}
	var sequence int64
	if err := tx.QueryRow(`
		SELECT COALESCE(MAX(sequence), 0) + 1
		FROM workflow_events WHERE project_id = ?
	`, event.ProjectID).Scan(&sequence); err != nil {
		return 0, false, fmt.Errorf("allocate workflow event sequence: %w", err)
	}
	result, err := tx.Exec(`
		INSERT INTO workflow_events (
			project_id, task_id, execution_id, sequence, source, event_type,
			message, data_json, idempotency_key, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		event.ProjectID,
		nullableInt64(event.TaskID),
		nullableInt64(event.ExecutionID),
		sequence,
		string(event.Source),
		string(event.Type),
		event.Message,
		string(event.Data),
		event.IdempotencyKey,
		time.Now().UTC(),
	)
	if err != nil {
		return 0, false, classifyWorkflowEventConstraint(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, false, fmt.Errorf("read workflow event ID: %w", err)
	}
	return id, true, nil
}

func classifyWorkflowEventConstraint(err error) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	switch {
	case strings.Contains(message, "workflow event task must belong"),
		strings.Contains(message, "workflow event execution must belong"),
		strings.Contains(message, "FOREIGN KEY constraint failed"):
		return fmt.Errorf("%w: %v", ErrWorkflowEventOwnership, err)
	case strings.Contains(message, "workflow_events.project_id, workflow_events.sequence"),
		strings.Contains(message, "idx_workflow_events_idempotency"):
		return fmt.Errorf("%w: %v", ErrWorkflowEventConflict, err)
	default:
		return fmt.Errorf("insert workflow event: %w", err)
	}
}

const workflowEventSelect = `
	SELECT
		id, project_id, task_id, execution_id, sequence, source, event_type,
		message, data_json, idempotency_key, created_at
	FROM workflow_events
`

func scanWorkflowEvent(row scanner) (*domain.WorkflowEvent, error) {
	var event domain.WorkflowEvent
	var taskID, executionID sql.NullInt64
	var source, eventType, data string
	if err := row.Scan(
		&event.ID,
		&event.ProjectID,
		&taskID,
		&executionID,
		&event.Sequence,
		&source,
		&eventType,
		&event.Message,
		&data,
		&event.IdempotencyKey,
		&event.CreatedAt,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("scan workflow event: %w", err)
	}
	event.TaskID = nullInt64Pointer(taskID)
	event.ExecutionID = nullInt64Pointer(executionID)
	event.Source = domain.WorkflowEventSource(source)
	event.Type = domain.WorkflowEventType(eventType)
	event.Data = json.RawMessage(data)
	event.CreatedAt = event.CreatedAt.UTC()
	return &event, nil
}
