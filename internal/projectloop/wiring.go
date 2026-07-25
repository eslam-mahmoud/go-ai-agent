package projectloop

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/eslam-mahmoud/go-ai-agent/internal/config"
	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	"github.com/eslam-mahmoud/go-ai-agent/internal/engine"
	"github.com/eslam-mahmoud/go-ai-agent/internal/execution"
	githubclient "github.com/eslam-mahmoud/go-ai-agent/internal/github"
	"github.com/eslam-mahmoud/go-ai-agent/internal/mode"
	"github.com/eslam-mahmoud/go-ai-agent/internal/notify"
	"github.com/eslam-mahmoud/go-ai-agent/internal/policy"
	"github.com/eslam-mahmoud/go-ai-agent/internal/project"
	"github.com/eslam-mahmoud/go-ai-agent/internal/projectissue"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
	"github.com/eslam-mahmoud/go-ai-agent/internal/workflow"
)

// GitHubClient is the union of what the v2 delivery cycle asks of GitHub:
// publishing discovery issues, syncing the parent dashboard issue, and
// reading check status. It is declared here rather than reusing the full
// client interface so a fake only has to implement what is actually used.
type GitHubClient interface {
	projectissue.Client
	project.DiscoveryIssueClient
	GetCheckSuiteStatus(
		ctx context.Context, owner, repo, branch string,
	) (githubclient.CheckStatus, error)
}

// Dependencies are the process-wide collaborators the loop is built from.
type Dependencies struct {
	Config *config.Config
	Store  *store.Store
	Engine engine.Engine
	GitHub GitHubClient
	// Status keeps the owner's live message current. Optional: a deployment
	// without Telegram delivers silently.
	Status    notify.StatusSender
	Log       *slog.Logger
	ProjectID int64
}

// Build assembles the v2 delivery loop from configuration. It exists so the
// wiring is testable end to end: the daemon calls this rather than
// constructing a dozen collaborators inline, where a missing one would go
// unnoticed until production. That is exactly how the v2 stack came to be
// fully implemented and never executed.
//
// A nil loop with a nil error means v2 is not enabled, which is the default.
func Build(dependencies Dependencies) (*Loop, error) {
	cfg := dependencies.Config
	if cfg == nil || !cfg.Project.Enabled {
		return nil, nil
	}
	projectStore := dependencies.Store
	switch {
	case projectStore == nil:
		return nil, fmt.Errorf("%w: store is required", ErrInvalidLoop)
	case dependencies.Engine == nil:
		return nil, fmt.Errorf("%w: engine is required", ErrInvalidLoop)
	}
	log := dependencies.Log
	if log == nil {
		log = slog.Default()
	}

	projectID := dependencies.ProjectID
	if projectID <= 0 {
		record, err := projectStore.GetProjectByRepo(cfg.Project.Repo)
		if err != nil {
			return nil, err
		}
		if record == nil {
			return nil, fmt.Errorf(
				"%w: no v2 project exists for %q; create it before enabling project mode",
				ErrInvalidLoop,
				cfg.Project.Repo,
			)
		}
		projectID = record.ID
	}

	workspaceRoot, err := filepath.Abs(cfg.WorkspaceDir)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace directory: %w", err)
	}
	recorder, err := execution.New(projectStore, execution.Options{
		Root:   filepath.Join(workspaceRoot, ".madar", "executions"),
		Engine: "claude",
		Model:  cfg.Claude.Model,
	})
	if err != nil {
		return nil, err
	}

	runner, err := buildModeRunner(cfg, projectStore, dependencies.Engine, recorder,
		project.WorkspaceRootResolver{Root: workspaceRoot}, dependencies.GitHub)
	if err != nil {
		return nil, err
	}
	manager, err := buildManager(cfg, projectStore, dependencies.Engine, recorder, workspaceRoot)
	if err != nil {
		return nil, err
	}
	return assemble(assembly{
		config:        cfg,
		store:         projectStore,
		projectID:     projectID,
		runner:        runner,
		manager:       manager,
		client:        dependencies.GitHub,
		status:        dependencies.Status,
		workspaceRoot: workspaceRoot,
		log:           log,
	})
}

// buildModeRunner registers the five delivery modes against one shared
// context provider and records every run, which is what lets the developer
// read the plan and the fixer read the review.
func buildModeRunner(
	cfg *config.Config,
	projectStore *store.Store,
	provider engine.Engine,
	recorder *execution.Recorder,
	workspaces mode.DeliveryWorkspaces,
	client GitHubClient,
) (workflow.ModeRunner, error) {
	options := mode.DeliveryContextOptions{
		CIRequired: cfg.CI.Enabled,
		BaseRef:    "main",
	}
	if cfg.CI.Enabled {
		if client == nil {
			return nil, fmt.Errorf(
				"%w: ci.enabled requires GitHub credentials", ErrInvalidLoop,
			)
		}
		options.CI = &checkSuiteCI{client: client, repo: cfg.Project.Repo}
	}
	contexts, err := mode.NewDurableDeliveryContextProvider(
		projectStore, recorder, recorder, workspaces, options,
	)
	if err != nil {
		return nil, err
	}

	planner, err := mode.NewPlanner(provider, contexts, mode.PlannerOptions{
		Model: cfg.Claude.Model,
	})
	if err != nil {
		return nil, err
	}
	developer, err := mode.NewDeveloper(provider, contexts, mode.DeveloperOptions{
		Model: cfg.Claude.Model,
	})
	if err != nil {
		return nil, err
	}
	reviewer, err := mode.NewReviewer(provider, contexts, mode.ReviewerOptions{
		Model: cfg.Claude.Model,
	})
	if err != nil {
		return nil, err
	}
	fixer, err := mode.NewFixer(provider, contexts, mode.FixerOptions{
		Model: cfg.Claude.Model,
	})
	if err != nil {
		return nil, err
	}
	verifier, err := mode.NewVerifier(provider, contexts, mode.VerifierOptions{
		Model: cfg.Claude.Model,
	})
	if err != nil {
		return nil, err
	}
	registry, err := mode.NewRegistry(planner, developer, reviewer, fixer, verifier)
	if err != nil {
		return nil, err
	}
	return mode.NewRecordingDispatcher(registry, recorder)
}

func buildManager(
	cfg *config.Config,
	projectStore *store.Store,
	provider engine.Engine,
	recorder *execution.Recorder,
	workspaceRoot string,
) (project.ManagerRunner, error) {
	runtime := mode.ManagerRuntimeContextProviderFunc(func(
		_ context.Context, projectID, completedTaskID int64,
	) (mode.ManagerRuntimeContext, error) {
		record, err := projectStore.GetProjectByID(projectID)
		if err != nil {
			return mode.ManagerRuntimeContext{}, err
		}
		if record == nil {
			return mode.ManagerRuntimeContext{}, fmt.Errorf(
				"%w: project %d not found", ErrInvalidLoop, projectID,
			)
		}
		workDir, err := project.WorkspaceRootResolver{Root: workspaceRoot}.
			ProjectWorkspace(record.Repo)
		if err != nil {
			return mode.ManagerRuntimeContext{}, err
		}
		opened, err := recorder.Begin(projectID, completedTaskID, string(workflow.ModeManager))
		if err != nil {
			return mode.ManagerRuntimeContext{}, err
		}
		return mode.ManagerRuntimeContext{
			WorkDir:     workDir,
			ExecutionID: opened.ID,
		}, nil
	})
	contexts, err := mode.NewDurableManagerContextProvider(projectStore, runtime)
	if err != nil {
		return nil, err
	}
	manager, err := mode.NewManager(provider, contexts, mode.ManagerOptions{
		Model: cfg.Claude.Model,
	})
	if err != nil {
		return nil, err
	}
	return &recordedManager{manager: manager, recorder: recorder}, nil
}

// BuildWithRunners builds the review cycle and the loop from an already
// constructed mode runner and manager. Build uses it after wiring the engine;
// a test uses it to drive the whole cycle with scripted provider output while
// still exercising the real controllers, workflow, and store.
func BuildWithRunners(
	dependencies Dependencies,
	runner workflow.ModeRunner,
	manager project.ManagerRunner,
) (*Loop, error) {
	cfg := dependencies.Config
	if cfg == nil {
		return nil, fmt.Errorf("%w: config is required", ErrInvalidLoop)
	}
	workspaceRoot, err := filepath.Abs(cfg.WorkspaceDir)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace directory: %w", err)
	}
	log := dependencies.Log
	if log == nil {
		log = slog.Default()
	}
	return assemble(assembly{
		config:        cfg,
		store:         dependencies.Store,
		projectID:     dependencies.ProjectID,
		runner:        runner,
		manager:       manager,
		client:        dependencies.GitHub,
		status:        dependencies.Status,
		workspaceRoot: workspaceRoot,
		log:           log,
	})
}

// assemble builds the review cycle and the loop itself.
type assembly struct {
	config        *config.Config
	store         *store.Store
	projectID     int64
	runner        workflow.ModeRunner
	manager       project.ManagerRunner
	client        GitHubClient
	status        notify.StatusSender
	workspaceRoot string
	log           *slog.Logger
}

func assemble(parts assembly) (*Loop, error) {
	cfg := parts.config
	projectStore := parts.store
	client := parts.client
	log := parts.log
	controller, err := project.NewController(projectStore)
	if err != nil {
		return nil, err
	}
	delivery, err := workflow.NewFeatureWorkflow(
		controller, parts.runner, workflow.FeatureOptions{CIRequired: cfg.CI.Enabled},
	)
	if err != nil {
		return nil, err
	}
	discovery, err := project.NewDiscoveryController(projectStore)
	if err != nil {
		return nil, err
	}
	backlog, err := project.NewBacklogController(projectStore)
	if err != nil {
		return nil, err
	}
	selection, err := project.NewSelectionController(projectStore)
	if err != nil {
		return nil, err
	}
	architecture, err := project.NewArchitectureController(projectStore)
	if err != nil {
		return nil, err
	}
	reviewOptions := project.ReviewCoordinatorOptions{Architecture: architecture}

	// Publication stages need GitHub. Without credentials the cycle still
	// runs and still decides; it just cannot publish what it decided.
	if client != nil {
		if reviewOptions.DiscoveryIssues, err = project.NewDiscoveryIssuePublisher(
			projectStore, client,
		); err != nil {
			return nil, err
		}
		if reviewOptions.DiscoveryBacklog, err = project.NewDiscoveryBacklogController(
			projectStore,
		); err != nil {
			return nil, err
		}
		if reviewOptions.Publisher, err = project.NewPublisher(
			projectStore, client, project.PublisherOptions{
				WorkspaceRoot: parts.workspaceRoot,
			},
		); err != nil {
			return nil, err
		}
	} else {
		log.Warn("v2 publication disabled: no GitHub credentials")
	}

	reviewer, err := project.NewReviewCoordinator(
		projectStore, parts.manager, discovery, backlog, selection, reviewOptions,
	)
	if err != nil {
		return nil, err
	}
	options := Options{
		Log:         log,
		BudgetGuard: projectStore,
		Budgets: policy.Budgets{
			MaxTaskDuration:    cfg.Project.Budgets.MaxTaskDuration,
			MaxReviewFixCycles: cfg.Project.Budgets.MaxReviewFixCycles,
			MaxCIFixCycles:     cfg.Project.Budgets.MaxCIFixCycles,
			MaxModeRetries:     cfg.Project.Budgets.MaxModeRetries,
		},
	}
	if parts.status != nil {
		if options.Status, err = notify.NewStatusPublisher(
			parts.status, projectStore,
		); err != nil {
			return nil, err
		}
	}
	if cfg.Project.AutoInitialize {
		if options.Initializer, err = buildInitializer(
			projectStore, client, reviewOptions.Publisher, parts.workspaceRoot,
		); err != nil {
			return nil, err
		}
	}
	return New(parts.projectID, controller, delivery, reviewer, options)
}

// buildInitializer bootstraps a project that has no backlog yet. It needs
// GitHub to file the task issues, so auto-initialization without credentials
// is refused rather than silently skipped.
func buildInitializer(
	projectStore *store.Store,
	client GitHubClient,
	publisher *project.Publisher,
	workspaceRoot string,
) (Initializer, error) {
	if client == nil {
		return nil, fmt.Errorf(
			"%w: project.auto_initialize requires GitHub credentials", ErrInvalidLoop,
		)
	}
	backlog, err := project.NewInitialBacklogController(projectStore, client)
	if err != nil {
		return nil, err
	}
	writer, err := project.NewWorkspaceArchitectureWriter(projectStore, workspaceRoot)
	if err != nil {
		return nil, err
	}
	architecture, err := project.NewArchitectureControllerWithDocuments(
		projectStore, nil, writer,
	)
	if err != nil {
		return nil, err
	}
	return project.NewInitializer(
		projectStore,
		architecture,
		project.WorkspaceRootResolver{Root: workspaceRoot},
		project.InitializerOptions{Backlog: backlog, Publisher: publisher},
	)
}

// recordedManager closes the execution the manager context opened. The
// manager runs outside the mode dispatcher, so it needs its own bookend.
type recordedManager struct {
	manager  *mode.Manager
	recorder *execution.Recorder
}

func (runner *recordedManager) RunManagerReview(
	ctx context.Context, projectID, completedTaskID int64,
) (json.RawMessage, error) {
	output, err := runner.manager.RunManagerReview(ctx, projectID, completedTaskID)
	if err != nil {
		_ = runner.recorder.FailRunning(
			completedTaskID, string(workflow.ModeManager), "mode-run", err.Error(),
		)
		return nil, err
	}
	if err := runner.recorder.CompleteRunning(
		completedTaskID, string(workflow.ModeManager), output,
	); err != nil {
		return nil, fmt.Errorf("record manager output: %w", err)
	}
	return output, nil
}

// checkSuiteCI maps GitHub check status onto the verifier's vocabulary.
type checkSuiteCI struct {
	client GitHubClient
	repo   string
}

func (ci *checkSuiteCI) TaskCIStatus(
	ctx context.Context, task *domain.Task,
) (mode.VerificationCIStatus, error) {
	if task == nil || strings.TrimSpace(task.BranchName) == "" {
		// No branch means nothing has been pushed, so CI has not started.
		return mode.VerificationCIPending, nil
	}
	owner, repo, err := githubclient.SplitRepo(ci.repo)
	if err != nil {
		return mode.VerificationCIPending, err
	}
	status, err := ci.client.GetCheckSuiteStatus(ctx, owner, repo, task.BranchName)
	if err != nil {
		return mode.VerificationCIPending, err
	}
	switch status {
	case githubclient.CheckSuccess:
		return mode.VerificationCIPassed, nil
	case githubclient.CheckFailure:
		return mode.VerificationCIFailed, nil
	default:
		return mode.VerificationCIPending, nil
	}
}

var _ mode.DeliveryCI = (*checkSuiteCI)(nil)
