package mode

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	"github.com/eslam-mahmoud/go-ai-agent/internal/engine"
	"github.com/eslam-mahmoud/go-ai-agent/internal/workflow"
)

type plannerTestEngine struct {
	mu            sync.Mutex
	name          string
	capabilities  engine.CapabilitySet
	capabilityErr error
	result        *engine.Result
	runErr        error
	requests      []engine.RunRequest
	emitEvent     *engine.Event
}

func (provider *plannerTestEngine) Name() string {
	if provider.name == "" {
		return "test-engine"
	}
	return provider.name
}

func (provider *plannerTestEngine) Capabilities(context.Context) (engine.CapabilitySet, error) {
	return provider.capabilities, provider.capabilityErr
}

func (provider *plannerTestEngine) Run(
	_ context.Context,
	request engine.RunRequest,
	emit func(engine.Event) error,
) (*engine.Result, error) {
	provider.mu.Lock()
	provider.requests = append(provider.requests, request)
	provider.mu.Unlock()
	if provider.emitEvent != nil && emit != nil {
		if err := emit(*provider.emitEvent); err != nil {
			return nil, err
		}
	}
	return provider.result, provider.runErr
}

func (provider *plannerTestEngine) Resume(
	context.Context,
	engine.RunRequest,
	func(engine.Event) error,
) (*engine.Result, error) {
	return nil, errors.New("unexpected resume")
}

func (provider *plannerTestEngine) Cancel(context.Context, string) error {
	return errors.New("unexpected cancel")
}

func (provider *plannerTestEngine) lastRequest(t *testing.T) engine.RunRequest {
	t.Helper()
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.requests) == 0 {
		t.Fatal("planner engine was not run")
	}
	return provider.requests[len(provider.requests)-1]
}

func TestPlannerBuildsReadOnlyStructuredRequest(t *testing.T) {
	t.Parallel()
	raw := validPlannerOutput(OutputCompleted)
	provider := successfulPlannerEngine(raw)
	event := engine.Event{Type: engine.EventProgress, Message: "inspecting"}
	provider.emitEvent = &event
	planningContext := validPlannerContext(t)
	planningContext.ExecutionID = 44
	// Deliberately reverse input order; prompt order must remain deterministic.
	planningContext.Backlog[0], planningContext.Backlog[1] =
		planningContext.Backlog[1], planningContext.Backlog[0]
	loadCalls := 0
	contexts := PlannerContextProviderFunc(func(
		_ context.Context,
		projectID, taskID int64,
	) (*PlannerContext, error) {
		loadCalls++
		if projectID != 7 || taskID != 11 {
			t.Fatalf("context IDs = %d/%d", projectID, taskID)
		}
		return planningContext, nil
	})
	emitted := 0
	environment := map[string]string{"MADAR_TRACE": "planner"}
	planner, err := NewPlanner(provider, contexts, PlannerOptions{
		Model:       "planning-model",
		Timeout:     2 * time.Minute,
		MaxTurns:    8,
		Environment: environment,
		Emit: func(got engine.Event) error {
			emitted++
			if got.Message != event.Message {
				t.Fatalf("emitted event = %#v", got)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewPlanner: %v", err)
	}
	environment["MADAR_TRACE"] = "mutated"

	got, err := planner.Run(context.Background(), plannerRequest())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(got) != string(raw) {
		t.Fatalf("output = %s, want %s", got, raw)
	}
	if loadCalls != 1 || emitted != 1 {
		t.Fatalf("load calls = %d, emitted = %d", loadCalls, emitted)
	}
	request := provider.lastRequest(t)
	if request.ExecutionID != 44 ||
		request.WorkDir != planningContext.WorkDir ||
		request.Mode != string(workflow.ModePlanner) ||
		request.Model != "planning-model" ||
		request.Timeout != 2*time.Minute ||
		request.MaxTurns != 8 {
		t.Fatalf("engine request metadata = %#v", request)
	}
	if request.ResumeSessionID != "" || request.SessionID != "" {
		t.Fatalf("planner unexpectedly resumed a session: %#v", request)
	}
	if request.Policy.Sandbox != "read-only" ||
		request.Policy.ApprovalPolicy != "never" ||
		request.Policy.SkipPermissions {
		t.Fatalf("planner policy = %#v", request.Policy)
	}
	if request.Environment["MADAR_TRACE"] != "planner" {
		t.Fatalf("environment was not defensively copied: %#v", request.Environment)
	}
	if string(request.OutputSchema) != string(planner.definition.OutputSchema) {
		t.Fatal("planner did not pass the canonical output schema")
	}
	for _, required := range []string{
		"Operate in read-only mode",
		"Inspect the relevant code",
		"Confirm prerequisite and backlog dependencies",
		"Produce concrete, testable acceptance criteria",
		"exact verification commands",
		"split_recommended=true",
		"status=needs_input",
		`"repository": "owner/repo"`,
		`"title": "Implement planner"`,
		`"dependency_state": "ready"`,
	} {
		if !strings.Contains(request.Prompt, required) {
			t.Errorf("prompt missing %q", required)
		}
	}
	backlogStart := strings.Index(request.Prompt, `"ordered_backlog"`)
	if backlogStart < 0 {
		t.Fatal("prompt has no ordered backlog")
	}
	backlogPrompt := request.Prompt[backlogStart:]
	completed := strings.Index(backlogPrompt, `"title": "Foundation"`)
	selected := strings.Index(backlogPrompt, `"title": "Implement planner"`)
	if completed < 0 || selected < 0 || completed > selected {
		t.Fatalf("backlog is not ordered by sequence:\n%s", request.Prompt)
	}

	definition := planner.Definition()
	if definition.Name != workflow.ModePlanner || definition.FreshSession {
		t.Fatalf("definition = %#v", definition)
	}
	definition.OutputSchema[0] = '!'
	if planner.Definition().OutputSchema[0] == '!' {
		t.Fatal("Definition leaked mutable schema storage")
	}
}

func TestPlannerNeedsInputStopsFeatureBeforeDevelopment(t *testing.T) {
	t.Parallel()
	raw := validPlannerOutput(OutputNeedsInput)
	provider := successfulPlannerEngine(raw)
	planner, err := NewPlanner(
		provider,
		PlannerContextProviderFunc(func(context.Context, int64, int64) (*PlannerContext, error) {
			return validPlannerContext(t), nil
		}),
		PlannerOptions{},
	)
	if err != nil {
		t.Fatalf("NewPlanner: %v", err)
	}
	registry, err := NewRegistry(planner)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	dispatcher, err := NewDispatcher(registry)
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	controller := &plannerTestController{status: domain.TaskPlanning}
	feature, err := workflow.NewFeatureWorkflow(
		controller,
		dispatcher,
		workflow.FeatureOptions{},
	)
	if err != nil {
		t.Fatalf("NewFeatureWorkflow: %v", err)
	}

	result, err := feature.Run(context.Background(), 7, 11)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.FinalStatus != domain.TaskWaitingInput {
		t.Fatalf("final status = %q", result.FinalStatus)
	}
	if len(result.ModesRun) != 1 || result.ModesRun[0] != workflow.ModePlanner {
		t.Fatalf("modes run = %#v", result.ModesRun)
	}
	if controller.status != domain.TaskWaitingInput {
		t.Fatalf("controller status = %q", controller.status)
	}
}

func TestPlannerRejectsInvalidRequestsBeforeLoadingContext(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		request workflow.ModeRequest
	}{
		{"wrong mode", workflow.ModeRequest{ProjectID: 7, TaskID: 11, Mode: workflow.ModeDeveloper, Status: domain.TaskPlanning}},
		{"missing project", workflow.ModeRequest{TaskID: 11, Mode: workflow.ModePlanner, Status: domain.TaskPlanning}},
		{"missing task", workflow.ModeRequest{ProjectID: 7, Mode: workflow.ModePlanner, Status: domain.TaskPlanning}},
		{"wrong state", workflow.ModeRequest{ProjectID: 7, TaskID: 11, Mode: workflow.ModePlanner, Status: domain.TaskSelected}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			loadCalls := 0
			planner := mustPlanner(t, successfulPlannerEngine(validPlannerOutput(OutputCompleted)),
				PlannerContextProviderFunc(func(context.Context, int64, int64) (*PlannerContext, error) {
					loadCalls++
					return validPlannerContext(t), nil
				}))
			if _, err := planner.Run(context.Background(), test.request); !errors.Is(err, ErrInvalidPlannerRequest) {
				t.Fatalf("Run error = %v", err)
			}
			if loadCalls != 0 {
				t.Fatalf("context loaded %d times", loadCalls)
			}
		})
	}
}

func TestPlannerRejectsInvalidContext(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*PlannerContext)
	}{
		{"nil project", func(value *PlannerContext) { value.Project = nil }},
		{"nil task", func(value *PlannerContext) { value.Task = nil }},
		{"project mismatch", func(value *PlannerContext) { value.Project.ID++ }},
		{"task mismatch", func(value *PlannerContext) { value.Task.ID++ }},
		{"cross project task", func(value *PlannerContext) { value.Task.ProjectID++ }},
		{"wrong task status", func(value *PlannerContext) { value.Task.Status = domain.TaskSelected }},
		{"missing repo", func(value *PlannerContext) { value.Project.Repo = "" }},
		{"missing workdir", func(value *PlannerContext) { value.WorkDir = "" }},
		{"relative workdir", func(value *PlannerContext) { value.WorkDir = "relative" }},
		{"negative execution", func(value *PlannerContext) { value.ExecutionID = -1 }},
		{"nil backlog task", func(value *PlannerContext) { value.Backlog[0] = nil }},
		{"cross project backlog", func(value *PlannerContext) { value.Backlog[0].ProjectID++ }},
		{"selected missing", func(value *PlannerContext) { value.Backlog = value.Backlog[:1] }},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			planningContext := validPlannerContext(t)
			test.mutate(planningContext)
			provider := successfulPlannerEngine(validPlannerOutput(OutputCompleted))
			planner := mustPlanner(t, provider,
				PlannerContextProviderFunc(func(context.Context, int64, int64) (*PlannerContext, error) {
					return planningContext, nil
				}))
			if _, err := planner.Run(context.Background(), plannerRequest()); !errors.Is(err, ErrInvalidPlannerContext) {
				t.Fatalf("Run error = %v", err)
			}
			provider.mu.Lock()
			runCount := len(provider.requests)
			provider.mu.Unlock()
			if runCount != 0 {
				t.Fatalf("engine ran %d times", runCount)
			}
		})
	}
}

func TestPlannerEngineAndResultFailures(t *testing.T) {
	t.Parallel()
	contextError := errors.New("snapshot unavailable")
	engineError := errors.New("provider unavailable")
	tests := []struct {
		name      string
		provider  *plannerTestEngine
		contexts  PlannerContextProvider
		want      error
		cancelled bool
	}{
		{
			name:     "capabilities error",
			provider: &plannerTestEngine{capabilityErr: engineError},
			contexts: validPlannerContextProvider(t),
			want:     engineError,
		},
		{
			name:     "missing structured output",
			provider: &plannerTestEngine{capabilities: engine.CapabilitySet{OutputSchema: true}},
			contexts: validPlannerContextProvider(t),
			want:     ErrPlannerUnsupported,
		},
		{
			name:     "missing schema support",
			provider: &plannerTestEngine{capabilities: engine.CapabilitySet{StructuredOutput: true}},
			contexts: validPlannerContextProvider(t),
			want:     ErrPlannerUnsupported,
		},
		{
			name:     "context error",
			provider: successfulPlannerEngine(validPlannerOutput(OutputCompleted)),
			contexts: PlannerContextProviderFunc(func(context.Context, int64, int64) (*PlannerContext, error) {
				return nil, contextError
			}),
			want: contextError,
		},
		{
			name: "engine error",
			provider: &plannerTestEngine{
				capabilities: plannerCapabilities(),
				runErr:       engineError,
			},
			contexts: validPlannerContextProvider(t),
			want:     engineError,
		},
		{
			name:     "nil result",
			provider: &plannerTestEngine{capabilities: plannerCapabilities()},
			contexts: validPlannerContextProvider(t),
			want:     ErrPlannerResult,
		},
		{
			name: "failed result",
			provider: &plannerTestEngine{
				capabilities: plannerCapabilities(),
				result:       &engine.Result{Status: engine.ResultFailed},
			},
			contexts: validPlannerContextProvider(t),
			want:     ErrPlannerResult,
		},
		{
			name: "unknown result",
			provider: &plannerTestEngine{
				capabilities: plannerCapabilities(),
				result:       &engine.Result{},
			},
			contexts: validPlannerContextProvider(t),
			want:     ErrPlannerResult,
		},
		{
			name: "cancelled result",
			provider: &plannerTestEngine{
				capabilities: plannerCapabilities(),
				result:       &engine.Result{Status: engine.ResultCancelled},
			},
			contexts:  validPlannerContextProvider(t),
			want:      context.Canceled,
			cancelled: true,
		},
		{
			name: "missing output",
			provider: &plannerTestEngine{
				capabilities: plannerCapabilities(),
				result:       &engine.Result{Status: engine.ResultCompleted},
			},
			contexts: validPlannerContextProvider(t),
			want:     ErrPlannerResult,
		},
		{
			name: "schema mismatch",
			provider: &plannerTestEngine{
				capabilities: plannerCapabilities(),
				result: &engine.Result{
					Status:     engine.ResultCompleted,
					OutputJSON: json.RawMessage(`{"status":"completed"}`),
				},
			},
			contexts: validPlannerContextProvider(t),
			want:     ErrPlannerResult,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			planner := mustPlanner(t, test.provider, test.contexts)
			_, err := planner.Run(context.Background(), plannerRequest())
			if !errors.Is(err, test.want) {
				t.Fatalf("Run error = %v, want errors.Is %v", err, test.want)
			}
			if test.cancelled && engine.ClassOf(err) != engine.ErrorCancelled {
				t.Fatalf("error class = %q", engine.ClassOf(err))
			}
		})
	}
}

func TestPlannerAcceptsValidatedOutputTextFallback(t *testing.T) {
	t.Parallel()
	raw := validPlannerOutput(OutputCompleted)
	provider := &plannerTestEngine{
		capabilities: plannerCapabilities(),
		result: &engine.Result{
			Status:     engine.ResultCompleted,
			OutputText: " \n" + string(raw) + "\n ",
		},
	}
	planner := mustPlanner(t, provider, validPlannerContextProvider(t))
	got, err := planner.Run(context.Background(), plannerRequest())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(got) != string(raw) {
		t.Fatalf("output = %q, want %q", got, raw)
	}
}

func TestPlannerConcurrentRuns(t *testing.T) {
	t.Parallel()
	provider := successfulPlannerEngine(validPlannerOutput(OutputCompleted))
	planningContext := validPlannerContext(t)
	planner := mustPlanner(t, provider,
		PlannerContextProviderFunc(func(context.Context, int64, int64) (*PlannerContext, error) {
			return planningContext, nil
		}))

	const runCount = 32
	errs := make(chan error, runCount)
	var group sync.WaitGroup
	for index := 0; index < runCount; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := planner.Run(context.Background(), plannerRequest())
			errs <- err
		}()
	}
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Run: %v", err)
		}
	}
	provider.mu.Lock()
	gotRuns := len(provider.requests)
	provider.mu.Unlock()
	if gotRuns != runCount {
		t.Fatalf("engine runs = %d, want %d", gotRuns, runCount)
	}
}

func TestPlannerPreservesPreRunCancellation(t *testing.T) {
	t.Parallel()
	provider := successfulPlannerEngine(validPlannerOutput(OutputCompleted))
	loadCalls := 0
	planner := mustPlanner(t, provider,
		PlannerContextProviderFunc(func(context.Context, int64, int64) (*PlannerContext, error) {
			loadCalls++
			return validPlannerContext(t), nil
		}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := planner.Run(ctx, plannerRequest()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v", err)
	}
	if loadCalls != 0 {
		t.Fatalf("context loaded %d times", loadCalls)
	}
}

func TestNewPlannerValidation(t *testing.T) {
	t.Parallel()
	contexts := validPlannerContextProvider(t)
	provider := successfulPlannerEngine(validPlannerOutput(OutputCompleted))
	var nilProvider *plannerTestEngine
	var nilContexts *plannerNilContextProvider
	var nilContextFunc PlannerContextProviderFunc
	tests := []struct {
		name     string
		provider engine.Engine
		contexts PlannerContextProvider
		options  PlannerOptions
	}{
		{"nil engine", nil, contexts, PlannerOptions{}},
		{"typed nil engine", nilProvider, contexts, PlannerOptions{}},
		{"nil contexts", provider, nil, PlannerOptions{}},
		{"typed nil contexts", provider, nilContexts, PlannerOptions{}},
		{"typed nil context function", provider, nilContextFunc, PlannerOptions{}},
		{"negative timeout", provider, contexts, PlannerOptions{Timeout: -time.Second}},
		{"negative turns", provider, contexts, PlannerOptions{MaxTurns: -1}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if planner, err := NewPlanner(test.provider, test.contexts, test.options); err == nil || planner != nil {
				t.Fatalf("NewPlanner = %#v, %v", planner, err)
			}
		})
	}
}

type plannerNilContextProvider struct{}

func (*plannerNilContextProvider) LoadPlannerContext(
	context.Context,
	int64,
	int64,
) (*PlannerContext, error) {
	panic("unexpected call")
}

type plannerTestController struct {
	status domain.TaskStatus
}

func (controller *plannerTestController) TaskStatus(int64, int64) (domain.TaskStatus, error) {
	return controller.status, nil
}

func (controller *plannerTestController) ApplyTaskTransition(
	_, _ int64,
	target domain.TaskStatus,
	_ workflow.TaskTransitionEvidence,
) (domain.TaskStatus, error) {
	controller.status = target
	return target, nil
}

func plannerCapabilities() engine.CapabilitySet {
	return engine.CapabilitySet{
		StructuredOutput: true,
		OutputSchema:     true,
	}
}

func successfulPlannerEngine(raw json.RawMessage) *plannerTestEngine {
	return &plannerTestEngine{
		capabilities: plannerCapabilities(),
		result: &engine.Result{
			Status:     engine.ResultCompleted,
			OutputJSON: append(json.RawMessage(nil), raw...),
		},
	}
}

func validPlannerContextProvider(t *testing.T) PlannerContextProvider {
	t.Helper()
	return PlannerContextProviderFunc(func(context.Context, int64, int64) (*PlannerContext, error) {
		return validPlannerContext(t), nil
	})
}

func validPlannerContext(t *testing.T) *PlannerContext {
	t.Helper()
	project := domain.NewProject("owner/repo", "Madar", "Ship v2", "Sequential delivery")
	project.ID = 7
	project.State = domain.ProjectExecuting
	task := domain.NewTask(project.ID, "Implement planner", "Plan selected work before coding")
	task.ID = 11
	task.IssueNumber = 125
	task.Status = domain.TaskPlanning
	task.Sequence = 2
	task.SelectedReason = "next dependency"
	task.BranchName = "madar/issue-125"
	task.DependencyState = "ready"
	foundation := domain.NewTask(project.ID, "Foundation", "Provide mode registry")
	foundation.ID = 10
	foundation.Status = domain.TaskCompleted
	foundation.Sequence = 1
	return &PlannerContext{
		Project: project,
		Task:    task,
		Backlog: []*domain.Task{foundation, task},
		WorkDir: t.TempDir(),
	}
}

func plannerRequest() workflow.ModeRequest {
	return workflow.ModeRequest{
		ProjectID: 7,
		TaskID:    11,
		Mode:      workflow.ModePlanner,
		Status:    domain.TaskPlanning,
	}
}

func mustPlanner(
	t *testing.T,
	provider engine.Engine,
	contexts PlannerContextProvider,
) *Planner {
	t.Helper()
	planner, err := NewPlanner(provider, contexts, PlannerOptions{})
	if err != nil {
		t.Fatalf("NewPlanner: %v", err)
	}
	return planner
}

var _ engine.Engine = (*plannerTestEngine)(nil)
var _ PlannerContextProvider = (*plannerNilContextProvider)(nil)
