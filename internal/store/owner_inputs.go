package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

// OwnerInputKind distinguishes the durable inputs the owner can supply.
type OwnerInputKind string

const (
	OwnerAnswer    OwnerInputKind = "answer"
	OwnerApproval  OwnerInputKind = "approval"
	OwnerRejection OwnerInputKind = "rejection"
)

// OwnerInput is one recorded answer or approval decision. The workflow
// consumes these; a chat message never mutates task state directly.
type OwnerInput struct {
	ID        int64
	ProjectID int64
	TaskID    *int64
	Kind      OwnerInputKind
	Subject   string
	Body      string
	Author    string
	Consumed  bool
	CreatedAt time.Time
}

func (kind OwnerInputKind) Valid() bool {
	switch kind {
	case OwnerAnswer, OwnerApproval, OwnerRejection:
		return true
	default:
		return false
	}
}

// RecordOwnerInput persists one owner decision.
func (s *Store) RecordOwnerInput(input OwnerInput) (*OwnerInput, error) {
	if input.ProjectID <= 0 || !input.Kind.Valid() {
		return nil, fmt.Errorf(
			"%w: owner input needs a project and a known kind",
			domain.ErrInvalidProject,
		)
	}
	if input.Kind == OwnerAnswer && strings.TrimSpace(input.Body) == "" {
		return nil, fmt.Errorf("%w: an answer needs text", domain.ErrInvalidProject)
	}
	if input.Kind != OwnerAnswer && strings.TrimSpace(input.Subject) == "" {
		return nil, fmt.Errorf("%w: an approval needs a subject", domain.ErrInvalidProject)
	}
	now := time.Now().UTC()
	result, err := s.db.Exec(`
		INSERT INTO owner_inputs (
			project_id, task_id, kind, subject, body, author, consumed, created_at
		) VALUES (?, ?, ?, ?, ?, ?, 0, ?)
	`,
		input.ProjectID,
		nullableInt64(input.TaskID),
		string(input.Kind),
		strings.TrimSpace(input.Subject),
		strings.TrimSpace(input.Body),
		strings.TrimSpace(input.Author),
		now,
	)
	if err != nil {
		return nil, fmt.Errorf("record owner input: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("read owner input ID: %w", err)
	}
	input.ID = id
	input.CreatedAt = now
	return &input, nil
}

// PendingOwnerInputs returns unconsumed inputs of one kind, oldest first.
func (s *Store) PendingOwnerInputs(
	projectID int64,
	kind OwnerInputKind,
) ([]*OwnerInput, error) {
	if projectID <= 0 || !kind.Valid() {
		return nil, fmt.Errorf(
			"%w: project and kind are required",
			domain.ErrInvalidProject,
		)
	}
	rows, err := s.db.Query(`
		SELECT id, project_id, task_id, kind, subject, body, author, consumed, created_at
		FROM owner_inputs
		WHERE project_id = ? AND kind = ? AND consumed = 0
		ORDER BY id ASC
	`, projectID, string(kind))
	if err != nil {
		return nil, fmt.Errorf("list owner inputs: %w", err)
	}
	defer rows.Close()
	var inputs []*OwnerInput
	for rows.Next() {
		input, err := scanOwnerInput(rows)
		if err != nil {
			return nil, err
		}
		inputs = append(inputs, input)
	}
	return inputs, rows.Err()
}

// ConsumeOwnerInput marks an input as acted on, so it is applied once.
func (s *Store) ConsumeOwnerInput(id int64) error {
	if id <= 0 {
		return fmt.Errorf("%w: owner input ID must be positive", domain.ErrInvalidProject)
	}
	result, err := s.db.Exec(`
		UPDATE owner_inputs SET consumed = 1 WHERE id = ? AND consumed = 0
	`, id)
	if err != nil {
		return fmt.Errorf("consume owner input: %w", err)
	}
	if changed, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("read owner input count: %w", err)
	} else if changed != 1 {
		return fmt.Errorf("owner input %d was already consumed", id)
	}
	return nil
}

func scanOwnerInput(row scanner) (*OwnerInput, error) {
	var input OwnerInput
	var kind string
	var consumed int
	var taskID sql.NullInt64
	if err := row.Scan(
		&input.ID,
		&input.ProjectID,
		&taskID,
		&kind,
		&input.Subject,
		&input.Body,
		&input.Author,
		&consumed,
		&input.CreatedAt,
	); err != nil {
		return nil, fmt.Errorf("scan owner input: %w", err)
	}
	input.TaskID = nullInt64Pointer(taskID)
	input.Kind = OwnerInputKind(kind)
	input.Consumed = consumed != 0
	input.CreatedAt = input.CreatedAt.UTC()
	return &input, nil
}
