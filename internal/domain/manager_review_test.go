package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestManagerReviewDefaultsAndValidation(t *testing.T) {
	review := NewManagerReview(7)
	review.ReleaseReadiness = "not-ready"
	review.OwnerUpdate = "Project remains on track."
	if err := review.Validate(); err != nil {
		t.Fatal(err)
	}
	if review.ProjectHealth != HealthOnTrack ||
		review.CompletedTaskDecision != TaskDecisionNotApplicable {
		t.Errorf("defaults = %#v", review)
	}
}

func TestManagerReviewValidation(t *testing.T) {
	valid := *NewManagerReview(7)
	valid.ReleaseReadiness = "not-ready"
	valid.OwnerUpdate = "Update"
	zero := int64(0)
	cases := []struct {
		name   string
		mutate func(*ManagerReview)
	}{
		{"project", func(r *ManagerReview) { r.ProjectID = 0 }},
		{"completed task", func(r *ManagerReview) { r.CompletedTaskID = &zero }},
		{"execution", func(r *ManagerReview) { r.ExecutionID = &zero }},
		{"artifact", func(r *ManagerReview) { r.ArtifactID = &zero }},
		{"health", func(r *ManagerReview) { r.ProjectHealth = "unknown" }},
		{"progress low", func(r *ManagerReview) { r.ProgressEstimate = -1 }},
		{"progress high", func(r *ManagerReview) { r.ProgressEstimate = 101 }},
		{"decision", func(r *ManagerReview) { r.CompletedTaskDecision = "unknown" }},
		{"next task", func(r *ManagerReview) { r.NextTaskID = &zero }},
		{"next issue", func(r *ManagerReview) { r.NextTaskIssueNumber = -1 }},
		{"next reason", func(r *ManagerReview) {
			id := int64(9)
			r.NextTaskID = &id
		}},
		{"readiness", func(r *ManagerReview) { r.ReleaseReadiness = "" }},
		{"owner update", func(r *ManagerReview) { r.OwnerUpdate = "" }},
		{"discovery object", func(r *ManagerReview) { r.DiscoveryDecisions = json.RawMessage(`{}`) }},
		{"backlog malformed", func(r *ManagerReview) { r.BacklogChanges = json.RawMessage(`[`) }},
		{"JSON too large", func(r *ManagerReview) {
			r.BacklogChanges = json.RawMessage(`["` + strings.Repeat("x", MaxManagerReviewJSONBytes) + `"]`)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			review := valid
			tc.mutate(&review)
			if err := review.Validate(); !errors.Is(err, ErrInvalidManagerReview) {
				t.Errorf("Validate error = %v", err)
			}
		})
	}
	if err := (*ManagerReview)(nil).Validate(); !errors.Is(err, ErrInvalidManagerReview) {
		t.Errorf("nil Validate error = %v", err)
	}
}
