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

func TestDeveloperBuildsScopedWorkspaceWriteRequest(t *testing.T) {
	t.Parallel()
	raw := validDeveloperOutput(OutputCompleted)
	provider := successfulDeveloperEngine(raw)
	developmentContext := validDeveloperContext(t)
	developmentContext.ExecutionID = 52
	developmentContext.Task.PRNumber = 40
	planBefore := string(developmentContext.Plan)
	environment := map[string]string{"MADAR_TRACE": "developer"}
	loadCalls := 0
	emitted := 0
	event := engine.Event{Type: engine.EventFileChanged, Message: "internal/mode/developer.go"}
	provider.emitEvent = &event
	developer, err := NewDeveloper(
		provider,
		DeveloperContextProviderFunc(func(
			_ context.Context,
			projectID, taskID int64,
		) (*DeveloperContext, error) {
			loadCalls++
			if projectID != 7 || taskID != 11 {
				t.Fatalf("context IDs = %d/%d", projectID, taskID)
			}
			return developmentContext, nil
		}),
		DeveloperOptions{
			Model:       "implementation-model",
			Timeout:     5 * time.Minute,
			MaxTurns:    20,
			Environment: environment,
			Emit: func(got engine.Event) error {
				emitted++
				if got.Message != event.Message {
					t.Fatalf("event = %#v", got)
				}
				return nil
			},
		},
	)
	if err != nil {
		t.Fatalf("NewDeveloper: %v", err)
	}
	environment["MADAR_TRACE"] = "mutated"

	got, err := developer.Run(context.Background(), developerRequest())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(got) != string(raw) {
		t.Fatalf("output = %s, want %s", got, raw)
	}
	if loadCalls != 1 || emitted != 1 {
		t.Fatalf("load calls = %d, emitted = %d", loadCalls, emitted)
	}
	if string(developmentContext.Plan) != planBefore {
		t.Fatal("Developer mutated the stored Planner artifact")
	}
	request := provider.lastRequest(t)
	if request.ExecutionID != 52 ||
		request.WorkDir != developmentContext.WorkDir ||
		request.Mode != string(workflow.ModeDeveloper) ||
		request.Model != "implementation-model" ||
		request.Timeout != 5*time.Minute ||
		request.MaxTurns != 20 {
		t.Fatalf("engine request metadata = %#v", request)
	}
	if request.SessionID != "" || request.ResumeSessionID != "" {
		t.Fatalf("developer unexpectedly resumed a session: %#v", request)
	}
	if request.Policy.Sandbox != "workspace-write" ||
		request.Policy.ApprovalPolicy != "never" ||
		request.Policy.SkipPermissions {
		t.Fatalf("developer policy = %#v", request.Policy)
	}
	if request.Environment["MADAR_TRACE"] != "developer" {
		t.Fatalf("environment was not defensively copied: %#v", request.Environment)
	}
	if string(request.OutputSchema) != string(developer.definition.OutputSchema) {
		t.Fatal("developer did not pass the canonical output schema")
	}
	for _, required := range []string{
		"exactly one selected and approved task",
		"current branch matches assigned_branch",
		"Follow the approved plan",
		"smallest coherent implementation",
		"Do not perform unrelated refactors",
		"verification commands and relevant regression tests",
		"Commit all intended task changes and push only the assigned branch",
		"exactly one pull request",
		"Record material discoveries",
		"status=needs_input",
		"actual commit_sha",
		`"assigned_branch": "madar/issue-127"`,
		`"existing_pr_number": 40`,
		`"acceptance_criteria"`,
	} {
		if !strings.Contains(request.Prompt, required) {
			t.Errorf("prompt missing %q", required)
		}
	}

	definition := developer.Definition()
	if definition.Name != workflow.ModeDeveloper || definition.FreshSession {
		t.Fatalf("definition = %#v", definition)
	}
	definition.OutputSchema[0] = '!'
	if developer.Definition().OutputSchema[0] == '!' {
		t.Fatal("Definition leaked mutable schema storage")
	}
}

func TestDeveloperCompletedAdvancesFeatureToIndependentReview(t *testing.T) {
	t.Parallel()
	developer := mustDeveloper(
		t,
		successfulDeveloperEngine(validDeveloperOutput(OutputCompleted)),
		validDeveloperContextProvider(t),
	)
	reviewer := &registryTestMode{
		definition: mustBuiltinDefinition(t, workflow.ModeReviewer),
		output:     validReviewerOutput(OutputBlocked, false),
	}
	registry, err := NewRegistry(developer, reviewer)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	dispatcher, err := NewDispatcher(registry)
	if err != nil {
		t.Fatalf("NewDispatcher: %v", err)
	}
	controller := &developerTestController{status: domain.TaskDeveloping}
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
	if result.FinalStatus != domain.TaskBlocked {
		t.Fatalf("final status = %q", result.FinalStatus)
	}
	if len(result.ModesRun) != 2 ||
		result.ModesRun[0] != workflow.ModeDeveloper ||
		result.ModesRun[1] != workflow.ModeReviewer {
		t.Fatalf("modes run = %#v", result.ModesRun)
	}
	if len(controller.transitions) != 2 ||
		controller.transitions[0] != domain.TaskReviewing ||
		controller.transitions[1] != domain.TaskBlocked {
		t.Fatalf("transitions = %#v", controller.transitions)
	}
}

func TestDeveloperRejectsInvalidRequestsBeforeLoadingContext(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		request workflow.ModeRequest
	}{
		{"wrong mode", workflow.ModeRequest{ProjectID: 7, TaskID: 11, Mode: workflow.ModePlanner, Status: domain.TaskDeveloping}},
		{"missing project", workflow.ModeRequest{TaskID: 11, Mode: workflow.ModeDeveloper, Status: domain.TaskDeveloping}},
		{"missing task", workflow.ModeRequest{ProjectID: 7, Mode: workflow.ModeDeveloper, Status: domain.TaskDeveloping}},
		{"wrong state", workflow.ModeRequest{ProjectID: 7, TaskID: 11, Mode: workflow.ModeDeveloper, Status: domain.TaskPlanning}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			loadCalls := 0
			developer := mustDeveloper(
				t,
				successfulDeveloperEngine(validDeveloperOutput(OutputCompleted)),
				DeveloperContextProviderFunc(func(context.Context, int64, int64) (*DeveloperContext, error) {
					loadCalls++
					return validDeveloperContext(t), nil
				}),
			)
			if _, err := developer.Run(context.Background(), test.request); !errors.Is(err, ErrInvalidDeveloperRequest) {
				t.Fatalf("Run error = %v", err)
			}
			if loadCalls != 0 {
				t.Fatalf("context loaded %d times", loadCalls)
			}
		})
	}
}

func TestDeveloperRejectsInvalidContext(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*DeveloperContext)
	}{
		{"nil project", func(value *DeveloperContext) { value.Project = nil }},
		{"nil task", func(value *DeveloperContext) { value.Task = nil }},
		{"project mismatch", func(value *DeveloperContext) { value.Project.ID++ }},
		{"task mismatch", func(value *DeveloperContext) { value.Task.ID++ }},
		{"cross project task", func(value *DeveloperContext) { value.Task.ProjectID++ }},
		{"wrong task status", func(value *DeveloperContext) { value.Task.Status = domain.TaskPlanning }},
		{"missing branch", func(value *DeveloperContext) { value.Task.BranchName = "" }},
		{"missing current branch", func(value *DeveloperContext) { value.CurrentBranch = "" }},
		{"branch mismatch", func(value *DeveloperContext) { value.CurrentBranch = "madar/other" }},
		{"missing workdir", func(value *DeveloperContext) { value.WorkDir = "" }},
		{"relative workdir", func(value *DeveloperContext) { value.WorkDir = "relative" }},
		{"negative execution", func(value *DeveloperContext) { value.ExecutionID = -1 }},
		{"missing plan", func(value *DeveloperContext) { value.Plan = nil }},
		{"malformed plan", func(value *DeveloperContext) { value.Plan = json.RawMessage(`{`) }},
		{"incomplete plan", func(value *DeveloperContext) { value.Plan = validPlannerOutput(OutputNeedsInput) }},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			developmentContext := validDeveloperContext(t)
			test.mutate(developmentContext)
			provider := successfulDeveloperEngine(validDeveloperOutput(OutputCompleted))
			developer := mustDeveloper(
				t,
				provider,
				DeveloperContextProviderFunc(func(context.Context, int64, int64) (*DeveloperContext, error) {
					return developmentContext, nil
				}),
			)
			if _, err := developer.Run(context.Background(), developerRequest()); !errors.Is(err, ErrInvalidDeveloperContext) {
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

func TestDeveloperEngineAndResultFailures(t *testing.T) {
	t.Parallel()
	contextError := errors.New("snapshot unavailable")
	engineError := errors.New("provider unavailable")
	tests := []struct {
		name      string
		provider  *plannerTestEngine
		contexts  DeveloperContextProvider
		want      error
		cancelled bool
	}{
		{
			name:     "capabilities error",
			provider: &plannerTestEngine{capabilityErr: engineError},
			contexts: validDeveloperContextProvider(t),
			want:     engineError,
		},
		{
			name:     "missing structured output",
			provider: &plannerTestEngine{capabilities: engine.CapabilitySet{OutputSchema: true}},
			contexts: validDeveloperContextProvider(t),
			want:     ErrDeveloperUnsupported,
		},
		{
			name:     "missing schema support",
			provider: &plannerTestEngine{capabilities: engine.CapabilitySet{StructuredOutput: true}},
			contexts: validDeveloperContextProvider(t),
			want:     ErrDeveloperUnsupported,
		},
		{
			name:     "context error",
			provider: successfulDeveloperEngine(validDeveloperOutput(OutputCompleted)),
			contexts: DeveloperContextProviderFunc(func(context.Context, int64, int64) (*DeveloperContext, error) {
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
			contexts: validDeveloperContextProvider(t),
			want:     engineError,
		},
		{
			name:     "nil result",
			provider: &plannerTestEngine{capabilities: plannerCapabilities()},
			contexts: validDeveloperContextProvider(t),
			want:     ErrDeveloperResult,
		},
		{
			name: "failed result",
			provider: &plannerTestEngine{
				capabilities: plannerCapabilities(),
				result:       &engine.Result{Status: engine.ResultFailed},
			},
			contexts: validDeveloperContextProvider(t),
			want:     ErrDeveloperResult,
		},
		{
			name: "unknown result",
			provider: &plannerTestEngine{
				capabilities: plannerCapabilities(),
				result:       &engine.Result{},
			},
			contexts: validDeveloperContextProvider(t),
			want:     ErrDeveloperResult,
		},
		{
			name: "cancelled result",
			provider: &plannerTestEngine{
				capabilities: plannerCapabilities(),
				result:       &engine.Result{Status: engine.ResultCancelled},
			},
			contexts:  validDeveloperContextProvider(t),
			want:      context.Canceled,
			cancelled: true,
		},
		{
			name: "missing output",
			provider: &plannerTestEngine{
				capabilities: plannerCapabilities(),
				result:       &engine.Result{Status: engine.ResultCompleted},
			},
			contexts: validDeveloperContextProvider(t),
			want:     ErrDeveloperResult,
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
			contexts: validDeveloperContextProvider(t),
			want:     ErrDeveloperResult,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			developer := mustDeveloper(t, test.provider, test.contexts)
			_, err := developer.Run(context.Background(), developerRequest())
			if !errors.Is(err, test.want) {
				t.Fatalf("Run error = %v, want errors.Is %v", err, test.want)
			}
			if test.cancelled && engine.ClassOf(err) != engine.ErrorCancelled {
				t.Fatalf("error class = %q", engine.ClassOf(err))
			}
		})
	}
}

func TestDeveloperAcceptsValidatedOutputTextFallback(t *testing.T) {
	t.Parallel()
	raw := validDeveloperOutput(OutputCompleted)
	provider := &plannerTestEngine{
		capabilities: plannerCapabilities(),
		result: &engine.Result{
			Status:     engine.ResultCompleted,
			OutputText: "\n" + string(raw) + "\n",
		},
	}
	developer := mustDeveloper(t, provider, validDeveloperContextProvider(t))
	got, err := developer.Run(context.Background(), developerRequest())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(got) != string(raw) {
		t.Fatalf("output = %q, want %q", got, raw)
	}
}

func TestDeveloperConcurrentRuns(t *testing.T) {
	t.Parallel()
	provider := successfulDeveloperEngine(validDeveloperOutput(OutputCompleted))
	developmentContext := validDeveloperContext(t)
	developer := mustDeveloper(
		t,
		provider,
		DeveloperContextProviderFunc(func(context.Context, int64, int64) (*DeveloperContext, error) {
			return developmentContext, nil
		}),
	)

	const runCount = 32
	errs := make(chan error, runCount)
	var group sync.WaitGroup
	for index := 0; index < runCount; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := developer.Run(context.Background(), developerRequest())
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

func TestDeveloperPreservesPreRunCancellation(t *testing.T) {
	t.Parallel()
	provider := successfulDeveloperEngine(validDeveloperOutput(OutputCompleted))
	loadCalls := 0
	developer := mustDeveloper(
		t,
		provider,
		DeveloperContextProviderFunc(func(context.Context, int64, int64) (*DeveloperContext, error) {
			loadCalls++
			return validDeveloperContext(t), nil
		}),
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := developer.Run(ctx, developerRequest()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v", err)
	}
	if loadCalls != 0 {
		t.Fatalf("context loaded %d times", loadCalls)
	}
}

func TestNewDeveloperValidation(t *testing.T) {
	t.Parallel()
	contexts := validDeveloperContextProvider(t)
	provider := successfulDeveloperEngine(validDeveloperOutput(OutputCompleted))
	var nilProvider *plannerTestEngine
	var nilContexts *developerNilContextProvider
	var nilContextFunc DeveloperContextProviderFunc
	tests := []struct {
		name     string
		provider engine.Engine
		contexts DeveloperContextProvider
		options  DeveloperOptions
	}{
		{"nil engine", nil, contexts, DeveloperOptions{}},
		{"typed nil engine", nilProvider, contexts, DeveloperOptions{}},
		{"nil contexts", provider, nil, DeveloperOptions{}},
		{"typed nil contexts", provider, nilContexts, DeveloperOptions{}},
		{"typed nil context function", provider, nilContextFunc, DeveloperOptions{}},
		{"negative timeout", provider, contexts, DeveloperOptions{Timeout: -time.Second}},
		{"negative turns", provider, contexts, DeveloperOptions{MaxTurns: -1}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if developer, err := NewDeveloper(test.provider, test.contexts, test.options); err == nil || developer != nil {
				t.Fatalf("NewDeveloper = %#v, %v", developer, err)
			}
		})
	}
}

type developerNilContextProvider struct{}

func (*developerNilContextProvider) LoadDeveloperContext(
	context.Context,
	int64,
	int64,
) (*DeveloperContext, error) {
	panic("unexpected call")
}

type developerTestController struct {
	status      domain.TaskStatus
	transitions []domain.TaskStatus
}

func (controller *developerTestController) TaskStatus(int64, int64) (domain.TaskStatus, error) {
	return controller.status, nil
}

func (controller *developerTestController) ApplyTaskTransition(
	_, _ int64,
	target domain.TaskStatus,
	_ workflow.TaskTransitionEvidence,
) (domain.TaskStatus, error) {
	controller.status = target
	controller.transitions = append(controller.transitions, target)
	return target, nil
}

func successfulDeveloperEngine(raw json.RawMessage) *plannerTestEngine {
	return &plannerTestEngine{
		capabilities: plannerCapabilities(),
		result: &engine.Result{
			Status:     engine.ResultCompleted,
			OutputJSON: append(json.RawMessage(nil), raw...),
		},
	}
}

func validDeveloperContextProvider(t *testing.T) DeveloperContextProvider {
	t.Helper()
	return DeveloperContextProviderFunc(func(context.Context, int64, int64) (*DeveloperContext, error) {
		return validDeveloperContext(t), nil
	})
}

func validDeveloperContext(t *testing.T) *DeveloperContext {
	t.Helper()
	project := domain.NewProject("owner/repo", "Madar", "Ship v2", "Sequential delivery")
	project.ID = 7
	project.State = domain.ProjectExecuting
	task := domain.NewTask(project.ID, "Implement developer", "Deliver one planned task")
	task.ID = 11
	task.IssueNumber = 127
	task.Status = domain.TaskDeveloping
	task.Sequence = 3
	task.SelectedReason = "planner completed"
	task.BranchName = "madar/issue-127"
	task.DependencyState = "ready"
	plan := validPlannerOutput(OutputCompleted)
	return &DeveloperContext{
		Project:       project,
		Task:          task,
		Plan:          append(json.RawMessage("\n"), append(plan, '\n')...),
		WorkDir:       t.TempDir(),
		CurrentBranch: task.BranchName,
	}
}

func developerRequest() workflow.ModeRequest {
	return workflow.ModeRequest{
		ProjectID: 7,
		TaskID:    11,
		Mode:      workflow.ModeDeveloper,
		Status:    domain.TaskDeveloping,
	}
}

func mustDeveloper(
	t *testing.T,
	provider engine.Engine,
	contexts DeveloperContextProvider,
) *Developer {
	t.Helper()
	developer, err := NewDeveloper(provider, contexts, DeveloperOptions{})
	if err != nil {
		t.Fatalf("NewDeveloper: %v", err)
	}
	return developer
}

var _ DeveloperContextProvider = (*developerNilContextProvider)(nil)
