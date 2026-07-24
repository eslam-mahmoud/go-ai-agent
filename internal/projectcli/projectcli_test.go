package projectcli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	"github.com/eslam-mahmoud/go-ai-agent/internal/projectfiles"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
)

func TestCreateListAndShowProject(t *testing.T) {
	configPath, dbPath := writeTestConfig(t)
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	err := Run(withConfig(configPath,
		"create",
		"--repo", " owner/repo ",
		"--name", " Madar ",
		"--goal", " Ship v2 ",
		"--scope", " Sequential delivery ",
		"--release-target", " v2.0.0 ",
	), &stdout, &stderr)
	if err != nil {
		t.Fatalf("create: %v\nstderr: %s", err, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "created project 1: Madar (owner/repo)") {
		t.Fatalf("create output = %q", got)
	}

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	project, err := s.GetProjectByRepo("owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	if project == nil ||
		project.Name != "Madar" ||
		project.Goal != "Ship v2" ||
		project.Scope != "Sequential delivery" ||
		project.ReleaseTarget != "v2.0.0" ||
		project.State != domain.ProjectInitializing ||
		project.Health != domain.HealthOnTrack {
		t.Fatalf("created project = %#v", project)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	stdout.Reset()
	stderr.Reset()
	if err := Run(withConfig(configPath, "list"), &stdout, &stderr); err != nil {
		t.Fatalf("list: %v\nstderr: %s", err, stderr.String())
	}
	listOutput := stdout.String()
	for _, want := range []string{"ID", "REPOSITORY", "STATE", "HEALTH", "owner/repo", "initializing", "on-track", "Madar"} {
		if !strings.Contains(listOutput, want) {
			t.Errorf("list output missing %q:\n%s", want, listOutput)
		}
	}

	stdout.Reset()
	stderr.Reset()
	if err := Run(withConfig(configPath, "show", "--repo", "owner/repo"), &stdout, &stderr); err != nil {
		t.Fatalf("show: %v\nstderr: %s", err, stderr.String())
	}
	showOutput := stdout.String()
	for _, want := range []string{
		"repository        : owner/repo",
		"name              : Madar",
		"goal              : Ship v2",
		"scope             : Sequential delivery",
		"state             : initializing",
		"health            : on-track",
		"release target    : v2.0.0",
		"backlog:",
		"SEQ",
	} {
		if !strings.Contains(showOutput, want) {
			t.Errorf("show output missing %q:\n%s", want, showOutput)
		}
	}
}

func TestAddAndListTasksInSequenceOrder(t *testing.T) {
	configPath, dbPath := writeTestConfig(t)
	mustRun(t, configPath,
		"create",
		"--repo", "owner/repo",
		"--name", "Madar",
		"--goal", "Ship v2",
	)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := Run(withConfig(configPath,
		"add-task",
		"--repo", "owner/repo",
		"--title", "First task",
		"--goal", "Complete the first task",
		"--issue", "101",
		"--priority", "20",
		"--type", "feature",
		"--source", "plan",
		"--blocks-release",
	), &stdout, &stderr)
	if err != nil {
		t.Fatalf("add first task: %v\nstderr: %s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "at sequence 1") {
		t.Fatalf("first task output = %q", stdout.String())
	}
	mustRun(t, configPath,
		"add-task",
		"--repo", "owner/repo",
		"--title", "Second task",
		"--goal", "Complete the second task",
	)

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	project, err := s.GetProjectByRepo("owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := s.ListProjectTasks(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 || tasks[0].Sequence != 1 || tasks[1].Sequence != 2 {
		t.Fatalf("tasks = %#v", tasks)
	}
	if tasks[0].IssueNumber != 101 ||
		tasks[0].Priority != 20 ||
		tasks[0].TaskType != "feature" ||
		tasks[0].Source != "plan" ||
		!tasks[0].BlocksRelease {
		t.Errorf("first task options = %#v", tasks[0])
	}
	if tasks[1].Source != "cli" {
		t.Errorf("second task source = %q, want cli", tasks[1].Source)
	}

	stdout.Reset()
	stderr.Reset()
	if err := Run(withConfig(configPath, "list-tasks", "--repo", "owner/repo"), &stdout, &stderr); err != nil {
		t.Fatalf("list tasks: %v\nstderr: %s", err, stderr.String())
	}
	output := stdout.String()
	firstIndex := strings.Index(output, "First task")
	secondIndex := strings.Index(output, "Second task")
	if firstIndex < 0 || secondIndex < 0 || firstIndex >= secondIndex {
		t.Fatalf("task output is not sequence ordered:\n%s", output)
	}
	for _, want := range []string{"101", "proposed", "20", "First task"} {
		if !strings.Contains(output, want) {
			t.Errorf("task output missing %q:\n%s", want, output)
		}
	}
}

func TestProjectCommandErrors(t *testing.T) {
	configPath, _ := writeTestConfig(t)
	mustRun(t, configPath,
		"create",
		"--repo", "owner/repo",
		"--name", "Madar",
		"--goal", "Ship v2",
	)

	tests := []struct {
		name   string
		args   []string
		target error
	}{
		{
			name:   "missing subcommand",
			args:   nil,
			target: ErrUsage,
		},
		{
			name:   "unknown subcommand",
			args:   []string{"remove"},
			target: ErrUsage,
		},
		{
			name: "missing required value",
			args: withConfig(configPath,
				"create",
				"--repo", "owner/other",
				"--name", "Other",
			),
			target: ErrUsage,
		},
		{
			name: "malformed repository",
			args: withConfig(configPath,
				"create",
				"--repo", "owner/repo/extra",
				"--name", "Other",
				"--goal", "Other",
			),
			target: ErrUsage,
		},
		{
			name: "repository path traversal",
			args: withConfig(configPath,
				"create",
				"--repo", "../repo",
				"--name", "Other",
				"--goal", "Other",
			),
			target: ErrUsage,
		},
		{
			name: "unexpected argument",
			args: withConfig(configPath,
				"list",
				"extra",
			),
			target: ErrUsage,
		},
		{
			name: "duplicate project",
			args: withConfig(configPath,
				"create",
				"--repo", "owner/repo",
				"--name", "Duplicate",
				"--goal", "Duplicate",
			),
			target: store.ErrProjectAlreadyExists,
		},
		{
			name: "missing project",
			args: withConfig(configPath,
				"show",
				"--repo", "owner/missing",
			),
			target: store.ErrProjectNotFound,
		},
		{
			name: "invalid task priority",
			args: withConfig(configPath,
				"add-task",
				"--repo", "owner/repo",
				"--title", "Invalid",
				"--goal", "Invalid priority",
				"--priority", "-1",
			),
			target: domain.ErrInvalidTask,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := Run(tc.args, &bytes.Buffer{}, &bytes.Buffer{})
			if !errors.Is(err, tc.target) {
				t.Fatalf("error = %v, want errors.Is(_, %v)", err, tc.target)
			}
		})
	}
}

func TestSyncFilesWritesConfiguredWorkspaceSnapshot(t *testing.T) {
	configPath, workspaceDir := writeSyncTestConfig(t)
	t.Setenv("GITHUB_TOKEN", "")
	mustRun(t, configPath,
		"create",
		"--repo", "owner/repo",
		"--name", "Madar",
		"--goal", "Ship v2",
		"--scope", "Project files",
	)
	mustRun(t, configPath,
		"add-task",
		"--repo", "owner/repo",
		"--title", "Generate snapshots",
		"--goal", "Write deterministic files",
	)

	repoWorkspace := filepath.Join(workspaceDir, "owner", "repo")
	if err := os.MkdirAll(repoWorkspace, 0o755); err != nil {
		t.Fatal(err)
	}
	output := mustRun(t, configPath, "sync-files", "--repo", "owner/repo")
	projectPath := filepath.Join(repoWorkspace, projectfiles.DirectoryName, projectfiles.ProjectFileName)
	planPath := filepath.Join(repoWorkspace, projectfiles.DirectoryName, projectfiles.PlanFileName)
	for _, path := range []string{projectPath, planPath} {
		if !strings.Contains(output, path) {
			t.Errorf("sync output missing %q: %s", path, output)
		}
	}
	projectYAML, err := os.ReadFile(projectPath)
	if err != nil {
		t.Fatal(err)
	}
	planMarkdown, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"repository: owner/repo", "title: Generate snapshots"} {
		if !strings.Contains(string(projectYAML), want) {
			t.Errorf("project YAML missing %q:\n%s", want, projectYAML)
		}
	}
	for _, want := range []string{"# Project Plan: Madar", "Generate snapshots", "Write deterministic files"} {
		if !strings.Contains(string(planMarkdown), want) {
			t.Errorf("plan Markdown missing %q:\n%s", want, planMarkdown)
		}
	}

	mustRun(t, configPath, "sync-files", "--repo", "owner/repo")
	projectYAMLAgain, err := os.ReadFile(projectPath)
	if err != nil {
		t.Fatal(err)
	}
	planMarkdownAgain, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(projectYAML, projectYAMLAgain) ||
		!bytes.Equal(planMarkdown, planMarkdownAgain) {
		t.Fatal("repeated sync changed generated bytes")
	}
}

func TestSyncFilesRejectsMissingWorkspace(t *testing.T) {
	configPath, _ := writeSyncTestConfig(t)
	mustRun(t, configPath,
		"create",
		"--repo", "owner/repo",
		"--name", "Madar",
		"--goal", "Ship v2",
	)
	err := Run(
		withConfig(configPath, "sync-files", "--repo", "owner/repo"),
		&bytes.Buffer{},
		&bytes.Buffer{},
	)
	if err == nil || !strings.Contains(err.Error(), "inspect project workspace") {
		t.Fatalf("error = %v, want missing workspace error", err)
	}
}

func TestProjectHelp(t *testing.T) {
	var stdout bytes.Buffer
	if err := Run([]string{"help"}, &stdout, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{
		"project create",
		"project list",
		"project show",
		"project add-task",
		"project list-tasks",
		"project sync-files",
	} {
		if !strings.Contains(stdout.String(), command) {
			t.Errorf("help output missing %q:\n%s", command, stdout.String())
		}
	}
}

func writeSyncTestConfig(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "madar.db")
	workspaceDir := filepath.Join(dir, "workspaces")
	configPath := filepath.Join(dir, "config.yaml")
	content := fmt.Sprintf(
		"db_path: %q\nworkspace_dir: %q\n",
		dbPath,
		workspaceDir,
	)
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath, workspaceDir
}

func writeTestConfig(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "madar.db")
	configPath := filepath.Join(dir, "config.yaml")
	content := fmt.Sprintf("db_path: %q\n", dbPath)
	if err := os.WriteFile(configPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath, dbPath
}

func withConfig(configPath string, args ...string) []string {
	if len(args) == 0 {
		return nil
	}
	result := []string{args[0]}
	result = append(result, args[1:]...)
	result = append(result, "--config", configPath, "--env", "")
	return result
}

func mustRun(t *testing.T, configPath string, args ...string) string {
	t.Helper()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := Run(withConfig(configPath, args...), &stdout, &stderr); err != nil {
		t.Fatalf("%s: %v\nstderr: %s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.String()
}
