package project

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
)

var ErrInitialBacklog = errors.New("initial backlog creation failed")

type InitialBacklogStore interface {
	LoadProjectAggregate(projectID int64) (*store.ProjectAggregate, error)
	CreateInitialBacklog(projectID int64, tasks []*domain.Task) ([]*domain.Task, error)
	RecordProjectTaskIssue(
		projectID, taskID int64,
		issueNumber int,
		reused bool,
	) (*domain.Task, error)
}

type InitialBacklogResult struct {
	Tasks          []*domain.Task
	AlreadyExisted bool
	FiledIssues    []*domain.Task
	ReusedIssues   []*domain.Task
}

// InitialBacklogController creates a project's first ordered backlog from an
// architecture proposal and files a GitHub issue for each task.
type InitialBacklogController struct {
	store  InitialBacklogStore
	client DiscoveryIssueClient
}

func NewInitialBacklogController(
	backlogStore InitialBacklogStore,
	client DiscoveryIssueClient,
) (*InitialBacklogController, error) {
	if backlogStore == nil {
		return nil, errors.New("initial backlog store is required")
	}
	if client == nil {
		return nil, errors.New("initial backlog issue client is required")
	}
	return &InitialBacklogController{store: backlogStore, client: client}, nil
}

// Initialize creates the backlog when the project has none, then files an
// issue for every task still missing one. Both halves are separately
// resumable, so a failure between them is repaired by running again.
func (controller *InitialBacklogController) Initialize(
	ctx context.Context,
	projectID int64,
	proposal json.RawMessage,
) (*InitialBacklogResult, error) {
	if projectID <= 0 {
		return nil, fmt.Errorf("%w: project ID must be positive", ErrInitialBacklog)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	aggregate, err := controller.store.LoadProjectAggregate(projectID)
	if err != nil {
		return nil, err
	}
	if aggregate == nil || aggregate.Project == nil {
		return nil, fmt.Errorf("%w: project aggregate is nil", ErrInconsistentState)
	}
	owner, repo, err := splitRepository(aggregate.Project.Repo)
	if err != nil {
		return nil, err
	}

	result := &InitialBacklogResult{}
	if len(aggregate.Tasks) > 0 {
		// Initialization happens once; a project with a backlog is left alone.
		result.AlreadyExisted = true
		result.Tasks = aggregate.Tasks
	} else {
		tasks, err := initialTasksFromProposal(projectID, proposal)
		if err != nil {
			return nil, err
		}
		if len(tasks) == 0 {
			return result, nil
		}
		created, err := controller.store.CreateInitialBacklog(projectID, tasks)
		if err != nil {
			return nil, err
		}
		result.Tasks = created
	}

	if err := controller.fileIssues(ctx, projectID, owner, repo, result); err != nil {
		return nil, err
	}
	return result, nil
}

// fileIssues gives every task without an issue exactly one, reusing a matching
// open issue rather than opening a duplicate.
func (controller *InitialBacklogController) fileIssues(
	ctx context.Context,
	projectID int64,
	owner, repo string,
	result *InitialBacklogResult,
) error {
	pending := make([]*domain.Task, 0, len(result.Tasks))
	for _, task := range result.Tasks {
		if task != nil && task.IssueNumber == 0 {
			pending = append(pending, task)
		}
	}
	if len(pending) == 0 {
		return nil
	}
	openIssues, err := controller.client.ListOpenIssues(ctx, owner, repo)
	if err != nil {
		return fmt.Errorf("%w: list open issues: %v", ErrInitialBacklog, err)
	}
	if err := controller.client.EnsureLabels(ctx, owner, repo, map[string]string{
		"type:architecture": "1d76db",
		"release:blocker":   "b60205",
	}); err != nil {
		return fmt.Errorf("%w: ensure labels: %v", ErrInitialBacklog, err)
	}
	for _, task := range pending {
		if err := ctx.Err(); err != nil {
			return err
		}
		if existing := matchOpenIssue(openIssues, task.Title); existing != nil {
			stored, err := controller.store.RecordProjectTaskIssue(
				projectID, task.ID, existing.Number, true,
			)
			if err != nil {
				return err
			}
			result.ReusedIssues = append(result.ReusedIssues, stored)
			continue
		}
		issue, err := controller.client.CreateIssue(
			ctx, owner, repo, task.Title, initialTaskIssueBody(task), initialTaskLabels(task),
		)
		if err != nil {
			return fmt.Errorf("%w: create issue: %v", ErrInitialBacklog, err)
		}
		if issue == nil || issue.Number <= 0 {
			return fmt.Errorf("%w: created issue has no number", ErrInitialBacklog)
		}
		stored, err := controller.store.RecordProjectTaskIssue(
			projectID, task.ID, issue.Number, false,
		)
		if err != nil {
			return err
		}
		result.FiledIssues = append(result.FiledIssues, stored)
		openIssues = append(openIssues, issue)
	}
	return nil
}

// initialTasksFromProposal preserves the architect's ordering: the proposal
// lists work in the order it should be done.
func initialTasksFromProposal(
	projectID int64,
	proposal json.RawMessage,
) ([]*domain.Task, error) {
	if len(proposal) == 0 {
		return nil, nil
	}
	var decoded struct {
		RecommendedTasks []struct {
			Title         string `json:"title"`
			Goal          string `json:"goal"`
			Reason        string `json:"reason"`
			BlocksRelease bool   `json:"blocks_release"`
		} `json:"recommended_tasks"`
	}
	if err := json.Unmarshal(proposal, &decoded); err != nil {
		return nil, fmt.Errorf("%w: decode proposal: %v", ErrInitialBacklog, err)
	}
	tasks := make([]*domain.Task, 0, len(decoded.RecommendedTasks))
	for index, recommended := range decoded.RecommendedTasks {
		title := strings.TrimSpace(recommended.Title)
		goal := strings.TrimSpace(recommended.Goal)
		if title == "" || goal == "" {
			return nil, fmt.Errorf(
				"%w: recommended task %d needs a title and a goal",
				ErrInitialBacklog,
				index,
			)
		}
		task := domain.NewTask(projectID, title, goal)
		task.Status = domain.TaskQueued
		task.Source = "architect"
		task.TaskType = "architecture"
		task.BlocksRelease = recommended.BlocksRelease
		task.SelectedReason = ""
		task.Sequence = index + 1
		tasks = append(tasks, task)
	}
	return tasks, nil
}

func initialTaskIssueBody(task *domain.Task) string {
	var body strings.Builder
	body.WriteString(strings.TrimSpace(task.Goal))
	body.WriteString("\n\n### Task\n\n")
	fmt.Fprintf(&body, "- **Backlog position:** %d\n", task.Sequence)
	fmt.Fprintf(&body, "- **Source:** %s\n", task.Source)
	if task.BlocksRelease {
		body.WriteString("- **Release blocker:** yes\n")
	}
	body.WriteString("\n_Opened by Madar during project initialization._\n")
	return body.String()
}

func initialTaskLabels(task *domain.Task) []string {
	labels := []string{"type:architecture"}
	if task.BlocksRelease {
		labels = append(labels, "release:blocker")
	}
	return labels
}

// ProposalTasks reports how many tasks a proposal would create, which lets a
// caller decide whether initialization is worth running.
func ProposalTasks(proposal json.RawMessage) (int, error) {
	tasks, err := initialTasksFromProposal(1, proposal)
	return len(tasks), err
}

var _ InitialBacklogStore = (*store.Store)(nil)
