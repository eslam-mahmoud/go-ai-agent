package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

func TestLoadManagerContextAggregateIncludesDurableProjectInputs(t *testing.T) {
	s := openTestStore(t)
	project := createTestProject(t, s, "owner/repo")
	task, err := s.CreateProjectTask(domain.NewTask(project.ID, "Manager context", "Build context"))
	if err != nil {
		t.Fatal(err)
	}
	task.Status = domain.TaskCompleted
	task.IssueNumber = 137
	task, err = s.UpdateProjectTask(task)
	if err != nil {
		t.Fatal(err)
	}
	execution, err := s.CreateExecution(domain.NewExecution(
		project.ID,
		task.ID,
		"verifier",
		"codex",
		"gpt-test",
		1,
	))
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	completed := started.Add(30 * time.Second)
	execution.Status = domain.ExecutionCompleted
	execution.StartedAt = &started
	execution.CompletedAt = &completed
	execution.InputTokens = 100
	execution.OutputTokens = 50
	execution, err = s.UpdateExecution(execution)
	if err != nil {
		t.Fatal(err)
	}
	artifact := domain.NewArtifact(
		project.ID,
		"architecture",
		"Architecture",
		"docs/architecture.md",
		"text/markdown",
		strings.Repeat("a", 64),
		100,
	)
	artifact.TaskID = &task.ID
	artifact.ExecutionID = &execution.ID
	if _, err := s.CreateArtifact(artifact); err != nil {
		t.Fatal(err)
	}
	review := domain.NewManagerReview(project.ID)
	review.CompletedTaskID = &task.ID
	review.ProjectHealth = domain.HealthReadyForRelease
	review.ProgressEstimate = 100
	review.CompletedTaskDecision = domain.TaskDecisionAccepted
	review.ReleaseReadiness = "ready"
	review.OwnerUpdate = "Ready."
	if _, err := s.CreateManagerReview(review); err != nil {
		t.Fatal(err)
	}
	event := domain.NewWorkflowEvent(
		project.ID,
		domain.WorkflowSourceExternal,
		domain.WorkflowEventType("human.comment"),
		"Ship when CI passes.",
	)
	event.TaskID = &task.ID
	event.Data = json.RawMessage(`{"author":"owner"}`)
	if _, _, err := s.AppendWorkflowEvent(event); err != nil {
		t.Fatal(err)
	}

	got, err := s.LoadManagerContextAggregate(project.ID)
	if err != nil {
		t.Fatalf("LoadManagerContextAggregate: %v", err)
	}
	if got.Project == nil || got.Project.ID != project.ID ||
		len(got.Tasks) != 1 || got.Tasks[0].ID != task.ID ||
		len(got.Executions) != 1 || got.Executions[0].ID != execution.ID ||
		len(got.Artifacts) != 1 || got.Artifacts[0].Kind != "architecture" ||
		len(got.ManagerReviews) != 1 ||
		got.ManagerReviews[0].CompletedTaskDecision != domain.TaskDecisionAccepted ||
		len(got.WorkflowEvents) != 1 ||
		got.WorkflowEvents[0].Source != domain.WorkflowSourceExternal {
		t.Fatalf("aggregate = %#v", got)
	}
}

func TestLoadManagerContextAggregateRejectsMissingProject(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.LoadManagerContextAggregate(0); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("zero project error = %v", err)
	}
	if _, err := s.LoadManagerContextAggregate(404); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("missing project error = %v", err)
	}
}
