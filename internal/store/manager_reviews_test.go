package store

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

func TestManagerReviewCreateListLatestAndProjectSnapshot(t *testing.T) {
	s := openTestStore(t)
	project := createTestProject(t, s, "owner/repo")
	completedTask, _ := s.CreateProjectTask(domain.NewTask(project.ID, "Completed", "Goal"))
	nextTaskInput := domain.NewTask(project.ID, "Next", "Goal")
	nextTaskInput.IssueNumber = 42
	nextTask, _ := s.CreateProjectTask(nextTaskInput)
	execution, _ := s.CreateExecution(domain.NewExecution(
		project.ID, completedTask.ID, "manager", "claude", "sonnet", 1,
	))
	artifactInput := domain.NewArtifact(
		project.ID, "manager-review", "Review", "reviews/1.json",
		"application/json", strings.Repeat("a", 64), 128,
	)
	artifactInput.TaskID = &completedTask.ID
	artifactInput.ExecutionID = &execution.ID
	artifact, _ := s.CreateArtifact(artifactInput)

	reviewedAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	review := domain.NewManagerReview(project.ID)
	review.CompletedTaskID = &completedTask.ID
	review.ExecutionID = &execution.ID
	review.ArtifactID = &artifact.ID
	review.ProjectHealth = domain.HealthAtRisk
	review.ProgressEstimate = 35
	review.CompletedTaskDecision = domain.TaskDecisionAccepted
	review.ArchitectureReviewRequired = true
	review.HumanApprovalRequired = false
	review.DiscoveryDecisions = json.RawMessage(`[ { "id": "D-1", "decision": "accept" } ]`)
	review.BacklogChanges = json.RawMessage(`[ { "task_id": 2, "sequence": 1 } ]`)
	review.NextTaskID = &nextTask.ID
	review.NextTaskIssueNumber = 42
	review.NextTaskReason = "Next dependency"
	review.ReleaseReadiness = "not-ready"
	review.OwnerUpdate = "Project remains at risk."
	review.ReviewedAt = reviewedAt

	created, err := s.CreateManagerReview(review)
	if err != nil {
		t.Fatalf("CreateManagerReview: %v", err)
	}
	if string(created.DiscoveryDecisions) != string(review.DiscoveryDecisions) ||
		string(created.BacklogChanges) != string(review.BacklogChanges) {
		t.Errorf("JSON did not round-trip exactly: %#v", created)
	}
	if !created.ReviewedAt.Equal(reviewedAt) || created.NextTaskID == nil ||
		*created.NextTaskID != nextTask.ID {
		t.Errorf("created review = %#v", created)
	}
	gotProject, _ := s.GetProjectByID(project.ID)
	if gotProject.Health != domain.HealthAtRisk ||
		gotProject.ReleaseReadiness != "not-ready" ||
		gotProject.LastManagerReviewAt == nil ||
		!gotProject.LastManagerReviewAt.Equal(reviewedAt) {
		t.Errorf("project snapshot = %#v", gotProject)
	}

	second := domain.NewManagerReview(project.ID)
	second.ProjectHealth = domain.HealthOnTrack
	second.ProgressEstimate = 40
	second.ReleaseReadiness = "not-ready"
	second.OwnerUpdate = "Recovered."
	second, err = s.CreateManagerReview(second)
	if err != nil {
		t.Fatal(err)
	}
	reviews, err := s.ListManagerReviews(project.ID)
	if err != nil || len(reviews) != 2 ||
		reviews[0].ID != created.ID || reviews[1].ID != second.ID {
		t.Errorf("reviews = %#v, error=%v", reviews, err)
	}
	latest, err := s.LatestManagerReview(project.ID)
	if err != nil || latest == nil || latest.ID != second.ID {
		t.Errorf("latest = %#v, error=%v", latest, err)
	}
}

func TestManagerReviewOwnershipAndValidation(t *testing.T) {
	s := openTestStore(t)
	first := createTestProject(t, s, "owner/first")
	second := createTestProject(t, s, "owner/second")
	firstTask, _ := s.CreateProjectTask(domain.NewTask(first.ID, "First", "Goal"))
	secondTask, _ := s.CreateProjectTask(domain.NewTask(second.ID, "Second", "Goal"))
	secondExecution, _ := s.CreateExecution(domain.NewExecution(
		second.ID, secondTask.ID, "manager", "claude", "", 1,
	))
	secondArtifactInput := domain.NewArtifact(
		second.ID, "review", "Review", "review.json", "application/json",
		strings.Repeat("b", 64), 1,
	)
	secondArtifact, _ := s.CreateArtifact(secondArtifactInput)

	base := domain.NewManagerReview(first.ID)
	base.ReleaseReadiness = "not-ready"
	base.OwnerUpdate = "Update"
	cases := []struct {
		name   string
		mutate func(*domain.ManagerReview)
	}{
		{"completed task", func(r *domain.ManagerReview) { r.CompletedTaskID = &secondTask.ID }},
		{"next task", func(r *domain.ManagerReview) {
			r.NextTaskID = &secondTask.ID
			r.NextTaskReason = "wrong project"
		}},
		{"execution", func(r *domain.ManagerReview) { r.ExecutionID = &secondExecution.ID }},
		{"artifact", func(r *domain.ManagerReview) { r.ArtifactID = &secondArtifact.ID }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			copy := *base
			tc.mutate(&copy)
			if _, err := s.CreateManagerReview(&copy); !errors.Is(err, domain.ErrInvalidManagerReview) {
				t.Errorf("error = %v", err)
			}
		})
	}
	valid := *base
	valid.CompletedTaskID = &firstTask.ID
	valid.NextTaskID = &firstTask.ID
	valid.NextTaskIssueNumber = 99
	valid.NextTaskReason = "mismatch"
	if _, err := s.CreateManagerReview(&valid); !errors.Is(err, domain.ErrInvalidManagerReview) {
		t.Errorf("issue mismatch error = %v", err)
	}
	invalid := domain.NewManagerReview(first.ID)
	if _, err := s.CreateManagerReview(invalid); !errors.Is(err, domain.ErrInvalidManagerReview) {
		t.Errorf("invalid review error = %v", err)
	}
}

func TestManagerReviewEvidenceNullingCascadeAndReopen(t *testing.T) {
	s := openTestStore(t)
	project := createTestProject(t, s, "owner/repo")
	task, _ := s.CreateProjectTask(domain.NewTask(project.ID, "Task", "Goal"))
	execution, _ := s.CreateExecution(domain.NewExecution(
		project.ID, task.ID, "manager", "claude", "", 1,
	))
	artifactInput := domain.NewArtifact(
		project.ID, "review", "Review", "review.json", "application/json",
		strings.Repeat("c", 64), 1,
	)
	artifactInput.TaskID = &task.ID
	artifactInput.ExecutionID = &execution.ID
	artifact, _ := s.CreateArtifact(artifactInput)
	review := domain.NewManagerReview(project.ID)
	review.CompletedTaskID = &task.ID
	review.NextTaskID = &task.ID
	review.NextTaskReason = "continue"
	review.ExecutionID = &execution.ID
	review.ArtifactID = &artifact.ID
	review.ReleaseReadiness = "not-ready"
	review.OwnerUpdate = "Update"
	review, err := s.CreateManagerReview(review)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM project_tasks WHERE id = ?`, task.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetManagerReviewByID(review.ID)
	if got.CompletedTaskID != nil || got.NextTaskID != nil ||
		got.ExecutionID != nil {
		t.Errorf("task/execution evidence was not nulled: %#v", got)
	}
	if _, err := s.db.Exec(`DELETE FROM artifacts WHERE id = ?`, artifact.ID); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetManagerReviewByID(review.ID)
	if got.ArtifactID != nil {
		t.Errorf("artifact evidence was not nulled: %#v", got)
	}

	if _, err := s.UpsertTask("owner/repo", 77, StateInProgress, "legacy"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM projects WHERE id = ?`, project.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetManagerReviewByID(review.ID); got != nil {
		t.Errorf("review survived project cascade: %#v", got)
	}
	if legacy, _ := s.GetTask("owner/repo", 77); legacy == nil {
		t.Error("legacy task was deleted")
	}
}

func TestManagerReviewMigrationReopenAndMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "madar.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	project := createTestProject(t, s, "owner/repo")
	review := domain.NewManagerReview(project.ID)
	review.ReleaseReadiness = "not-ready"
	review.OwnerUpdate = "Persisted"
	created, err := s.CreateManagerReview(review)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got, err := s.GetManagerReviewByID(created.ID); err != nil || got == nil {
		t.Fatalf("stored review = %#v, error=%v", got, err)
	}
	if missing, err := s.GetManagerReviewByID(404); err != nil || missing != nil {
		t.Errorf("missing review = %#v, error=%v", missing, err)
	}
	if latest, err := s.LatestManagerReview(9999); err != nil || latest != nil {
		t.Errorf("missing latest = %#v, error=%v", latest, err)
	}
}
