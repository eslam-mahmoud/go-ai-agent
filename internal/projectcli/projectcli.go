// Package projectcli implements the local command surface for v2 projects.
package projectcli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/config"
	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	githubclient "github.com/eslam-mahmoud/go-ai-agent/internal/github"
	"github.com/eslam-mahmoud/go-ai-agent/internal/project"
	"github.com/eslam-mahmoud/go-ai-agent/internal/projectfiles"
	"github.com/eslam-mahmoud/go-ai-agent/internal/projectissue"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
)

var ErrUsage = errors.New("invalid project command")

type configFlags struct {
	configPath string
	envPath    string
}

// RunMigration executes the top-level legacy compatibility command specified
// by the v2 migration plan.
func RunMigration(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("migrate-project", stderr)
	cfgFlags := addConfigFlags(fs)
	repoValue := fs.String("repo", "", "legacy repository identity in owner/name form")
	name := fs.String("name", "", "project display name (defaults to repository name)")
	goal := fs.String("goal", "", "project goal")
	scope := fs.String("scope", "Migrated from legacy issue mode.", "project scope")
	releaseTarget := fs.String("release-target", "", "optional release target")
	parentIssue := fs.Int("parent-issue", 0, "optional existing parent issue number")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := requireValues(requiredValue{"repo", *repoValue}); err != nil {
		return err
	}
	repo, err := normalizeRepo(*repoValue)
	if err != nil {
		return err
	}
	repoParts := strings.Split(repo, "/")
	if strings.TrimSpace(*name) == "" {
		*name = repoParts[1]
	}
	if strings.TrimSpace(*goal) == "" {
		*goal = fmt.Sprintf("Continue %s under Madar v2.", repo)
	}

	s, err := openStore(cfgFlags)
	if err != nil {
		return err
	}
	defer s.Close()
	report, err := s.MigrateLegacyProject(store.LegacyProjectMigrationOptions{
		Repo:              repo,
		Name:              *name,
		Goal:              *goal,
		Scope:             *scope,
		ReleaseTarget:     *releaseTarget,
		ParentIssueNumber: *parentIssue,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "migrated legacy repository %s to project %d\n", repo, report.Project.ID)
	fmt.Fprintf(stdout, "  project created      : %t\n", report.ProjectCreated)
	fmt.Fprintf(stdout, "  legacy tasks         : %d\n", report.LegacyTasks)
	fmt.Fprintf(stdout, "  newly mapped         : %d\n", report.MigratedTasks)
	fmt.Fprintf(stdout, "  already mapped       : %d\n", report.AlreadyMigrated)
	fmt.Fprintf(stdout, "  project tasks created: %d\n", report.ProjectTasksCreated)
	fmt.Fprintf(stdout, "  executions created   : %d\n", report.ExecutionsCreated)
	fmt.Fprintln(stdout, "  legacy rows preserved: true")
	return nil
}

// Run executes one project subcommand using args without the leading
// "project" token. Project commands intentionally load only configuration and
// storage; they do not require GitHub credentials or an installed engine CLI.
func Run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		printUsage(stderr)
		return fmt.Errorf("%w: a subcommand is required", ErrUsage)
	}

	switch args[0] {
	case "create":
		return runCreate(args[1:], stdout, stderr)
	case "list":
		return runList(args[1:], stdout, stderr)
	case "show":
		return runShow(args[1:], stdout, stderr)
	case "add-task":
		return runAddTask(args[1:], stdout, stderr)
	case "list-tasks":
		return runListTasks(args[1:], stdout, stderr)
	case "sync-files":
		return runSyncFiles(args[1:], stdout, stderr)
	case "sync-issue":
		return runSyncIssue(
			args[1:],
			stdout,
			stderr,
			func(token string) projectissue.Client {
				return githubclient.New(token)
			},
		)
	case "reconcile":
		return runReconcile(
			args[1:],
			stdout,
			stderr,
			func(token string) project.ReconcileClient {
				return githubclient.New(token)
			},
		)
	case "help", "-h", "--help":
		printUsage(stdout)
		return nil
	default:
		printUsage(stderr)
		return fmt.Errorf("%w: unknown subcommand %q", ErrUsage, args[0])
	}
}

func runCreate(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("project create", stderr)
	cfgFlags := addConfigFlags(fs)
	repo := fs.String("repo", "", "repository identity in owner/name form")
	name := fs.String("name", "", "project display name")
	goal := fs.String("goal", "", "project goal")
	scope := fs.String("scope", "", "project scope")
	releaseTarget := fs.String("release-target", "", "optional release target")
	parentIssue := fs.Int("parent-issue", 0, "optional existing parent issue number")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := requireValues(
		requiredValue{"repo", *repo},
		requiredValue{"name", *name},
		requiredValue{"goal", *goal},
	); err != nil {
		return err
	}
	normalizedRepo, err := normalizeRepo(*repo)
	if err != nil {
		return err
	}

	s, err := openStore(cfgFlags)
	if err != nil {
		return err
	}
	defer s.Close()

	project := domain.NewProject(
		normalizedRepo,
		strings.TrimSpace(*name),
		strings.TrimSpace(*goal),
		strings.TrimSpace(*scope),
	)
	project.ReleaseTarget = strings.TrimSpace(*releaseTarget)
	project.ParentIssueNumber = *parentIssue
	created, err := s.CreateProject(project)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "created project %d: %s (%s)\n", created.ID, created.Name, created.Repo)
	return nil
}

func runList(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("project list", stderr)
	cfgFlags := addConfigFlags(fs)
	if err := parseFlags(fs, args); err != nil {
		return err
	}

	s, err := openStore(cfgFlags)
	if err != nil {
		return err
	}
	defer s.Close()
	projects, err := s.ListProjects()
	if err != nil {
		return err
	}

	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tREPOSITORY\tSTATE\tHEALTH\tNAME")
	for _, project := range projects {
		fmt.Fprintf(
			tw,
			"%d\t%s\t%s\t%s\t%s\n",
			project.ID,
			project.Repo,
			project.State,
			project.Health,
			project.Name,
		)
	}
	return tw.Flush()
}

func runShow(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("project show", stderr)
	cfgFlags := addConfigFlags(fs)
	repo := fs.String("repo", "", "repository identity in owner/name form")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := requireValues(requiredValue{"repo", *repo}); err != nil {
		return err
	}

	s, err := openStore(cfgFlags)
	if err != nil {
		return err
	}
	defer s.Close()
	project, err := requireProject(s, *repo)
	if err != nil {
		return err
	}
	tasks, err := s.ListProjectTasks(project.ID)
	if err != nil {
		return err
	}

	printProject(stdout, project)
	fmt.Fprintln(stdout, "backlog:")
	return printTasks(stdout, tasks)
}

func runAddTask(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("project add-task", stderr)
	cfgFlags := addConfigFlags(fs)
	repo := fs.String("repo", "", "project repository identity")
	title := fs.String("title", "", "task title")
	goal := fs.String("goal", "", "task goal")
	issue := fs.Int("issue", 0, "optional GitHub issue number")
	priority := fs.Int("priority", 0, "task priority")
	taskType := fs.String("type", "", "task type")
	source := fs.String("source", "cli", "task source")
	blocksRelease := fs.Bool("blocks-release", false, "mark the task as release-blocking")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := requireValues(
		requiredValue{"repo", *repo},
		requiredValue{"title", *title},
		requiredValue{"goal", *goal},
	); err != nil {
		return err
	}

	s, err := openStore(cfgFlags)
	if err != nil {
		return err
	}
	defer s.Close()
	project, err := requireProject(s, *repo)
	if err != nil {
		return err
	}

	task := domain.NewTask(
		project.ID,
		strings.TrimSpace(*title),
		strings.TrimSpace(*goal),
	)
	task.IssueNumber = *issue
	task.Priority = *priority
	task.TaskType = strings.TrimSpace(*taskType)
	task.Source = strings.TrimSpace(*source)
	task.BlocksRelease = *blocksRelease
	created, err := s.CreateProjectTask(task)
	if err != nil {
		return err
	}
	fmt.Fprintf(
		stdout,
		"added task %d at sequence %d: %s\n",
		created.ID,
		created.Sequence,
		created.Title,
	)
	return nil
}

func runListTasks(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("project list-tasks", stderr)
	cfgFlags := addConfigFlags(fs)
	repo := fs.String("repo", "", "project repository identity")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := requireValues(requiredValue{"repo", *repo}); err != nil {
		return err
	}

	s, err := openStore(cfgFlags)
	if err != nil {
		return err
	}
	defer s.Close()
	project, err := requireProject(s, *repo)
	if err != nil {
		return err
	}
	tasks, err := s.ListProjectTasks(project.ID)
	if err != nil {
		return err
	}
	return printTasks(stdout, tasks)
}

func runSyncFiles(args []string, stdout, stderr io.Writer) error {
	fs := newFlagSet("project sync-files", stderr)
	cfgFlags := addConfigFlags(fs)
	repo := fs.String("repo", "", "project repository identity")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := requireValues(requiredValue{"repo", *repo}); err != nil {
		return err
	}

	cfg, err := loadConfig(cfgFlags)
	if err != nil {
		return err
	}
	s, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer s.Close()
	project, err := requireProject(s, *repo)
	if err != nil {
		return err
	}
	tasks, err := s.ListProjectTasks(project.ID)
	if err != nil {
		return err
	}
	files, err := projectfiles.Render(project, tasks)
	if err != nil {
		return err
	}

	repoParts := strings.Split(project.Repo, "/")
	workspace := filepath.Join(cfg.WorkspaceDir, repoParts[0], repoParts[1])
	if err := projectfiles.Write(workspace, files); err != nil {
		return err
	}
	fmt.Fprintf(
		stdout,
		"wrote %s and %s\n",
		filepath.Join(workspace, projectfiles.DirectoryName, projectfiles.ProjectFileName),
		filepath.Join(workspace, projectfiles.DirectoryName, projectfiles.PlanFileName),
	)
	return nil
}

type issueClientFactory func(token string) projectissue.Client

type reconcileClientFactory func(token string) project.ReconcileClient

// runReconcile runs one reconciliation pass and prints what converged and what
// drifted, so an operator can see the difference without reading logs.
func runReconcile(
	args []string,
	stdout, stderr io.Writer,
	newClient reconcileClientFactory,
) error {
	fs := newFlagSet("project reconcile", stderr)
	cfgFlags := addConfigFlags(fs)
	repo := fs.String("repo", "", "project repository identity")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := requireValues(requiredValue{"repo", *repo}); err != nil {
		return err
	}
	cfg, err := loadConfig(cfgFlags)
	if err != nil {
		return err
	}
	if strings.TrimSpace(cfg.GitHub.Token) == "" {
		return errors.New("GITHUB_TOKEN is required for project reconcile")
	}
	s, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer s.Close()
	projectRecord, err := requireProject(s, *repo)
	if err != nil {
		return err
	}
	reconciler, err := project.NewReconciler(s, newClient(cfg.GitHub.Token))
	if err != nil {
		return err
	}
	result, err := reconciler.Reconcile(context.Background(), projectRecord.ID)
	if err != nil {
		return err
	}

	converged := 0
	drift := 0
	for _, task := range result.Tasks {
		if task.LabelsUpdated || task.IssueClosed || task.PullRequestBound > 0 {
			converged++
			fmt.Fprintf(stdout, "task %d:", task.TaskID)
			if task.LabelsUpdated {
				fmt.Fprint(stdout, " labels")
			}
			if task.IssueClosed {
				fmt.Fprintf(stdout, " closed #%d", task.IssueNumber)
			}
			if task.PullRequestBound > 0 {
				fmt.Fprintf(stdout, " pull request #%d", task.PullRequestBound)
			}
			fmt.Fprintln(stdout)
		}
		for _, entry := range task.Drift {
			drift++
			fmt.Fprintf(stdout, "drift: task %d: %s\n", task.TaskID, entry)
		}
	}
	for _, ambiguous := range result.Ambiguous {
		fmt.Fprintf(
			stdout,
			"ambiguous: task %d branch %s has open pull requests %v\n",
			ambiguous.TaskID,
			ambiguous.Branch,
			ambiguous.Numbers,
		)
	}
	fmt.Fprintf(
		stdout,
		"reconciled %d task(s): %d converged, %d drift, %d ambiguous\n",
		len(result.Tasks), converged, drift, len(result.Ambiguous),
	)
	return nil
}

func runSyncIssue(
	args []string,
	stdout, stderr io.Writer,
	newClient issueClientFactory,
) error {
	fs := newFlagSet("project sync-issue", stderr)
	cfgFlags := addConfigFlags(fs)
	repo := fs.String("repo", "", "project repository identity")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := requireValues(requiredValue{"repo", *repo}); err != nil {
		return err
	}

	cfg, err := loadConfig(cfgFlags)
	if err != nil {
		return err
	}
	if strings.TrimSpace(cfg.GitHub.Token) == "" {
		return errors.New("GITHUB_TOKEN is required for project sync-issue")
	}
	s, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer s.Close()
	project, err := requireProject(s, *repo)
	if err != nil {
		return err
	}
	tasks, err := s.ListProjectTasks(project.ID)
	if err != nil {
		return err
	}
	review, err := s.LatestManagerReview(project.ID)
	if err != nil {
		return err
	}

	repoParts := strings.Split(project.Repo, "/")
	result, err := projectissue.Sync(
		context.Background(),
		newClient(cfg.GitHub.Token),
		repoParts[0],
		repoParts[1],
		project,
		tasks,
		review,
	)
	if err != nil {
		return err
	}
	if result == nil || result.Issue == nil || result.Issue.Number <= 0 {
		return errors.New("sync parent project issue returned no issue number")
	}
	if result.Created {
		project.ParentIssueNumber = result.Issue.Number
		if _, err := s.UpdateProject(project); err != nil {
			return fmt.Errorf("persist parent project issue number: %w", err)
		}
		fmt.Fprintf(
			stdout,
			"created parent project issue #%d: %s\n",
			result.Issue.Number,
			result.Issue.HTMLURL,
		)
		return nil
	}
	if result.Unchanged {
		fmt.Fprintf(
			stdout,
			"parent project issue #%d is already up to date\n",
			result.Issue.Number,
		)
		return nil
	}
	fmt.Fprintf(
		stdout,
		"updated parent project issue #%d: %s\n",
		result.Issue.Number,
		result.Issue.HTMLURL,
	)
	return nil
}

func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func addConfigFlags(fs *flag.FlagSet) *configFlags {
	values := &configFlags{}
	fs.StringVar(&values.configPath, "config", "",
		"path to config.yaml (default: discovered)")
	fs.StringVar(&values.envPath, "env", "",
		"path to the .env file (default: beside the config that was found)")
	return values
}

func parseFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("%w: unexpected argument %q", ErrUsage, fs.Arg(0))
	}
	return nil
}

type requiredValue struct {
	name  string
	value string
}

func requireValues(values ...requiredValue) error {
	for _, value := range values {
		if strings.TrimSpace(value.value) == "" {
			return fmt.Errorf("%w: --%s is required", ErrUsage, value.name)
		}
	}
	return nil
}

func openStore(flags *configFlags) (*store.Store, error) {
	cfg, err := loadConfig(flags)
	if err != nil {
		return nil, err
	}
	return store.Open(cfg.DBPath)
}

// loadConfig resolves the configuration the same way the daemon does, so a
// subcommand run from any directory finds the same files the daemon would.
func loadConfig(flags *configFlags) (*config.Config, error) {
	discovery := config.DiscoverConfig(flags.configPath)
	if discovery.ConfigPath == "" {
		return nil, discovery.NotFoundError()
	}
	return config.Load(
		discovery.ConfigPath,
		config.ResolveEnv(flags.envPath, discovery.ConfigPath),
	)
}

func requireProject(s *store.Store, repo string) (*domain.Project, error) {
	normalized, err := normalizeRepo(repo)
	if err != nil {
		return nil, err
	}
	project, err := s.GetProjectByRepo(normalized)
	if err != nil {
		return nil, err
	}
	if project == nil {
		return nil, fmt.Errorf("%w: repository %q", store.ErrProjectNotFound, normalized)
	}
	return project, nil
}

func normalizeRepo(value string) (string, error) {
	repo := strings.TrimSpace(value)
	parts := strings.Split(repo, "/")
	if len(parts) != 2 ||
		parts[0] == "" ||
		parts[1] == "" ||
		parts[0] == "." ||
		parts[0] == ".." ||
		parts[1] == "." ||
		parts[1] == ".." ||
		strings.ContainsAny(repo, " \t\r\n\\") {
		return "", fmt.Errorf("%w: --repo must use owner/name format", ErrUsage)
	}
	return repo, nil
}

func printProject(w io.Writer, project *domain.Project) {
	fmt.Fprintf(w, "project %d\n", project.ID)
	fmt.Fprintf(w, "  repository        : %s\n", project.Repo)
	fmt.Fprintf(w, "  name              : %s\n", project.Name)
	fmt.Fprintf(w, "  goal              : %s\n", project.Goal)
	fmt.Fprintf(w, "  scope             : %s\n", displayValue(project.Scope))
	fmt.Fprintf(w, "  state             : %s\n", project.State)
	fmt.Fprintf(w, "  health            : %s\n", project.Health)
	fmt.Fprintf(w, "  release target    : %s\n", displayValue(project.ReleaseTarget))
	fmt.Fprintf(w, "  release readiness : %s\n", displayValue(project.ReleaseReadiness))
	fmt.Fprintf(w, "  parent issue      : %s\n", displayNumber(project.ParentIssueNumber))
	fmt.Fprintf(w, "  current task      : %s\n", displayInt64(project.CurrentTaskID))
	fmt.Fprintf(w, "  plan version      : %d\n", project.CurrentPlanVersion)
	fmt.Fprintf(w, "  architecture      : %d\n", project.ArchitectureVersion)
	fmt.Fprintf(w, "  manager review    : %s\n", displayTime(project.LastManagerReviewAt))
	fmt.Fprintf(w, "  created           : %s\n", project.CreatedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(w, "  updated           : %s\n", project.UpdatedAt.UTC().Format(time.RFC3339))
}

func printTasks(w io.Writer, tasks []*domain.Task) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SEQ\tID\tISSUE\tSTATUS\tPRIORITY\tTITLE")
	for _, task := range tasks {
		fmt.Fprintf(
			tw,
			"%d\t%d\t%s\t%s\t%d\t%s\n",
			task.Sequence,
			task.ID,
			displayNumber(task.IssueNumber),
			task.Status,
			task.Priority,
			task.Title,
		)
	}
	return tw.Flush()
}

func displayValue(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func displayNumber(value int) string {
	if value == 0 {
		return "-"
	}
	return strconv.Itoa(value)
}

func displayInt64(value *int64) string {
	if value == nil {
		return "-"
	}
	return strconv.FormatInt(*value, 10)
}

func displayTime(value *time.Time) string {
	if value == nil {
		return "-"
	}
	return value.UTC().Format(time.RFC3339)
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, `Usage:
  madar migrate-project --repo owner/name [options]
  madar project create --repo owner/name --name NAME --goal GOAL [options]
  madar project list [options]
  madar project show --repo owner/name [options]
  madar project add-task --repo owner/name --title TITLE --goal GOAL [options]
  madar project list-tasks --repo owner/name [options]
  madar project sync-files --repo owner/name [options]
  madar project sync-issue --repo owner/name [options]
  madar project reconcile --repo owner/name [options]

Every subcommand accepts --config and --env.`)
}
