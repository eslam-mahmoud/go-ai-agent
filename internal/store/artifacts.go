package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

var (
	ErrArtifactAlreadyExists = errors.New("artifact already exists")
	ErrArtifactNotFound      = errors.New("artifact not found")
)

func (s *Store) CreateArtifact(artifact *domain.Artifact) (*domain.Artifact, error) {
	if err := artifact.Validate(); err != nil {
		return nil, fmt.Errorf("create artifact: %w", err)
	}
	if artifact.ID != 0 {
		return nil, fmt.Errorf("%w: new artifact ID must be zero", domain.ErrInvalidArtifact)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin create artifact: %w", err)
	}
	defer tx.Rollback()
	if err := requireProject(tx, artifact.ProjectID); err != nil {
		return nil, err
	}
	if artifact.TaskID != nil {
		if err := requireProjectTask(tx, artifact.ProjectID, *artifact.TaskID); err != nil {
			return nil, err
		}
	}
	if artifact.ExecutionID != nil {
		var projectID, taskID int64
		err := tx.QueryRow(`
			SELECT project_id, task_id FROM executions WHERE id = ?
		`, *artifact.ExecutionID).Scan(&projectID, &taskID)
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("%w: ID %d", ErrExecutionNotFound, *artifact.ExecutionID)
		}
		if err != nil {
			return nil, fmt.Errorf("check artifact execution: %w", err)
		}
		if projectID != artifact.ProjectID ||
			(artifact.TaskID != nil && taskID != *artifact.TaskID) {
			return nil, fmt.Errorf(
				"%w: execution belongs to another project or task",
				domain.ErrInvalidArtifact,
			)
		}
	}
	var existing int64
	err = tx.QueryRow(`
		SELECT id FROM artifacts WHERE project_id = ? AND path = ?
	`, artifact.ProjectID, artifact.Path).Scan(&existing)
	if err == nil {
		return nil, fmt.Errorf(
			"%w: project %d path %q",
			ErrArtifactAlreadyExists, artifact.ProjectID, artifact.Path,
		)
	}
	if err != sql.ErrNoRows {
		return nil, fmt.Errorf("check artifact path: %w", err)
	}
	result, err := tx.Exec(`
		INSERT INTO artifacts (
			project_id, task_id, execution_id, kind, name, path,
			media_type, sha256, size_bytes, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		artifact.ProjectID, nullableInt64(artifact.TaskID),
		nullableInt64(artifact.ExecutionID), artifact.Kind, artifact.Name,
		artifact.Path, artifact.MediaType, artifact.SHA256, artifact.SizeBytes,
		time.Now().UTC(),
	)
	if err != nil {
		return nil, fmt.Errorf("insert artifact: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("read artifact ID: %w", err)
	}
	if err := touchProject(tx, artifact.ProjectID, time.Now().UTC()); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit create artifact: %w", err)
	}
	return s.GetArtifactByID(id)
}

func (s *Store) GetArtifactByID(id int64) (*domain.Artifact, error) {
	return scanArtifact(s.db.QueryRow(artifactSelect+` WHERE id = ?`, id))
}

func (s *Store) ListProjectArtifacts(projectID int64) ([]*domain.Artifact, error) {
	if project, err := s.GetProjectByID(projectID); err != nil {
		return nil, err
	} else if project == nil {
		return nil, fmt.Errorf("%w: ID %d", ErrProjectNotFound, projectID)
	}
	return s.listArtifacts(artifactSelect+` WHERE project_id = ? ORDER BY id ASC`, projectID)
}

func (s *Store) ListExecutionArtifacts(executionID int64) ([]*domain.Artifact, error) {
	if execution, err := s.GetExecutionByID(executionID); err != nil {
		return nil, err
	} else if execution == nil {
		return nil, fmt.Errorf("%w: ID %d", ErrExecutionNotFound, executionID)
	}
	return s.listArtifacts(artifactSelect+` WHERE execution_id = ? ORDER BY id ASC`, executionID)
}

func (s *Store) listArtifacts(query string, argument any) ([]*domain.Artifact, error) {
	rows, err := s.db.Query(query, argument)
	if err != nil {
		return nil, fmt.Errorf("list artifacts: %w", err)
	}
	defer rows.Close()
	var artifacts []*domain.Artifact
	for rows.Next() {
		artifact, err := scanArtifact(rows)
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, artifact)
	}
	return artifacts, rows.Err()
}

const artifactSelect = `
	SELECT
		id, project_id, task_id, execution_id, kind, name, path,
		media_type, sha256, size_bytes, created_at
	FROM artifacts
`

func scanArtifact(row scanner) (*domain.Artifact, error) {
	var artifact domain.Artifact
	var taskID, executionID sql.NullInt64
	if err := row.Scan(
		&artifact.ID, &artifact.ProjectID, &taskID, &executionID,
		&artifact.Kind, &artifact.Name, &artifact.Path, &artifact.MediaType,
		&artifact.SHA256, &artifact.SizeBytes, &artifact.CreatedAt,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("scan artifact: %w", err)
	}
	artifact.TaskID = nullInt64Pointer(taskID)
	artifact.ExecutionID = nullInt64Pointer(executionID)
	artifact.CreatedAt = artifact.CreatedAt.UTC()
	return &artifact, nil
}
