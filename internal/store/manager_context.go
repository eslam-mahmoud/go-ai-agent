package store

import (
	"database/sql"
	"fmt"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

// ManagerContextAggregate is one transactionally consistent, read-only view
// of all durable inputs currently available to an Engineering Manager review.
type ManagerContextAggregate struct {
	Project        *domain.Project
	Tasks          []*domain.Task
	Executions     []*domain.Execution
	Artifacts      []*domain.Artifact
	ManagerReviews []*domain.ManagerReview
	WorkflowEvents []*domain.WorkflowEvent
}

func (s *Store) LoadManagerContextAggregate(
	projectID int64,
) (*ManagerContextAggregate, error) {
	if projectID <= 0 {
		return nil, fmt.Errorf("%w: ID %d", ErrProjectNotFound, projectID)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin manager context read: %w", err)
	}
	defer tx.Rollback()

	project, err := scanProject(tx.QueryRow(projectSelect+` WHERE id = ?`, projectID))
	if err != nil {
		return nil, err
	}
	if project == nil {
		return nil, fmt.Errorf("%w: ID %d", ErrProjectNotFound, projectID)
	}
	aggregate := &ManagerContextAggregate{Project: project}
	if aggregate.Tasks, err = readManagerContextTasks(tx, projectID); err != nil {
		return nil, err
	}
	if aggregate.Executions, err = readManagerContextExecutions(tx, projectID); err != nil {
		return nil, err
	}
	if aggregate.Artifacts, err = readManagerContextArtifacts(tx, projectID); err != nil {
		return nil, err
	}
	if aggregate.ManagerReviews, err = readManagerContextReviews(tx, projectID); err != nil {
		return nil, err
	}
	if aggregate.WorkflowEvents, err = readManagerContextEvents(tx, projectID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit manager context read: %w", err)
	}
	return aggregate, nil
}

func readManagerContextTasks(
	tx queryer,
	projectID int64,
) ([]*domain.Task, error) {
	rows, err := tx.Query(
		projectTaskSelect+` WHERE project_id = ? ORDER BY sequence ASC, id ASC`,
		projectID,
	)
	if err != nil {
		return nil, fmt.Errorf("read manager context tasks: %w", err)
	}
	defer rows.Close()
	var tasks []*domain.Task
	for rows.Next() {
		task, err := scanProjectTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate manager context tasks: %w", err)
	}
	return tasks, nil
}

func readManagerContextExecutions(
	tx queryer,
	projectID int64,
) ([]*domain.Execution, error) {
	rows, err := tx.Query(
		executionSelect+` WHERE project_id = ? ORDER BY id ASC`,
		projectID,
	)
	if err != nil {
		return nil, fmt.Errorf("read manager context executions: %w", err)
	}
	defer rows.Close()
	var executions []*domain.Execution
	for rows.Next() {
		execution, err := scanExecution(rows)
		if err != nil {
			return nil, err
		}
		executions = append(executions, execution)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate manager context executions: %w", err)
	}
	return executions, nil
}

func readManagerContextArtifacts(
	tx queryer,
	projectID int64,
) ([]*domain.Artifact, error) {
	rows, err := tx.Query(
		artifactSelect+` WHERE project_id = ? ORDER BY id ASC`,
		projectID,
	)
	if err != nil {
		return nil, fmt.Errorf("read manager context artifacts: %w", err)
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
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate manager context artifacts: %w", err)
	}
	return artifacts, nil
}

func readManagerContextReviews(
	tx queryer,
	projectID int64,
) ([]*domain.ManagerReview, error) {
	rows, err := tx.Query(
		managerReviewSelect+` WHERE project_id = ? ORDER BY id ASC`,
		projectID,
	)
	if err != nil {
		return nil, fmt.Errorf("read manager context reviews: %w", err)
	}
	defer rows.Close()
	var reviews []*domain.ManagerReview
	for rows.Next() {
		review, err := scanManagerReview(rows)
		if err != nil {
			return nil, err
		}
		reviews = append(reviews, review)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate manager context reviews: %w", err)
	}
	return reviews, nil
}

func readManagerContextEvents(
	tx queryer,
	projectID int64,
) ([]*domain.WorkflowEvent, error) {
	rows, err := tx.Query(
		workflowEventSelect+` WHERE project_id = ? ORDER BY sequence ASC, id ASC`,
		projectID,
	)
	if err != nil {
		return nil, fmt.Errorf("read manager context events: %w", err)
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
		return nil, fmt.Errorf("iterate manager context events: %w", err)
	}
	return events, nil
}

type queryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
}
