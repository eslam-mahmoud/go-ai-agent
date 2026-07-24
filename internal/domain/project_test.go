package domain

import (
	"errors"
	"testing"
)

func TestNewProjectDefaultsAndValidation(t *testing.T) {
	project := NewProject("owner/repo", "Madar", "Ship v2", "Sequential automation")
	if project.State != ProjectInitializing || project.Health != HealthOnTrack {
		t.Errorf("defaults = state %q health %q", project.State, project.Health)
	}
	if err := project.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestProjectEnums(t *testing.T) {
	states := []ProjectState{
		ProjectInitializing,
		ProjectPlanning,
		ProjectExecuting,
		ProjectBlocked,
		ProjectReleaseReview,
		ProjectCompleted,
		ProjectPaused,
	}
	for _, state := range states {
		if !state.Valid() {
			t.Errorf("state %q is invalid", state)
		}
	}
	if ProjectState("unknown").Valid() {
		t.Error("unknown project state is valid")
	}

	healthValues := []ProjectHealth{
		HealthOnTrack,
		HealthAtRisk,
		HealthOffTrack,
		HealthBlocked,
		HealthReadyForRelease,
	}
	for _, health := range healthValues {
		if !health.Valid() {
			t.Errorf("health %q is invalid", health)
		}
	}
	if ProjectHealth("unknown").Valid() {
		t.Error("unknown project health is valid")
	}
}

func TestProjectValidationRejectsInvalidRecords(t *testing.T) {
	valid := *NewProject("owner/repo", "Madar", "Ship v2", "scope")
	zero := int64(0)
	cases := []struct {
		name   string
		mutate func(*Project)
	}{
		{"nil repository", func(p *Project) { p.Repo = " " }},
		{"missing name", func(p *Project) { p.Name = "" }},
		{"missing goal", func(p *Project) { p.Goal = "" }},
		{"invalid state", func(p *Project) { p.State = "invalid" }},
		{"paused without prior state", func(p *Project) { p.State = ProjectPaused }},
		{"invalid prior state", func(p *Project) { p.PausedFromState = "invalid" }},
		{"paused prior state", func(p *Project) { p.PausedFromState = ProjectPaused }},
		{"active with prior state", func(p *Project) { p.PausedFromState = ProjectExecuting }},
		{"invalid health", func(p *Project) { p.Health = "invalid" }},
		{"negative parent issue", func(p *Project) { p.ParentIssueNumber = -1 }},
		{"nonpositive current task", func(p *Project) { p.CurrentTaskID = &zero }},
		{"negative plan version", func(p *Project) { p.CurrentPlanVersion = -1 }},
		{"negative architecture version", func(p *Project) { p.ArchitectureVersion = -1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			project := valid
			tc.mutate(&project)
			if err := project.Validate(); !errors.Is(err, ErrInvalidProject) {
				t.Errorf("Validate error = %v", err)
			}
		})
	}
	if err := (*Project)(nil).Validate(); !errors.Is(err, ErrInvalidProject) {
		t.Errorf("nil Validate error = %v", err)
	}
}

func TestPausedProjectValidation(t *testing.T) {
	project := NewProject("owner/repo", "Madar", "Ship v2", "scope")
	project.State = ProjectPaused
	project.PausedFromState = ProjectExecuting
	if err := project.Validate(); err != nil {
		t.Fatalf("valid paused project: %v", err)
	}
}
