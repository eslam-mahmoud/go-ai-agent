package command

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
)

var ErrNoProject = errors.New("no project is configured")

// ProjectReader is the durable state the read-only commands answer from.
type ProjectReader interface {
	ListProjects() ([]*domain.Project, error)
	ListProjectTasks(projectID int64) ([]*domain.Task, error)
	LatestManagerReview(projectID int64) (*domain.ManagerReview, error)
	ListWorkflowEvents(projectID, afterSequence int64, limit int) ([]*domain.WorkflowEvent, error)
}

// RegisterProjectCommands adds the read-only command surface. Mutating
// commands are registered separately, so a reader-only deployment cannot
// accidentally expose control.
func RegisterProjectCommands(router *Router, reader ProjectReader) error {
	if router == nil {
		return errors.New("router is required")
	}
	if reader == nil {
		return errors.New("project reader is required")
	}
	commands := []struct {
		name        Name
		description string
		handler     Handler
	}{
		{NameStatus, "current delivery status", statusHandler(reader)},
		{NameProject, "project goal, health, and progress", projectHandler(reader)},
		{NamePlan, "ordered backlog", planHandler(reader)},
		{NameNext, "the task queued next", nextHandler(reader)},
		{NameLogs, "recent workflow events", logsHandler(reader)},
	}
	for _, command := range commands {
		if err := router.Register(command.name, command.description, command.handler); err != nil {
			return err
		}
	}
	return nil
}

func statusHandler(reader ProjectReader) Handler {
	return func(_ context.Context, _ Command) (string, error) {
		project, tasks, err := currentProject(reader)
		if err != nil {
			return "", err
		}
		current := currentTask(project, tasks)
		var reply strings.Builder
		fmt.Fprintf(&reply, "%s — %s\n", project.Name, project.State)
		fmt.Fprintf(&reply, "Health: %s\n", project.Health)
		if current == nil {
			reply.WriteString("Active task: none")
			return reply.String(), nil
		}
		fmt.Fprintf(&reply, "Active task: %s %s\n", issueRef(current.IssueNumber), current.Title)
		fmt.Fprintf(&reply, "Status: %s", current.Status)
		if current.BranchName != "" {
			fmt.Fprintf(&reply, "\nBranch: %s", current.BranchName)
		}
		if current.PRNumber > 0 {
			fmt.Fprintf(&reply, "\nPull request: #%d", current.PRNumber)
		}
		return reply.String(), nil
	}
}

func projectHandler(reader ProjectReader) Handler {
	return func(_ context.Context, _ Command) (string, error) {
		project, tasks, err := currentProject(reader)
		if err != nil {
			return "", err
		}
		completed := 0
		for _, task := range tasks {
			if task.Status == domain.TaskCompleted {
				completed++
			}
		}
		var reply strings.Builder
		fmt.Fprintf(&reply, "%s (%s)\n", project.Name, project.Repo)
		fmt.Fprintf(&reply, "Goal: %s\n", project.Goal)
		fmt.Fprintf(&reply, "Health: %s\n", project.Health)
		fmt.Fprintf(&reply, "Phase: %s\n", project.State)
		fmt.Fprintf(&reply, "Tasks: %d completed of %d\n", completed, len(tasks))
		if project.ReleaseReadiness != "" {
			fmt.Fprintf(&reply, "Release: %s\n", project.ReleaseReadiness)
		}
		review, err := reader.LatestManagerReview(project.ID)
		if err != nil {
			return "", err
		}
		if review != nil && strings.TrimSpace(review.OwnerUpdate) != "" {
			fmt.Fprintf(&reply, "\nLast manager update:\n%s", review.OwnerUpdate)
		}
		return reply.String(), nil
	}
}

func planHandler(reader ProjectReader) Handler {
	return func(_ context.Context, _ Command) (string, error) {
		project, tasks, err := currentProject(reader)
		if err != nil {
			return "", err
		}
		if len(tasks) == 0 {
			return fmt.Sprintf("%s has no backlog yet.", project.Name), nil
		}
		var reply strings.Builder
		fmt.Fprintf(&reply, "%s backlog:", project.Name)
		for _, task := range tasks {
			fmt.Fprintf(&reply, "\n%d. %s %s — %s",
				task.Sequence, issueRef(task.IssueNumber), task.Title, task.Status)
			if task.BlocksRelease {
				reply.WriteString(" (release blocker)")
			}
		}
		return reply.String(), nil
	}
}

func nextHandler(reader ProjectReader) Handler {
	return func(_ context.Context, _ Command) (string, error) {
		project, tasks, err := currentProject(reader)
		if err != nil {
			return "", err
		}
		for _, task := range tasks {
			if task.Status != domain.TaskQueued && task.Status != domain.TaskProposed {
				continue
			}
			var reply strings.Builder
			fmt.Fprintf(&reply, "Next: %s %s\n", issueRef(task.IssueNumber), task.Title)
			fmt.Fprintf(&reply, "Position: %d\n", task.Sequence)
			fmt.Fprintf(&reply, "Goal: %s", task.Goal)
			if reason := strings.TrimSpace(task.SelectedReason); reason != "" {
				fmt.Fprintf(&reply, "\nReason: %s", reason)
			}
			return reply.String(), nil
		}
		return fmt.Sprintf("%s has nothing queued.", project.Name), nil
	}
}

func logsHandler(reader ProjectReader) Handler {
	return func(_ context.Context, _ Command) (string, error) {
		project, _, err := currentProject(reader)
		if err != nil {
			return "", err
		}
		events, err := reader.ListWorkflowEvents(project.ID, 0, 100)
		if err != nil {
			return "", err
		}
		if len(events) == 0 {
			return fmt.Sprintf("%s has no recorded events.", project.Name), nil
		}
		// Newest last is how a log reads; show the tail.
		const shown = 15
		if len(events) > shown {
			events = events[len(events)-shown:]
		}
		var reply strings.Builder
		fmt.Fprintf(&reply, "%s recent events:", project.Name)
		for _, event := range events {
			fmt.Fprintf(&reply, "\n%s · %s", event.Type, oneLine(event.Message))
		}
		return reply.String(), nil
	}
}

// currentProject resolves the single managed project. Madar manages one
// project at a time, so a command needs no project argument.
func currentProject(reader ProjectReader) (*domain.Project, []*domain.Task, error) {
	projects, err := reader.ListProjects()
	if err != nil {
		return nil, nil, err
	}
	if len(projects) == 0 {
		return nil, nil, ErrNoProject
	}
	project := projects[0]
	tasks, err := reader.ListProjectTasks(project.ID)
	if err != nil {
		return nil, nil, err
	}
	return project, tasks, nil
}

func currentTask(project *domain.Project, tasks []*domain.Task) *domain.Task {
	if project.CurrentTaskID == nil {
		return nil
	}
	for _, task := range tasks {
		if task.ID == *project.CurrentTaskID {
			return task
		}
	}
	return nil
}

func issueRef(number int) string {
	if number <= 0 {
		return "(no issue)"
	}
	return fmt.Sprintf("#%d", number)
}

func oneLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

var _ ProjectReader = (*store.Store)(nil)
