package store

import (
	"errors"
	"testing"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

func TestLoadProjectAggregateAndAtomicTransition(t *testing.T) {
	s := openTestStore(t)
	project, err := s.CreateProject(domain.NewProject("owner/repo", "Project", "Goal", ""))
	if err != nil {
		t.Fatal(err)
	}
	task := domain.NewTask(project.ID, "Task", "Goal")
	task.Status = domain.TaskQueued
	task, err = s.CreateProjectTask(task)
	if err != nil {
		t.Fatal(err)
	}
	taskID := task.ID
	if err := s.ApplyProjectTaskTransition(ProjectTaskTransitionUpdate{
		ProjectID:      project.ID,
		TaskID:         task.ID,
		ExpectedStatus: domain.TaskQueued,
		NewStatus:      domain.TaskSelected,
		ProjectState:   domain.ProjectExecuting,
		SetCurrentTask: true,
		CurrentTaskID:  &taskID,
	}); err != nil {
		t.Fatal(err)
	}
	aggregate, err := s.LoadProjectAggregate(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if aggregate.Project.State != domain.ProjectExecuting ||
		aggregate.Project.CurrentTaskID == nil ||
		len(aggregate.Tasks) != 1 ||
		aggregate.Tasks[0].Status != domain.TaskSelected ||
		aggregate.LatestManagerReview != nil {
		t.Fatalf("aggregate = %#v", aggregate)
	}
}

func TestApplyProjectTaskTransitionRejectsStaleStatusAtomically(t *testing.T) {
	s := openTestStore(t)
	project, err := s.CreateProject(domain.NewProject("owner/repo", "Project", "Goal", ""))
	if err != nil {
		t.Fatal(err)
	}
	task := domain.NewTask(project.ID, "Task", "Goal")
	task.Status = domain.TaskQueued
	task, err = s.CreateProjectTask(task)
	if err != nil {
		t.Fatal(err)
	}
	err = s.ApplyProjectTaskTransition(ProjectTaskTransitionUpdate{
		ProjectID:      project.ID,
		TaskID:         task.ID,
		ExpectedStatus: domain.TaskPlanning,
		NewStatus:      domain.TaskDeveloping,
		ProjectState:   domain.ProjectExecuting,
		SetCurrentTask: true,
		CurrentTaskID:  &task.ID,
	})
	if !errors.Is(err, ErrProjectTaskTransitionConflict) {
		t.Fatalf("error = %v, want conflict", err)
	}
	storedTask, _ := s.GetProjectTaskByID(task.ID)
	storedProject, _ := s.GetProjectByID(project.ID)
	if storedTask.Status != domain.TaskQueued ||
		storedProject.State != domain.ProjectInitializing ||
		storedProject.CurrentTaskID != nil {
		t.Fatalf("stale transition partially applied: task=%#v project=%#v", storedTask, storedProject)
	}
}
