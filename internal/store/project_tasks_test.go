package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

func createTestProject(t *testing.T, s *Store, repo string) *domain.Project {
	t.Helper()
	project, err := s.CreateProject(domain.NewProject(repo, repo, "Ship the goal", ""))
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	return project
}

func TestProjectTaskAppendCRUDAndNullableFields(t *testing.T) {
	s := openTestStore(t)
	project := createTestProject(t, s, "owner/repo")

	first := domain.NewTask(project.ID, "First", "First goal")
	createdFirst, err := s.CreateProjectTask(first)
	if err != nil {
		t.Fatalf("CreateProjectTask first: %v", err)
	}
	second := domain.NewTask(project.ID, "Second", "Second goal")
	createdSecond, err := s.CreateProjectTask(second)
	if err != nil {
		t.Fatalf("CreateProjectTask second: %v", err)
	}
	if createdFirst.Sequence != 1 || createdSecond.Sequence != 2 {
		t.Errorf("appended sequences = %d, %d", createdFirst.Sequence, createdSecond.Sequence)
	}

	discoveryID := int64(77)
	createdFirst.IssueNumber = 42
	createdFirst.Title = "First updated"
	createdFirst.Goal = "Updated goal"
	createdFirst.Status = domain.TaskDeveloping
	createdFirst.Priority = 9
	createdFirst.TaskType = "feature"
	createdFirst.Source = "manager"
	createdFirst.SourceDiscoveryID = &discoveryID
	createdFirst.BlocksRelease = true
	createdFirst.SelectedReason = "highest priority"
	createdFirst.BranchName = "madar/issue-42"
	createdFirst.PRNumber = 55
	createdFirst.DependencyState = "ready"
	updated, err := s.UpdateProjectTask(createdFirst)
	if err != nil {
		t.Fatalf("UpdateProjectTask: %v", err)
	}
	if updated.IssueNumber != 42 ||
		updated.Status != domain.TaskDeveloping ||
		updated.Priority != 9 ||
		updated.TaskType != "feature" ||
		updated.Source != "manager" ||
		updated.SourceDiscoveryID == nil ||
		*updated.SourceDiscoveryID != discoveryID ||
		!updated.BlocksRelease ||
		updated.SelectedReason != "highest priority" ||
		updated.BranchName != "madar/issue-42" ||
		updated.PRNumber != 55 ||
		updated.DependencyState != "ready" {
		t.Errorf("updated task = %#v", updated)
	}

	byID, err := s.GetProjectTaskByID(updated.ID)
	if err != nil || byID == nil {
		t.Fatalf("GetProjectTaskByID: task=%#v error=%v", byID, err)
	}
	byIssue, err := s.GetProjectTaskByIssue(project.ID, 42)
	if err != nil || byIssue == nil || byIssue.ID != updated.ID {
		t.Fatalf("GetProjectTaskByIssue: task=%#v error=%v", byIssue, err)
	}

	updated.SourceDiscoveryID = nil
	updated, err = s.UpdateProjectTask(updated)
	if err != nil {
		t.Fatal(err)
	}
	if updated.SourceDiscoveryID != nil {
		t.Errorf("cleared discovery ID = %v", updated.SourceDiscoveryID)
	}

	tasks, err := s.ListProjectTasks(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 || tasks[0].ID != updated.ID || tasks[1].ID != createdSecond.ID {
		t.Errorf("ordered tasks = %#v", tasks)
	}
}

func TestProjectTaskCreateAndUpdateConflicts(t *testing.T) {
	s := openTestStore(t)
	project := createTestProject(t, s, "owner/repo")
	first := domain.NewTask(project.ID, "First", "Goal")
	first.IssueNumber = 10
	first.Sequence = 3
	createdFirst, err := s.CreateProjectTask(first)
	if err != nil {
		t.Fatal(err)
	}

	duplicateIssue := domain.NewTask(project.ID, "Duplicate issue", "Goal")
	duplicateIssue.IssueNumber = 10
	if _, err := s.CreateProjectTask(duplicateIssue); !errors.Is(err, ErrProjectTaskAlreadyExists) {
		t.Errorf("duplicate issue error = %v", err)
	}
	duplicateSequence := domain.NewTask(project.ID, "Duplicate sequence", "Goal")
	duplicateSequence.Sequence = 3
	if _, err := s.CreateProjectTask(duplicateSequence); !errors.Is(err, ErrProjectTaskPositionTaken) {
		t.Errorf("duplicate sequence error = %v", err)
	}

	second := domain.NewTask(project.ID, "Second", "Goal")
	second.IssueNumber = 11
	createdSecond, err := s.CreateProjectTask(second)
	if err != nil {
		t.Fatal(err)
	}
	createdSecond.IssueNumber = 10
	if _, err := s.UpdateProjectTask(createdSecond); !errors.Is(err, ErrProjectTaskAlreadyExists) {
		t.Errorf("duplicate update issue error = %v", err)
	}
	createdSecond.IssueNumber = 11
	createdSecond.Sequence = createdFirst.Sequence
	if _, err := s.UpdateProjectTask(createdSecond); !errors.Is(err, ErrProjectTaskPositionTaken) {
		t.Errorf("duplicate update sequence error = %v", err)
	}

	missingProject := domain.NewTask(9999, "Missing project", "Goal")
	if _, err := s.CreateProjectTask(missingProject); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("missing project error = %v", err)
	}
	invalid := domain.NewTask(project.ID, "", "Goal")
	if _, err := s.CreateProjectTask(invalid); !errors.Is(err, domain.ErrInvalidTask) {
		t.Errorf("invalid task error = %v", err)
	}
	missingTask := domain.NewTask(project.ID, "Missing", "Goal")
	missingTask.ID = 9999
	missingTask.Sequence = 1
	if _, err := s.UpdateProjectTask(missingTask); !errors.Is(err, ErrProjectTaskNotFound) {
		t.Errorf("missing task error = %v", err)
	}
}

func TestReorderProjectTasksAndRollbackInvalidOrders(t *testing.T) {
	s := openTestStore(t)
	project := createTestProject(t, s, "owner/repo")
	var tasks []*domain.Task
	for _, title := range []string{"one", "two", "three"} {
		task, err := s.CreateProjectTask(domain.NewTask(project.ID, title, "Goal"))
		if err != nil {
			t.Fatal(err)
		}
		tasks = append(tasks, task)
	}

	reordered, err := s.ReorderProjectTasks(project.ID, []int64{
		tasks[2].ID,
		tasks[0].ID,
		tasks[1].ID,
	})
	if err != nil {
		t.Fatalf("ReorderProjectTasks: %v", err)
	}
	for index, task := range reordered {
		if task.Sequence != index+1 {
			t.Errorf("task %d sequence = %d", task.ID, task.Sequence)
		}
	}
	wantIDs := []int64{tasks[2].ID, tasks[0].ID, tasks[1].ID}
	assertProjectTaskOrder(t, reordered, wantIDs)

	otherProject := createTestProject(t, s, "owner/other")
	otherTask, err := s.CreateProjectTask(domain.NewTask(otherProject.ID, "other", "Goal"))
	if err != nil {
		t.Fatal(err)
	}
	invalidOrders := [][]int64{
		{wantIDs[0], wantIDs[1]},
		{wantIDs[0], wantIDs[0], wantIDs[2]},
		{wantIDs[0], wantIDs[1], otherTask.ID},
	}
	for _, order := range invalidOrders {
		if _, err := s.ReorderProjectTasks(project.ID, order); !errors.Is(err, ErrInvalidProjectTaskOrder) {
			t.Errorf("invalid order %v error = %v", order, err)
		}
		current, err := s.ListProjectTasks(project.ID)
		if err != nil {
			t.Fatal(err)
		}
		assertProjectTaskOrder(t, current, wantIDs)
	}

	emptyProject := createTestProject(t, s, "owner/empty")
	empty, err := s.ReorderProjectTasks(emptyProject.ID, nil)
	if err != nil || len(empty) != 0 {
		t.Errorf("empty reorder = %#v, error=%v", empty, err)
	}
}

func TestProjectTaskCurrentSelectionAndCascade(t *testing.T) {
	s := openTestStore(t)
	firstProject := createTestProject(t, s, "owner/first")
	firstTask, err := s.CreateProjectTask(domain.NewTask(firstProject.ID, "first", "Goal"))
	if err != nil {
		t.Fatal(err)
	}
	secondProject := createTestProject(t, s, "owner/second")
	secondTask, err := s.CreateProjectTask(domain.NewTask(secondProject.ID, "second", "Goal"))
	if err != nil {
		t.Fatal(err)
	}

	firstProject.CurrentTaskID = &firstTask.ID
	if _, err := s.UpdateProject(firstProject); err != nil {
		t.Fatalf("select own task: %v", err)
	}
	firstProject.CurrentTaskID = &secondTask.ID
	if _, err := s.UpdateProject(firstProject); err == nil {
		t.Fatal("selected a task from another project")
	}
	if _, err := s.db.Exec(`DELETE FROM project_tasks WHERE id = ?`, firstTask.ID); err == nil {
		t.Fatal("deleted the selected current task")
	}
	if _, err := s.db.Exec(
		`UPDATE project_tasks SET project_id = ? WHERE id = ?`,
		secondProject.ID,
		firstTask.ID,
	); err == nil {
		t.Fatal("moved the selected current task to another project")
	}

	firstProject.CurrentTaskID = nil
	if _, err := s.UpdateProject(firstProject); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM project_tasks WHERE id = ?`, firstTask.ID); err != nil {
		t.Fatalf("delete unselected task: %v", err)
	}

	if _, err := s.UpsertTask("owner/second", 99, StateInProgress, "legacy-session"); err != nil {
		t.Fatal(err)
	}
	secondProject.CurrentTaskID = &secondTask.ID
	if _, err := s.UpdateProject(secondProject); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM projects WHERE id = ?`, secondProject.ID); err != nil {
		t.Fatalf("delete project: %v", err)
	}
	if task, err := s.GetProjectTaskByID(secondTask.ID); err != nil || task != nil {
		t.Errorf("cascaded project task = %#v, error=%v", task, err)
	}
	if legacy, err := s.GetTask("owner/second", 99); err != nil || legacy == nil {
		t.Errorf("legacy task after project cascade = %#v, error=%v", legacy, err)
	}
}

func TestProjectTaskConstraintsAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "madar.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	project := createTestProject(t, s, "owner/repo")
	task := domain.NewTask(project.ID, "Persisted", "Goal")
	task.IssueNumber = 12
	created, err := s.CreateProjectTask(task)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`
		INSERT INTO project_tasks (
			project_id, title, goal, status, priority, sequence, created_at, updated_at
		) VALUES (?, 'bad', 'goal', 'invalid', 0, 20, ?, ?)
	`, project.ID, time.Now().UTC(), time.Now().UTC()); err == nil {
		t.Fatal("database accepted invalid task status")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err := reopened.GetProjectTaskByIssue(project.ID, 12)
	if err != nil || got == nil || got.ID != created.ID || got.Sequence != 1 {
		t.Errorf("reopened task = %#v, error=%v", got, err)
	}
}

func TestGetProjectTaskMissing(t *testing.T) {
	s := openTestStore(t)
	task, err := s.GetProjectTaskByID(404)
	if err != nil || task != nil {
		t.Errorf("GetProjectTaskByID missing = %#v, error=%v", task, err)
	}
	project := createTestProject(t, s, "owner/repo")
	task, err = s.GetProjectTaskByIssue(project.ID, 0)
	if err != nil || task != nil {
		t.Errorf("GetProjectTaskByIssue zero = %#v, error=%v", task, err)
	}
	if _, err := s.ListProjectTasks(404); !errors.Is(err, ErrProjectNotFound) {
		t.Errorf("ListProjectTasks missing project error = %v", err)
	}
}

func assertProjectTaskOrder(t *testing.T, tasks []*domain.Task, wantIDs []int64) {
	t.Helper()
	if len(tasks) != len(wantIDs) {
		t.Fatalf("task count = %d, want %d", len(tasks), len(wantIDs))
	}
	for index, wantID := range wantIDs {
		if tasks[index].ID != wantID || tasks[index].Sequence != index+1 {
			t.Errorf(
				"task[%d] = ID %d sequence %d, want ID %d sequence %d",
				index,
				tasks[index].ID,
				tasks[index].Sequence,
				wantID,
				index+1,
			)
		}
	}
}
