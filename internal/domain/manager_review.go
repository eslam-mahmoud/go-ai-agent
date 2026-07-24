package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const MaxManagerReviewJSONBytes = 1024 * 1024

type CompletedTaskDecision string

const (
	TaskDecisionAccepted      CompletedTaskDecision = "accepted"
	TaskDecisionRejected      CompletedTaskDecision = "rejected"
	TaskDecisionDeferred      CompletedTaskDecision = "deferred"
	TaskDecisionNotApplicable CompletedTaskDecision = "not-applicable"
)

var ErrInvalidManagerReview = errors.New("invalid manager review")

// ManagerReview is the immutable, auditable result of one Engineering Manager
// decision cycle. JSON decisions remain unapplied until Milestones 5 and 6.
type ManagerReview struct {
	ID                         int64
	ProjectID                  int64
	CompletedTaskID            *int64
	ExecutionID                *int64
	ArtifactID                 *int64
	ProjectHealth              ProjectHealth
	ProgressEstimate           int
	CompletedTaskDecision      CompletedTaskDecision
	ArchitectureReviewRequired bool
	HumanApprovalRequired      bool
	DiscoveryDecisions         json.RawMessage
	BacklogChanges             json.RawMessage
	NextTaskID                 *int64
	NextTaskIssueNumber        int
	NextTaskReason             string
	ReleaseReadiness           string
	OwnerUpdate                string
	ReviewedAt                 time.Time
}

func NewManagerReview(projectID int64) *ManagerReview {
	return &ManagerReview{
		ProjectID:             projectID,
		ProjectHealth:         HealthOnTrack,
		CompletedTaskDecision: TaskDecisionNotApplicable,
		DiscoveryDecisions:    json.RawMessage("[]"),
		BacklogChanges:        json.RawMessage("[]"),
	}
}

func (decision CompletedTaskDecision) Valid() bool {
	switch decision {
	case TaskDecisionAccepted,
		TaskDecisionRejected,
		TaskDecisionDeferred,
		TaskDecisionNotApplicable:
		return true
	default:
		return false
	}
}

func (review *ManagerReview) Validate() error {
	if review == nil {
		return fmt.Errorf("%w: review is nil", ErrInvalidManagerReview)
	}
	switch {
	case review.ProjectID <= 0:
		return fmt.Errorf("%w: project ID must be positive", ErrInvalidManagerReview)
	case review.CompletedTaskID != nil && *review.CompletedTaskID <= 0:
		return fmt.Errorf("%w: completed task ID must be positive", ErrInvalidManagerReview)
	case review.ExecutionID != nil && *review.ExecutionID <= 0:
		return fmt.Errorf("%w: execution ID must be positive", ErrInvalidManagerReview)
	case review.ArtifactID != nil && *review.ArtifactID <= 0:
		return fmt.Errorf("%w: artifact ID must be positive", ErrInvalidManagerReview)
	case !review.ProjectHealth.Valid():
		return fmt.Errorf("%w: unknown project health %q", ErrInvalidManagerReview, review.ProjectHealth)
	case review.ProgressEstimate < 0 || review.ProgressEstimate > 100:
		return fmt.Errorf("%w: progress estimate must be between 0 and 100", ErrInvalidManagerReview)
	case !review.CompletedTaskDecision.Valid():
		return fmt.Errorf("%w: unknown completed-task decision %q", ErrInvalidManagerReview, review.CompletedTaskDecision)
	case review.NextTaskID != nil && *review.NextTaskID <= 0:
		return fmt.Errorf("%w: next task ID must be positive", ErrInvalidManagerReview)
	case review.NextTaskIssueNumber < 0:
		return fmt.Errorf("%w: next task issue number cannot be negative", ErrInvalidManagerReview)
	case (review.NextTaskID != nil || review.NextTaskIssueNumber > 0) &&
		strings.TrimSpace(review.NextTaskReason) == "":
		return fmt.Errorf("%w: next task selection requires a reason", ErrInvalidManagerReview)
	case strings.TrimSpace(review.ReleaseReadiness) == "":
		return fmt.Errorf("%w: release readiness is required", ErrInvalidManagerReview)
	case strings.TrimSpace(review.OwnerUpdate) == "":
		return fmt.Errorf("%w: owner update is required", ErrInvalidManagerReview)
	}
	if err := validateJSONArray(review.DiscoveryDecisions); err != nil {
		return fmt.Errorf("%w: discovery decisions: %v", ErrInvalidManagerReview, err)
	}
	if err := validateJSONArray(review.BacklogChanges); err != nil {
		return fmt.Errorf("%w: backlog changes: %v", ErrInvalidManagerReview, err)
	}
	return nil
}

func validateJSONArray(raw json.RawMessage) error {
	if len(raw) == 0 {
		return errors.New("JSON is required")
	}
	if len(raw) > MaxManagerReviewJSONBytes {
		return fmt.Errorf("JSON exceeds %d bytes", MaxManagerReviewJSONBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	if _, ok := value.([]any); !ok {
		return errors.New("JSON value must be an array")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}
