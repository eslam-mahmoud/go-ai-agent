package project

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
	"github.com/eslam-mahmoud/go-ai-agent/internal/workflow"
)

func TestStartupRecoveryInterruptsAndQueuesResumableExecutionIdempotently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "madar.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	project, task := createCurrentControllerTask(
		t,
		s,
		"owner/repo",
		domain.ProjectExecuting,
		domain.TaskDeveloping,
	)
	execution := domain.NewExecution(
		project.ID,
		task.ID,
		"developer",
		"codex",
		"gpt-test",
		1,
	)
	execution.Status = domain.ExecutionRunning
	execution.ProviderSessionID = "resume-session"
	execution, err = s.CreateExecution(execution)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpsertTask("legacy/repo", 1, store.StateInProgress, "legacy-session"); err != nil {
		t.Fatal(err)
	}
	controller, _ := NewController(s)
	recovery, _ := NewStartupRecovery(controller, s)

	report, err := recovery.Run()
	if err != nil {
		t.Fatal(err)
	}
	if len(report.InterruptedExecutions) != 1 ||
		report.InterruptedExecutions[0].ID != execution.ID ||
		len(report.Projects) != 1 ||
		report.Projects[0].Decision.Action != workflow.RecoveryQueueRetry ||
		report.Projects[0].Retry == nil ||
		report.Projects[0].Retry.Attempt != 2 ||
		report.Projects[0].Retry.ProviderSessionID != "resume-session" {
		t.Fatalf("recovery report = %#v", report)
	}
	original, _ := s.GetExecutionByID(execution.ID)
	if original.Status != domain.ExecutionInterrupted {
		t.Fatalf("original execution = %#v", original)
	}
	legacy, err := s.GetTask("legacy/repo", 1)
	if err != nil || legacy.State != store.StateInProgress {
		t.Fatalf("legacy task = %#v error=%v", legacy, err)
	}
	events, err := s.ListWorkflowEvents(project.ID, 0, 0)
	if err != nil || len(events) != 3 {
		t.Fatalf("first recovery events=%#v error=%v", events, err)
	}
	assertRecoveryEventTypes(t, events, []domain.WorkflowEventType{
		domain.WorkflowExecutionInterrupted,
		domain.WorkflowExecutionRetried,
		domain.WorkflowRecoveryDecided,
	})

	report, err = recovery.Run()
	if err != nil ||
		len(report.InterruptedExecutions) != 0 ||
		report.Projects[0].Decision.Action != workflow.RecoveryContinue {
		t.Fatalf("second recovery report=%#v error=%v", report, err)
	}
	events, _ = s.ListWorkflowEvents(project.ID, 0, 0)
	if len(events) != 4 || events[3].Type != domain.WorkflowRecoveryDecided {
		t.Fatalf("second recovery events = %#v", events)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	controller, _ = NewController(s)
	recovery, _ = NewStartupRecovery(controller, s)
	report, err = recovery.Run()
	if err != nil || report.Projects[0].Decision.Action != workflow.RecoveryContinue {
		t.Fatalf("reopened recovery report=%#v error=%v", report, err)
	}
	events, _ = s.ListWorkflowEvents(project.ID, 0, 0)
	if len(events) != 4 {
		t.Fatalf("unchanged recovery duplicated events: %#v", events)
	}
}

func TestStartupRecoveryPausesAmbiguousStateAndThenHolds(t *testing.T) {
	s := openControllerStore(t)
	project, _ := createCurrentControllerTask(
		t,
		s,
		"owner/repo",
		domain.ProjectExecuting,
		domain.TaskDeveloping,
	)
	controller, _ := NewController(s)
	recovery, _ := NewStartupRecovery(controller, s)

	report, err := recovery.Run()
	if err != nil ||
		report.Projects[0].Decision.Action != workflow.RecoveryPauseAmbiguous ||
		report.Projects[0].Snapshot.Project.State != domain.ProjectPaused {
		t.Fatalf("ambiguous report=%#v error=%v", report, err)
	}
	events, _ := s.ListWorkflowEvents(project.ID, 0, 0)
	assertRecoveryEventTypes(t, events, []domain.WorkflowEventType{
		domain.WorkflowProjectPaused,
		domain.WorkflowRecoveryDecided,
	})

	report, err = recovery.Run()
	if err != nil ||
		report.Projects[0].Decision.Action != workflow.RecoveryHoldPaused {
		t.Fatalf("paused report=%#v error=%v", report, err)
	}
	events, _ = s.ListWorkflowEvents(project.ID, 0, 0)
	if len(events) != 3 {
		t.Fatalf("paused recovery events = %#v", events)
	}
	if _, err := recovery.Run(); err != nil {
		t.Fatal(err)
	}
	events, _ = s.ListWorkflowEvents(project.ID, 0, 0)
	if len(events) != 3 {
		t.Fatalf("unchanged paused recovery duplicated events: %#v", events)
	}
}

func TestControllerMutationsProduceOrderedTransactionalAuditFacts(t *testing.T) {
	s := openControllerStore(t)
	project, task := createCurrentControllerTask(
		t,
		s,
		"owner/repo",
		domain.ProjectExecuting,
		domain.TaskPlanning,
	)
	controller, _ := NewController(s)
	if _, err := controller.TransitionTask(
		project.ID,
		task.ID,
		domain.TaskDeveloping,
		workflow.TaskTransitionEvidence{PlanCompleted: true},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Pause(project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Resume(project.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Cancel(project.ID); err != nil {
		t.Fatal(err)
	}

	events, err := s.ListWorkflowEvents(project.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	assertRecoveryEventTypes(t, events, []domain.WorkflowEventType{
		domain.WorkflowTaskTransitioned,
		domain.WorkflowProjectPaused,
		domain.WorkflowProjectResumed,
		domain.WorkflowTaskCancelled,
	})
	for index, event := range events {
		if event.Sequence != int64(index+1) ||
			event.Source != domain.WorkflowSourceController ||
			!json.Valid(event.Data) {
			t.Fatalf("event %d = %#v", index, event)
		}
	}
	var transitionData struct {
		Evidence workflow.TaskTransitionEvidence `json:"evidence"`
	}
	if err := json.Unmarshal(events[0].Data, &transitionData); err != nil ||
		!transitionData.Evidence.PlanCompleted {
		t.Fatalf("transition evidence=%#v error=%v", transitionData, err)
	}
}

func assertRecoveryEventTypes(
	t *testing.T,
	events []*domain.WorkflowEvent,
	want []domain.WorkflowEventType,
) {
	t.Helper()
	if len(events) != len(want) {
		t.Fatalf("event count = %d, want %d: %#v", len(events), len(want), events)
	}
	for index, eventType := range want {
		if events[index].Sequence != int64(index+1) ||
			events[index].Type != eventType {
			t.Fatalf(
				"event %d = sequence %d type %q, want sequence %d type %q",
				index,
				events[index].Sequence,
				events[index].Type,
				index+1,
				eventType,
			)
		}
	}
}
