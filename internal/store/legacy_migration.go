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
	ErrNoLegacyTasks           = errors.New("no legacy tasks found")
	ErrLegacyMigrationConflict = errors.New("legacy project migration conflict")
)

type LegacyProjectMigrationOptions struct {
	Repo              string
	Name              string
	Goal              string
	Scope             string
	ReleaseTarget     string
	ParentIssueNumber int
}

type LegacyProjectMigrationReport struct {
	Project             *domain.Project
	ProjectCreated      bool
	LegacyTasks         int
	MigratedTasks       int
	AlreadyMigrated     int
	ProjectTasksCreated int
	ExecutionsCreated   int
}

// MigrateLegacyProject atomically creates or reuses one v2 project and maps
// all of the repository's legacy task rows into its ordered backlog. The
// legacy rows are read-only inputs and remain available to the v1 daemon.
func (s *Store) MigrateLegacyProject(
	options LegacyProjectMigrationOptions,
) (*LegacyProjectMigrationReport, error) {
	options.Repo = strings.TrimSpace(options.Repo)
	options.Name = strings.TrimSpace(options.Name)
	options.Goal = strings.TrimSpace(options.Goal)
	options.Scope = strings.TrimSpace(options.Scope)
	options.ReleaseTarget = strings.TrimSpace(options.ReleaseTarget)

	candidate := domain.NewProject(
		options.Repo,
		options.Name,
		options.Goal,
		options.Scope,
	)
	candidate.ReleaseTarget = options.ReleaseTarget
	candidate.ParentIssueNumber = options.ParentIssueNumber
	if err := candidate.Validate(); err != nil {
		return nil, fmt.Errorf("migrate legacy project: %w", err)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin legacy project migration: %w", err)
	}
	defer tx.Rollback()

	legacyTasks, err := listLegacyTasksForMigration(tx, options.Repo)
	if err != nil {
		return nil, err
	}
	if len(legacyTasks) == 0 {
		return nil, fmt.Errorf("%w: repository %q", ErrNoLegacyTasks, options.Repo)
	}
	for _, task := range legacyTasks {
		if task.IssueNumber <= 0 {
			return nil, fmt.Errorf(
				"%w: legacy task %d has invalid issue number %d",
				ErrLegacyMigrationConflict,
				task.ID,
				task.IssueNumber,
			)
		}
		if _, err := migratedTaskStatus(task); err != nil {
			return nil, err
		}
	}

	project, created, err := findOrCreateMigrationProject(tx, candidate)
	if err != nil {
		return nil, err
	}
	report := &LegacyProjectMigrationReport{
		ProjectCreated: created,
		LegacyTasks:    len(legacyTasks),
	}

	nextSequence, err := nextProjectTaskSequence(tx, project.ID)
	if err != nil {
		return nil, err
	}
	var currentTaskID *int64
	var currentTaskStatus domain.TaskStatus
	hasIncomplete := false

	for _, legacy := range legacyTasks {
		mappedProjectID, found, err := existingLegacyMapping(tx, legacy.ID)
		if err != nil {
			return nil, err
		}
		if found {
			if mappedProjectID != project.ID {
				return nil, fmt.Errorf(
					"%w: legacy task %d is mapped to project %d, not %d",
					ErrLegacyMigrationConflict,
					legacy.ID,
					mappedProjectID,
					project.ID,
				)
			}
			report.AlreadyMigrated++
			continue
		}

		status, err := migratedTaskStatus(legacy)
		if err != nil {
			return nil, err
		}
		projectTask, taskCreated, err := findOrCreateMigratedTask(
			tx,
			project.ID,
			nextSequence,
			legacy,
			status,
		)
		if err != nil {
			return nil, err
		}
		if taskCreated {
			nextSequence++
			report.ProjectTasksCreated++
		}
		if status != domain.TaskCompleted && status != domain.TaskCancelled {
			hasIncomplete = true
		}
		if currentTaskID == nil && legacyTaskIsActive(legacy.State) {
			value := projectTask.ID
			currentTaskID = &value
			currentTaskStatus = status
		}

		executionID, executionCreated, err := migrateLegacyExecution(
			tx,
			project.ID,
			projectTask.ID,
			legacy,
		)
		if err != nil {
			return nil, err
		}
		if executionCreated {
			report.ExecutionsCreated++
		}
		if _, err := tx.Exec(`
			INSERT INTO legacy_project_migrations (
				legacy_task_id, project_id, project_task_id, execution_id, migrated_at
			) VALUES (?, ?, ?, ?, ?)
		`, legacy.ID, project.ID, projectTask.ID, nullableLegacyID(executionID), time.Now().UTC()); err != nil {
			return nil, fmt.Errorf("record legacy project migration: %w", err)
		}
		if _, err := tx.Exec(`
			INSERT INTO audit_log (repo, issue_number, event, details)
			VALUES (?, ?, 'migrated-to-project', ?)
		`, legacy.Repo, legacy.IssueNumber, fmt.Sprintf(
			"project=%d project_task=%d execution=%d",
			project.ID,
			projectTask.ID,
			executionID,
		)); err != nil {
			return nil, fmt.Errorf("audit legacy project migration: %w", err)
		}
		report.MigratedTasks++
	}

	if created {
		state := domain.ProjectPlanning
		switch {
		case currentTaskID != nil && currentTaskStatus == domain.TaskBlocked:
			state = domain.ProjectBlocked
		case currentTaskID != nil:
			state = domain.ProjectExecuting
		case !hasIncomplete:
			state = domain.ProjectCompleted
		}
		if _, err := tx.Exec(`
			UPDATE projects
			SET state = ?, current_task_id = ?, updated_at = ?
			WHERE id = ?
		`, string(state), nullableInt64(currentTaskID), time.Now().UTC(), project.ID); err != nil {
			return nil, fmt.Errorf("initialize migrated project state: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit legacy project migration: %w", err)
	}
	report.Project, err = s.GetProjectByID(project.ID)
	if err != nil {
		return nil, err
	}
	return report, nil
}

func listLegacyTasksForMigration(tx *sql.Tx, repo string) ([]*Task, error) {
	rows, err := tx.Query(`
		SELECT id, repo, issue_number, session_id, engine, model, state,
		       last_clarification_at, pr_number, ci_state, ci_retries,
		       ci_watch_started_at, created_at, updated_at
		FROM tasks
		WHERE repo = ?
		ORDER BY created_at ASC, id ASC
	`, repo)
	if err != nil {
		return nil, fmt.Errorf("list legacy tasks for migration: %w", err)
	}
	defer rows.Close()
	var tasks []*Task
	for rows.Next() {
		task, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate legacy tasks for migration: %w", err)
	}
	return tasks, nil
}

func findOrCreateMigrationProject(
	tx *sql.Tx,
	candidate *domain.Project,
) (*domain.Project, bool, error) {
	project, err := scanProject(tx.QueryRow(projectSelect+` WHERE repo = ?`, candidate.Repo))
	if err != nil {
		return nil, false, err
	}
	if project != nil {
		return project, false, nil
	}
	now := time.Now().UTC()
	result, err := tx.Exec(`
		INSERT INTO projects (
			repo, parent_issue_number, name, goal, scope, state, health,
			current_task_id, current_plan_version, architecture_version,
			release_target, release_readiness, last_manager_review_at,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, NULL, 0, 0, ?, '', NULL, ?, ?)
	`,
		candidate.Repo,
		candidate.ParentIssueNumber,
		candidate.Name,
		candidate.Goal,
		candidate.Scope,
		string(domain.ProjectInitializing),
		string(domain.HealthOnTrack),
		candidate.ReleaseTarget,
		now,
		now,
	)
	if err != nil {
		return nil, false, fmt.Errorf("create migrated project: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, false, fmt.Errorf("read migrated project ID: %w", err)
	}
	project, err = scanProject(tx.QueryRow(projectSelect+` WHERE id = ?`, id))
	return project, true, err
}

func nextProjectTaskSequence(tx *sql.Tx, projectID int64) (int, error) {
	var sequence int
	if err := tx.QueryRow(`
		SELECT COALESCE(MAX(sequence), 0) + 1
		FROM project_tasks
		WHERE project_id = ?
	`, projectID).Scan(&sequence); err != nil {
		return 0, fmt.Errorf("choose migrated task sequence: %w", err)
	}
	return sequence, nil
}

func existingLegacyMapping(
	tx *sql.Tx,
	legacyTaskID int64,
) (projectID int64, found bool, err error) {
	err = tx.QueryRow(`
		SELECT project_id
		FROM legacy_project_migrations
		WHERE legacy_task_id = ?
	`, legacyTaskID).Scan(&projectID)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read legacy project migration: %w", err)
	}
	return projectID, true, nil
}

func findOrCreateMigratedTask(
	tx *sql.Tx,
	projectID int64,
	sequence int,
	legacy *Task,
	status domain.TaskStatus,
) (*domain.Task, bool, error) {
	task, err := scanProjectTask(tx.QueryRow(
		projectTaskSelect+` WHERE project_id = ? AND issue_number = ?`,
		projectID,
		legacy.IssueNumber,
	))
	if err != nil {
		return nil, false, err
	}
	if task != nil {
		return task, false, nil
	}
	title := fmt.Sprintf("Legacy issue #%d", legacy.IssueNumber)
	goal := fmt.Sprintf("Continue work from legacy issue #%d.", legacy.IssueNumber)
	result, err := tx.Exec(`
		INSERT INTO project_tasks (
			project_id, issue_number, title, goal, status, priority, sequence,
			task_type, source, source_discovery_id, blocks_release,
			selected_reason, branch_name, pr_number, dependency_state,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, 0, ?, 'legacy-issue', 'legacy', NULL, 0, '', ?, ?, '', ?, ?)
	`,
		projectID,
		legacy.IssueNumber,
		title,
		goal,
		string(status),
		sequence,
		fmt.Sprintf("madar/issue-%d", legacy.IssueNumber),
		legacy.PRNumber,
		legacy.CreatedAt.UTC(),
		legacy.UpdatedAt.UTC(),
	)
	if err != nil {
		return nil, false, fmt.Errorf("create migrated project task: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, false, fmt.Errorf("read migrated project task ID: %w", err)
	}
	task, err = scanProjectTask(tx.QueryRow(projectTaskSelect+` WHERE id = ?`, id))
	return task, true, err
}

func migrateLegacyExecution(
	tx *sql.Tx,
	projectID, projectTaskID int64,
	legacy *Task,
) (int64, bool, error) {
	if legacy.Engine == "" && legacy.SessionID == "" {
		return 0, false, nil
	}
	var existingID int64
	err := tx.QueryRow(`
		SELECT id FROM executions
		WHERE task_id = ? AND mode = 'legacy-developer' AND attempt = 1
	`, projectTaskID).Scan(&existingID)
	if err == nil {
		return existingID, false, nil
	}
	if err != sql.ErrNoRows {
		return 0, false, fmt.Errorf("find migrated legacy execution: %w", err)
	}

	engineName := legacy.Engine
	if engineName == "" {
		engineName = "claude"
	}
	status := migratedExecutionStatus(legacy)
	var startedAt, completedAt *time.Time
	if status != domain.ExecutionPending {
		started := legacy.CreatedAt.UTC()
		startedAt = &started
	}
	if status == domain.ExecutionCompleted {
		completed := legacy.UpdatedAt.UTC()
		completedAt = &completed
	}
	result, err := tx.Exec(`
		INSERT INTO executions (
			project_id, task_id, mode, engine, model, provider_session_id,
			attempt, status, input_artifact_id, output_artifact_id,
			started_at, completed_at, error_class, error_message,
			input_tokens, output_tokens, estimated_cost
		) VALUES (?, ?, 'legacy-developer', ?, ?, ?, 1, ?, NULL, NULL, ?, ?, '', '', 0, 0, 0)
	`,
		projectID,
		projectTaskID,
		engineName,
		legacy.Model,
		legacy.SessionID,
		string(status),
		nullableTime(startedAt),
		nullableTime(completedAt),
	)
	if err != nil {
		return 0, false, fmt.Errorf("create migrated legacy execution: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, false, fmt.Errorf("read migrated legacy execution ID: %w", err)
	}
	return id, true, nil
}

func migratedTaskStatus(task *Task) (domain.TaskStatus, error) {
	if task.PRNumber < 0 {
		return "", fmt.Errorf(
			"%w: legacy task %d has invalid PR number %d",
			ErrLegacyMigrationConflict,
			task.ID,
			task.PRNumber,
		)
	}
	switch task.CIState {
	case CIStateWaiting, CIStateFailed:
		return domain.TaskWaitingCI, nil
	case CIStateGaveUp:
		return domain.TaskBlocked, nil
	case CIStatePassed:
		if task.State == StateDone {
			return domain.TaskCompleted, nil
		}
		return domain.TaskVerifying, nil
	case CIStateNone:
	default:
		return "", fmt.Errorf(
			"%w: legacy task %d has unknown CI state %q",
			ErrLegacyMigrationConflict,
			task.ID,
			task.CIState,
		)
	}
	switch task.State {
	case StateReady:
		return domain.TaskQueued, nil
	case StateInProgress, StateRecovering, StateInterrupted:
		return domain.TaskDeveloping, nil
	case StateAwaitingFeedback:
		return domain.TaskWaitingInput, nil
	case StateDone:
		return domain.TaskCompleted, nil
	default:
		return "", fmt.Errorf(
			"%w: legacy task %d has unknown state %q",
			ErrLegacyMigrationConflict,
			task.ID,
			task.State,
		)
	}
}

func migratedExecutionStatus(task *Task) domain.ExecutionStatus {
	if task.CIState != CIStateNone || task.State == StateDone {
		return domain.ExecutionCompleted
	}
	switch task.State {
	case StateReady:
		return domain.ExecutionPending
	case StateInterrupted, StateAwaitingFeedback:
		return domain.ExecutionInterrupted
	default:
		return domain.ExecutionRunning
	}
}

func legacyTaskIsActive(state TaskState) bool {
	switch state {
	case StateInProgress, StateInterrupted, StateRecovering, StateAwaitingFeedback:
		return true
	default:
		return false
	}
}

func nullableLegacyID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}
