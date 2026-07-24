package project

import (
	"errors"
	"fmt"
	"sort"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

var ErrInvalidHealthInput = errors.New("invalid project health input")

type HealthRisk struct {
	Code   string `json:"code"`
	TaskID *int64 `json:"task_id"`
	Detail string `json:"detail"`
}

type HealthAssessment struct {
	Health          domain.ProjectHealth `json:"health"`
	ProgressPercent int                  `json:"progress_percent"`
	CompletedTasks  int                  `json:"completed_tasks"`
	DeliveryTasks   int                  `json:"delivery_tasks"`
	Risks           []HealthRisk         `json:"risks"`
}

// AssessHealth derives project health and progress exclusively from a durable
// project snapshot. Cancelled and deferred work is excluded from progress, but
// cancelling or deferring a release blocker is an off-track risk.
func AssessHealth(
	project *domain.Project,
	tasks []*domain.Task,
	executions []*domain.Execution,
) (HealthAssessment, error) {
	assessment := HealthAssessment{Risks: []HealthRisk{}}
	if err := project.Validate(); err != nil {
		return assessment, fmt.Errorf("%w: %v", ErrInvalidHealthInput, err)
	}
	taskByID := make(map[int64]*domain.Task, len(tasks))
	activeTasks := 0
	blocked := project.State == domain.ProjectBlocked ||
		project.Health == domain.HealthBlocked
	atRisk := project.Health == domain.HealthAtRisk
	offTrack := project.Health == domain.HealthOffTrack
	for index, task := range tasks {
		if err := task.Validate(); err != nil {
			return assessment, fmt.Errorf("%w: task %d: %v", ErrInvalidHealthInput, index, err)
		}
		if task.ID <= 0 || task.ProjectID != project.ID {
			return assessment, fmt.Errorf(
				"%w: task %d does not belong to project %d",
				ErrInvalidHealthInput,
				task.ID,
				project.ID,
			)
		}
		if _, duplicate := taskByID[task.ID]; duplicate {
			return assessment, fmt.Errorf("%w: duplicate task %d", ErrInvalidHealthInput, task.ID)
		}
		taskByID[task.ID] = task
		if task.Status.Active() {
			activeTasks++
		}
		switch task.Status {
		case domain.TaskCancelled, domain.TaskDeferred:
			if task.BlocksRelease {
				offTrack = true
				assessment.Risks = append(assessment.Risks, healthRisk(
					"release-blocker-removed",
					task.ID,
					fmt.Sprintf("Release-blocking task is %s.", task.Status),
				))
			}
		default:
			assessment.DeliveryTasks++
			if task.Status == domain.TaskCompleted {
				assessment.CompletedTasks++
			}
		}
		switch task.Status {
		case domain.TaskBlocked:
			blocked = true
			assessment.Risks = append(assessment.Risks, healthRisk(
				"task-blocked",
				task.ID,
				"Task is blocked.",
			))
		case domain.TaskWaitingInput:
			atRisk = true
			assessment.Risks = append(assessment.Risks, healthRisk(
				"waiting-input",
				task.ID,
				"Task is waiting for owner input.",
			))
		case domain.TaskWaitingCI:
			atRisk = true
			assessment.Risks = append(assessment.Risks, healthRisk(
				"waiting-ci",
				task.ID,
				"Task is waiting for CI.",
			))
		}
	}
	if activeTasks > 1 {
		return assessment, fmt.Errorf(
			"%w: project has %d active tasks",
			ErrInvalidHealthInput,
			activeTasks,
		)
	}
	if project.CurrentTaskID == nil && activeTasks != 0 {
		return assessment, fmt.Errorf("%w: active task is not selected by project", ErrInvalidHealthInput)
	}
	if project.CurrentTaskID != nil {
		current := taskByID[*project.CurrentTaskID]
		if current == nil || !current.Status.Active() {
			return assessment, fmt.Errorf(
				"%w: current task %d is missing or inactive",
				ErrInvalidHealthInput,
				*project.CurrentTaskID,
			)
		}
	}

	latest := make(map[int64]*domain.Execution)
	for index, execution := range executions {
		if err := execution.Validate(); err != nil {
			return assessment, fmt.Errorf("%w: execution %d: %v", ErrInvalidHealthInput, index, err)
		}
		if execution.ProjectID != project.ID || taskByID[execution.TaskID] == nil {
			return assessment, fmt.Errorf(
				"%w: execution %d has inconsistent ownership",
				ErrInvalidHealthInput,
				execution.ID,
			)
		}
		if previous := latest[execution.TaskID]; previous == nil || execution.ID > previous.ID {
			latest[execution.TaskID] = execution
		}
	}
	for taskID, execution := range latest {
		switch execution.Status {
		case domain.ExecutionFailed, domain.ExecutionInterrupted:
			atRisk = true
			assessment.Risks = append(assessment.Risks, healthRisk(
				"latest-execution-"+string(execution.Status),
				taskID,
				"Latest task execution requires recovery.",
			))
		}
	}

	if assessment.DeliveryTasks > 0 {
		assessment.ProgressPercent =
			assessment.CompletedTasks * 100 / assessment.DeliveryTasks
	}
	complete := assessment.DeliveryTasks > 0 &&
		assessment.CompletedTasks == assessment.DeliveryTasks
	if project.State == domain.ProjectCompleted && !complete {
		return assessment, fmt.Errorf(
			"%w: completed project has unfinished delivery tasks",
			ErrInvalidHealthInput,
		)
	}
	if project.Health == domain.HealthReadyForRelease && !complete {
		return assessment, fmt.Errorf(
			"%w: ready-for-release project has unfinished delivery tasks",
			ErrInvalidHealthInput,
		)
	}
	switch {
	case blocked:
		assessment.Health = domain.HealthBlocked
	case offTrack:
		assessment.Health = domain.HealthOffTrack
	case complete:
		assessment.Health = domain.HealthReadyForRelease
	case atRisk:
		assessment.Health = domain.HealthAtRisk
	default:
		assessment.Health = domain.HealthOnTrack
	}
	sort.Slice(assessment.Risks, func(left, right int) bool {
		leftID := riskTaskID(assessment.Risks[left])
		rightID := riskTaskID(assessment.Risks[right])
		if leftID != rightID {
			return leftID < rightID
		}
		return assessment.Risks[left].Code < assessment.Risks[right].Code
	})
	return assessment, nil
}

func healthRisk(code string, taskID int64, detail string) HealthRisk {
	id := taskID
	return HealthRisk{Code: code, TaskID: &id, Detail: detail}
}

func riskTaskID(risk HealthRisk) int64 {
	if risk.TaskID == nil {
		return 0
	}
	return *risk.TaskID
}
