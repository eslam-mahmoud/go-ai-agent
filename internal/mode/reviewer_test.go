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

func TestReviewerBuildsFreshReadOnlyIndependentRequest(t *testing.T) {
	t.Parallel()
	raw := validReviewerOutput(OutputCompleted, false)
	provider := successfulReviewerEngine(raw)
	reviewContext := validReviewerContext(t)
	reviewContext.ExecutionID = 61
	reviewContext.Task.PRNumber = 130
	planBefore := string(reviewContext.Plan)
	deliveryBefore := string(reviewContext.Delivery)
	environment := map[string]string{"MADAR_TRACE": "reviewer"}
	loadCalls := 0
	emitted := 0
	event := engine.Event{Type: engine.EventProgress, Message: "reviewing diff"}
	provider.emitEvent = &event
	reviewer, err := NewReviewer(
		provider,
		ReviewerContextProviderFunc(func(
			_ context.Context,
			projectID, taskID int64,
		) (*ReviewerContext, error) {
			loadCalls++
			if projectID != 7 || taskID != 11 {
				t.Fatalf("context IDs = %d/%d", projectID, taskID)
			}
			return reviewContext, nil
		}),
		ReviewerOptions{
			Model:       "independent-review-model",
			Timeout:     3 * time.Minute,
			MaxTurns:    12,
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
		t.Fatalf("NewReviewer: %v", err)
	}
	environment["MADAR_TRACE"] = "mutated"

	got, err := reviewer.Run(context.Background(), reviewerRequest())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(got) != string(raw) {
		t.Fatalf("output = %s, want %s", got, raw)
	}
	if loadCalls != 1 || emitted != 1 {
		t.Fatalf("load calls = %d, emitted = %d", loadCalls, emitted)
	}
	if string(reviewContext.Plan) != planBefore ||
		string(reviewContext.Delivery) != deliveryBefore {
		t.Fatal("Reviewer mutated an upstream artifact")
	}
	request := provider.lastRequest(t)
	if request.ExecutionID != 61 ||
		request.WorkDir != reviewContext.WorkDir ||
		request.Mode != string(workflow.ModeReviewer) ||
		request.Model != "independent-review-model" ||
		request.Timeout != 3*time.Minute ||
		request.MaxTurns != 12 {
		t.Fatalf("engine request metadata = %#v", request)
	}
	if request.SessionID != "" || request.ResumeSessionID != "" {
		t.Fatalf("reviewer was not started in a fresh session: %#v", request)
	}
	if request.Policy.Sandbox != "read-only" ||
		request.Policy.ApprovalPolicy != "never" ||
		request.Policy.SkipPermissions {
		t.Fatalf("reviewer policy = %#v", request.Policy)
	}
	if request.Environment["MADAR_TRACE"] != "reviewer" {
		t.Fatalf("environment was not defensively copied: %#v", request.Environment)
	}
	if string(request.OutputSchema) != string(reviewer.definition.OutputSchema) {
		t.Fatal("reviewer did not pass the canonical output schema")
	}
	for _, required := range []string{
		"independent Reviewer in a fresh provider session",
		"Operate in read-only mode",
		"actual repository diff and commits",
		"Do not rely only on the Developer's claimed",
		"approved-plan acceptance criterion",
		"test coverage and likely regressions",
		"security, correctness, maintainability",
		"blocking_findings",
		"future_improvements",
		"unrelated future work",
		`"base_ref": "main"`,
		`"head_ref": "madar/issue-129"`,
		`"commit_sha"`,
	} {
		if !strings.Contains(request.Prompt, required) {
			t.Errorf("prompt missing %q", required)
		}
	}

	definition := reviewer.Definition()
	if definition.Name != workflow.ModeReviewer || !definition.FreshSession {
		t.Fatalf("definition = %#v", definition)
	}
	definition.OutputSchema[0] = '!'
	if reviewer.Definition().OutputSchema[0] == '!' {
		t.Fatal("Definition leaked mutable schema storage")
	}
}

func TestReviewerWorkflowRouting(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		output          json.RawMessage
		nextMode        workflow.ModeName
		nextOutput      json.RawMessage
		firstTransition domain.TaskStatus
	}{
		{
			name:            "accepted review advances to verifier",
			output:          validReviewerOutput(OutputCompleted, false),
			nextMode:        workflow.ModeVerifier,
			nextOutput:      validVerifierOutput(OutputBlocked),
			firstTransition: domain.TaskVerifying,
		},
		{
			name:            "blocking findings advance to fixer",
			output:          validReviewerOutput(OutputCompleted, true),
			nextMode:        workflow.ModeFixer,
			nextOutput:      validFixerOutput(OutputBlocked),
			firstTransition: domain.TaskFixing,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			reviewer := mustReviewer(
				t,
				successfulReviewerEngine(test.output),
				validReviewerContextProvider(t),
			)
			next := &registryTestMode{
				definition: mustBuiltinDefinition(t, test.nextMode),
				output:     test.nextOutput,
			}
			registry, err := NewRegistry(reviewer, next)
			if err != nil {
				t.Fatalf("NewRegistry: %v", err)
			}
			dispatcher, err := NewDispatcher(registry)
			if err != nil {
				t.Fatalf("NewDispatcher: %v", err)
			}
			controller := &developerTestController{status: domain.TaskReviewing}
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
				result.ModesRun[0] != workflow.ModeReviewer ||
				result.ModesRun[1] != test.nextMode {
				t.Fatalf("modes run = %#v", result.ModesRun)
			}
			if len(controller.transitions) != 2 ||
				controller.transitions[0] != test.firstTransition ||
				controller.transitions[1] != domain.TaskBlocked {
				t.Fatalf("transitions = %#v", controller.transitions)
			}
		})
	}
}

func TestReviewerRejectsInvalidRequestsBeforeLoadingContext(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		request workflow.ModeRequest
	}{
		{"wrong mode", workflow.ModeRequest{ProjectID: 7, TaskID: 11, Mode: workflow.ModeDeveloper, Status: domain.TaskReviewing}},
		{"missing project", workflow.ModeRequest{TaskID: 11, Mode: workflow.ModeReviewer, Status: domain.TaskReviewing}},
		{"missing task", workflow.ModeRequest{ProjectID: 7, Mode: workflow.ModeReviewer, Status: domain.TaskReviewing}},
		{"wrong state", workflow.ModeRequest{ProjectID: 7, TaskID: 11, Mode: workflow.ModeReviewer, Status: domain.TaskDeveloping}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			loadCalls := 0
			reviewer := mustReviewer(
				t,
				successfulReviewerEngine(validReviewerOutput(OutputCompleted, false)),
				ReviewerContextProviderFunc(func(context.Context, int64, int64) (*ReviewerContext, error) {
					loadCalls++
					return validReviewerContext(t), nil
				}),
			)
			if _, err := reviewer.Run(context.Background(), test.request); !errors.Is(err, ErrInvalidReviewerRequest) {
				t.Fatalf("Run error = %v", err)
			}
			if loadCalls != 0 {
				t.Fatalf("context loaded %d times", loadCalls)
			}
		})
	}
}

func TestReviewerRejectsInvalidContext(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*ReviewerContext)
	}{
		{"nil project", func(value *ReviewerContext) { value.Project = nil }},
		{"nil task", func(value *ReviewerContext) { value.Task = nil }},
		{"project mismatch", func(value *ReviewerContext) { value.Project.ID++ }},
		{"task mismatch", func(value *ReviewerContext) { value.Task.ID++ }},
		{"cross project task", func(value *ReviewerContext) { value.Task.ProjectID++ }},
		{"wrong task status", func(value *ReviewerContext) { value.Task.Status = domain.TaskDeveloping }},
		{"missing assigned branch", func(value *ReviewerContext) { value.Task.BranchName = "" }},
		{"missing base", func(value *ReviewerContext) { value.BaseRef = "" }},
		{"missing head", func(value *ReviewerContext) { value.HeadRef = "" }},
		{"head mismatch", func(value *ReviewerContext) { value.HeadRef = "madar/other" }},
		{"same refs", func(value *ReviewerContext) { value.BaseRef = value.HeadRef }},
		{"missing workdir", func(value *ReviewerContext) { value.WorkDir = "" }},
		{"relative workdir", func(value *ReviewerContext) { value.WorkDir = "relative" }},
		{"negative execution", func(value *ReviewerContext) { value.ExecutionID = -1 }},
		{"missing plan", func(value *ReviewerContext) { value.Plan = nil }},
		{"malformed plan", func(value *ReviewerContext) { value.Plan = json.RawMessage(`{`) }},
		{"incomplete plan", func(value *ReviewerContext) { value.Plan = validPlannerOutput(OutputNeedsInput) }},
		{"missing delivery", func(value *ReviewerContext) { value.Delivery = nil }},
		{"malformed delivery", func(value *ReviewerContext) { value.Delivery = json.RawMessage(`{`) }},
		{"incomplete delivery", func(value *ReviewerContext) { value.Delivery = validDeveloperOutput(OutputNeedsInput) }},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			reviewContext := validReviewerContext(t)
			test.mutate(reviewContext)
			provider := successfulReviewerEngine(validReviewerOutput(OutputCompleted, false))
			reviewer := mustReviewer(
				t,
				provider,
				ReviewerContextProviderFunc(func(context.Context, int64, int64) (*ReviewerContext, error) {
					return reviewContext, nil
				}),
			)
			if _, err := reviewer.Run(context.Background(), reviewerRequest()); !errors.Is(err, ErrInvalidReviewerContext) {
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

func TestReviewerRejectsContradictoryCompletedResults(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		output json.RawMessage
	}{
		{
			name: "findings while criteria accepted",
			output: reviewerOutputWithSemantics(t, true, []any{
				map[string]any{
					"title":       "Regression",
					"description": "The old behavior is no longer covered.",
					"severity":    "high",
				},
			}),
		},
		{
			name:   "no findings while criteria rejected",
			output: reviewerOutputWithSemantics(t, false, []any{}),
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			reviewer := mustReviewer(
				t,
				successfulReviewerEngine(test.output),
				validReviewerContextProvider(t),
			)
			if _, err := reviewer.Run(context.Background(), reviewerRequest()); !errors.Is(err, ErrReviewerResult) {
				t.Fatalf("Run error = %v", err)
			}
		})
	}
}

func TestReviewerEngineAndResultFailures(t *testing.T) {
	t.Parallel()
	contextError := errors.New("snapshot unavailable")
	engineError := errors.New("provider unavailable")
	tests := []struct {
		name      string
		provider  *plannerTestEngine
		contexts  ReviewerContextProvider
		want      error
		cancelled bool
	}{
		{
			name:     "capabilities error",
			provider: &plannerTestEngine{capabilityErr: engineError},
			contexts: validReviewerContextProvider(t),
			want:     engineError,
		},
		{
			name:     "missing structured output",
			provider: &plannerTestEngine{capabilities: engine.CapabilitySet{OutputSchema: true}},
			contexts: validReviewerContextProvider(t),
			want:     ErrReviewerUnsupported,
		},
		{
			name:     "missing schema support",
			provider: &plannerTestEngine{capabilities: engine.CapabilitySet{StructuredOutput: true}},
			contexts: validReviewerContextProvider(t),
			want:     ErrReviewerUnsupported,
		},
		{
			name:     "context error",
			provider: successfulReviewerEngine(validReviewerOutput(OutputCompleted, false)),
			contexts: ReviewerContextProviderFunc(func(context.Context, int64, int64) (*ReviewerContext, error) {
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
			contexts: validReviewerContextProvider(t),
			want:     engineError,
		},
		{
			name:     "nil result",
			provider: &plannerTestEngine{capabilities: plannerCapabilities()},
			contexts: validReviewerContextProvider(t),
			want:     ErrReviewerResult,
		},
		{
			name: "failed result",
			provider: &plannerTestEngine{
				capabilities: plannerCapabilities(),
				result:       &engine.Result{Status: engine.ResultFailed},
			},
			contexts: validReviewerContextProvider(t),
			want:     ErrReviewerResult,
		},
		{
			name: "unknown result",
			provider: &plannerTestEngine{
				capabilities: plannerCapabilities(),
				result:       &engine.Result{},
			},
			contexts: validReviewerContextProvider(t),
			want:     ErrReviewerResult,
		},
		{
			name: "cancelled result",
			provider: &plannerTestEngine{
				capabilities: plannerCapabilities(),
				result:       &engine.Result{Status: engine.ResultCancelled},
			},
			contexts:  validReviewerContextProvider(t),
			want:      context.Canceled,
			cancelled: true,
		},
		{
			name: "missing output",
			provider: &plannerTestEngine{
				capabilities: plannerCapabilities(),
				result:       &engine.Result{Status: engine.ResultCompleted},
			},
			contexts: validReviewerContextProvider(t),
			want:     ErrReviewerResult,
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
			contexts: validReviewerContextProvider(t),
			want:     ErrReviewerResult,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			reviewer := mustReviewer(t, test.provider, test.contexts)
			_, err := reviewer.Run(context.Background(), reviewerRequest())
			if !errors.Is(err, test.want) {
				t.Fatalf("Run error = %v, want errors.Is %v", err, test.want)
			}
			if test.cancelled && engine.ClassOf(err) != engine.ErrorCancelled {
				t.Fatalf("error class = %q", engine.ClassOf(err))
			}
		})
	}
}

func TestReviewerAcceptsValidatedOutputTextFallback(t *testing.T) {
	t.Parallel()
	raw := validReviewerOutput(OutputCompleted, false)
	provider := &plannerTestEngine{
		capabilities: plannerCapabilities(),
		result: &engine.Result{
			Status:     engine.ResultCompleted,
			OutputText: "\n" + string(raw) + "\n",
		},
	}
	reviewer := mustReviewer(t, provider, validReviewerContextProvider(t))
	got, err := reviewer.Run(context.Background(), reviewerRequest())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(got) != string(raw) {
		t.Fatalf("output = %q, want %q", got, raw)
	}
}

func TestReviewerConcurrentRuns(t *testing.T) {
	t.Parallel()
	provider := successfulReviewerEngine(validReviewerOutput(OutputCompleted, false))
	reviewContext := validReviewerContext(t)
	reviewer := mustReviewer(
		t,
		provider,
		ReviewerContextProviderFunc(func(context.Context, int64, int64) (*ReviewerContext, error) {
			return reviewContext, nil
		}),
	)

	const runCount = 32
	errs := make(chan error, runCount)
	var group sync.WaitGroup
	for index := 0; index < runCount; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := reviewer.Run(context.Background(), reviewerRequest())
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

func TestReviewerPreservesPreRunCancellation(t *testing.T) {
	t.Parallel()
	provider := successfulReviewerEngine(validReviewerOutput(OutputCompleted, false))
	loadCalls := 0
	reviewer := mustReviewer(
		t,
		provider,
		ReviewerContextProviderFunc(func(context.Context, int64, int64) (*ReviewerContext, error) {
			loadCalls++
			return validReviewerContext(t), nil
		}),
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := reviewer.Run(ctx, reviewerRequest()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v", err)
	}
	if loadCalls != 0 {
		t.Fatalf("context loaded %d times", loadCalls)
	}
}

func TestNewReviewerValidation(t *testing.T) {
	t.Parallel()
	contexts := validReviewerContextProvider(t)
	provider := successfulReviewerEngine(validReviewerOutput(OutputCompleted, false))
	var nilProvider *plannerTestEngine
	var nilContexts *reviewerNilContextProvider
	var nilContextFunc ReviewerContextProviderFunc
	tests := []struct {
		name     string
		provider engine.Engine
		contexts ReviewerContextProvider
		options  ReviewerOptions
	}{
		{"nil engine", nil, contexts, ReviewerOptions{}},
		{"typed nil engine", nilProvider, contexts, ReviewerOptions{}},
		{"nil contexts", provider, nil, ReviewerOptions{}},
		{"typed nil contexts", provider, nilContexts, ReviewerOptions{}},
		{"typed nil context function", provider, nilContextFunc, ReviewerOptions{}},
		{"negative timeout", provider, contexts, ReviewerOptions{Timeout: -time.Second}},
		{"negative turns", provider, contexts, ReviewerOptions{MaxTurns: -1}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if reviewer, err := NewReviewer(test.provider, test.contexts, test.options); err == nil || reviewer != nil {
				t.Fatalf("NewReviewer = %#v, %v", reviewer, err)
			}
		})
	}
}

type reviewerNilContextProvider struct{}

func (*reviewerNilContextProvider) LoadReviewerContext(
	context.Context,
	int64,
	int64,
) (*ReviewerContext, error) {
	panic("unexpected call")
}

func successfulReviewerEngine(raw json.RawMessage) *plannerTestEngine {
	return &plannerTestEngine{
		capabilities: plannerCapabilities(),
		result: &engine.Result{
			Status:     engine.ResultCompleted,
			OutputJSON: append(json.RawMessage(nil), raw...),
		},
	}
}

func validReviewerContextProvider(t *testing.T) ReviewerContextProvider {
	t.Helper()
	return ReviewerContextProviderFunc(func(context.Context, int64, int64) (*ReviewerContext, error) {
		return validReviewerContext(t), nil
	})
}

func validReviewerContext(t *testing.T) *ReviewerContext {
	t.Helper()
	project := domain.NewProject("owner/repo", "Madar", "Ship v2", "Sequential delivery")
	project.ID = 7
	project.State = domain.ProjectExecuting
	task := domain.NewTask(project.ID, "Implement reviewer", "Review independently")
	task.ID = 11
	task.IssueNumber = 129
	task.Status = domain.TaskReviewing
	task.Sequence = 4
	task.SelectedReason = "development completed"
	task.BranchName = "madar/issue-129"
	task.DependencyState = "ready"
	plan := validPlannerOutput(OutputCompleted)
	delivery := validDeveloperOutput(OutputCompleted)
	return &ReviewerContext{
		Project:  project,
		Task:     task,
		Plan:     withJSONWhitespace(plan),
		Delivery: withJSONWhitespace(delivery),
		WorkDir:  t.TempDir(),
		BaseRef:  "main",
		HeadRef:  task.BranchName,
	}
}

func reviewerRequest() workflow.ModeRequest {
	return workflow.ModeRequest{
		ProjectID: 7,
		TaskID:    11,
		Mode:      workflow.ModeReviewer,
		Status:    domain.TaskReviewing,
	}
}

func reviewerOutputWithSemantics(
	t *testing.T,
	accepted bool,
	findings []any,
) json.RawMessage {
	t.Helper()
	var output map[string]any
	if err := json.Unmarshal(validReviewerOutput(OutputCompleted, false), &output); err != nil {
		t.Fatal(err)
	}
	output["acceptance_criteria_met"] = accepted
	output["blocking_findings"] = findings
	return mustJSON(output)
}

func withJSONWhitespace(raw json.RawMessage) json.RawMessage {
	result := make(json.RawMessage, 0, len(raw)+2)
	result = append(result, '\n')
	result = append(result, raw...)
	result = append(result, '\n')
	return result
}

func mustReviewer(
	t *testing.T,
	provider engine.Engine,
	contexts ReviewerContextProvider,
) *Reviewer {
	t.Helper()
	reviewer, err := NewReviewer(provider, contexts, ReviewerOptions{})
	if err != nil {
		t.Fatalf("NewReviewer: %v", err)
	}
	return reviewer
}

var _ ReviewerContextProvider = (*reviewerNilContextProvider)(nil)
