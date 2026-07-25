package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

// Notification is one recorded delivery attempt.
type Notification struct {
	ID        int64
	ProjectID int64
	Kind      string
	Key       string
	Delivered bool
	Detail    string
	CreatedAt time.Time
}

// RecordNotification appends one attempt, delivered or not. A repeated
// delivered key is tolerated: the unique index makes the second one a no-op
// rather than an error, so recording can never fail a delivery step.
func (s *Store) RecordNotification(
	projectID int64,
	kind string,
	key string,
	delivered bool,
	detail string,
) error {
	if projectID <= 0 || strings.TrimSpace(kind) == "" {
		return fmt.Errorf(
			"%w: notification project and kind are required",
			domain.ErrInvalidProject,
		)
	}
	_, err := s.db.Exec(`
		INSERT INTO notifications (
			project_id, kind, idempotency_key, delivered, detail, created_at
		) VALUES (?, ?, ?, ?, ?, ?)
	`, projectID, kind, strings.TrimSpace(key), delivered, detail, time.Now().UTC())
	if err != nil && isDeliveredNotificationConflict(err) {
		// The same delivered key twice is the idempotent case, not a failure:
		// recording must never be able to fail a delivery step.
		return nil
	}
	if err != nil {
		return fmt.Errorf("record notification: %w", err)
	}
	return nil
}

// NotificationDelivered reports whether a key was already delivered, which is
// what makes notification idempotency survive a restart.
func (s *Store) NotificationDelivered(projectID int64, key string) (bool, error) {
	trimmed := strings.TrimSpace(key)
	if projectID <= 0 || trimmed == "" {
		return false, nil
	}
	var id int64
	err := s.db.QueryRow(`
		SELECT id FROM notifications
		WHERE project_id = ? AND idempotency_key = ? AND delivered = 1
	`, projectID, trimmed).Scan(&id)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read notification history: %w", err)
	}
	return true, nil
}

// ListNotifications returns a project's notification history in order.
func (s *Store) ListNotifications(projectID int64) ([]*Notification, error) {
	rows, err := s.db.Query(`
		SELECT id, project_id, kind, idempotency_key, delivered, detail, created_at
		FROM notifications WHERE project_id = ? ORDER BY id ASC
	`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list notifications: %w", err)
	}
	defer rows.Close()
	var notifications []*Notification
	for rows.Next() {
		var notification Notification
		var delivered int
		if err := rows.Scan(
			&notification.ID,
			&notification.ProjectID,
			&notification.Kind,
			&notification.Key,
			&delivered,
			&notification.Detail,
			&notification.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan notification: %w", err)
		}
		notification.Delivered = delivered != 0
		notification.CreatedAt = notification.CreatedAt.UTC()
		notifications = append(notifications, &notification)
	}
	return notifications, rows.Err()
}

// isDeliveredNotificationConflict recognizes the duplicate-delivery guard by
// index name or by the column list, since SQLite reports either.
func isDeliveredNotificationConflict(err error) bool {
	message := err.Error()
	return strings.Contains(message, "idx_notifications_delivered_key") ||
		strings.Contains(message, "notifications.project_id, notifications.idempotency_key")
}
