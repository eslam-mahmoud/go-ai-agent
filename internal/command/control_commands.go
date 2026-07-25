package command

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	"github.com/eslam-mahmoud/go-ai-agent/internal/project"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
)

const (
	NamePause   Name = "pause"
	NameResume  Name = "resume"
	NameCancel  Name = "cancel"
	NameRetry   Name = "retry"
	NameAnswer  Name = "answer"
	NameApprove Name = "approve"
	NameReject  Name = "reject"
)

// Controller is the project control surface. Commands go through it rather
// than writing state, so every change keeps its transactional guarantees.
type Controller interface {
	Snapshot(projectID int64) (*project.Snapshot, error)
	Pause(projectID int64) (*project.Snapshot, error)
	Resume(projectID int64) (*project.Snapshot, error)
	Cancel(projectID int64) (*project.Snapshot, error)
	Retry(projectID int64) (*project.Snapshot, *domain.Execution, error)
}

// InputRecorder persists owner answers and approvals for the workflow to
// consume. A chat message never mutates task state directly.
type InputRecorder interface {
	RecordOwnerInput(input store.OwnerInput) (*store.OwnerInput, error)
}

// ControlAuditor records who issued a control command, so actions that change
// delivery are attributable.
type ControlAuditor interface {
	AppendWorkflowEvent(
		event *domain.WorkflowEvent,
	) (*domain.WorkflowEvent, bool, error)
}

// RegisterControlCommands adds the mutating commands. It is separate from the
// read-only registration so a deployment can expose inspection without
// handing over control.
func RegisterControlCommands(
	router *Router,
	reader ProjectReader,
	controller Controller,
	inputs InputRecorder,
	auditor ControlAuditor,
) error {
	switch {
	case router == nil:
		return errors.New("router is required")
	case reader == nil:
		return errors.New("project reader is required")
	case controller == nil:
		return errors.New("project controller is required")
	case inputs == nil:
		return errors.New("input recorder is required")
	}
	control := &controlCommands{
		reader:     reader,
		controller: controller,
		inputs:     inputs,
		auditor:    auditor,
	}
	commands := []struct {
		name        Name
		description string
		handler     Handler
	}{
		{NamePause, "pause delivery", control.pause},
		{NameResume, "resume delivery", control.resume},
		{NameCancel, "cancel the active task", control.cancel},
		{NameRetry, "retry the last failed execution", control.retry},
		{NameAnswer, "answer a blocking question", control.answer},
		{NameApprove, "approve a pending request", control.approve},
		{NameReject, "reject a pending request", control.reject},
	}
	for _, command := range commands {
		if err := router.Register(command.name, command.description, command.handler); err != nil {
			return err
		}
	}
	return nil
}

type controlCommands struct {
	reader     ProjectReader
	controller Controller
	inputs     InputRecorder
	auditor    ControlAuditor
}

func (control *controlCommands) pause(
	_ context.Context,
	command Command,
) (string, error) {
	project, err := control.project()
	if err != nil {
		return "", err
	}
	snapshot, err := control.controller.Pause(project.ID)
	if err != nil {
		return refusal("pause", err), nil
	}
	control.audit(project.ID, command, "paused delivery")
	return fmt.Sprintf("Paused. Project is %s.", snapshot.Project.State), nil
}

func (control *controlCommands) resume(
	_ context.Context,
	command Command,
) (string, error) {
	project, err := control.project()
	if err != nil {
		return "", err
	}
	snapshot, err := control.controller.Resume(project.ID)
	if err != nil {
		return refusal("resume", err), nil
	}
	control.audit(project.ID, command, "resumed delivery")
	return fmt.Sprintf("Resumed. Project is %s.", snapshot.Project.State), nil
}

func (control *controlCommands) cancel(
	_ context.Context,
	command Command,
) (string, error) {
	project, err := control.project()
	if err != nil {
		return "", err
	}
	snapshot, err := control.controller.Cancel(project.ID)
	if err != nil {
		return refusal("cancel", err), nil
	}
	control.audit(project.ID, command, "cancelled the active task")
	return fmt.Sprintf(
		"Cancelled the active task. Project is %s.",
		snapshot.Project.State,
	), nil
}

func (control *controlCommands) retry(
	_ context.Context,
	command Command,
) (string, error) {
	project, err := control.project()
	if err != nil {
		return "", err
	}
	snapshot, execution, err := control.controller.Retry(project.ID)
	if err != nil {
		return refusal("retry", err), nil
	}
	control.audit(project.ID, command, "retried the last execution")
	mode := ""
	if execution != nil {
		mode = " " + execution.Mode
	}
	return fmt.Sprintf(
		"Retrying%s. Task is %s.",
		mode,
		currentStatus(snapshot),
	), nil
}

func (control *controlCommands) answer(
	_ context.Context,
	command Command,
) (string, error) {
	body := strings.TrimSpace(strings.Join(command.Args, " "))
	if body == "" {
		// An empty answer applied as an answer would unblock work with
		// nothing to act on.
		return "Usage: /answer <your answer to the pending question>", nil
	}
	project, err := control.project()
	if err != nil {
		return "", err
	}
	snapshot, err := control.controller.Snapshot(project.ID)
	if err != nil {
		return "", err
	}
	input := store.OwnerInput{
		ProjectID: project.ID,
		Kind:      store.OwnerAnswer,
		Body:      body,
		Author:    author(command),
	}
	if snapshot.CurrentTask != nil {
		taskID := snapshot.CurrentTask.ID
		input.TaskID = &taskID
	}
	if _, err := control.inputs.RecordOwnerInput(input); err != nil {
		return refusal("answer", err), nil
	}
	control.audit(project.ID, command, "recorded an answer")
	return "Answer recorded. Delivery will pick it up.", nil
}

func (control *controlCommands) approve(
	_ context.Context,
	command Command,
) (string, error) {
	return control.decide(command, store.OwnerApproval, "approve")
}

func (control *controlCommands) reject(
	_ context.Context,
	command Command,
) (string, error) {
	return control.decide(command, store.OwnerRejection, "reject")
}

func (control *controlCommands) decide(
	command Command,
	kind store.OwnerInputKind,
	verb string,
) (string, error) {
	subject := strings.TrimSpace(strings.Join(command.Args, " "))
	if subject == "" {
		// Without a subject the decision could be applied to the wrong
		// request, which is worse than refusing it.
		return fmt.Sprintf("Usage: /%s <what you are %sing>", verb, verb), nil
	}
	project, err := control.project()
	if err != nil {
		return "", err
	}
	if _, err := control.inputs.RecordOwnerInput(store.OwnerInput{
		ProjectID: project.ID,
		Kind:      kind,
		Subject:   subject,
		Author:    author(command),
	}); err != nil {
		return refusal(verb, err), nil
	}
	control.audit(project.ID, command, verb+"ed "+subject)
	return fmt.Sprintf("Recorded: %s %s.", verb, subject), nil
}

func (control *controlCommands) project() (*domain.Project, error) {
	projects, err := control.reader.ListProjects()
	if err != nil {
		return nil, err
	}
	if len(projects) == 0 {
		return nil, ErrNoProject
	}
	return projects[0], nil
}

// audit records who changed delivery. A failure to record must not undo the
// change the owner asked for, so it is not surfaced as a command failure.
func (control *controlCommands) audit(
	projectID int64,
	command Command,
	action string,
) {
	if control.auditor == nil {
		return
	}
	event := domain.NewWorkflowEvent(
		projectID,
		domain.WorkflowSourceExternal,
		domain.WorkflowOwnerCommand,
		fmt.Sprintf("Owner %s.", action),
	)
	event.Data = []byte(fmt.Sprintf(
		`{"command":%q,"author":%q,"action":%q}`,
		command.Name, author(command), action,
	))
	_, _, _ = control.auditor.AppendWorkflowEvent(event)
}

func author(command Command) string {
	if command.UserID == 0 {
		return "unknown"
	}
	return strconv.FormatInt(command.UserID, 10)
}

func currentStatus(snapshot *project.Snapshot) string {
	if snapshot == nil || snapshot.CurrentTask == nil {
		return "idle"
	}
	return string(snapshot.CurrentTask.Status)
}

// refusal explains why a control command did not apply, which is more useful
// than a generic failure for actions the owner expected to work.
func refusal(verb string, err error) string {
	return fmt.Sprintf("Cannot %s: %s.", verb, readableError(err))
}

var (
	_ Controller    = (*project.Controller)(nil)
	_ InputRecorder = (*store.Store)(nil)
)
