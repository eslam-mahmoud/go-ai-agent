package command

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	"github.com/eslam-mahmoud/go-ai-agent/internal/project"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
)

func TestControlCommandsRouteThroughTheController(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    Name
		expect  string
		applied func(*fakeController) int
	}{
		{NamePause, "Paused", func(c *fakeController) int { return c.pauses }},
		{NameResume, "Resumed", func(c *fakeController) int { return c.resumes }},
		{NameCancel, "Cancelled", func(c *fakeController) int { return c.cancels }},
		{NameRetry, "Retrying", func(c *fakeController) int { return c.retries }},
	}
	for _, test := range tests {
		test := test
		t.Run(string(test.name), func(t *testing.T) {
			t.Parallel()
			controller := newFakeController()
			router, _, auditor := newControlRouter(t, controller)
			reply, err := router.Dispatch(context.Background(), Command{
				Name: test.name, UserID: 99,
			})
			if err != nil {
				t.Fatalf("Dispatch: %v", err)
			}
			if !strings.Contains(reply, test.expect) {
				t.Fatalf("reply = %q", reply)
			}
			if test.applied(controller) != 1 {
				t.Fatalf("controller was not called once")
			}
			// The change is attributable to whoever issued it.
			if len(auditor.events) != 1 {
				t.Fatalf("audit events = %d", len(auditor.events))
			}
			if !strings.Contains(string(auditor.events[0].Data), `"author":"99"`) {
				t.Fatalf("audit data = %s", auditor.events[0].Data)
			}
		})
	}
}

func TestControlCommandsExplainWhyTheyCannotApply(t *testing.T) {
	t.Parallel()
	controller := newFakeController()
	controller.err = errors.New("invalid project control action: project 7 is already paused")
	router, _, auditor := newControlRouter(t, controller)

	reply, err := router.Dispatch(context.Background(), Command{Name: NamePause})
	if err != nil {
		t.Fatalf("Dispatch returned an error instead of a reply: %v", err)
	}
	if !strings.Contains(reply, "Cannot pause") {
		t.Fatalf("reply = %q", reply)
	}
	// A refused command is not recorded as a change.
	if len(auditor.events) != 0 {
		t.Fatalf("a refusal was audited as a change: %#v", auditor.events)
	}
}

func TestAnswerAndApprovalRecordDurableInput(t *testing.T) {
	t.Parallel()
	controller := newFakeController()
	router, inputs, _ := newControlRouter(t, controller)
	ctx := context.Background()

	reply, err := router.Dispatch(ctx, Command{
		Name: NameAnswer, Args: []string{"use", "eu-west"}, UserID: 99,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "Answer recorded") {
		t.Fatalf("reply = %q", reply)
	}
	if len(inputs.recorded) != 1 {
		t.Fatalf("recorded = %#v", inputs.recorded)
	}
	answer := inputs.recorded[0]
	if answer.Kind != store.OwnerAnswer || answer.Body != "use eu-west" ||
		answer.Author != "99" {
		t.Fatalf("answer = %#v", answer)
	}
	// The answer is attached to the active task so the workflow can apply it.
	if answer.TaskID == nil || *answer.TaskID != 3 {
		t.Fatalf("answer task = %#v", answer.TaskID)
	}

	if _, err := router.Dispatch(ctx, Command{
		Name: NameApprove, Args: []string{"force", "push"}, UserID: 99,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := router.Dispatch(ctx, Command{
		Name: NameReject, Args: []string{"schema", "drop"}, UserID: 99,
	}); err != nil {
		t.Fatal(err)
	}
	if len(inputs.recorded) != 3 {
		t.Fatalf("recorded = %d inputs", len(inputs.recorded))
	}
	if inputs.recorded[1].Kind != store.OwnerApproval ||
		inputs.recorded[1].Subject != "force push" {
		t.Fatalf("approval = %#v", inputs.recorded[1])
	}
	if inputs.recorded[2].Kind != store.OwnerRejection ||
		inputs.recorded[2].Subject != "schema drop" {
		t.Fatalf("rejection = %#v", inputs.recorded[2])
	}
}

// An empty answer or a subjectless approval must be refused, not applied.
func TestAnswerAndApprovalRequireContent(t *testing.T) {
	t.Parallel()
	router, inputs, _ := newControlRouter(t, newFakeController())
	ctx := context.Background()
	for _, command := range []Command{
		{Name: NameAnswer},
		{Name: NameAnswer, Args: []string{"   "}},
		{Name: NameApprove},
		{Name: NameReject, Args: []string{" "}},
	} {
		reply, err := router.Dispatch(ctx, command)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(reply, "Usage:") {
			t.Fatalf("command %v reply = %q", command.Name, reply)
		}
	}
	if len(inputs.recorded) != 0 {
		t.Fatalf("an empty command was recorded: %#v", inputs.recorded)
	}
}

func TestControlRegistrationValidatesDependencies(t *testing.T) {
	t.Parallel()
	router := newTestRouter(t, allowAll())
	reader := fixtureReader()
	controller := newFakeController()
	inputs := &fakeInputRecorder{}
	cases := []struct {
		name       string
		router     *Router
		reader     ProjectReader
		controller Controller
		inputs     InputRecorder
	}{
		{"router", nil, reader, controller, inputs},
		{"reader", router, nil, controller, inputs},
		{"controller", router, reader, nil, inputs},
		{"inputs", router, reader, controller, nil},
	}
	for _, test := range cases {
		if err := RegisterControlCommands(
			test.router, test.reader, test.controller, test.inputs, nil,
		); err == nil {
			t.Errorf("missing %s accepted", test.name)
		}
	}
	// Auditing is optional; control still works without it.
	fresh := newTestRouter(t, allowAll())
	if err := RegisterControlCommands(fresh, reader, controller, inputs, nil); err != nil {
		t.Fatalf("registration without an auditor: %v", err)
	}
	if _, err := fresh.Dispatch(context.Background(), Command{Name: NamePause}); err != nil {
		t.Fatalf("pause without an auditor: %v", err)
	}
}

// Read-only and control commands register separately, so a deployment can
// expose inspection without handing over control.
func TestReadOnlyRegistrationExposesNoControl(t *testing.T) {
	t.Parallel()
	router := newTestRouter(t, allowAll())
	if err := RegisterProjectCommands(router, fixtureReader()); err != nil {
		t.Fatal(err)
	}
	reply, err := router.Dispatch(context.Background(), Command{Name: NamePause})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "Unknown command") {
		t.Fatalf("control was exposed by read-only registration: %q", reply)
	}
}

func newControlRouter(
	t *testing.T,
	controller *fakeController,
) (*Router, *fakeInputRecorder, *fakeAuditor) {
	t.Helper()
	router := newTestRouter(t, allowAll())
	inputs := &fakeInputRecorder{}
	auditor := &fakeAuditor{}
	if err := RegisterControlCommands(
		router, fixtureReader(), controller, inputs, auditor,
	); err != nil {
		t.Fatal(err)
	}
	return router, inputs, auditor
}

type fakeController struct {
	pauses  int
	resumes int
	cancels int
	retries int
	err     error
}

func newFakeController() *fakeController { return &fakeController{} }

func (fake *fakeController) snapshot() *project.Snapshot {
	projectRecord := fixtureProject()
	task := domain.NewTask(7, "Active work", "goal")
	task.ID = 3
	task.Status = domain.TaskDeveloping
	return &project.Snapshot{
		Project:     projectRecord,
		Tasks:       []*domain.Task{task},
		CurrentTask: task,
	}
}

func (fake *fakeController) Snapshot(int64) (*project.Snapshot, error) {
	if fake.err != nil {
		return nil, fake.err
	}
	return fake.snapshot(), nil
}

func (fake *fakeController) Pause(int64) (*project.Snapshot, error) {
	if fake.err != nil {
		return nil, fake.err
	}
	fake.pauses++
	return fake.snapshot(), nil
}

func (fake *fakeController) Resume(int64) (*project.Snapshot, error) {
	if fake.err != nil {
		return nil, fake.err
	}
	fake.resumes++
	return fake.snapshot(), nil
}

func (fake *fakeController) Cancel(int64) (*project.Snapshot, error) {
	if fake.err != nil {
		return nil, fake.err
	}
	fake.cancels++
	return fake.snapshot(), nil
}

func (fake *fakeController) Retry(int64) (*project.Snapshot, *domain.Execution, error) {
	if fake.err != nil {
		return nil, nil, fake.err
	}
	fake.retries++
	return fake.snapshot(), &domain.Execution{Mode: "developer"}, nil
}

type fakeInputRecorder struct {
	recorded []store.OwnerInput
	err      error
}

func (fake *fakeInputRecorder) RecordOwnerInput(
	input store.OwnerInput,
) (*store.OwnerInput, error) {
	if fake.err != nil {
		return nil, fake.err
	}
	fake.recorded = append(fake.recorded, input)
	return &input, nil
}

type fakeAuditor struct {
	events []*domain.WorkflowEvent
}

func (fake *fakeAuditor) AppendWorkflowEvent(
	event *domain.WorkflowEvent,
) (*domain.WorkflowEvent, bool, error) {
	fake.events = append(fake.events, event)
	return event, true, nil
}
