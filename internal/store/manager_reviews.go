package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

var ErrManagerReviewNotFound = errors.New("manager review not found")

func (s *Store) CreateManagerReview(review *domain.ManagerReview) (*domain.ManagerReview, error) {
	if err := review.Validate(); err != nil {
		return nil, fmt.Errorf("create manager review: %w", err)
	}
	if review.ID != 0 {
		return nil, fmt.Errorf("%w: new review ID must be zero", domain.ErrInvalidManagerReview)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin create manager review: %w", err)
	}
	defer tx.Rollback()
	if err := requireProject(tx, review.ProjectID); err != nil {
		return nil, err
	}
	if err := validateReviewOwnership(tx, review); err != nil {
		return nil, err
	}
	reviewedAt := review.ReviewedAt.UTC()
	if review.ReviewedAt.IsZero() {
		reviewedAt = time.Now().UTC()
	}
	result, err := tx.Exec(`
		INSERT INTO manager_reviews (
			project_id, completed_task_id, execution_id, artifact_id,
			project_health, progress_estimate, completed_task_decision,
			architecture_review_required, human_approval_required,
			discovery_decisions, backlog_changes,
			next_task_id, next_task_issue_number, next_task_reason,
			release_readiness, owner_update, reviewed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		review.ProjectID, nullableInt64(review.CompletedTaskID),
		nullableInt64(review.ExecutionID), nullableInt64(review.ArtifactID),
		string(review.ProjectHealth), review.ProgressEstimate,
		string(review.CompletedTaskDecision), review.ArchitectureReviewRequired,
		review.HumanApprovalRequired, string(review.DiscoveryDecisions),
		string(review.BacklogChanges), nullableInt64(review.NextTaskID),
		review.NextTaskIssueNumber, review.NextTaskReason,
		review.ReleaseReadiness, review.OwnerUpdate, reviewedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("insert manager review: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("read manager review ID: %w", err)
	}
	projectUpdate, err := tx.Exec(`
		UPDATE projects SET
			health = ?,
			release_readiness = ?,
			last_manager_review_at = ?,
			updated_at = ?
		WHERE id = ?
	`, string(review.ProjectHealth), review.ReleaseReadiness, reviewedAt, reviewedAt, review.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("update project from manager review: %w", err)
	}
	if updated, err := projectUpdate.RowsAffected(); err != nil || updated != 1 {
		if err != nil {
			return nil, fmt.Errorf("read manager project update count: %w", err)
		}
		return nil, fmt.Errorf("%w: ID %d", ErrProjectNotFound, review.ProjectID)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit manager review: %w", err)
	}
	return s.GetManagerReviewByID(id)
}

func (s *Store) GetManagerReviewByID(id int64) (*domain.ManagerReview, error) {
	return scanManagerReview(s.db.QueryRow(managerReviewSelect+` WHERE id = ?`, id))
}

func (s *Store) LatestManagerReview(projectID int64) (*domain.ManagerReview, error) {
	return scanManagerReview(s.db.QueryRow(
		managerReviewSelect+` WHERE project_id = ? ORDER BY id DESC LIMIT 1`,
		projectID,
	))
}

func (s *Store) ListManagerReviews(projectID int64) ([]*domain.ManagerReview, error) {
	if project, err := s.GetProjectByID(projectID); err != nil {
		return nil, err
	} else if project == nil {
		return nil, fmt.Errorf("%w: ID %d", ErrProjectNotFound, projectID)
	}
	rows, err := s.db.Query(
		managerReviewSelect+` WHERE project_id = ? ORDER BY id ASC`,
		projectID,
	)
	if err != nil {
		return nil, fmt.Errorf("list manager reviews: %w", err)
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
	return reviews, rows.Err()
}

const managerReviewSelect = `
	SELECT
		id, project_id, completed_task_id, execution_id, artifact_id,
		project_health, progress_estimate, completed_task_decision,
		architecture_review_required, human_approval_required,
		discovery_decisions, backlog_changes,
		next_task_id, next_task_issue_number, next_task_reason,
		release_readiness, owner_update, reviewed_at
	FROM manager_reviews
`

func scanManagerReview(row scanner) (*domain.ManagerReview, error) {
	var review domain.ManagerReview
	var completedTaskID, executionID, artifactID, nextTaskID sql.NullInt64
	var health, decision, discoveryJSON, backlogJSON string
	var architectureRequired, approvalRequired int
	if err := row.Scan(
		&review.ID, &review.ProjectID, &completedTaskID, &executionID, &artifactID,
		&health, &review.ProgressEstimate, &decision,
		&architectureRequired, &approvalRequired,
		&discoveryJSON, &backlogJSON, &nextTaskID,
		&review.NextTaskIssueNumber, &review.NextTaskReason,
		&review.ReleaseReadiness, &review.OwnerUpdate, &review.ReviewedAt,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("scan manager review: %w", err)
	}
	review.CompletedTaskID = nullInt64Pointer(completedTaskID)
	review.ExecutionID = nullInt64Pointer(executionID)
	review.ArtifactID = nullInt64Pointer(artifactID)
	review.NextTaskID = nullInt64Pointer(nextTaskID)
	review.ProjectHealth = domain.ProjectHealth(health)
	review.CompletedTaskDecision = domain.CompletedTaskDecision(decision)
	review.ArchitectureReviewRequired = architectureRequired != 0
	review.HumanApprovalRequired = approvalRequired != 0
	review.DiscoveryDecisions = []byte(discoveryJSON)
	review.BacklogChanges = []byte(backlogJSON)
	review.ReviewedAt = review.ReviewedAt.UTC()
	return &review, nil
}

func validateReviewOwnership(tx *sql.Tx, review *domain.ManagerReview) error {
	for label, id := range map[string]*int64{
		"completed task": review.CompletedTaskID,
		"next task":      review.NextTaskID,
	} {
		if id == nil {
			continue
		}
		var issueNumber int
		err := tx.QueryRow(`
			SELECT issue_number FROM project_tasks WHERE id = ? AND project_id = ?
		`, *id, review.ProjectID).Scan(&issueNumber)
		if err == sql.ErrNoRows {
			return fmt.Errorf("%w: %s %d belongs to another project", domain.ErrInvalidManagerReview, label, *id)
		}
		if err != nil {
			return fmt.Errorf("check manager review %s: %w", label, err)
		}
		if label == "next task" && review.NextTaskIssueNumber > 0 &&
			issueNumber != review.NextTaskIssueNumber {
			return fmt.Errorf(
				"%w: next task issue %d does not match stored issue %d",
				domain.ErrInvalidManagerReview,
				review.NextTaskIssueNumber,
				issueNumber,
			)
		}
	}
	for label, check := range map[string]struct {
		id    *int64
		table string
	}{
		"execution": {review.ExecutionID, "executions"},
		"artifact":  {review.ArtifactID, "artifacts"},
	} {
		if check.id == nil {
			continue
		}
		var exists int
		query := fmt.Sprintf(`SELECT 1 FROM %s WHERE id = ? AND project_id = ?`, check.table)
		err := tx.QueryRow(query, *check.id, review.ProjectID).Scan(&exists)
		if err == sql.ErrNoRows {
			return fmt.Errorf("%w: %s %d belongs to another project", domain.ErrInvalidManagerReview, label, *check.id)
		}
		if err != nil {
			return fmt.Errorf("check manager review %s: %w", label, err)
		}
	}
	return nil
}
