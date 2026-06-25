package store

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

type TaskState string

const (
	StateReady            TaskState = "ready"
	StateInProgress       TaskState = "in-progress"
	StateAwaitingFeedback TaskState = "awaiting-feedback"
	StateDone             TaskState = "done"
)

type Task struct {
	ID                  int64
	Repo                string
	IssueNumber         int
	SessionID           string
	State               TaskState
	LastClarificationAt *time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(1) // SQLite: single writer

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS tasks (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			repo TEXT NOT NULL,
			issue_number INTEGER NOT NULL,
			session_id TEXT NOT NULL DEFAULT '',
			state TEXT NOT NULL DEFAULT 'ready',
			last_clarification_at DATETIME,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(repo, issue_number)
		);

		CREATE TABLE IF NOT EXISTS audit_log (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			repo TEXT NOT NULL,
			issue_number INTEGER NOT NULL,
			event TEXT NOT NULL,
			details TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		);
	`)
	return err
}

func (s *Store) UpsertTask(repo string, issueNumber int, state TaskState, sessionID string) (*Task, error) {
	now := time.Now().UTC()
	_, err := s.db.Exec(`
		INSERT INTO tasks (repo, issue_number, state, session_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(repo, issue_number) DO UPDATE SET
			state = excluded.state,
			session_id = CASE WHEN excluded.session_id != '' THEN excluded.session_id ELSE session_id END,
			updated_at = excluded.updated_at
	`, repo, issueNumber, string(state), sessionID, now, now)
	if err != nil {
		return nil, fmt.Errorf("upsert task: %w", err)
	}
	return s.GetTask(repo, issueNumber)
}

func (s *Store) GetTask(repo string, issueNumber int) (*Task, error) {
	row := s.db.QueryRow(`
		SELECT id, repo, issue_number, session_id, state, last_clarification_at, created_at, updated_at
		FROM tasks WHERE repo = ? AND issue_number = ?
	`, repo, issueNumber)
	return scanTask(row)
}

func (s *Store) SetSessionID(repo string, issueNumber int, sessionID string) error {
	_, err := s.db.Exec(`
		UPDATE tasks SET session_id = ?, updated_at = ? WHERE repo = ? AND issue_number = ?
	`, sessionID, time.Now().UTC(), repo, issueNumber)
	return err
}

func (s *Store) SetClarificationTime(repo string, issueNumber int, t time.Time) error {
	_, err := s.db.Exec(`
		UPDATE tasks SET last_clarification_at = ?, updated_at = ? WHERE repo = ? AND issue_number = ?
	`, t.UTC(), time.Now().UTC(), repo, issueNumber)
	return err
}

func (s *Store) ListByState(state TaskState) ([]*Task, error) {
	rows, err := s.db.Query(`
		SELECT id, repo, issue_number, session_id, state, last_clarification_at, created_at, updated_at
		FROM tasks WHERE state = ? ORDER BY created_at ASC
	`, string(state))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tasks []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

func (s *Store) CountActive() (int, error) {
	var count int
	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM tasks WHERE state IN ('in-progress', 'awaiting-feedback')
	`).Scan(&count)
	return count, err
}

func (s *Store) Log(repo string, issueNumber int, event, details string) error {
	_, err := s.db.Exec(`
		INSERT INTO audit_log (repo, issue_number, event, details) VALUES (?, ?, ?, ?)
	`, repo, issueNumber, event, details)
	return err
}

func (s *Store) GetAuditLog(repo string, issueNumber int) ([]AuditEntry, error) {
	rows, err := s.db.Query(`
		SELECT id, repo, issue_number, event, details, created_at
		FROM audit_log WHERE repo = ? AND issue_number = ? ORDER BY created_at ASC
	`, repo, issueNumber)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.Repo, &e.IssueNumber, &e.Event, &e.Details, &e.CreatedAt); err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

type AuditEntry struct {
	ID          int64
	Repo        string
	IssueNumber int
	Event       string
	Details     string
	CreatedAt   time.Time
}

type scanner interface {
	Scan(dest ...any) error
}

func scanTask(s scanner) (*Task, error) {
	var t Task
	var state string
	var clarAt sql.NullTime
	if err := s.Scan(&t.ID, &t.Repo, &t.IssueNumber, &t.SessionID, &state, &clarAt, &t.CreatedAt, &t.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("scan task: %w", err)
	}
	t.State = TaskState(state)
	if clarAt.Valid {
		t.LastClarificationAt = &clarAt.Time
	}
	return &t, nil
}
