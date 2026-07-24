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

func TestFixerBuildsBoundedScopedWriteRequest(t *testing.T) {
	t.Parallel()
	raw := validFixerOutput(OutputCompleted)
	provider := successfulFixerEngine(raw)
	fixContext := validFixerContext(t)
	fixContext.ExecutionID = 72
	planBefore := string(fixContext.Plan)
	deliveryBefore := string(fixContext.Delivery)
	reviewBefore := string(fixContext.Review)
	environment := map[string]string{"MADAR_TRACE": "fixer"}
	emitted := 0
	event := engine.Event{Type: engine.EventFileChanged, Message: "internal/workflow/feature.go"}
	provider.emitEvent = &event
	fixer, err := NewFixer(
		provider,
		FixerContextProviderFunc(func(
			_ context.Context,
			projectID, taskID int64,
		) (*FixerContext, error) {
			if projectID != 7 || taskID != 11 {
				t.Fatalf("context IDs = %d/%d", projectID, taskID)
			}
			return fixContext, nil
		}),
		FixerOptions{
			Model:       "repair-model",
			Timeout:     4 * time.Minute,
			MaxTurns:    16,
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
		t.Fatalf("NewFixer: %v", err)
	}
	environment["MADAR_TRACE"] = "mutated"

	got, err := fixer.Run(context.Background(), fixerRequest())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(got) != string(raw) || emitted != 1 {
		t.Fatalf("output=%s emitted=%d", got, emitted)
	}
	if string(fixContext.Plan) != planBefore ||
		string(fixContext.Delivery) != deliveryBefore ||
		string(fixContext.Review) != reviewBefore {
		t.Fatal("Fixer mutated an upstream artifact")
	}
	request := provider.lastRequest(t)
	if request.ExecutionID != 72 ||
		request.WorkDir != fixContext.WorkDir ||
		request.Mode != string(workflow.ModeFixer) ||
		request.Model != "repair-model" ||
		request.Timeout != 4*time.Minute ||
		request.MaxTurns != 16 {
		t.Fatalf("engine request metadata = %#v", request)
	}
	if request.Policy.Sandbox != "workspace-write" ||
		request.Policy.ApprovalPolicy != "never" ||
		request.Policy.SkipPermissions {
		t.Fatalf("fixer policy = %#v", request.Policy)
	}
	if request.Environment["MADAR_TRACE"] != "fixer" {
		t.Fatalf("environment was not defensively copied: %#v", request.Environment)
	}
	for _, required := range []string{
		"one bounded repair cycle",
		"Address only the blocking_findings",
		"focused regression tests",
		"Do not implement future_improvements",
		"push only the assigned branch",
		"never open a duplicate pull request",
		"actual commit SHA",
		`"fix_cycle": 1`,
		`"max_fix_cycles": 2`,
		`"blocking_findings"`,
	} {
		if !strings.Contains(request.Prompt, required) {
			t.Errorf("prompt missing %q", required)
		}
	}
	definition := fixer.Definition()
	if definition.Name != workflow.ModeFixer || definition.FreshSession {
		t.Fatalf("definition = %#v", definition)
	}
}

func TestFixerRegistryWorkflowIntegration(t *testing.T) {
	t.Parallel()
	provider := successfulFixerEngine(validFixerOutput(OutputCompleted))
	fixer := mustFixer(t, provider, validFixerContextProvider(t))
	reviewer := &registryTestMode{
		definition: mustBuiltinDefinition(t, workflow.ModeReviewer),
		output:     validReviewerOutput(OutputBlocked, false),
	}
	registry, err := NewRegistry(fixer, reviewer)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, _ := NewDispatcher(registry)
	controller := &developerTestController{status: domain.TaskFixing}
	feature, err := workflow.NewFeatureWorkflow(
		controller,
		dispatcher,
		workflow.FeatureOptions{MaxReviewFixCycles: 2},
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := feature.Run(context.Background(), 7, 11)
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalStatus != domain.TaskBlocked ||
		result.ReviewFixCycles != 1 ||
		len(result.ModesRun) != 2 ||
		result.ModesRun[0] != workflow.ModeFixer ||
		result.ModesRun[1] != workflow.ModeReviewer {
		t.Fatalf("result = %#v", result)
	}
	request := provider.lastRequest(t)
	if !strings.Contains(request.Prompt, `"fix_cycle": 1`) ||
		!strings.Contains(request.Prompt, `"max_fix_cycles": 2`) {
		t.Fatalf("workflow budget missing from prompt:\n%s", request.Prompt)
	}
}

func TestFixerRejectsInvalidRequestsBeforeLoadingContext(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		request workflow.ModeRequest
	}{
		{"wrong mode", workflow.ModeRequest{ProjectID: 7, TaskID: 11, Mode: workflow.ModeReviewer, Status: domain.TaskFixing, FixCycle: 1, MaxFixCycles: 2}},
		{"wrong state", workflow.ModeRequest{ProjectID: 7, TaskID: 11, Mode: workflow.ModeFixer, Status: domain.TaskReviewing, FixCycle: 1, MaxFixCycles: 2}},
		{"missing project", workflow.ModeRequest{TaskID: 11, Mode: workflow.ModeFixer, Status: domain.TaskFixing, FixCycle: 1, MaxFixCycles: 2}},
		{"missing task", workflow.ModeRequest{ProjectID: 7, Mode: workflow.ModeFixer, Status: domain.TaskFixing, FixCycle: 1, MaxFixCycles: 2}},
		{"missing max", workflow.ModeRequest{ProjectID: 7, TaskID: 11, Mode: workflow.ModeFixer, Status: domain.TaskFixing, FixCycle: 1}},
		{"missing cycle", workflow.ModeRequest{ProjectID: 7, TaskID: 11, Mode: workflow.ModeFixer, Status: domain.TaskFixing, MaxFixCycles: 2}},
		{"over limit", workflow.ModeRequest{ProjectID: 7, TaskID: 11, Mode: workflow.ModeFixer, Status: domain.TaskFixing, FixCycle: 3, MaxFixCycles: 2}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			loadCalls := 0
			fixer := mustFixer(
				t,
				successfulFixerEngine(validFixerOutput(OutputCompleted)),
				FixerContextProviderFunc(func(context.Context, int64, int64) (*FixerContext, error) {
					loadCalls++
					return validFixerContext(t), nil
				}),
			)
			if _, err := fixer.Run(context.Background(), test.request); !errors.Is(err, ErrInvalidFixerRequest) {
				t.Fatalf("Run error = %v", err)
			}
			if loadCalls != 0 {
				t.Fatalf("context loaded %d times", loadCalls)
			}
		})
	}
}

func TestFixerRejectsInvalidContext(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*FixerContext)
	}{
		{"nil project", func(value *FixerContext) { value.Project = nil }},
		{"nil task", func(value *FixerContext) { value.Task = nil }},
		{"project mismatch", func(value *FixerContext) { value.Project.ID++ }},
		{"task mismatch", func(value *FixerContext) { value.Task.ID++ }},
		{"cross project", func(value *FixerContext) { value.Task.ProjectID++ }},
		{"wrong state", func(value *FixerContext) { value.Task.Status = domain.TaskReviewing }},
		{"missing branch", func(value *FixerContext) { value.Task.BranchName = "" }},
		{"branch mismatch", func(value *FixerContext) { value.CurrentBranch = "madar/other" }},
		{"missing workdir", func(value *FixerContext) { value.WorkDir = "" }},
		{"relative workdir", func(value *FixerContext) { value.WorkDir = "relative" }},
		{"negative execution", func(value *FixerContext) { value.ExecutionID = -1 }},
		{"missing plan", func(value *FixerContext) { value.Plan = nil }},
		{"incomplete plan", func(value *FixerContext) { value.Plan = validPlannerOutput(OutputNeedsInput) }},
		{"missing delivery", func(value *FixerContext) { value.Delivery = nil }},
		{"incomplete delivery", func(value *FixerContext) { value.Delivery = validDeveloperOutput(OutputNeedsInput) }},
		{"missing review", func(value *FixerContext) { value.Review = nil }},
		{"review without findings", func(value *FixerContext) { value.Review = validReviewerOutput(OutputCompleted, false) }},
		{"incomplete review", func(value *FixerContext) { value.Review = validReviewerOutput(OutputBlocked, true) }},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixContext := validFixerContext(t)
			test.mutate(fixContext)
			provider := successfulFixerEngine(validFixerOutput(OutputCompleted))
			fixer := mustFixer(
				t,
				provider,
				FixerContextProviderFunc(func(context.Context, int64, int64) (*FixerContext, error) {
					return fixContext, nil
				}),
			)
			if _, err := fixer.Run(context.Background(), fixerRequest()); !errors.Is(err, ErrInvalidFixerContext) {
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

func TestFixerRejectsIncompleteCompletedEvidence(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"addressed_findings", "changed_files", "tests"} {
		field := field
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			var output map[string]any
			if err := json.Unmarshal(validFixerOutput(OutputCompleted), &output); err != nil {
				t.Fatal(err)
			}
			output[field] = []any{}
			fixer := mustFixer(
				t,
				successfulFixerEngine(mustJSON(output)),
				validFixerContextProvider(t),
			)
			if _, err := fixer.Run(context.Background(), fixerRequest()); !errors.Is(err, ErrFixerResult) {
				t.Fatalf("Run error = %v", err)
			}
		})
	}
}

func TestFixerEngineAndResultFailures(t *testing.T) {
	t.Parallel()
	contextError := errors.New("snapshot unavailable")
	engineError := errors.New("provider unavailable")
	tests := []struct {
		name      string
		provider  *plannerTestEngine
		contexts  FixerContextProvider
		want      error
		cancelled bool
	}{
		{"capabilities", &plannerTestEngine{capabilityErr: engineError}, validFixerContextProvider(t), engineError, false},
		{"unsupported", &plannerTestEngine{capabilities: engine.CapabilitySet{StructuredOutput: true}}, validFixerContextProvider(t), ErrFixerUnsupported, false},
		{
			"context",
			successfulFixerEngine(validFixerOutput(OutputCompleted)),
			FixerContextProviderFunc(func(context.Context, int64, int64) (*FixerContext, error) {
				return nil, contextError
			}),
			contextError,
			false,
		},
		{"engine", &plannerTestEngine{capabilities: plannerCapabilities(), runErr: engineError}, validFixerContextProvider(t), engineError, false},
		{"nil result", &plannerTestEngine{capabilities: plannerCapabilities()}, validFixerContextProvider(t), ErrFixerResult, false},
		{"failed", &plannerTestEngine{capabilities: plannerCapabilities(), result: &engine.Result{Status: engine.ResultFailed}}, validFixerContextProvider(t), ErrFixerResult, false},
		{"cancelled", &plannerTestEngine{capabilities: plannerCapabilities(), result: &engine.Result{Status: engine.ResultCancelled}}, validFixerContextProvider(t), context.Canceled, true},
		{"missing output", &plannerTestEngine{capabilities: plannerCapabilities(), result: &engine.Result{Status: engine.ResultCompleted}}, validFixerContextProvider(t), ErrFixerResult, false},
		{
			"schema mismatch",
			&plannerTestEngine{
				capabilities: plannerCapabilities(),
				result: &engine.Result{
					Status:     engine.ResultCompleted,
					OutputJSON: json.RawMessage(`{"status":"completed"}`),
				},
			},
			validFixerContextProvider(t),
			ErrFixerResult,
			false,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixer := mustFixer(t, test.provider, test.contexts)
			_, err := fixer.Run(context.Background(), fixerRequest())
			if !errors.Is(err, test.want) {
				t.Fatalf("Run error = %v, want %v", err, test.want)
			}
			if test.cancelled && engine.ClassOf(err) != engine.ErrorCancelled {
				t.Fatalf("error class = %q", engine.ClassOf(err))
			}
		})
	}
}

func TestFixerConcurrentRuns(t *testing.T) {
	t.Parallel()
	provider := successfulFixerEngine(validFixerOutput(OutputCompleted))
	fixContext := validFixerContext(t)
	fixer := mustFixer(
		t,
		provider,
		FixerContextProviderFunc(func(context.Context, int64, int64) (*FixerContext, error) {
			return fixContext, nil
		}),
	)
	const runCount = 32
	errs := make(chan error, runCount)
	var group sync.WaitGroup
	for index := 0; index < runCount; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := fixer.Run(context.Background(), fixerRequest())
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
	got := len(provider.requests)
	provider.mu.Unlock()
	if got != runCount {
		t.Fatalf("engine runs = %d", got)
	}
}

func TestNewFixerValidationAndCancellation(t *testing.T) {
	t.Parallel()
	contexts := validFixerContextProvider(t)
	provider := successfulFixerEngine(validFixerOutput(OutputCompleted))
	var nilProvider *plannerTestEngine
	var nilContexts *fixerNilContextProvider
	var nilContextFunc FixerContextProviderFunc
	for _, test := range []struct {
		name     string
		provider engine.Engine
		contexts FixerContextProvider
		options  FixerOptions
	}{
		{"nil engine", nil, contexts, FixerOptions{}},
		{"typed nil engine", nilProvider, contexts, FixerOptions{}},
		{"nil contexts", provider, nil, FixerOptions{}},
		{"typed nil contexts", provider, nilContexts, FixerOptions{}},
		{"nil context function", provider, nilContextFunc, FixerOptions{}},
		{"negative timeout", provider, contexts, FixerOptions{Timeout: -time.Second}},
		{"negative turns", provider, contexts, FixerOptions{MaxTurns: -1}},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if fixer, err := NewFixer(test.provider, test.contexts, test.options); err == nil || fixer != nil {
				t.Fatalf("NewFixer = %#v, %v", fixer, err)
			}
		})
	}

	loadCalls := 0
	fixer := mustFixer(
		t,
		provider,
		FixerContextProviderFunc(func(context.Context, int64, int64) (*FixerContext, error) {
			loadCalls++
			return validFixerContext(t), nil
		}),
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fixer.Run(ctx, fixerRequest()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
	if loadCalls != 0 {
		t.Fatalf("context loaded %d times", loadCalls)
	}
}

type fixerNilContextProvider struct{}

func (*fixerNilContextProvider) LoadFixerContext(
	context.Context,
	int64,
	int64,
) (*FixerContext, error) {
	panic("unexpected call")
}

func successfulFixerEngine(raw json.RawMessage) *plannerTestEngine {
	return &plannerTestEngine{
		capabilities: plannerCapabilities(),
		result: &engine.Result{
			Status:     engine.ResultCompleted,
			OutputJSON: append(json.RawMessage(nil), raw...),
		},
	}
}

func validFixerContextProvider(t *testing.T) FixerContextProvider {
	t.Helper()
	return FixerContextProviderFunc(func(context.Context, int64, int64) (*FixerContext, error) {
		return validFixerContext(t), nil
	})
}

func validFixerContext(t *testing.T) *FixerContext {
	t.Helper()
	project := domain.NewProject("owner/repo", "Madar", "Ship v2", "Sequential delivery")
	project.ID = 7
	project.State = domain.ProjectExecuting
	task := domain.NewTask(project.ID, "Bound repair cycles", "Fix current blockers")
	task.ID = 11
	task.IssueNumber = 131
	task.Status = domain.TaskFixing
	task.Sequence = 5
	task.SelectedReason = "blocking review finding"
	task.BranchName = "madar/issue-131"
	task.DependencyState = "ready"
	return &FixerContext{
		Project:       project,
		Task:          task,
		Plan:          withJSONWhitespace(validPlannerOutput(OutputCompleted)),
		Delivery:      withJSONWhitespace(validDeveloperOutput(OutputCompleted)),
		Review:        withJSONWhitespace(validReviewerOutput(OutputCompleted, true)),
		WorkDir:       t.TempDir(),
		CurrentBranch: task.BranchName,
	}
}

func fixerRequest() workflow.ModeRequest {
	return workflow.ModeRequest{
		ProjectID:    7,
		TaskID:       11,
		Mode:         workflow.ModeFixer,
		Status:       domain.TaskFixing,
		FixCycle:     1,
		MaxFixCycles: 2,
	}
}

func mustFixer(
	t *testing.T,
	provider engine.Engine,
	contexts FixerContextProvider,
) *Fixer {
	t.Helper()
	fixer, err := NewFixer(provider, contexts, FixerOptions{})
	if err != nil {
		t.Fatalf("NewFixer: %v", err)
	}
	return fixer
}

var _ FixerContextProvider = (*fixerNilContextProvider)(nil)
