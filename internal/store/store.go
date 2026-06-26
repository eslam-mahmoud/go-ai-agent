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

type CIState string

const (
	CIStateNone    CIState = ""
	CIStateWaiting CIState = "waiting"
	CIStatePassed  CIState = "passed"
	CIStateFailed  CIState = "failed"
	CIStateGaveUp  CIState = "gave_up"
)

type Task struct {
	ID                  int64
	Repo                string
	IssueNumber         int
	SessionID           string
	State               TaskState
	LastClarificationAt *time.Time
	PRNumber            int
	CIState             CIState
	CIRetries           int
	CIWatchStartedAt    *time.Time
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

// migrations is the ordered list of schema migrations. Each entry is applied
// exactly once and permanently recorded in schema_migrations. Add new entries
// at the end — never edit or reorder existing ones.
var migrations = []struct {
	version int
	sql     string
}{
	{1, `
		CREATE TABLE IF NOT EXISTS tasks (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			repo TEXT NOT NULL,
			issue_number INTEGER NOT NULL,
			session_id TEXT NOT NULL DEFAULT '',
			state TEXT NOT NULL DEFAULT 'ready',
			last_clarification_at DATETIME,
			pr_number INTEGER NOT NULL DEFAULT 0,
			ci_state TEXT NOT NULL DEFAULT '',
			ci_retries INTEGER NOT NULL DEFAULT 0,
			ci_watch_started_at DATETIME,
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
	`},
}

func (s *Store) migrate() error {
	// Bootstrap the migrations table itself.
	if _, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY,
			applied_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)
	`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	// Find the highest applied version.
	var current int
	row := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`)
	if err := row.Scan(&current); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	// Apply any pending migrations in order.
	for _, m := range migrations {
		if m.version <= current {
			continue
		}
		if _, err := s.db.Exec(m.sql); err != nil {
			return fmt.Errorf("migration v%d: %w", m.version, err)
		}
		if _, err := s.db.Exec(
			`INSERT INTO schema_migrations (version) VALUES (?)`, m.version,
		); err != nil {
			return fmt.Errorf("record migration v%d: %w", m.version, err)
		}
	}
	return nil
}

// SchemaVersion returns the currently applied schema version.
func (s *Store) SchemaVersion() (int, error) {
	var v int
	err := s.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v)
	return v, err
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
		SELECT id, repo, issue_number, session_id, state, last_clarification_at,
		       pr_number, ci_state, ci_retries, ci_watch_started_at, created_at, updated_at
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

func (s *Store) SetPRNumber(repo string, issueNumber, prNumber int) error {
	_, err := s.db.Exec(`
		UPDATE tasks SET pr_number = ?, updated_at = ? WHERE repo = ? AND issue_number = ?
	`, prNumber, time.Now().UTC(), repo, issueNumber)
	return err
}

func (s *Store) SetCIState(repo string, issueNumber int, ciState CIState) error {
	_, err := s.db.Exec(`
		UPDATE tasks SET ci_state = ?, updated_at = ? WHERE repo = ? AND issue_number = ?
	`, string(ciState), time.Now().UTC(), repo, issueNumber)
	return err
}

func (s *Store) SetCIWatchStartedAt(repo string, issueNumber int, t time.Time) error {
	_, err := s.db.Exec(`
		UPDATE tasks SET ci_watch_started_at = ?, updated_at = ? WHERE repo = ? AND issue_number = ?
	`, t.UTC(), time.Now().UTC(), repo, issueNumber)
	return err
}

func (s *Store) IncrementCIRetries(repo string, issueNumber int) (int, error) {
	_, err := s.db.Exec(`
		UPDATE tasks SET ci_retries = ci_retries + 1, updated_at = ? WHERE repo = ? AND issue_number = ?
	`, time.Now().UTC(), repo, issueNumber)
	if err != nil {
		return 0, err
	}
	task, err := s.GetTask(repo, issueNumber)
	if err != nil || task == nil {
		return 0, err
	}
	return task.CIRetries, nil
}

func (s *Store) ListByState(state TaskState) ([]*Task, error) {
	rows, err := s.db.Query(`
		SELECT id, repo, issue_number, session_id, state, last_clarification_at,
		       pr_number, ci_state, ci_retries, ci_watch_started_at, created_at, updated_at
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

func (s *Store) ListByCIState(ciState CIState) ([]*Task, error) {
	rows, err := s.db.Query(`
		SELECT id, repo, issue_number, session_id, state, last_clarification_at,
		       pr_number, ci_state, ci_retries, ci_watch_started_at, created_at, updated_at
		FROM tasks WHERE ci_state = ? ORDER BY created_at ASC
	`, string(ciState))
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

// CountActive returns the number of tasks Claude is actively executing.
// Excluded from the count:
//   - awaiting-feedback: parked, waiting for human input, Claude is idle
//   - in-progress with ci_state=waiting: Claude finished, only CI polling remains
//
// Only tasks where Claude is genuinely running (in-progress, not CI-watching)
// count toward the max_parallel ceiling.
func (s *Store) CountActive() (int, error) {
	var count int
	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM tasks
		WHERE state = 'in-progress' AND ci_state = ''
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
	var state, ciState string
	var clarAt, ciWatchAt sql.NullTime
	if err := s.Scan(
		&t.ID, &t.Repo, &t.IssueNumber, &t.SessionID, &state, &clarAt,
		&t.PRNumber, &ciState, &t.CIRetries, &ciWatchAt, &t.CreatedAt, &t.UpdatedAt,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("scan task: %w", err)
	}
	t.State = TaskState(state)
	t.CIState = CIState(ciState)
	if clarAt.Valid {
		t.LastClarificationAt = &clarAt.Time
	}
	if ciWatchAt.Valid {
		t.CIWatchStartedAt = &ciWatchAt.Time
	}
	return &t, nil
}
