package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

var (
	ErrProjectAlreadyExists = errors.New("project already exists")
	ErrProjectNotFound      = errors.New("project not found")
)

// CreateProject persists a new Project aggregate. Repository identity is
// unique, and timestamps are assigned by the store in UTC.
func (s *Store) CreateProject(project *domain.Project) (*domain.Project, error) {
	if err := project.Validate(); err != nil {
		return nil, fmt.Errorf("create project: %w", err)
	}
	if project.ID != 0 {
		return nil, fmt.Errorf("%w: new project ID must be zero", domain.ErrInvalidProject)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin create project: %w", err)
	}
	defer tx.Rollback()

	var existingID int64
	err = tx.QueryRow(`SELECT id FROM projects WHERE repo = ?`, project.Repo).Scan(&existingID)
	switch {
	case err == nil:
		return nil, fmt.Errorf("%w: repository %q is project %d", ErrProjectAlreadyExists, project.Repo, existingID)
	case err != sql.ErrNoRows:
		return nil, fmt.Errorf("check project repository: %w", err)
	}

	now := time.Now().UTC()
	result, err := tx.Exec(`
		INSERT INTO projects (
			repo, parent_issue_number, name, goal, scope, state, paused_from_state, health,
			current_task_id, current_plan_version, architecture_version,
			release_target, release_readiness, last_manager_review_at,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		project.Repo,
		project.ParentIssueNumber,
		project.Name,
		project.Goal,
		project.Scope,
		string(project.State),
		string(project.PausedFromState),
		string(project.Health),
		nullableInt64(project.CurrentTaskID),
		project.CurrentPlanVersion,
		project.ArchitectureVersion,
		project.ReleaseTarget,
		project.ReleaseReadiness,
		nullableTime(project.LastManagerReviewAt),
		now,
		now,
	)
	if err != nil {
		return nil, fmt.Errorf("insert project: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("read project ID: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit create project: %w", err)
	}
	return s.GetProjectByID(id)
}

func (s *Store) GetProjectByID(id int64) (*domain.Project, error) {
	return scanProject(s.db.QueryRow(projectSelect+` WHERE id = ?`, id))
}

func (s *Store) GetProjectByRepo(repo string) (*domain.Project, error) {
	return scanProject(s.db.QueryRow(projectSelect+` WHERE repo = ?`, repo))
}

// UpdateProject replaces all mutable aggregate fields while preserving the
// database identity and creation timestamp.
func (s *Store) UpdateProject(project *domain.Project) (*domain.Project, error) {
	if err := project.Validate(); err != nil {
		return nil, fmt.Errorf("update project: %w", err)
	}
	if project.ID <= 0 {
		return nil, fmt.Errorf("%w: persisted project ID must be positive", domain.ErrInvalidProject)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin update project: %w", err)
	}
	defer tx.Rollback()

	var conflictingID int64
	err = tx.QueryRow(
		`SELECT id FROM projects WHERE repo = ? AND id <> ?`,
		project.Repo,
		project.ID,
	).Scan(&conflictingID)
	switch {
	case err == nil:
		return nil, fmt.Errorf(
			"%w: repository %q is project %d",
			ErrProjectAlreadyExists,
			project.Repo,
			conflictingID,
		)
	case err != sql.ErrNoRows:
		return nil, fmt.Errorf("check project repository: %w", err)
	}

	result, err := tx.Exec(`
		UPDATE projects SET
			repo = ?,
			parent_issue_number = ?,
			name = ?,
			goal = ?,
			scope = ?,
			state = ?,
			paused_from_state = ?,
			health = ?,
			current_task_id = ?,
			current_plan_version = ?,
			architecture_version = ?,
			release_target = ?,
			release_readiness = ?,
			last_manager_review_at = ?,
			updated_at = ?
		WHERE id = ?
	`,
		project.Repo,
		project.ParentIssueNumber,
		project.Name,
		project.Goal,
		project.Scope,
		string(project.State),
		string(project.PausedFromState),
		string(project.Health),
		nullableInt64(project.CurrentTaskID),
		project.CurrentPlanVersion,
		project.ArchitectureVersion,
		project.ReleaseTarget,
		project.ReleaseReadiness,
		nullableTime(project.LastManagerReviewAt),
		time.Now().UTC(),
		project.ID,
	)
	if err != nil {
		return nil, fmt.Errorf("update project: %w", err)
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("read project update count: %w", err)
	}
	if updated != 1 {
		return nil, fmt.Errorf("%w: ID %d", ErrProjectNotFound, project.ID)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit update project: %w", err)
	}
	return s.GetProjectByID(project.ID)
}

// ListProjects returns a stable creation-order snapshot.
func (s *Store) ListProjects() ([]*domain.Project, error) {
	rows, err := s.db.Query(projectSelect + ` ORDER BY id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()

	var projects []*domain.Project
	for rows.Next() {
		project, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		projects = append(projects, project)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate projects: %w", err)
	}
	return projects, nil
}

const projectSelect = `
	SELECT
		id, repo, parent_issue_number, name, goal, scope, state, paused_from_state, health,
		current_task_id, current_plan_version, architecture_version,
		release_target, release_readiness, last_manager_review_at,
		created_at, updated_at
	FROM projects
`

func scanProject(row scanner) (*domain.Project, error) {
	var (
		project       domain.Project
		state         string
		pausedFrom    string
		health        string
		currentTaskID sql.NullInt64
		lastReviewAt  sql.NullTime
	)
	if err := row.Scan(
		&project.ID,
		&project.Repo,
		&project.ParentIssueNumber,
		&project.Name,
		&project.Goal,
		&project.Scope,
		&state,
		&pausedFrom,
		&health,
		&currentTaskID,
		&project.CurrentPlanVersion,
		&project.ArchitectureVersion,
		&project.ReleaseTarget,
		&project.ReleaseReadiness,
		&lastReviewAt,
		&project.CreatedAt,
		&project.UpdatedAt,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("scan project: %w", err)
	}
	project.State = domain.ProjectState(state)
	project.PausedFromState = domain.ProjectState(pausedFrom)
	project.Health = domain.ProjectHealth(health)
	if currentTaskID.Valid {
		value := currentTaskID.Int64
		project.CurrentTaskID = &value
	}
	if lastReviewAt.Valid {
		value := lastReviewAt.Time.UTC()
		project.LastManagerReviewAt = &value
	}
	project.CreatedAt = project.CreatedAt.UTC()
	project.UpdatedAt = project.UpdatedAt.UTC()
	return &project, nil
}

func nullableInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UTC()
}
