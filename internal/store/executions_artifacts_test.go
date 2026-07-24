package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

func TestExecutionArtifactLifecycle(t *testing.T) {
	s := openTestStore(t)
	project := createTestProject(t, s, "owner/repo")
	task, err := s.CreateProjectTask(domain.NewTask(project.ID, "Task", "Goal"))
	if err != nil {
		t.Fatal(err)
	}
	input := domain.NewArtifact(
		project.ID, "plan", "Plan", "plans/task.json", "application/json",
		strings.Repeat("a", 64), 100,
	)
	input.TaskID = &task.ID
	input, err = s.CreateArtifact(input)
	if err != nil {
		t.Fatal(err)
	}

	execution := domain.NewExecution(project.ID, task.ID, "developer", "codex", "gpt-test", 1)
	execution.InputArtifactID = &input.ID
	execution, err = s.CreateExecution(execution)
	if err != nil {
		t.Fatal(err)
	}
	output := domain.NewArtifact(
		project.ID, "result", "Result", "results/task.json", "application/json",
		strings.Repeat("b", 64), 250,
	)
	output.TaskID = &task.ID
	output.ExecutionID = &execution.ID
	output, err = s.CreateArtifact(output)
	if err != nil {
		t.Fatal(err)
	}

	started := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	completed := started.Add(30 * time.Second)
	execution.Status = domain.ExecutionCompleted
	execution.ProviderSessionID = "provider-session"
	execution.OutputArtifactID = &output.ID
	execution.StartedAt = &started
	execution.CompletedAt = &completed
	execution.ErrorClass = ""
	execution.ErrorMessage = ""
	execution.InputTokens = 123
	execution.OutputTokens = 45
	execution.EstimatedCost = 1.25
	execution, err = s.UpdateExecution(execution)
	if err != nil {
		t.Fatal(err)
	}
	if execution.OutputArtifactID == nil || *execution.OutputArtifactID != output.ID ||
		execution.ProviderSessionID != "provider-session" ||
		execution.InputTokens != 123 || execution.OutputTokens != 45 ||
		execution.EstimatedCost != 1.25 ||
		execution.StartedAt == nil || !execution.StartedAt.Equal(started) ||
		execution.CompletedAt == nil || !execution.CompletedAt.Equal(completed) {
		t.Errorf("updated execution = %#v", execution)
	}
	artifacts, err := s.ListExecutionArtifacts(execution.ID)
	if err != nil || len(artifacts) != 1 || artifacts[0].ID != output.ID {
		t.Errorf("execution artifacts = %#v, error=%v", artifacts, err)
	}
	executions, err := s.ListTaskExecutions(task.ID)
	if err != nil || len(executions) != 1 || executions[0].ID != execution.ID {
		t.Errorf("task executions = %#v, error=%v", executions, err)
	}
}

func TestExecutionArtifactIdentityAndOwnershipErrors(t *testing.T) {
	s := openTestStore(t)
	firstProject := createTestProject(t, s, "owner/first")
	firstTask, _ := s.CreateProjectTask(domain.NewTask(firstProject.ID, "First", "Goal"))
	secondTask, _ := s.CreateProjectTask(domain.NewTask(firstProject.ID, "Second", "Goal"))
	secondProject := createTestProject(t, s, "owner/second")
	otherTask, _ := s.CreateProjectTask(domain.NewTask(secondProject.ID, "Other", "Goal"))

	execution, err := s.CreateExecution(domain.NewExecution(
		firstProject.ID, firstTask.ID, "developer", "codex", "", 1,
	))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateExecution(domain.NewExecution(
		firstProject.ID, firstTask.ID, "developer", "claude", "", 1,
	)); !errors.Is(err, ErrExecutionAlreadyExists) {
		t.Errorf("duplicate execution error = %v", err)
	}
	if _, err := s.CreateExecution(domain.NewExecution(
		firstProject.ID, otherTask.ID, "developer", "codex", "", 1,
	)); !errors.Is(err, ErrProjectTaskNotFound) {
		t.Errorf("cross-project task error = %v", err)
	}

	artifact := domain.NewArtifact(
		firstProject.ID, "result", "Result", "same/path", "application/json",
		strings.Repeat("c", 64), 1,
	)
	artifact.TaskID = &firstTask.ID
	created, err := s.CreateArtifact(artifact)
	if err != nil {
		t.Fatal(err)
	}
	duplicate := *artifact
	if _, err := s.CreateArtifact(&duplicate); !errors.Is(err, ErrArtifactAlreadyExists) {
		t.Errorf("duplicate artifact error = %v", err)
	}

	wrongTask := domain.NewArtifact(
		firstProject.ID, "result", "Wrong", "wrong/task", "application/json",
		strings.Repeat("d", 64), 1,
	)
	wrongTask.TaskID = &secondTask.ID
	wrongTask.ExecutionID = &execution.ID
	if _, err := s.CreateArtifact(wrongTask); !errors.Is(err, domain.ErrInvalidArtifact) {
		t.Errorf("incompatible execution artifact error = %v", err)
	}
	execution.OutputArtifactID = &created.ID
	execution.TaskID = secondTask.ID
	if _, err := s.UpdateExecution(execution); !errors.Is(err, domain.ErrInvalidExecution) {
		t.Errorf("immutable execution identity error = %v", err)
	}
}

func TestExecutionArtifactsReopenAndProjectCascade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "madar.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	project := createTestProject(t, s, "owner/repo")
	task, _ := s.CreateProjectTask(domain.NewTask(project.ID, "Task", "Goal"))
	execution, _ := s.CreateExecution(domain.NewExecution(project.ID, task.ID, "planner", "claude", "", 1))
	artifact := domain.NewArtifact(
		project.ID, "plan", "Plan", "plan.json", "application/json",
		strings.Repeat("e", 64), 10,
	)
	artifact.ExecutionID = &execution.ID
	artifact, err = s.CreateArtifact(artifact)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertTask("owner/repo", 99, StateInProgress, "legacy"); err != nil {
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
	if got, _ := s.GetArtifactByID(artifact.ID); got == nil {
		t.Fatal("artifact missing after reopen")
	}
	if _, err := s.db.Exec(`DELETE FROM projects WHERE id = ?`, project.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetExecutionByID(execution.ID); got != nil {
		t.Errorf("execution survived project cascade: %#v", got)
	}
	if got, _ := s.GetArtifactByID(artifact.ID); got != nil {
		t.Errorf("artifact survived project cascade: %#v", got)
	}
	if legacy, _ := s.GetTask("owner/repo", 99); legacy == nil {
		t.Error("legacy task was deleted by project cascade")
	}
}
