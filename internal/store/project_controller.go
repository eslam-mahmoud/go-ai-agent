package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

var (
	ErrProjectTaskTransitionConflict = errors.New("project task transition conflict")
	ErrProjectPaused                 = errors.New("project is paused")
)

type ProjectAggregate struct {
	Project             *domain.Project
	Tasks               []*domain.Task
	LatestManagerReview *domain.ManagerReview
}

type ProjectTaskTransitionUpdate struct {
	ProjectID      int64
	TaskID         int64
	ExpectedStatus domain.TaskStatus
	NewStatus      domain.TaskStatus
	ProjectState   domain.ProjectState
	SetCurrentTask bool
	CurrentTaskID  *int64
	Evidence       json.RawMessage
}

// LoadProjectAggregate returns one transactionally consistent project
// snapshot for the Project Controller.
func (s *Store) LoadProjectAggregate(projectID int64) (*ProjectAggregate, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin project aggregate read: %w", err)
	}
	defer tx.Rollback()

	project, err := scanProject(tx.QueryRow(projectSelect+` WHERE id = ?`, projectID))
	if err != nil {
		return nil, err
	}
	if project == nil {
		return nil, fmt.Errorf("%w: ID %d", ErrProjectNotFound, projectID)
	}
	rows, err := tx.Query(
		projectTaskSelect+` WHERE project_id = ? ORDER BY sequence ASC, id ASC`,
		projectID,
	)
	if err != nil {
		return nil, fmt.Errorf("read project aggregate tasks: %w", err)
	}
	var tasks []*domain.Task
	for rows.Next() {
		task, err := scanProjectTask(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		tasks = append(tasks, task)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate project aggregate tasks: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close project aggregate tasks: %w", err)
	}
	review, err := scanManagerReview(tx.QueryRow(
		managerReviewSelect+` WHERE project_id = ? ORDER BY id DESC LIMIT 1`,
		projectID,
	))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit project aggregate read: %w", err)
	}
	return &ProjectAggregate{
		Project:             project,
		Tasks:               tasks,
		LatestManagerReview: review,
	}, nil
}

// ApplyProjectTaskTransition compare-and-sets a task status and its derived
// project fields in one transaction. Transition validity is owned by the
// Project Controller; this method owns only persistence and stale-write safety.
func (s *Store) ApplyProjectTaskTransition(
	update ProjectTaskTransitionUpdate,
) error {
	if update.ProjectID <= 0 || update.TaskID <= 0 {
		return fmt.Errorf("%w: project and task IDs must be positive", domain.ErrInvalidTask)
	}
	if !update.ExpectedStatus.Valid() || !update.NewStatus.Valid() {
		return fmt.Errorf("%w: transition statuses must be valid", domain.ErrInvalidTask)
	}
	if !update.ProjectState.Valid() {
		return fmt.Errorf("%w: project state must be valid", domain.ErrInvalidProject)
	}
	if update.SetCurrentTask &&
		update.CurrentTaskID != nil &&
		*update.CurrentTaskID != update.TaskID {
		return fmt.Errorf("%w: current task must be the transitioned task", domain.ErrInvalidProject)
	}
	if len(update.Evidence) > 0 && !json.Valid(update.Evidence) {
		return fmt.Errorf("%w: transition evidence must be valid JSON", domain.ErrInvalidTask)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin project task transition: %w", err)
	}
	defer tx.Rollback()
	var storedProjectID int64
	var storedStatus string
	var projectState string
	err = tx.QueryRow(`
		SELECT task.project_id, task.status, project.state
		FROM project_tasks task
		JOIN projects project ON project.id = task.project_id
		WHERE task.id = ?
	`, update.TaskID).Scan(&storedProjectID, &storedStatus, &projectState)
	if err == sql.ErrNoRows {
		return fmt.Errorf("%w: ID %d", ErrProjectTaskNotFound, update.TaskID)
	}
	if err != nil {
		return fmt.Errorf("read task transition state: %w", err)
	}
	if storedProjectID != update.ProjectID {
		return fmt.Errorf(
			"%w: task %d belongs to project %d, not %d",
			domain.ErrInvalidTask,
			update.TaskID,
			storedProjectID,
			update.ProjectID,
		)
	}
	if domain.ProjectState(projectState) == domain.ProjectPaused {
		return fmt.Errorf("%w: project %d", ErrProjectPaused, update.ProjectID)
	}
	if domain.TaskStatus(storedStatus) != update.ExpectedStatus {
		return fmt.Errorf(
			"%w: task %d is %q, expected %q",
			ErrProjectTaskTransitionConflict,
			update.TaskID,
			storedStatus,
			update.ExpectedStatus,
		)
	}

	now := time.Now().UTC()
	result, err := tx.Exec(`
		UPDATE project_tasks
		SET status = ?, updated_at = ?
		WHERE id = ? AND project_id = ? AND status = ?
	`, string(update.NewStatus), now, update.TaskID, update.ProjectID, string(update.ExpectedStatus))
	if err != nil {
		return fmt.Errorf("update transitioned task: %w", classifyActiveTaskConstraint(err))
	}
	if changed, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("read transitioned task count: %w", err)
	} else if changed != 1 {
		return fmt.Errorf(
			"%w: task %d changed while transitioning",
			ErrProjectTaskTransitionConflict,
			update.TaskID,
		)
	}

	if update.SetCurrentTask {
		result, err = tx.Exec(`
			UPDATE projects
			SET state = ?, current_task_id = ?, updated_at = ?
			WHERE id = ?
		`, string(update.ProjectState), nullableInt64(update.CurrentTaskID), now, update.ProjectID)
	} else {
		result, err = tx.Exec(`
			UPDATE projects
			SET state = ?, updated_at = ?
			WHERE id = ?
		`, string(update.ProjectState), now, update.ProjectID)
	}
	if err != nil {
		return fmt.Errorf("update project from task transition: %w", err)
	}
	if changed, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("read transitioned project count: %w", err)
	} else if changed != 1 {
		return fmt.Errorf("%w: ID %d", ErrProjectNotFound, update.ProjectID)
	}
	taskID := update.TaskID
	var evidence any
	if len(update.Evidence) > 0 {
		evidence = json.RawMessage(update.Evidence)
	}
	if err := appendWorkflowFactTx(
		tx,
		update.ProjectID,
		&taskID,
		nil,
		domain.WorkflowSourceController,
		domain.WorkflowTaskTransitioned,
		fmt.Sprintf("Task transitioned from %s to %s.", update.ExpectedStatus, update.NewStatus),
		map[string]any{
			"from":            update.ExpectedStatus,
			"to":              update.NewStatus,
			"project_state":   update.ProjectState,
			"current_task_id": update.CurrentTaskID,
			"evidence":        evidence,
		},
		"",
	); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit project task transition: %w", err)
	}
	return nil
}
