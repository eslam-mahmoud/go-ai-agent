package project

import (
	"errors"
	"fmt"
	"strings"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
)

var ErrDiscoveryBacklogInsert = errors.New("discovery backlog insertion failed")

type DiscoveryBacklogStore interface {
	ListDiscoveries(projectID int64) ([]*domain.Discovery, error)
	InsertDiscoveryTask(insert store.DiscoveryTaskInsert) (*domain.Task, error)
}

type DiscoveryBacklogResult struct {
	Inserted []*domain.Task
	Skipped  []*domain.Discovery
}

// DiscoveryBacklogController turns published discoveries into ordered backlog
// tasks. It owns placement policy; the store owns ordering and atomicity.
type DiscoveryBacklogController struct {
	store DiscoveryBacklogStore
}

func NewDiscoveryBacklogController(
	backlogStore DiscoveryBacklogStore,
) (*DiscoveryBacklogController, error) {
	if backlogStore == nil {
		return nil, errors.New("discovery backlog store is required")
	}
	return &DiscoveryBacklogController{store: backlogStore}, nil
}

// InsertAcceptedDiscoveries queues every accepted discovery that has an issue
// and no task yet. Re-running inserts nothing new.
func (controller *DiscoveryBacklogController) InsertAcceptedDiscoveries(
	projectID int64,
) (*DiscoveryBacklogResult, error) {
	if projectID <= 0 {
		return nil, fmt.Errorf("%w: project ID must be positive", ErrDiscoveryBacklogInsert)
	}
	discoveries, err := controller.store.ListDiscoveries(projectID)
	if err != nil {
		return nil, err
	}
	result := &DiscoveryBacklogResult{}
	for _, discovery := range discoveries {
		if !queueableDiscovery(discovery) {
			continue
		}
		task, err := controller.store.InsertDiscoveryTask(store.DiscoveryTaskInsert{
			ProjectID:   projectID,
			DiscoveryID: discovery.ID,
			Task:        discoveryTask(discovery),
			Front:       discoveryEntersFront(discovery.Decision),
		})
		if errors.Is(err, store.ErrDiscoveryTaskExists) {
			result.Skipped = append(result.Skipped, discovery)
			continue
		}
		if err != nil {
			return nil, err
		}
		result.Inserted = append(result.Inserted, task)
	}
	return result, nil
}

// queueableDiscovery reports whether a discovery is ready for the backlog: the
// manager accepted it as new work and it already has an issue to point at.
func queueableDiscovery(discovery *domain.Discovery) bool {
	return discovery != nil &&
		discovery.Status == domain.DiscoveryAccepted &&
		discovery.Decision.CreatesTask() &&
		discovery.CreatedIssueNumber > 0
}

// discoveryEntersFront reports whether the verdict means the work should run
// before whatever the backlog currently plans next.
func discoveryEntersFront(decision domain.DiscoveryDecision) bool {
	switch decision {
	case domain.DecisionCreateNextTask,
		domain.DecisionCreatePrioritized,
		domain.DecisionCreateReleaseBlocker:
		return true
	default:
		return false
	}
}

func discoveryTask(discovery *domain.Discovery) *domain.Task {
	goal := strings.TrimSpace(discovery.Description)
	if goal == "" {
		goal = strings.TrimSpace(discovery.Title)
	}
	task := domain.NewTask(discovery.ProjectID, discovery.Title, goal)
	task.Status = domain.TaskQueued
	task.IssueNumber = discovery.CreatedIssueNumber
	task.TaskType = string(discovery.Category)
	task.Source = "discovery"
	task.BlocksRelease = discovery.Decision == domain.DecisionCreateReleaseBlocker
	task.Priority = discoveryTaskPriority(discovery.Severity)
	return task
}

// discoveryTaskPriority ranks severity into the task priority scale, where a
// lower number is more urgent.
func discoveryTaskPriority(severity domain.DiscoverySeverity) int {
	switch {
	case severity.AtLeast(domain.SeverityCritical):
		return 1
	case severity.AtLeast(domain.SeverityHigh):
		return 2
	case severity.AtLeast(domain.SeverityMedium):
		return 3
	default:
		return 4
	}
}

var _ DiscoveryBacklogStore = (*store.Store)(nil)
