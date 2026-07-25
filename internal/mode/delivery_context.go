package mode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	"github.com/eslam-mahmoud/go-ai-agent/internal/workflow"
)

var ErrInvalidDeliveryContext = errors.New("invalid delivery context")

// DeliveryStore is the read surface the delivery modes need. It is narrow on
// purpose: these modes read project state, they never mutate it.
type DeliveryStore interface {
	GetProjectByID(id int64) (*domain.Project, error)
	GetProjectTaskByID(id int64) (*domain.Task, error)
	ListProjectTasks(projectID int64) ([]*domain.Task, error)
}

// DeliveryOutputs supplies the outputs of earlier modes in the same task. A
// mode may only read what has actually been recorded, which is what keeps the
// chain honest: the developer cannot invent a plan that was never produced.
type DeliveryOutputs interface {
	LatestOutput(taskID int64, mode string) (json.RawMessage, error)
	Outputs(taskID int64, mode string) ([]json.RawMessage, error)
}

// DeliveryExecutions opens the execution record a mode run is attributed to.
type DeliveryExecutions interface {
	Begin(projectID, taskID int64, mode string) (*domain.Execution, error)
}

// DeliveryWorkspaces resolves the checkout a mode runs against. The method
// name matches project.WorkspaceRootResolver so the existing resolver
// satisfies it without an adapter.
type DeliveryWorkspaces interface {
	ProjectWorkspace(repo string) (string, error)
}

// DeliveryCI reports check status for a task's pull request. It is optional:
// a deployment without GitHub credentials still delivers, it just cannot
// verify CI.
type DeliveryCI interface {
	TaskCIStatus(ctx context.Context, task *domain.Task) (VerificationCIStatus, error)
}

type DeliveryContextOptions struct {
	CIRequired bool
	CI         DeliveryCI
	// BaseRef is the branch pull requests target, used as the review base.
	BaseRef string
}

// DurableDeliveryContextProvider builds the context for every delivery mode
// from durable state. One type implements all five providers because they draw
// on the same sources and must agree about what "the current task" means; five
// separate implementations would be five chances to disagree.
type DurableDeliveryContextProvider struct {
	store      DeliveryStore
	outputs    DeliveryOutputs
	executions DeliveryExecutions
	workspaces DeliveryWorkspaces
	options    DeliveryContextOptions
}

func NewDurableDeliveryContextProvider(
	store DeliveryStore,
	outputs DeliveryOutputs,
	executions DeliveryExecutions,
	workspaces DeliveryWorkspaces,
	options DeliveryContextOptions,
) (*DurableDeliveryContextProvider, error) {
	switch {
	case isNilDependency(store):
		return nil, errors.New("delivery context store is required")
	case isNilDependency(outputs):
		return nil, errors.New("delivery context outputs are required")
	case isNilDependency(executions):
		return nil, errors.New("delivery context executions are required")
	case isNilDependency(workspaces):
		return nil, errors.New("delivery context workspaces are required")
	}
	if options.CIRequired && isNilDependency(options.CI) {
		// Requiring CI without a way to read it would silently report
		// "not-required" to the verifier, which is worse than refusing to start.
		return nil, errors.New("CI is required but no CI status reader is configured")
	}
	if strings.TrimSpace(options.BaseRef) == "" {
		options.BaseRef = "main"
	}
	return &DurableDeliveryContextProvider{
		store:      store,
		outputs:    outputs,
		executions: executions,
		workspaces: workspaces,
		options:    options,
	}, nil
}

// base is the shared half of every delivery context.
type deliveryBase struct {
	project     *domain.Project
	task        *domain.Task
	workDir     string
	executionID int64
}

func (provider *DurableDeliveryContextProvider) load(
	ctx context.Context, projectID, taskID int64, mode workflow.ModeName,
) (*deliveryBase, error) {
	if projectID <= 0 || taskID <= 0 {
		return nil, fmt.Errorf(
			"%w: project and task IDs must be positive",
			ErrInvalidDeliveryContext,
		)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	projectRecord, err := provider.store.GetProjectByID(projectID)
	if err != nil {
		return nil, err
	}
	if projectRecord == nil {
		return nil, fmt.Errorf("%w: project %d not found", ErrInvalidDeliveryContext, projectID)
	}
	task, err := provider.store.GetProjectTaskByID(taskID)
	if err != nil {
		return nil, err
	}
	if task == nil || task.ProjectID != projectID {
		return nil, fmt.Errorf(
			"%w: task %d does not belong to project %d",
			ErrInvalidDeliveryContext, taskID, projectID,
		)
	}
	workDir, err := provider.workspaces.ProjectWorkspace(projectRecord.Repo)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace for %s: %w", projectRecord.Repo, err)
	}
	if !filepath.IsAbs(workDir) {
		return nil, fmt.Errorf(
			"%w: workspace path %q must be absolute",
			ErrInvalidDeliveryContext, workDir,
		)
	}
	record, err := provider.executions.Begin(projectID, taskID, string(mode))
	if err != nil {
		return nil, fmt.Errorf("open %s execution: %w", mode, err)
	}
	return &deliveryBase{
		project:     projectRecord,
		task:        task,
		workDir:     filepath.Clean(workDir),
		executionID: record.ID,
	}, nil
}

// required fetches an output a mode cannot run without.
func (provider *DurableDeliveryContextProvider) required(
	taskID int64, mode workflow.ModeName, dependent workflow.ModeName,
) (json.RawMessage, error) {
	output, err := provider.outputs.LatestOutput(taskID, string(mode))
	if err != nil {
		return nil, fmt.Errorf("%s needs the %s output: %w", dependent, mode, err)
	}
	return output, nil
}

func (provider *DurableDeliveryContextProvider) LoadPlannerContext(
	ctx context.Context, projectID, taskID int64,
) (*PlannerContext, error) {
	base, err := provider.load(ctx, projectID, taskID, workflow.ModePlanner)
	if err != nil {
		return nil, err
	}
	backlog, err := provider.store.ListProjectTasks(projectID)
	if err != nil {
		return nil, err
	}
	return &PlannerContext{
		Project:     base.project,
		Task:        base.task,
		Backlog:     backlog,
		WorkDir:     base.workDir,
		ExecutionID: base.executionID,
	}, nil
}

func (provider *DurableDeliveryContextProvider) LoadDeveloperContext(
	ctx context.Context, projectID, taskID int64,
) (*DeveloperContext, error) {
	base, err := provider.load(ctx, projectID, taskID, workflow.ModeDeveloper)
	if err != nil {
		return nil, err
	}
	plan, err := provider.required(taskID, workflow.ModePlanner, workflow.ModeDeveloper)
	if err != nil {
		return nil, err
	}
	return &DeveloperContext{
		Project:       base.project,
		Task:          base.task,
		Plan:          plan,
		WorkDir:       base.workDir,
		CurrentBranch: base.task.BranchName,
		ExecutionID:   base.executionID,
	}, nil
}

func (provider *DurableDeliveryContextProvider) LoadReviewerContext(
	ctx context.Context, projectID, taskID int64,
) (*ReviewerContext, error) {
	base, err := provider.load(ctx, projectID, taskID, workflow.ModeReviewer)
	if err != nil {
		return nil, err
	}
	plan, err := provider.required(taskID, workflow.ModePlanner, workflow.ModeReviewer)
	if err != nil {
		return nil, err
	}
	delivery, err := provider.required(taskID, workflow.ModeDeveloper, workflow.ModeReviewer)
	if err != nil {
		return nil, err
	}
	return &ReviewerContext{
		Project:     base.project,
		Task:        base.task,
		Plan:        plan,
		Delivery:    delivery,
		WorkDir:     base.workDir,
		BaseRef:     provider.options.BaseRef,
		HeadRef:     base.task.BranchName,
		ExecutionID: base.executionID,
	}, nil
}

func (provider *DurableDeliveryContextProvider) LoadFixerContext(
	ctx context.Context, projectID, taskID int64,
) (*FixerContext, error) {
	base, err := provider.load(ctx, projectID, taskID, workflow.ModeFixer)
	if err != nil {
		return nil, err
	}
	plan, err := provider.required(taskID, workflow.ModePlanner, workflow.ModeFixer)
	if err != nil {
		return nil, err
	}
	delivery, err := provider.required(taskID, workflow.ModeDeveloper, workflow.ModeFixer)
	if err != nil {
		return nil, err
	}
	review, err := provider.required(taskID, workflow.ModeReviewer, workflow.ModeFixer)
	if err != nil {
		return nil, err
	}
	return &FixerContext{
		Project:       base.project,
		Task:          base.task,
		Plan:          plan,
		Delivery:      delivery,
		Review:        review,
		WorkDir:       base.workDir,
		CurrentBranch: base.task.BranchName,
		ExecutionID:   base.executionID,
	}, nil
}

func (provider *DurableDeliveryContextProvider) LoadVerifierContext(
	ctx context.Context, projectID, taskID int64,
) (*VerifierContext, error) {
	base, err := provider.load(ctx, projectID, taskID, workflow.ModeVerifier)
	if err != nil {
		return nil, err
	}
	plan, err := provider.required(taskID, workflow.ModePlanner, workflow.ModeVerifier)
	if err != nil {
		return nil, err
	}
	delivery, err := provider.required(taskID, workflow.ModeDeveloper, workflow.ModeVerifier)
	if err != nil {
		return nil, err
	}
	review, err := provider.required(taskID, workflow.ModeReviewer, workflow.ModeVerifier)
	if err != nil {
		return nil, err
	}
	// Fixes are genuinely optional: a task that passed review the first time
	// has none, and that is a clean run rather than a missing input.
	fixes, err := provider.outputs.Outputs(taskID, string(workflow.ModeFixer))
	if err != nil {
		return nil, fmt.Errorf("verifier needs the fix history: %w", err)
	}
	status := VerificationCINotRequired
	if provider.options.CIRequired {
		status, err = provider.options.CI.TaskCIStatus(ctx, base.task)
		if err != nil {
			return nil, fmt.Errorf("read CI status for task %d: %w", taskID, err)
		}
	}
	return &VerifierContext{
		Project:       base.project,
		Task:          base.task,
		Plan:          plan,
		Delivery:      delivery,
		Review:        review,
		Fixes:         fixes,
		WorkDir:       base.workDir,
		CurrentBranch: base.task.BranchName,
		PRNumber:      base.task.PRNumber,
		PRHead:        base.task.BranchName,
		PRBase:        provider.options.BaseRef,
		CIRequired:    provider.options.CIRequired,
		CIStatus:      status,
		ExecutionID:   base.executionID,
	}, nil
}

var (
	_ PlannerContextProvider   = (*DurableDeliveryContextProvider)(nil)
	_ DeveloperContextProvider = (*DurableDeliveryContextProvider)(nil)
	_ ReviewerContextProvider  = (*DurableDeliveryContextProvider)(nil)
	_ FixerContextProvider     = (*DurableDeliveryContextProvider)(nil)
	_ VerifierContextProvider  = (*DurableDeliveryContextProvider)(nil)
)
