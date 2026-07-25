package projectcli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/eslam-mahmoud/go-ai-agent/internal/project"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	githubclient "github.com/eslam-mahmoud/go-ai-agent/internal/github"
	"github.com/eslam-mahmoud/go-ai-agent/internal/projectfiles"
	"github.com/eslam-mahmoud/go-ai-agent/internal/projectissue"
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
		"--parent-issue", "123",
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
		project.ParentIssueNumber != 123 ||
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

func TestSyncIssueCreatesLinksAndUpdatesDashboard(t *testing.T) {
	configPath, dbPath := writeTestConfig(t)
	mustRun(t, configPath,
		"create",
		"--repo", "owner/repo",
		"--name", "Madar",
		"--goal", "Ship v2",
	)
	t.Setenv("GITHUB_TOKEN", "test-token")

	client := &cliIssueClient{}
	factoryCalls := 0
	factory := func(token string) projectissue.Client {
		factoryCalls++
		if token != "test-token" {
			t.Errorf("factory token = %q", token)
		}
		return client
	}
	args := withConfig(configPath, "sync-issue", "--repo", "owner/repo")[1:]
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := runSyncIssue(args, &stdout, &stderr, factory); err != nil {
		t.Fatalf("create issue: %v\nstderr: %s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "created parent project issue #90") ||
		client.createCalls != 1 {
		t.Fatalf("create output = %q, calls=%d", stdout.String(), client.createCalls)
	}

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	project, err := s.GetProjectByRepo("owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if project.ParentIssueNumber != 90 {
		t.Fatalf("parent issue = %d, want 90", project.ParentIssueNumber)
	}

	// A stale managed section so the merge is a real change.
	stale := strings.Replace(client.createdBody, "**Health:** on-track", "**Health:** at-risk", 1)
	if stale == client.createdBody {
		t.Fatal("fixture did not produce a stale section")
	}
	client.existingBody = "Human notes\n\n" + stale + "\n\nHuman footer"
	stdout.Reset()
	stderr.Reset()
	if err := runSyncIssue(args, &stdout, &stderr, factory); err != nil {
		t.Fatalf("update issue: %v\nstderr: %s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "updated parent project issue #90") ||
		client.updateCalls != 1 ||
		!strings.HasPrefix(client.updatedBody, "Human notes\n\n") ||
		!strings.HasSuffix(client.updatedBody, "\n\nHuman footer") {
		t.Fatalf(
			"update output=%q calls=%d body=%q",
			stdout.String(),
			client.updateCalls,
			client.updatedBody,
		)
	}
	if factoryCalls != 2 {
		t.Errorf("factory calls = %d, want 2", factoryCalls)
	}

	// Re-syncing an up-to-date issue must not spend a GitHub write.
	client.existingBody = client.updatedBody
	stdout.Reset()
	stderr.Reset()
	if err := runSyncIssue(args, &stdout, &stderr, factory); err != nil {
		t.Fatalf("unchanged issue: %v\nstderr: %s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "already up to date") || client.updateCalls != 1 {
		t.Fatalf("unchanged output=%q calls=%d", stdout.String(), client.updateCalls)
	}
}

func TestSyncIssueRequiresGitHubToken(t *testing.T) {
	configPath, _ := writeTestConfig(t)
	mustRun(t, configPath,
		"create",
		"--repo", "owner/repo",
		"--name", "Madar",
		"--goal", "Ship v2",
	)
	t.Setenv("GITHUB_TOKEN", "")
	factoryCalled := false
	err := runSyncIssue(
		withConfig(configPath, "sync-issue", "--repo", "owner/repo")[1:],
		&bytes.Buffer{},
		&bytes.Buffer{},
		func(string) projectissue.Client {
			factoryCalled = true
			return &cliIssueClient{}
		},
	)
	if err == nil || !strings.Contains(err.Error(), "GITHUB_TOKEN is required") {
		t.Fatalf("error = %v", err)
	}
	if factoryCalled {
		t.Fatal("GitHub client was created without credentials")
	}
}

func TestRunMigrationUsesDefaultsPreservesLegacyAndIsIdempotent(t *testing.T) {
	configPath, dbPath := writeTestConfig(t)
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := s.BindTaskExecution(
		"owner/repo",
		42,
		store.StateInProgress,
		store.ExecutionBinding{
			Engine:            "claude",
			Model:             "sonnet-test",
			ProviderSessionID: "legacy-session",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("PATH", "")

	args := []string{
		"--repo", "owner/repo",
		"--config", configPath,
		"--env", "",
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := RunMigration(args, &stdout, &stderr); err != nil {
		t.Fatalf("migration: %v\nstderr: %s", err, stderr.String())
	}
	for _, want := range []string{
		"migrated legacy repository owner/repo to project 1",
		"project created      : true",
		"legacy tasks         : 1",
		"newly mapped         : 1",
		"legacy rows preserved: true",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("migration output missing %q:\n%s", want, stdout.String())
		}
	}

	reopened, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	project, err := reopened.GetProjectByRepo("owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	if project == nil ||
		project.Name != "repo" ||
		project.Goal != "Continue owner/repo under Madar v2." ||
		project.Scope != "Migrated from legacy issue mode." {
		t.Fatalf("default migrated project = %#v", project)
	}
	storedLegacy, err := reopened.GetTask("owner/repo", 42)
	if err != nil ||
		storedLegacy == nil ||
		storedLegacy.ID != legacy.ID ||
		storedLegacy.SessionID != "legacy-session" {
		t.Fatalf("legacy row = %#v, error=%v", storedLegacy, err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	stdout.Reset()
	stderr.Reset()
	if err := RunMigration(args, &stdout, &stderr); err != nil {
		t.Fatalf("repeat migration: %v\nstderr: %s", err, stderr.String())
	}
	for _, want := range []string{
		"project created      : false",
		"newly mapped         : 0",
		"already mapped       : 1",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("repeat output missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestRunMigrationErrors(t *testing.T) {
	configPath, _ := writeTestConfig(t)
	if err := RunMigration(
		[]string{"--config", configPath, "--env", ""},
		&bytes.Buffer{},
		&bytes.Buffer{},
	); !errors.Is(err, ErrUsage) {
		t.Fatalf("missing repo error = %v", err)
	}
	if err := RunMigration(
		[]string{"--repo", "owner/repo", "--config", configPath, "--env", ""},
		&bytes.Buffer{},
		&bytes.Buffer{},
	); !errors.Is(err, store.ErrNoLegacyTasks) {
		t.Fatalf("missing legacy tasks error = %v", err)
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
		"project sync-issue",
	} {
		if !strings.Contains(stdout.String(), command) {
			t.Errorf("help output missing %q:\n%s", command, stdout.String())
		}
	}
}

type cliIssueClient struct {
	createCalls  int
	updateCalls  int
	createdBody  string
	existingBody string
	updatedBody  string
}

func (c *cliIssueClient) GetIssue(
	context.Context,
	string,
	string,
	int,
) (*githubclient.Issue, error) {
	return &githubclient.Issue{
		Number: 90,
		Body:   c.existingBody,
	}, nil
}

func (c *cliIssueClient) CreateIssue(
	_ context.Context,
	_, _, _ string,
	body string,
	_ []string,
) (*githubclient.Issue, error) {
	c.createCalls++
	c.createdBody = body
	return &githubclient.Issue{
		Number:  90,
		Body:    body,
		HTMLURL: "https://github.com/owner/repo/issues/90",
	}, nil
}

func (c *cliIssueClient) UpdateIssueBody(
	_ context.Context,
	_, _ string,
	number int,
	body string,
) (*githubclient.Issue, error) {
	c.updateCalls++
	c.updatedBody = body
	return &githubclient.Issue{
		Number:  number,
		Body:    body,
		HTMLURL: "https://github.com/owner/repo/issues/90",
	}, nil
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

func TestReconcileReportsConvergenceAndDrift(t *testing.T) {
	configPath, dbPath := writeTestConfig(t)
	mustRun(t, configPath,
		"create", "--repo", "owner/repo", "--name", "Madar", "--goal", "Ship v2",
	)
	t.Setenv("GITHUB_TOKEN", "test-token")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	projectRecord, err := s.GetProjectByRepo("owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	task := domain.NewTask(projectRecord.ID, "Filed work", "Do it")
	task.Status = domain.TaskDeveloping
	task.IssueNumber = 91
	task.BranchName = "madar/issue-91"
	if _, err := s.CreateProjectTask(task); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	client := &fakeReconcileCLIClient{
		issues: map[int]*githubclient.Issue{
			91: {Number: 91, State: "open", Labels: []string{"madar:queued", "keep-me"}},
		},
		pulls: map[string][]*githubclient.PullRequest{
			"madar/issue-91": {
				{Number: 51, State: "closed", Merged: true, HeadBranch: "madar/issue-91"},
			},
		},
	}
	args := withConfig(configPath, "reconcile", "--repo", "owner/repo")[1:]
	var stdout, stderr bytes.Buffer
	if err := runReconcile(args, &stdout, &stderr, func(string) project.ReconcileClient {
		return client
	}); err != nil {
		t.Fatalf("reconcile: %v\nstderr: %s", err, stderr.String())
	}
	output := stdout.String()
	for _, fragment := range []string{
		"labels",
		"pull request #51",
		"drift:",
		"merged while the task is",
		"reconciled 1 task(s)",
	} {
		if !strings.Contains(output, fragment) {
			t.Fatalf("output missing %q:\n%s", fragment, output)
		}
	}
	// The human label survived reconciliation.
	if !containsLabel(client.lastLabels, "keep-me") {
		t.Fatalf("labels = %v", client.lastLabels)
	}
}

func TestReconcileRequiresGitHubToken(t *testing.T) {
	configPath, _ := writeTestConfig(t)
	mustRun(t, configPath,
		"create", "--repo", "owner/repo", "--name", "Madar", "--goal", "Ship v2",
	)
	t.Setenv("GITHUB_TOKEN", "")
	args := withConfig(configPath, "reconcile", "--repo", "owner/repo")[1:]
	var stdout, stderr bytes.Buffer
	err := runReconcile(args, &stdout, &stderr, func(string) project.ReconcileClient {
		t.Fatal("client was built without a token")
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Fatalf("error = %v", err)
	}
}

func containsLabel(labels []string, want string) bool {
	for _, label := range labels {
		if label == want {
			return true
		}
	}
	return false
}

type fakeReconcileCLIClient struct {
	issues     map[int]*githubclient.Issue
	pulls      map[string][]*githubclient.PullRequest
	lastLabels []string
}

func (fake *fakeReconcileCLIClient) GetIssue(
	_ context.Context, _, _ string, number int,
) (*githubclient.Issue, error) {
	return fake.issues[number], nil
}

func (fake *fakeReconcileCLIClient) ReplaceLabels(
	_ context.Context, _, _ string, number int, labels []string,
) error {
	fake.lastLabels = labels
	if issue, ok := fake.issues[number]; ok {
		issue.Labels = labels
	}
	return nil
}

func (fake *fakeReconcileCLIClient) CloseIssue(
	_ context.Context, _, _ string, number int,
) error {
	if issue, ok := fake.issues[number]; ok {
		issue.State = "closed"
	}
	return nil
}

func (fake *fakeReconcileCLIClient) ListPullRequestsForBranch(
	_ context.Context, _, _, branch string,
) ([]*githubclient.PullRequest, error) {
	return fake.pulls[branch], nil
}
