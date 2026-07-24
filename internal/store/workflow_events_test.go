package store

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

func TestWorkflowEventAppendIdempotencyPaginationAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "madar.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	project, task := createQueuedTask(t, s, "owner/repo")
	execution := mustCreateExecution(t, s, project.ID, task.ID, "planner", 1)

	event := domain.NewWorkflowEvent(
		project.ID,
		domain.WorkflowSourceWorkflow,
		domain.WorkflowEventType("mode.started"),
		"Planner started.",
	)
	event.TaskID = &task.ID
	event.ExecutionID = &execution.ID
	event.Data = json.RawMessage(`{"mode":"planner"}`)
	event.IdempotencyKey = "execution:planner:started:1"
	first, created, err := s.AppendWorkflowEvent(event)
	if err != nil || !created {
		t.Fatalf("first append event=%#v created=%t error=%v", first, created, err)
	}
	if first.Sequence != 1 || first.CreatedAt.IsZero() ||
		first.TaskID == nil || *first.TaskID != task.ID ||
		first.ExecutionID == nil || *first.ExecutionID != execution.ID {
		t.Fatalf("first event = %#v", first)
	}

	duplicate := *event
	duplicate.Message = "This must not replace the original."
	again, created, err := s.AppendWorkflowEvent(&duplicate)
	if err != nil || created || again.ID != first.ID ||
		again.Message != first.Message || again.Sequence != 1 {
		t.Fatalf("duplicate event=%#v created=%t error=%v", again, created, err)
	}
	second := domain.NewWorkflowEvent(
		project.ID,
		domain.WorkflowSourceWorkflow,
		domain.WorkflowEventType("mode.completed"),
		"Planner completed.",
	)
	second.TaskID = &task.ID
	second.ExecutionID = &execution.ID
	second, created, err = s.AppendWorkflowEvent(second)
	if err != nil || !created || second.Sequence != 2 {
		t.Fatalf("second event=%#v created=%t error=%v", second, created, err)
	}
	page, err := s.ListWorkflowEvents(project.ID, 1, 1)
	if err != nil || len(page) != 1 || page[0].ID != second.ID {
		t.Fatalf("event page=%#v error=%v", page, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	events, err := s.ListWorkflowEvents(project.ID, 0, 0)
	if err != nil || len(events) != 2 ||
		events[0].ID != first.ID || events[1].ID != second.ID {
		t.Fatalf("reopened events=%#v error=%v", events, err)
	}
}

func TestWorkflowEventConcurrentSequenceAndIdempotency(t *testing.T) {
	s := openTestStore(t)
	project := createTestProject(t, s, "owner/repo")

	const writers = 20
	start := make(chan struct{})
	results := make(chan *domain.WorkflowEvent, writers)
	errs := make(chan error, writers)
	var wait sync.WaitGroup
	for i := 0; i < writers; i++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			event := domain.NewWorkflowEvent(
				project.ID,
				domain.WorkflowSourceExternal,
				domain.WorkflowEventType("test.concurrent"),
				"",
			)
			event.Data, _ = json.Marshal(map[string]int{"writer": index})
			stored, _, err := s.AppendWorkflowEvent(event)
			results <- stored
			errs <- err
		}(i)
	}
	close(start)
	wait.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	seen := make(map[int64]bool, writers)
	for event := range results {
		if event == nil {
			t.Fatal("nil concurrent event")
		}
		seen[event.Sequence] = true
	}
	for sequence := int64(1); sequence <= writers; sequence++ {
		if !seen[sequence] {
			t.Fatalf("missing sequence %d: %#v", sequence, seen)
		}
	}

	start = make(chan struct{})
	ids := make(chan int64, writers)
	createdResults := make(chan bool, writers)
	idempotentErrors := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			event := domain.NewWorkflowEvent(
				project.ID,
				domain.WorkflowSourceExternal,
				domain.WorkflowEventType("test.idempotent"),
				"",
			)
			event.IdempotencyKey = "same-command"
			stored, created, err := s.AppendWorkflowEvent(event)
			if err != nil {
				idempotentErrors <- err
				return
			}
			ids <- stored.ID
			createdResults <- created
		}()
	}
	close(start)
	wait.Wait()
	close(ids)
	close(createdResults)
	close(idempotentErrors)
	for err := range idempotentErrors {
		if err != nil {
			t.Fatal(err)
		}
	}
	firstID := int64(0)
	for id := range ids {
		if firstID == 0 {
			firstID = id
		}
		if id != firstID {
			t.Fatalf("idempotent IDs differ: %d and %d", firstID, id)
		}
	}
	creates := 0
	for created := range createdResults {
		if created {
			creates++
		}
	}
	if creates != 1 {
		t.Fatalf("idempotent creates = %d, want 1", creates)
	}
}

func TestWorkflowEventOwnershipAndTransactionalTransitionAudit(t *testing.T) {
	s := openTestStore(t)
	firstProject, firstTask := createQueuedTask(t, s, "owner/one")
	secondProject, secondTask := createQueuedTask(t, s, "owner/two")
	execution := mustCreateExecution(
		t,
		s,
		secondProject.ID,
		secondTask.ID,
		"planner",
		1,
	)

	event := domain.NewWorkflowEvent(
		firstProject.ID,
		domain.WorkflowSourceExternal,
		domain.WorkflowEventType("test.ownership"),
		"",
	)
	event.TaskID = &secondTask.ID
	if _, _, err := s.AppendWorkflowEvent(event); !errors.Is(
		err,
		ErrWorkflowEventOwnership,
	) {
		t.Fatalf("cross-project task error = %v", err)
	}
	event.TaskID = &firstTask.ID
	event.ExecutionID = &execution.ID
	if _, _, err := s.AppendWorkflowEvent(event); !errors.Is(
		err,
		ErrWorkflowEventOwnership,
	) {
		t.Fatalf("cross-task execution error = %v", err)
	}
	if _, err := s.db.Exec(`
		INSERT INTO workflow_events (
			project_id, task_id, sequence, source, event_type, created_at
		) VALUES (?, ?, 1, 'external', 'direct.invalid', CURRENT_TIMESTAMP)
	`, firstProject.ID, secondTask.ID); err == nil {
		t.Fatal("direct SQL bypassed workflow event ownership")
	}

	if _, err := s.db.Exec(`DROP TABLE workflow_events`); err != nil {
		t.Fatal(err)
	}
	err := s.ApplyProjectTaskTransition(ProjectTaskTransitionUpdate{
		ProjectID:      firstProject.ID,
		TaskID:         firstTask.ID,
		ExpectedStatus: domain.TaskQueued,
		NewStatus:      domain.TaskSelected,
		ProjectState:   domain.ProjectExecuting,
		SetCurrentTask: true,
		CurrentTaskID:  &firstTask.ID,
	})
	if err == nil {
		t.Fatal("transition succeeded without its audit event")
	}
	stored, getErr := s.GetProjectTaskByID(firstTask.ID)
	if getErr != nil || stored.Status != domain.TaskQueued {
		t.Fatalf("unaudited transition was not rolled back: %#v error=%v", stored, getErr)
	}
}
