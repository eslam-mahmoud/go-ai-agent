package project

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
)

var (
	ErrInvalidBacklogChanges = errors.New("invalid manager backlog changes")
	ErrStaleManagerReview    = errors.New("stale manager review")
)

type BacklogStore interface {
	LoadProjectAggregate(projectID int64) (*store.ProjectAggregate, error)
	ApplyProjectBacklogOrder(
		update store.ProjectBacklogOrderUpdate,
	) ([]*domain.Task, error)
}

type BacklogResult struct {
	Tasks   []*domain.Task
	Changed bool
	Moves   []store.ProjectBacklogMove
}

type BacklogController struct {
	store BacklogStore
}

func NewBacklogController(backlogStore BacklogStore) (*BacklogController, error) {
	if backlogStore == nil {
		return nil, errors.New("backlog controller store is required")
	}
	return &BacklogController{store: backlogStore}, nil
}

func (controller *BacklogController) ApplyManagerReview(
	projectID, managerReviewID int64,
) (*BacklogResult, error) {
	if projectID <= 0 || managerReviewID <= 0 {
		return nil, fmt.Errorf(
			"%w: project and manager review IDs must be positive",
			ErrInvalidBacklogChanges,
		)
	}
	aggregate, err := controller.store.LoadProjectAggregate(projectID)
	if err != nil {
		return nil, err
	}
	if aggregate == nil || aggregate.Project == nil {
		return nil, fmt.Errorf("%w: project aggregate is nil", ErrInconsistentState)
	}
	review := aggregate.LatestManagerReview
	if review == nil || review.ID != managerReviewID || review.ProjectID != projectID {
		return nil, fmt.Errorf(
			"%w: review %d is not the latest review for project %d",
			ErrStaleManagerReview,
			managerReviewID,
			projectID,
		)
	}
	changes, err := decodeBacklogChanges(review.BacklogChanges)
	if err != nil {
		return nil, err
	}
	expected := make([]int64, 0, len(aggregate.Tasks))
	taskByID := make(map[int64]*domain.Task, len(aggregate.Tasks))
	for index, task := range aggregate.Tasks {
		if task == nil || task.ID <= 0 || task.ProjectID != projectID {
			return nil, fmt.Errorf(
				"%w: invalid task at backlog position %d",
				ErrInconsistentState,
				index+1,
			)
		}
		expected = append(expected, task.ID)
		taskByID[task.ID] = task
	}
	ordered := append([]int64(nil), expected...)
	moves := make([]store.ProjectBacklogMove, 0, len(changes))
	seen := make(map[int64]struct{}, len(changes))
	for index, change := range changes {
		if change.Action != "reorder" && change.Action != "reprioritize" {
			return nil, fmt.Errorf(
				"%w: change %d action %q is not a reorder",
				ErrInvalidBacklogChanges,
				index,
				change.Action,
			)
		}
		if change.TaskID == nil || *change.TaskID <= 0 ||
			change.Position == nil || *change.Position <= 0 ||
			*change.Position > len(ordered) ||
			strings.TrimSpace(change.Reason) == "" {
			return nil, fmt.Errorf(
				"%w: change %d requires task, valid position, and reason",
				ErrInvalidBacklogChanges,
				index,
			)
		}
		task := taskByID[*change.TaskID]
		if task == nil {
			return nil, fmt.Errorf(
				"%w: task %d does not belong to project",
				ErrInvalidBacklogChanges,
				*change.TaskID,
			)
		}
		if task.Status != domain.TaskProposed && task.Status != domain.TaskQueued {
			return nil, fmt.Errorf(
				"%w: task %d in status %q cannot be reordered",
				ErrInvalidBacklogChanges,
				task.ID,
				task.Status,
			)
		}
		if _, duplicate := seen[task.ID]; duplicate {
			return nil, fmt.Errorf(
				"%w: task %d has duplicate moves",
				ErrInvalidBacklogChanges,
				task.ID,
			)
		}
		seen[task.ID] = struct{}{}
		ordered = moveTaskID(ordered, task.ID, *change.Position-1)
		moves = append(moves, store.ProjectBacklogMove{
			TaskID:   task.ID,
			Position: *change.Position,
			Reason:   strings.TrimSpace(change.Reason),
		})
	}
	if equalBacklogOrder(expected, ordered) {
		return &BacklogResult{
			Tasks:   append([]*domain.Task(nil), aggregate.Tasks...),
			Changed: false,
			Moves:   moves,
		}, nil
	}
	tasks, err := controller.store.ApplyProjectBacklogOrder(
		store.ProjectBacklogOrderUpdate{
			ProjectID:       projectID,
			ManagerReviewID: managerReviewID,
			ExpectedTaskIDs: expected,
			OrderedTaskIDs:  ordered,
			Moves:           moves,
		},
	)
	if err != nil {
		return nil, err
	}
	return &BacklogResult{Tasks: tasks, Changed: true, Moves: moves}, nil
}

type managerBacklogChange struct {
	Action   string `json:"action"`
	TaskID   *int64 `json:"task_id"`
	Position *int   `json:"position"`
	Reason   string `json:"reason"`
}

func decodeBacklogChanges(raw json.RawMessage) ([]managerBacklogChange, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var changes []managerBacklogChange
	if err := decoder.Decode(&changes); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidBacklogChanges, err)
	}
	if changes == nil {
		return nil, fmt.Errorf("%w: changes must be a JSON array", ErrInvalidBacklogChanges)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: trailing JSON", ErrInvalidBacklogChanges)
	}
	return changes, nil
}

func moveTaskID(order []int64, taskID int64, target int) []int64 {
	source := -1
	for index, id := range order {
		if id == taskID {
			source = index
			break
		}
	}
	if source < 0 || source == target {
		return order
	}
	copy(order[source:], order[source+1:])
	order = order[:len(order)-1]
	order = append(order, 0)
	copy(order[target+1:], order[target:])
	order[target] = taskID
	return order
}

func equalBacklogOrder(left, right []int64) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

var _ BacklogStore = (*store.Store)(nil)
