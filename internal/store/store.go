package store

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

type TaskState string

const (
	StateReady            TaskState = "ready"
	StateInProgress       TaskState = "in-progress"
	StateInterrupted      TaskState = "interrupted"
	StateRecovering       TaskState = "recovering"
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
	Engine              string
	Model               string
	State               TaskState
	LastClarificationAt *time.Time
	PRNumber            int
	CIState             CIState
	CIRetries           int
	CIWatchStartedAt    *time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// ExecutionBinding identifies the provider context assigned to a legacy task
// execution. The v2 executions table will supersede this compatibility record.
type ExecutionBinding struct {
	Engine            string
	Model             string
	ProviderSessionID string
}

var ErrExecutionBindingConflict = errors.New("execution binding conflict")

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	// Create parent directories so db_path: /opt/madar/madar.db works on first run.
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db directory %s: %w", dir, err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(1) // SQLite: single writer

	// WAL mode: readers never block writers and vice versa.
	if _, err := db.Exec(`
		PRAGMA journal_mode=WAL;
		PRAGMA synchronous=NORMAL;
		PRAGMA foreign_keys=ON;
	`); err != nil {
		db.Close()
		return nil, fmt.Errorf("configure sqlite: %w", err)
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// OpenReadOnly opens an existing database without migrations or mutating
// pragmas. It is intended for status and diagnostic commands that must remain
// usable while the daemon owns the exclusive instance lock.
func OpenReadOnly(path string) (*Store, error) {
	dsn := (&url.URL{
		Scheme:   "file",
		Path:     path,
		RawQuery: "mode=ro",
	}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db read-only: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open db read-only: %w", err)
	}
	return &Store{db: db}, nil
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
	// v2: indexes on frequently-filtered columns to avoid full table scans on
	// every tick. Applied after v1 so the tables exist.
	{2, `
		CREATE INDEX IF NOT EXISTS idx_tasks_state    ON tasks(state);
		CREATE INDEX IF NOT EXISTS idx_tasks_ci_state ON tasks(ci_state);
		CREATE INDEX IF NOT EXISTS idx_audit_created  ON audit_log(created_at);
	`},
	// v3: pin the provider and model used by the legacy task execution. The
	// existing session_id column remains the provider session for compatibility.
	{3, `
		ALTER TABLE tasks ADD COLUMN engine TEXT NOT NULL DEFAULT '';
		ALTER TABLE tasks ADD COLUMN model TEXT NOT NULL DEFAULT '';
	`},
	// v4: introduce the v2 project aggregate without changing legacy issue
	// tables. Related project tables are added by later ordered migrations.
	{4, `
		CREATE TABLE projects (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			repo TEXT NOT NULL UNIQUE CHECK (length(trim(repo)) > 0),
			parent_issue_number INTEGER NOT NULL DEFAULT 0 CHECK (parent_issue_number >= 0),
			name TEXT NOT NULL CHECK (length(trim(name)) > 0),
			goal TEXT NOT NULL CHECK (length(trim(goal)) > 0),
			scope TEXT NOT NULL DEFAULT '',
			state TEXT NOT NULL CHECK (
				state IN (
					'initializing',
					'planning',
					'executing',
					'blocked',
					'release-review',
					'completed',
					'paused'
				)
			),
			health TEXT NOT NULL CHECK (
				health IN (
					'on-track',
					'at-risk',
					'off-track',
					'blocked',
					'ready-for-release'
				)
			),
			current_task_id INTEGER CHECK (current_task_id IS NULL OR current_task_id > 0),
			current_plan_version INTEGER NOT NULL DEFAULT 0 CHECK (current_plan_version >= 0),
			architecture_version INTEGER NOT NULL DEFAULT 0 CHECK (architecture_version >= 0),
			release_target TEXT NOT NULL DEFAULT '',
			release_readiness TEXT NOT NULL DEFAULT '',
			last_manager_review_at DATETIME,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL
		);
		CREATE INDEX idx_projects_state ON projects(state);
		CREATE INDEX idx_projects_health ON projects(health);
	`},
	// v5: add the ordered v2 project backlog. The legacy tasks table remains
	// independent until the explicit migration path in backlog item 20.
	{5, `
		CREATE TABLE project_tasks (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
			issue_number INTEGER NOT NULL DEFAULT 0 CHECK (issue_number >= 0),
			title TEXT NOT NULL CHECK (length(trim(title)) > 0),
			goal TEXT NOT NULL CHECK (length(trim(goal)) > 0),
			status TEXT NOT NULL CHECK (
				status IN (
					'proposed',
					'queued',
					'selected',
					'planning',
					'waiting-input',
					'developing',
					'reviewing',
					'fixing',
					'verifying',
					'waiting-ci',
					'blocked',
					'completed',
					'cancelled',
					'deferred'
				)
			),
			priority INTEGER NOT NULL DEFAULT 0 CHECK (priority >= 0),
			sequence INTEGER NOT NULL CHECK (sequence > 0),
			task_type TEXT NOT NULL DEFAULT '',
			source TEXT NOT NULL DEFAULT '',
			source_discovery_id INTEGER CHECK (
				source_discovery_id IS NULL OR source_discovery_id > 0
			),
			blocks_release INTEGER NOT NULL DEFAULT 0 CHECK (blocks_release IN (0, 1)),
			selected_reason TEXT NOT NULL DEFAULT '',
			branch_name TEXT NOT NULL DEFAULT '',
			pr_number INTEGER NOT NULL DEFAULT 0 CHECK (pr_number >= 0),
			dependency_state TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL,
			UNIQUE(project_id, sequence)
		);
		CREATE UNIQUE INDEX idx_project_tasks_issue
			ON project_tasks(project_id, issue_number)
			WHERE issue_number > 0;
		CREATE INDEX idx_project_tasks_status
			ON project_tasks(project_id, status);
		CREATE INDEX idx_project_tasks_priority
			ON project_tasks(project_id, priority DESC, sequence ASC);

		CREATE TRIGGER projects_current_task_insert
		BEFORE INSERT ON projects
		WHEN NEW.current_task_id IS NOT NULL
		BEGIN
			SELECT CASE WHEN NOT EXISTS (
				SELECT 1 FROM project_tasks
				WHERE id = NEW.current_task_id AND project_id = NEW.id
			) THEN RAISE(ABORT, 'current task must belong to project') END;
		END;

		CREATE TRIGGER projects_current_task_update
		BEFORE UPDATE OF current_task_id ON projects
		WHEN NEW.current_task_id IS NOT NULL
		BEGIN
			SELECT CASE WHEN NOT EXISTS (
				SELECT 1 FROM project_tasks
				WHERE id = NEW.current_task_id AND project_id = NEW.id
			) THEN RAISE(ABORT, 'current task must belong to project') END;
		END;

		CREATE TRIGGER project_tasks_selected_delete
		BEFORE DELETE ON project_tasks
		WHEN EXISTS (
			SELECT 1 FROM projects WHERE current_task_id = OLD.id
		)
		BEGIN
			SELECT RAISE(ABORT, 'selected current task cannot be deleted');
		END;

		CREATE TRIGGER project_tasks_selected_move
		BEFORE UPDATE OF project_id ON project_tasks
		WHEN EXISTS (
			SELECT 1 FROM projects
			WHERE current_task_id = OLD.id AND id <> NEW.project_id
		)
		BEGIN
			SELECT RAISE(ABORT, 'selected current task cannot move projects');
		END;
	`},
	// v6: persist immutable artifact metadata and sequential mode executions.
	// Nullable links break the execution/output-artifact creation cycle safely.
	{6, `
		CREATE TABLE artifacts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
			task_id INTEGER REFERENCES project_tasks(id) ON DELETE SET NULL,
			execution_id INTEGER REFERENCES executions(id) ON DELETE SET NULL,
			kind TEXT NOT NULL CHECK (length(trim(kind)) > 0),
			name TEXT NOT NULL CHECK (length(trim(name)) > 0),
			path TEXT NOT NULL CHECK (length(trim(path)) > 0),
			media_type TEXT NOT NULL CHECK (length(trim(media_type)) > 0),
			sha256 TEXT NOT NULL CHECK (length(sha256) = 64),
			size_bytes INTEGER NOT NULL CHECK (size_bytes >= 0),
			created_at DATETIME NOT NULL,
			UNIQUE(project_id, path)
		);

		CREATE TABLE executions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
			task_id INTEGER NOT NULL REFERENCES project_tasks(id) ON DELETE CASCADE,
			mode TEXT NOT NULL CHECK (length(trim(mode)) > 0),
			engine TEXT NOT NULL CHECK (length(trim(engine)) > 0),
			model TEXT NOT NULL DEFAULT '',
			provider_session_id TEXT NOT NULL DEFAULT '',
			attempt INTEGER NOT NULL CHECK (attempt > 0),
			status TEXT NOT NULL CHECK (
				status IN ('pending', 'running', 'completed', 'failed', 'cancelled', 'interrupted')
			),
			input_artifact_id INTEGER REFERENCES artifacts(id) ON DELETE SET NULL,
			output_artifact_id INTEGER REFERENCES artifacts(id) ON DELETE SET NULL,
			started_at DATETIME,
			completed_at DATETIME,
			error_class TEXT NOT NULL DEFAULT '',
			error_message TEXT NOT NULL DEFAULT '',
			input_tokens INTEGER NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
			output_tokens INTEGER NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
			estimated_cost REAL NOT NULL DEFAULT 0 CHECK (estimated_cost >= 0),
			CHECK (completed_at IS NULL OR started_at IS NOT NULL),
			CHECK (
				completed_at IS NULL OR
				julianday(completed_at) >= julianday(started_at)
			),
			UNIQUE(task_id, mode, attempt)
		);

		CREATE INDEX idx_executions_project_task
			ON executions(project_id, task_id, id);
		CREATE INDEX idx_executions_status
			ON executions(project_id, status);
		CREATE INDEX idx_artifacts_task
			ON artifacts(project_id, task_id, id);
		CREATE INDEX idx_artifacts_execution
			ON artifacts(execution_id, id);

		CREATE TRIGGER executions_owner_insert
		BEFORE INSERT ON executions
		BEGIN
			SELECT CASE WHEN NOT EXISTS (
				SELECT 1 FROM project_tasks
				WHERE id = NEW.task_id AND project_id = NEW.project_id
			) THEN RAISE(ABORT, 'execution task must belong to project') END;
		END;

		CREATE TRIGGER executions_owner_update
		BEFORE UPDATE OF project_id, task_id ON executions
		BEGIN
			SELECT CASE WHEN NOT EXISTS (
				SELECT 1 FROM project_tasks
				WHERE id = NEW.task_id AND project_id = NEW.project_id
			) THEN RAISE(ABORT, 'execution task must belong to project') END;
		END;

		CREATE TRIGGER artifacts_owner_insert
		BEFORE INSERT ON artifacts
		BEGIN
			SELECT CASE WHEN NEW.task_id IS NOT NULL AND NOT EXISTS (
				SELECT 1 FROM project_tasks
				WHERE id = NEW.task_id AND project_id = NEW.project_id
			) THEN RAISE(ABORT, 'artifact task must belong to project') END;
			SELECT CASE WHEN NEW.execution_id IS NOT NULL AND NOT EXISTS (
				SELECT 1 FROM executions
				WHERE id = NEW.execution_id
				  AND project_id = NEW.project_id
				  AND (NEW.task_id IS NULL OR task_id = NEW.task_id)
			) THEN RAISE(ABORT, 'artifact execution must match project and task') END;
		END;

		CREATE TRIGGER artifacts_owner_update
		BEFORE UPDATE OF project_id, task_id, execution_id ON artifacts
		BEGIN
			SELECT CASE WHEN NEW.task_id IS NOT NULL AND
				(NEW.project_id IS NOT OLD.project_id OR
				 NEW.task_id IS NOT OLD.task_id) AND NOT EXISTS (
				SELECT 1 FROM project_tasks
				WHERE id = NEW.task_id AND project_id = NEW.project_id
			) THEN RAISE(ABORT, 'artifact task must belong to project') END;
			SELECT CASE WHEN NEW.execution_id IS NOT NULL AND
				(NEW.project_id IS NOT OLD.project_id OR
				 NEW.task_id IS NOT OLD.task_id OR
				 NEW.execution_id IS NOT OLD.execution_id) AND NOT EXISTS (
				SELECT 1 FROM executions
				WHERE id = NEW.execution_id
				  AND project_id = NEW.project_id
				  AND (NEW.task_id IS NULL OR task_id = NEW.task_id)
			) THEN RAISE(ABORT, 'artifact execution must match project and task') END;
		END;

		CREATE TRIGGER executions_artifacts_insert
		BEFORE INSERT ON executions
		BEGIN
			SELECT CASE WHEN NEW.input_artifact_id IS NOT NULL AND NOT EXISTS (
				SELECT 1 FROM artifacts
				WHERE id = NEW.input_artifact_id
				  AND project_id = NEW.project_id
				  AND (task_id IS NULL OR task_id = NEW.task_id)
			) THEN RAISE(ABORT, 'input artifact must match execution project and task') END;
			SELECT CASE WHEN NEW.output_artifact_id IS NOT NULL AND NOT EXISTS (
				SELECT 1 FROM artifacts
				WHERE id = NEW.output_artifact_id
				  AND project_id = NEW.project_id
				  AND (task_id IS NULL OR task_id = NEW.task_id)
			) THEN RAISE(ABORT, 'output artifact must match execution project and task') END;
		END;

		CREATE TRIGGER executions_artifacts_update
		BEFORE UPDATE OF project_id, task_id, input_artifact_id, output_artifact_id ON executions
		BEGIN
			SELECT CASE WHEN NEW.input_artifact_id IS NOT NULL AND
				(NEW.project_id IS NOT OLD.project_id OR
				 NEW.task_id IS NOT OLD.task_id OR
				 NEW.input_artifact_id IS NOT OLD.input_artifact_id) AND NOT EXISTS (
				SELECT 1 FROM artifacts
				WHERE id = NEW.input_artifact_id
				  AND project_id = NEW.project_id
				  AND (task_id IS NULL OR task_id = NEW.task_id)
			) THEN RAISE(ABORT, 'input artifact must match execution project and task') END;
			SELECT CASE WHEN NEW.output_artifact_id IS NOT NULL AND
				(NEW.project_id IS NOT OLD.project_id OR
				 NEW.task_id IS NOT OLD.task_id OR
				 NEW.output_artifact_id IS NOT OLD.output_artifact_id) AND NOT EXISTS (
				SELECT 1 FROM artifacts
				WHERE id = NEW.output_artifact_id
				  AND project_id = NEW.project_id
				  AND (task_id IS NULL OR task_id = NEW.task_id)
			) THEN RAISE(ABORT, 'output artifact must match execution project and task') END;
		END;
	`},
	// v7: persist immutable Engineering Manager decisions and evidence.
	{7, `
		CREATE TABLE manager_reviews (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
			completed_task_id INTEGER REFERENCES project_tasks(id) ON DELETE SET NULL,
			execution_id INTEGER REFERENCES executions(id) ON DELETE SET NULL,
			artifact_id INTEGER REFERENCES artifacts(id) ON DELETE SET NULL,
			project_health TEXT NOT NULL CHECK (
				project_health IN (
					'on-track', 'at-risk', 'off-track', 'blocked', 'ready-for-release'
				)
			),
			progress_estimate INTEGER NOT NULL CHECK (
				progress_estimate >= 0 AND progress_estimate <= 100
			),
			completed_task_decision TEXT NOT NULL CHECK (
				completed_task_decision IN ('accepted', 'rejected', 'deferred', 'not-applicable')
			),
			architecture_review_required INTEGER NOT NULL CHECK (
				architecture_review_required IN (0, 1)
			),
			human_approval_required INTEGER NOT NULL CHECK (
				human_approval_required IN (0, 1)
			),
			discovery_decisions TEXT NOT NULL CHECK (
				length(CAST(discovery_decisions AS BLOB)) <= 1048576 AND
				json_valid(discovery_decisions) AND
				json_type(discovery_decisions) = 'array'
			),
			backlog_changes TEXT NOT NULL CHECK (
				length(CAST(backlog_changes AS BLOB)) <= 1048576 AND
				json_valid(backlog_changes) AND
				json_type(backlog_changes) = 'array'
			),
			next_task_id INTEGER REFERENCES project_tasks(id) ON DELETE SET NULL,
			next_task_issue_number INTEGER NOT NULL DEFAULT 0 CHECK (
				next_task_issue_number >= 0
			),
			next_task_reason TEXT NOT NULL DEFAULT '',
			release_readiness TEXT NOT NULL CHECK (length(trim(release_readiness)) > 0),
			owner_update TEXT NOT NULL CHECK (length(trim(owner_update)) > 0),
			reviewed_at DATETIME NOT NULL
		);
		CREATE INDEX idx_manager_reviews_project
			ON manager_reviews(project_id, id);
		CREATE INDEX idx_manager_reviews_reviewed
			ON manager_reviews(project_id, reviewed_at, id);

		CREATE TRIGGER manager_reviews_owner_insert
		BEFORE INSERT ON manager_reviews
		BEGIN
			SELECT CASE WHEN NEW.completed_task_id IS NOT NULL AND NOT EXISTS (
				SELECT 1 FROM project_tasks
					WHERE id = NEW.completed_task_id AND project_id = NEW.project_id
			) THEN RAISE(ABORT, 'completed task must belong to review project') END;
			SELECT CASE WHEN NEW.execution_id IS NOT NULL AND NOT EXISTS (
				SELECT 1 FROM executions
					WHERE id = NEW.execution_id AND project_id = NEW.project_id
			) THEN RAISE(ABORT, 'execution must belong to review project') END;
			SELECT CASE WHEN NEW.artifact_id IS NOT NULL AND NOT EXISTS (
				SELECT 1 FROM artifacts
					WHERE id = NEW.artifact_id AND project_id = NEW.project_id
			) THEN RAISE(ABORT, 'artifact must belong to review project') END;
			SELECT CASE WHEN NEW.next_task_id IS NOT NULL AND NOT EXISTS (
				SELECT 1 FROM project_tasks
					WHERE id = NEW.next_task_id AND project_id = NEW.project_id
			) THEN RAISE(ABORT, 'next task must belong to review project') END;
		END;

		CREATE TRIGGER manager_reviews_owner_update
		BEFORE UPDATE OF project_id, completed_task_id, execution_id, artifact_id, next_task_id
		ON manager_reviews
		BEGIN
			SELECT CASE WHEN NEW.completed_task_id IS NOT NULL AND
				(NEW.project_id IS NOT OLD.project_id OR
				 NEW.completed_task_id IS NOT OLD.completed_task_id) AND NOT EXISTS (
				SELECT 1 FROM project_tasks
				WHERE id = NEW.completed_task_id AND project_id = NEW.project_id
			) THEN RAISE(ABORT, 'completed task must belong to review project') END;
			SELECT CASE WHEN NEW.execution_id IS NOT NULL AND
				(NEW.project_id IS NOT OLD.project_id OR
				 NEW.execution_id IS NOT OLD.execution_id) AND NOT EXISTS (
				SELECT 1 FROM executions
				WHERE id = NEW.execution_id AND project_id = NEW.project_id
			) THEN RAISE(ABORT, 'execution must belong to review project') END;
			SELECT CASE WHEN NEW.artifact_id IS NOT NULL AND
				(NEW.project_id IS NOT OLD.project_id OR
				 NEW.artifact_id IS NOT OLD.artifact_id) AND NOT EXISTS (
				SELECT 1 FROM artifacts
				WHERE id = NEW.artifact_id AND project_id = NEW.project_id
			) THEN RAISE(ABORT, 'artifact must belong to review project') END;
			SELECT CASE WHEN NEW.next_task_id IS NOT NULL AND
				(NEW.project_id IS NOT OLD.project_id OR
				 NEW.next_task_id IS NOT OLD.next_task_id) AND NOT EXISTS (
				SELECT 1 FROM project_tasks
				WHERE id = NEW.next_task_id AND project_id = NEW.project_id
			) THEN RAISE(ABORT, 'next task must belong to review project') END;
		END;
	`},
	// v8: record idempotent legacy-to-project conversions while preserving the
	// legacy task rows for backward-compatible daemon operation.
	{8, `
		CREATE TABLE legacy_project_migrations (
			legacy_task_id INTEGER PRIMARY KEY REFERENCES tasks(id) ON DELETE CASCADE,
			project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
			project_task_id INTEGER NOT NULL REFERENCES project_tasks(id) ON DELETE CASCADE,
			execution_id INTEGER REFERENCES executions(id) ON DELETE SET NULL,
			migrated_at DATETIME NOT NULL,
			UNIQUE(project_task_id)
		);
		CREATE INDEX idx_legacy_project_migrations_project
			ON legacy_project_migrations(project_id, legacy_task_id);
	`},
	// v9: one Madar instance owns one global v2 delivery lane. An expression
	// index makes active status uniqueness independent of project identity.
	{9, `
		CREATE UNIQUE INDEX idx_project_tasks_single_active
			ON project_tasks((1))
			WHERE status IN (
				'selected', 'planning', 'waiting-input', 'developing',
				'reviewing', 'fixing', 'verifying', 'waiting-ci', 'blocked'
			);
	`},
	// v10: one Madar instance may run only one provider process at a time.
	// The expression index makes the execution lane global across all projects,
	// tasks, modes, and attempts.
	{10, `
		CREATE UNIQUE INDEX idx_executions_single_running
			ON executions((1))
			WHERE status = 'running';
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
		tx, err := s.db.Begin()
		if err != nil {
			return fmt.Errorf("begin migration v%d: %w", m.version, err)
		}
		if _, err := tx.Exec(m.sql); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf(
				"migration v%d: %w",
				m.version,
				classifyRunningExecutionConstraint(err),
			)
		}
		if _, err := tx.Exec(
			`INSERT INTO schema_migrations (version) VALUES (?)`, m.version,
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration v%d: %w", m.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration v%d: %w", m.version, err)
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
		SELECT id, repo, issue_number, session_id, engine, model, state, last_clarification_at,
		       pr_number, ci_state, ci_retries, ci_watch_started_at, created_at, updated_at
		FROM tasks WHERE repo = ? AND issue_number = ?
	`, repo, issueNumber)
	return scanTask(row)
}

// BindTaskExecution atomically creates or binds a legacy task to one provider
// execution context. Existing non-empty bindings cannot be changed implicitly.
// A pre-migration task with a provider session but no engine is bound lazily
// without replacing that session.
func (s *Store) BindTaskExecution(
	repo string,
	issueNumber int,
	state TaskState,
	binding ExecutionBinding,
) (*Task, error) {
	if binding.Engine == "" {
		return nil, fmt.Errorf("bind task execution: engine is required")
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin task execution binding: %w", err)
	}
	defer tx.Rollback()

	var currentEngine, currentModel, currentSession string
	err = tx.QueryRow(`
		SELECT engine, model, session_id
		FROM tasks
		WHERE repo = ? AND issue_number = ?
	`, repo, issueNumber).Scan(&currentEngine, &currentModel, &currentSession)
	switch {
	case err == sql.ErrNoRows:
		now := time.Now().UTC()
		if _, err := tx.Exec(`
			INSERT INTO tasks (
				repo, issue_number, state, session_id, engine, model, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		`, repo, issueNumber, string(state), binding.ProviderSessionID, binding.Engine, binding.Model, now, now); err != nil {
			return nil, fmt.Errorf("insert task execution binding: %w", err)
		}
	case err != nil:
		return nil, fmt.Errorf("read task execution binding: %w", err)
	default:
		if currentEngine != "" &&
			(currentEngine != binding.Engine || currentModel != binding.Model) {
			return nil, fmt.Errorf(
				"%w: stored engine/model %q/%q, requested %q/%q",
				ErrExecutionBindingConflict,
				currentEngine,
				currentModel,
				binding.Engine,
				binding.Model,
			)
		}
		sessionID := currentSession
		if sessionID == "" {
			sessionID = binding.ProviderSessionID
		}
		if _, err := tx.Exec(`
			UPDATE tasks
			SET state = ?, session_id = ?, engine = ?, model = ?, updated_at = ?
			WHERE repo = ? AND issue_number = ?
		`, string(state), sessionID, binding.Engine, binding.Model, time.Now().UTC(), repo, issueNumber); err != nil {
			return nil, fmt.Errorf("update task execution binding: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit task execution binding: %w", err)
	}
	return s.GetTask(repo, issueNumber)
}

func (s *Store) SetSessionID(repo string, issueNumber int, sessionID string) error {
	result, err := s.db.Exec(`
		UPDATE tasks SET session_id = ?, updated_at = ? WHERE repo = ? AND issue_number = ?
	`, sessionID, time.Now().UTC(), repo, issueNumber)
	if err != nil {
		return fmt.Errorf("set provider session ID: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read provider session update count: %w", err)
	}
	if updated != 1 {
		return fmt.Errorf("set provider session ID: task %s#%d not found", repo, issueNumber)
	}
	return nil
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
		SELECT id, repo, issue_number, session_id, engine, model, state, last_clarification_at,
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
		SELECT id, repo, issue_number, session_id, engine, model, state, last_clarification_at,
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

// MarkActiveTasksInterrupted atomically records process-bound tasks left by a
// previous daemon instance. CI-waiting tasks are intentionally excluded
// because they have no provider process to recover.
func (s *Store) MarkActiveTasksInterrupted() (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin interruption transaction: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.Query(`
		SELECT repo, issue_number
		FROM tasks
		WHERE state IN ('in-progress', 'recovering') AND ci_state = ''
		ORDER BY created_at ASC
	`)
	if err != nil {
		return 0, fmt.Errorf("list active tasks for interruption: %w", err)
	}

	type taskKey struct {
		repo        string
		issueNumber int
	}
	var tasks []taskKey
	for rows.Next() {
		var task taskKey
		if err := rows.Scan(&task.repo, &task.issueNumber); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan active task for interruption: %w", err)
		}
		tasks = append(tasks, task)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close active task rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate active tasks for interruption: %w", err)
	}

	now := time.Now().UTC()
	for _, task := range tasks {
		if _, err := tx.Exec(`
			UPDATE tasks SET state = ?, updated_at = ?
			WHERE repo = ? AND issue_number = ?
		`, string(StateInterrupted), now, task.repo, task.issueNumber); err != nil {
			return 0, fmt.Errorf("mark task interrupted: %w", err)
		}
		if _, err := tx.Exec(`
			INSERT INTO audit_log (repo, issue_number, event, details)
			VALUES (?, ?, 'interrupted', 'startup detected an unfinished provider execution')
		`, task.repo, task.issueNumber); err != nil {
			return 0, fmt.Errorf("audit interrupted task: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit interruption transaction: %w", err)
	}
	return int64(len(tasks)), nil
}

// CountActive returns the number of tasks with an active provider execution.
// Excluded from the count:
//   - awaiting-feedback: parked, waiting for human input, the provider is idle
//   - in-progress with ci_state=waiting: the provider finished, only CI polling remains
//
// Only tasks where a provider is genuinely running (in-progress, not CI-watching)
// count toward the max_parallel ceiling.
func (s *Store) CountActive() (int, error) {
	var count int
	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM tasks
		WHERE state IN ('in-progress', 'recovering') AND ci_state = ''
	`).Scan(&count)
	return count, err
}

func (s *Store) Log(repo string, issueNumber int, event, details string) error {
	_, err := s.db.Exec(`
		INSERT INTO audit_log (repo, issue_number, event, details) VALUES (?, ?, ?, ?)
	`, repo, issueNumber, event, details)
	return err
}

// PruneAuditLog deletes audit log entries older than the given duration.
// Returns the number of rows deleted.
func (s *Store) PruneAuditLog(olderThan time.Duration) (int64, error) {
	threshold := time.Now().UTC().Add(-olderThan)
	res, err := s.db.Exec(`DELETE FROM audit_log WHERE created_at < ?`, threshold)
	if err != nil {
		return 0, fmt.Errorf("prune audit_log: %w", err)
	}
	return res.RowsAffected()
}

// PruneCompletedTasks deletes done tasks whose updated_at is older than the
// given duration. Returns the number of rows deleted.
func (s *Store) PruneCompletedTasks(olderThan time.Duration) (int64, error) {
	threshold := time.Now().UTC().Add(-olderThan)
	res, err := s.db.Exec(
		`DELETE FROM tasks WHERE state = 'done' AND updated_at < ?`, threshold,
	)
	if err != nil {
		return 0, fmt.Errorf("prune tasks: %w", err)
	}
	return res.RowsAffected()
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
		&t.ID, &t.Repo, &t.IssueNumber, &t.SessionID, &t.Engine, &t.Model, &state, &clarAt,
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
