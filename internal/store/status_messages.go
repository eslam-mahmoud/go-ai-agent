package store

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

// StatusMessage identifies a project's live Telegram status message and the
// text it currently shows.
type StatusMessage struct {
	ProjectID int64
	ChatID    int64
	MessageID int64
	LastText  string
	UpdatedAt time.Time
}

// GetStatusMessage returns the recorded status message, or nil when the
// project has none yet.
func (s *Store) GetStatusMessage(projectID int64) (*StatusMessage, error) {
	if projectID <= 0 {
		return nil, fmt.Errorf("%w: project ID must be positive", domain.ErrInvalidProject)
	}
	var message StatusMessage
	err := s.db.QueryRow(`
		SELECT project_id, chat_id, message_id, last_text, updated_at
		FROM status_messages WHERE project_id = ?
	`, projectID).Scan(
		&message.ProjectID,
		&message.ChatID,
		&message.MessageID,
		&message.LastText,
		&message.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read status message: %w", err)
	}
	message.UpdatedAt = message.UpdatedAt.UTC()
	return &message, nil
}

// SaveStatusMessage records the message identity and the text it shows, so a
// later pass can edit it and skip unchanged renders.
func (s *Store) SaveStatusMessage(message StatusMessage) error {
	if message.ProjectID <= 0 || message.ChatID == 0 || message.MessageID == 0 {
		return fmt.Errorf(
			"%w: status message needs a project, chat, and message",
			domain.ErrInvalidProject,
		)
	}
	if _, err := s.db.Exec(`
		INSERT INTO status_messages (
			project_id, chat_id, message_id, last_text, updated_at
		) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(project_id) DO UPDATE SET
			chat_id = excluded.chat_id,
			message_id = excluded.message_id,
			last_text = excluded.last_text,
			updated_at = excluded.updated_at
	`,
		message.ProjectID,
		message.ChatID,
		message.MessageID,
		message.LastText,
		time.Now().UTC(),
	); err != nil {
		return fmt.Errorf("save status message: %w", err)
	}
	return nil
}
