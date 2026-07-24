package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

type ProjectState string

const (
	ProjectInitializing  ProjectState = "initializing"
	ProjectPlanning      ProjectState = "planning"
	ProjectExecuting     ProjectState = "executing"
	ProjectBlocked       ProjectState = "blocked"
	ProjectReleaseReview ProjectState = "release-review"
	ProjectCompleted     ProjectState = "completed"
	ProjectPaused        ProjectState = "paused"
)

type ProjectHealth string

const (
	HealthOnTrack         ProjectHealth = "on-track"
	HealthAtRisk          ProjectHealth = "at-risk"
	HealthOffTrack        ProjectHealth = "off-track"
	HealthBlocked         ProjectHealth = "blocked"
	HealthReadyForRelease ProjectHealth = "ready-for-release"
)

var ErrInvalidProject = errors.New("invalid project")

// Project is the durable aggregate for one sequentially managed repository.
type Project struct {
	ID                  int64
	Repo                string
	ParentIssueNumber   int
	Name                string
	Goal                string
	Scope               string
	State               ProjectState
	PausedFromState     ProjectState
	Health              ProjectHealth
	CurrentTaskID       *int64
	CurrentPlanVersion  int
	ArchitectureVersion int
	ReleaseTarget       string
	ReleaseReadiness    string
	LastManagerReviewAt *time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// NewProject constructs a new aggregate with the v2 lifecycle defaults.
func NewProject(repo, name, goal, scope string) *Project {
	return &Project{
		Repo:   repo,
		Name:   name,
		Goal:   goal,
		Scope:  scope,
		State:  ProjectInitializing,
		Health: HealthOnTrack,
	}
}

func (state ProjectState) Valid() bool {
	switch state {
	case ProjectInitializing,
		ProjectPlanning,
		ProjectExecuting,
		ProjectBlocked,
		ProjectReleaseReview,
		ProjectCompleted,
		ProjectPaused:
		return true
	default:
		return false
	}
}

func (health ProjectHealth) Valid() bool {
	switch health {
	case HealthOnTrack,
		HealthAtRisk,
		HealthOffTrack,
		HealthBlocked,
		HealthReadyForRelease:
		return true
	default:
		return false
	}
}

// Validate checks aggregate invariants but deliberately does not enforce
// lifecycle transitions; the workflow state machine owns those in Milestone 3.
func (project *Project) Validate() error {
	if project == nil {
		return fmt.Errorf("%w: project is nil", ErrInvalidProject)
	}
	switch {
	case strings.TrimSpace(project.Repo) == "":
		return fmt.Errorf("%w: repository is required", ErrInvalidProject)
	case strings.TrimSpace(project.Name) == "":
		return fmt.Errorf("%w: name is required", ErrInvalidProject)
	case strings.TrimSpace(project.Goal) == "":
		return fmt.Errorf("%w: goal is required", ErrInvalidProject)
	case !project.State.Valid():
		return fmt.Errorf("%w: unknown state %q", ErrInvalidProject, project.State)
	case project.PausedFromState != "" &&
		(!project.PausedFromState.Valid() || project.PausedFromState == ProjectPaused):
		return fmt.Errorf(
			"%w: invalid pre-pause state %q",
			ErrInvalidProject,
			project.PausedFromState,
		)
	case project.State == ProjectPaused && project.PausedFromState == "":
		return fmt.Errorf("%w: paused project requires its prior state", ErrInvalidProject)
	case project.State != ProjectPaused && project.PausedFromState != "":
		return fmt.Errorf("%w: active project cannot retain a pre-pause state", ErrInvalidProject)
	case !project.Health.Valid():
		return fmt.Errorf("%w: unknown health %q", ErrInvalidProject, project.Health)
	case project.ParentIssueNumber < 0:
		return fmt.Errorf("%w: parent issue number cannot be negative", ErrInvalidProject)
	case project.CurrentTaskID != nil && *project.CurrentTaskID <= 0:
		return fmt.Errorf("%w: current task ID must be positive", ErrInvalidProject)
	case project.CurrentPlanVersion < 0:
		return fmt.Errorf("%w: current plan version cannot be negative", ErrInvalidProject)
	case project.ArchitectureVersion < 0:
		return fmt.Errorf("%w: architecture version cannot be negative", ErrInvalidProject)
	default:
		return nil
	}
}
