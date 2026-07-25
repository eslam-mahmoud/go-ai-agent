package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

var (
	ErrProjectAlreadyInitialized = errors.New("project backlog already exists")
	ErrProjectTaskIssueConflict  = errors.New("project task issue conflict")
)

// CreateInitialBacklog writes a project's first backlog in one transaction.
// A project that already has tasks is left untouched.
func (s *Store) CreateInitialBacklog(
	projectID int64,
	tasks []*domain.Task,
) ([]*domain.Task, error) {
	if projectID <= 0 {
		return nil, fmt.Errorf("%w: project ID must be positive", domain.ErrInvalidTask)
	}
	if len(tasks) == 0 {
		return nil, nil
	}
	for index, task := range tasks {
		if task == nil {
			return nil, fmt.Errorf("%w: task %d is nil", domain.ErrInvalidTask, index)
		}
		candidate := *task
		candidate.ProjectID = projectID
		candidate.Sequence = index + 1
		if err := candidate.Validate(); err != nil {
			return nil, fmt.Errorf("initial backlog task %d: %w", index, err)
		}
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin initial backlog: %w", err)
	}
	defer tx.Rollback()
	if err := requireProject(tx, projectID); err != nil {
		return nil, err
	}
	var existing int
	if err := tx.QueryRow(`
		SELECT COUNT(*) FROM project_tasks WHERE project_id = ?
	`, projectID).Scan(&existing); err != nil {
		return nil, fmt.Errorf("count existing backlog: %w", err)
	}
	if existing > 0 {
		return nil, fmt.Errorf(
			"%w: project %d already has %d task(s)",
			ErrProjectAlreadyInitialized,
			projectID,
			existing,
		)
	}

	now := time.Now().UTC()
	ids := make([]int64, 0, len(tasks))
	for index, task := range tasks {
		candidate := *task
		candidate.ProjectID = projectID
		candidate.Sequence = index + 1
		id, err := insertBacklogTask(tx, &candidate, now)
		if err != nil {
			return nil, fmt.Errorf("insert initial task %d: %w", index, err)
		}
		ids = append(ids, id)
	}
	if err := touchProject(tx, projectID, now); err != nil {
		return nil, err
	}
	if err := appendWorkflowFactTx(
		tx,
		projectID,
		nil,
		nil,
		domain.WorkflowSourceController,
		domain.WorkflowBacklogInitialized,
		fmt.Sprintf("Created the initial backlog with %d task(s).", len(ids)),
		map[string]any{"task_ids": ids, "count": len(ids)},
		fmt.Sprintf("project:%d:initial-backlog", projectID),
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit initial backlog: %w", err)
	}
	return s.ListProjectTasks(projectID)
}

// RecordProjectTaskIssue binds a task to the GitHub issue that represents it.
// The compare-and-set on an unset number is what stops a repeated or
// concurrent run from filing the same task twice.
func (s *Store) RecordProjectTaskIssue(
	projectID, taskID int64,
	issueNumber int,
	reused bool,
) (*domain.Task, error) {
	if projectID <= 0 || taskID <= 0 || issueNumber <= 0 {
		return nil, fmt.Errorf(
			"%w: project, task, and issue numbers must be positive",
			domain.ErrInvalidTask,
		)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin record task issue: %w", err)
	}
	defer tx.Rollback()

	var storedProjectID int64
	var storedIssue int
	err = tx.QueryRow(`
		SELECT project_id, issue_number FROM project_tasks WHERE id = ?
	`, taskID).Scan(&storedProjectID, &storedIssue)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%w: ID %d", ErrProjectTaskNotFound, taskID)
	}
	if err != nil {
		return nil, fmt.Errorf("read task for issue: %w", err)
	}
	if storedProjectID != projectID {
		return nil, fmt.Errorf(
			"%w: task %d belongs to project %d, not %d",
			domain.ErrInvalidTask,
			taskID,
			storedProjectID,
			projectID,
		)
	}
	if storedIssue == issueNumber {
		if err := tx.Rollback(); err != nil {
			return nil, fmt.Errorf("close replayed task issue: %w", err)
		}
		return s.GetProjectTaskByID(taskID)
	}
	if storedIssue != 0 {
		return nil, fmt.Errorf(
			"%w: task %d already records issue #%d",
			ErrProjectTaskIssueConflict,
			taskID,
			storedIssue,
		)
	}

	now := time.Now().UTC()
	result, err := tx.Exec(`
		UPDATE project_tasks
		SET issue_number = ?, updated_at = ?
		WHERE id = ? AND project_id = ? AND issue_number = 0
	`, issueNumber, now, taskID, projectID)
	if err != nil {
		return nil, fmt.Errorf("record task issue: %w", err)
	}
	if changed, err := result.RowsAffected(); err != nil {
		return nil, fmt.Errorf("read task issue count: %w", err)
	} else if changed != 1 {
		return nil, fmt.Errorf(
			"%w: task %d changed while being filed",
			ErrProjectTaskIssueConflict,
			taskID,
		)
	}
	boundTaskID := taskID
	if err := appendWorkflowFactTx(
		tx,
		projectID,
		&boundTaskID,
		nil,
		domain.WorkflowSourceController,
		domain.WorkflowTaskIssueFiled,
		fmt.Sprintf("Task %d filed as issue #%d.", taskID, issueNumber),
		map[string]any{
			"task_id":      taskID,
			"issue_number": issueNumber,
			"reused_issue": reused,
		},
		fmt.Sprintf("task:%d:issue", taskID),
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit record task issue: %w", err)
	}
	return s.GetProjectTaskByID(taskID)
}

// RecordProjectTaskPullRequest binds a task to the pull request discovered for
// its branch. As with issue numbers, the compare-and-set is what makes
// repeated reconciliation safe.
func (s *Store) RecordProjectTaskPullRequest(
	projectID, taskID int64,
	prNumber int,
) (*domain.Task, error) {
	if projectID <= 0 || taskID <= 0 || prNumber <= 0 {
		return nil, fmt.Errorf(
			"%w: project, task, and pull request numbers must be positive",
			domain.ErrInvalidTask,
		)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin record task pull request: %w", err)
	}
	defer tx.Rollback()

	var storedProjectID int64
	var storedPR int
	err = tx.QueryRow(`
		SELECT project_id, pr_number FROM project_tasks WHERE id = ?
	`, taskID).Scan(&storedProjectID, &storedPR)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("%w: ID %d", ErrProjectTaskNotFound, taskID)
	}
	if err != nil {
		return nil, fmt.Errorf("read task for pull request: %w", err)
	}
	if storedProjectID != projectID {
		return nil, fmt.Errorf(
			"%w: task %d belongs to project %d, not %d",
			domain.ErrInvalidTask,
			taskID,
			storedProjectID,
			projectID,
		)
	}
	if storedPR == prNumber {
		if err := tx.Rollback(); err != nil {
			return nil, fmt.Errorf("close replayed task pull request: %w", err)
		}
		return s.GetProjectTaskByID(taskID)
	}
	if storedPR != 0 {
		return nil, fmt.Errorf(
			"%w: task %d already records pull request #%d",
			ErrProjectTaskIssueConflict,
			taskID,
			storedPR,
		)
	}

	now := time.Now().UTC()
	result, err := tx.Exec(`
		UPDATE project_tasks
		SET pr_number = ?, updated_at = ?
		WHERE id = ? AND project_id = ? AND pr_number = 0
	`, prNumber, now, taskID, projectID)
	if err != nil {
		return nil, fmt.Errorf("record task pull request: %w", err)
	}
	if changed, err := result.RowsAffected(); err != nil {
		return nil, fmt.Errorf("read task pull request count: %w", err)
	} else if changed != 1 {
		return nil, fmt.Errorf(
			"%w: task %d changed while binding its pull request",
			ErrProjectTaskIssueConflict,
			taskID,
		)
	}
	boundTaskID := taskID
	if err := appendWorkflowFactTx(
		tx,
		projectID,
		&boundTaskID,
		nil,
		domain.WorkflowSourceExternal,
		domain.WorkflowTaskPullRequestFound,
		fmt.Sprintf("Task %d matched pull request #%d.", taskID, prNumber),
		map[string]any{"task_id": taskID, "pr_number": prNumber},
		fmt.Sprintf("task:%d:pull-request", taskID),
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit record task pull request: %w", err)
	}
	return s.GetProjectTaskByID(taskID)
}
