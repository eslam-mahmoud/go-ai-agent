package project

import (
	"errors"
	"testing"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

func TestAssessHealthStatesAndProgress(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		mutate       func(*domain.Project, []*domain.Task, *[]*domain.Execution)
		wantHealth   domain.ProjectHealth
		wantProgress int
		wantRisk     string
	}{
		{
			name:         "healthy active delivery",
			wantHealth:   domain.HealthOnTrack,
			wantProgress: 50,
		},
		{
			name: "waiting input is at risk",
			mutate: func(_ *domain.Project, tasks []*domain.Task, _ *[]*domain.Execution) {
				tasks[1].Status = domain.TaskWaitingInput
			},
			wantHealth:   domain.HealthAtRisk,
			wantProgress: 50,
			wantRisk:     "waiting-input",
		},
		{
			name: "latest interrupted execution is at risk",
			mutate: func(_ *domain.Project, _ []*domain.Task, executions *[]*domain.Execution) {
				*executions = append(*executions, healthExecution(11, domain.ExecutionInterrupted))
			},
			wantHealth:   domain.HealthAtRisk,
			wantProgress: 50,
			wantRisk:     "latest-execution-interrupted",
		},
		{
			name: "blocked task blocks project",
			mutate: func(project *domain.Project, tasks []*domain.Task, _ *[]*domain.Execution) {
				tasks[1].Status = domain.TaskBlocked
				project.State = domain.ProjectBlocked
			},
			wantHealth:   domain.HealthBlocked,
			wantProgress: 50,
			wantRisk:     "task-blocked",
		},
		{
			name: "cancelled release blocker is off track",
			mutate: func(project *domain.Project, tasks []*domain.Task, _ *[]*domain.Execution) {
				tasks[1].Status = domain.TaskCancelled
				tasks[1].BlocksRelease = true
				project.CurrentTaskID = nil
			},
			wantHealth:   domain.HealthOffTrack,
			wantProgress: 100,
			wantRisk:     "release-blocker-removed",
		},
		{
			name: "all delivery tasks complete",
			mutate: func(project *domain.Project, tasks []*domain.Task, _ *[]*domain.Execution) {
				tasks[1].Status = domain.TaskCompleted
				project.CurrentTaskID = nil
				project.State = domain.ProjectReleaseReview
			},
			wantHealth:   domain.HealthReadyForRelease,
			wantProgress: 100,
		},
		{
			name: "empty project",
			mutate: func(project *domain.Project, tasks []*domain.Task, _ *[]*domain.Execution) {
				project.CurrentTaskID = nil
				tasks[0] = nil
				tasks[1] = nil
			},
			wantHealth:   domain.HealthOnTrack,
			wantProgress: 0,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			project, tasks := healthFixture()
			executions := []*domain.Execution{}
			if test.mutate != nil {
				test.mutate(project, tasks, &executions)
			}
			filtered := tasks[:0]
			for _, task := range tasks {
				if task != nil {
					filtered = append(filtered, task)
				}
			}
			got, err := AssessHealth(project, filtered, executions)
			if err != nil {
				t.Fatalf("AssessHealth: %v", err)
			}
			if got.Health != test.wantHealth || got.ProgressPercent != test.wantProgress {
				t.Fatalf("assessment = %#v", got)
			}
			if test.wantRisk != "" && !hasHealthRisk(got.Risks, test.wantRisk) {
				t.Fatalf("risks = %#v, want %q", got.Risks, test.wantRisk)
			}
		})
	}
}

func TestAssessHealthRejectsInconsistentDurableState(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*domain.Project, []*domain.Task, *[]*domain.Execution)
	}{
		{"nil project", func(project *domain.Project, _ []*domain.Task, _ *[]*domain.Execution) {
			*project = domain.Project{}
		}},
		{"cross project task", func(_ *domain.Project, tasks []*domain.Task, _ *[]*domain.Execution) {
			tasks[0].ProjectID++
		}},
		{"duplicate task", func(_ *domain.Project, tasks []*domain.Task, _ *[]*domain.Execution) {
			tasks[1].ID = tasks[0].ID
		}},
		{"multiple active tasks", func(_ *domain.Project, tasks []*domain.Task, _ *[]*domain.Execution) {
			tasks[0].Status = domain.TaskReviewing
		}},
		{"missing current task", func(project *domain.Project, _ []*domain.Task, _ *[]*domain.Execution) {
			value := int64(404)
			project.CurrentTaskID = &value
		}},
		{"inactive current task", func(_ *domain.Project, tasks []*domain.Task, _ *[]*domain.Execution) {
			tasks[1].Status = domain.TaskCompleted
		}},
		{"execution ownership", func(_ *domain.Project, _ []*domain.Task, executions *[]*domain.Execution) {
			execution := healthExecution(11, domain.ExecutionCompleted)
			execution.ProjectID++
			*executions = append(*executions, execution)
		}},
		{"ready health unfinished", func(project *domain.Project, _ []*domain.Task, _ *[]*domain.Execution) {
			project.Health = domain.HealthReadyForRelease
		}},
		{"completed project unfinished", func(project *domain.Project, _ []*domain.Task, _ *[]*domain.Execution) {
			project.State = domain.ProjectCompleted
		}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			project, tasks := healthFixture()
			executions := []*domain.Execution{}
			test.mutate(project, tasks, &executions)
			if _, err := AssessHealth(project, tasks, executions); !errors.Is(err, ErrInvalidHealthInput) {
				t.Fatalf("AssessHealth error = %v", err)
			}
		})
	}
	if _, err := AssessHealth(nil, nil, nil); !errors.Is(err, ErrInvalidHealthInput) {
		t.Fatalf("nil project error = %v", err)
	}
}

func healthFixture() (*domain.Project, []*domain.Task) {
	project := domain.NewProject("owner/repo", "Madar", "Ship v2", "Sequential delivery")
	project.ID = 7
	project.State = domain.ProjectExecuting
	first := domain.NewTask(project.ID, "Foundation", "Build the foundation")
	first.ID = 10
	first.Sequence = 1
	first.Status = domain.TaskCompleted
	second := domain.NewTask(project.ID, "Manager", "Build manager context")
	second.ID = 11
	second.Sequence = 2
	second.Status = domain.TaskReviewing
	project.CurrentTaskID = int64Pointer(second.ID)
	return project, []*domain.Task{first, second}
}

func healthExecution(taskID int64, status domain.ExecutionStatus) *domain.Execution {
	started := time.Date(2026, 7, 24, 10, 0, 0, 0, time.UTC)
	completed := started.Add(time.Minute)
	execution := domain.NewExecution(7, taskID, "developer", "codex", "model", 1)
	execution.ID = int64(status[0]) + taskID
	execution.Status = status
	execution.StartedAt = &started
	if status != domain.ExecutionRunning && status != domain.ExecutionPending {
		execution.CompletedAt = &completed
	}
	return execution
}

func hasHealthRisk(risks []HealthRisk, code string) bool {
	for _, risk := range risks {
		if risk.Code == code {
			return true
		}
	}
	return false
}

func int64Pointer(value int64) *int64 {
	return &value
}
