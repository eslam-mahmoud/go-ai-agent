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

func TestVerifierBuildsMandatoryEvidenceRequest(t *testing.T) {
	t.Parallel()
	raw := verifierModeOutput(OutputCompleted, nil)
	provider := successfulVerifierEngine(raw)
	verificationContext := validVerifierContext(t)
	verificationContext.ExecutionID = 83
	planBefore := string(verificationContext.Plan)
	fixBefore := string(verificationContext.Fixes[0])
	environment := map[string]string{"MADAR_TRACE": "verifier"}
	event := engine.Event{Type: engine.EventProgress, Message: "running tests"}
	provider.emitEvent = &event
	emitted := 0
	verifier, err := NewVerifier(
		provider,
		VerifierContextProviderFunc(func(
			_ context.Context,
			projectID, taskID int64,
		) (*VerifierContext, error) {
			if projectID != 7 || taskID != 11 {
				t.Fatalf("context IDs = %d/%d", projectID, taskID)
			}
			return verificationContext, nil
		}),
		VerifierOptions{
			Model:       "verification-model",
			Timeout:     6 * time.Minute,
			MaxTurns:    18,
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
		t.Fatalf("NewVerifier: %v", err)
	}
	environment["MADAR_TRACE"] = "mutated"

	got, err := verifier.Run(context.Background(), verifierRequest())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(got) != string(raw) || emitted != 1 {
		t.Fatalf("output=%s emitted=%d", got, emitted)
	}
	if string(verificationContext.Plan) != planBefore ||
		string(verificationContext.Fixes[0]) != fixBefore {
		t.Fatal("Verifier mutated upstream artifacts")
	}
	request := provider.lastRequest(t)
	if request.ExecutionID != 83 ||
		request.WorkDir != verificationContext.WorkDir ||
		request.Mode != string(workflow.ModeVerifier) ||
		request.Model != "verification-model" ||
		request.Timeout != 6*time.Minute ||
		request.MaxTurns != 18 {
		t.Fatalf("engine request metadata = %#v", request)
	}
	if request.Policy.Sandbox != "workspace-write" ||
		request.Policy.ApprovalPolicy != "never" ||
		request.Policy.SkipPermissions {
		t.Fatalf("verifier policy = %#v", request.Policy)
	}
	if request.Environment["MADAR_TRACE"] != "verifier" {
		t.Fatalf("environment was not defensively copied: %#v", request.Environment)
	}
	for _, required := range []string{
		"mandatory Verifier",
		"do not edit",
		"Run every configured verification command",
		"Evaluate every acceptance criterion",
		"Confirm current_branch, pr_head, pr_base, and pr_number",
		"working tree is clean",
		"zero blocking findings",
		"null while required CI is pending",
		"bounded repair workflow",
		`"pr_number": 134`,
		`"ci_status": "not-required"`,
		`"completed_fixes"`,
	} {
		if !strings.Contains(request.Prompt, required) {
			t.Errorf("prompt missing %q", required)
		}
	}
	definition := verifier.Definition()
	if definition.Name != workflow.ModeVerifier || definition.FreshSession {
		t.Fatalf("definition = %#v", definition)
	}
}

func TestVerifierWorkflowCompletionAndPendingCI(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		ciRequired bool
		ciStatus   VerificationCIStatus
		ciEvidence any
		final      domain.TaskStatus
	}{
		{
			name:       "CI not required completes task",
			ciRequired: false,
			ciStatus:   VerificationCINotRequired,
			ciEvidence: nil,
			final:      domain.TaskCompleted,
		},
		{
			name:       "required pending CI waits",
			ciRequired: true,
			ciStatus:   VerificationCIPending,
			ciEvidence: nil,
			final:      domain.TaskWaitingCI,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			verificationContext := validVerifierContext(t)
			verificationContext.CIRequired = test.ciRequired
			verificationContext.CIStatus = test.ciStatus
			verifier := mustVerifier(
				t,
				successfulVerifierEngine(verifierModeOutput(OutputCompleted, test.ciEvidence)),
				VerifierContextProviderFunc(func(context.Context, int64, int64) (*VerifierContext, error) {
					return verificationContext, nil
				}),
			)
			registry, err := NewRegistry(verifier)
			if err != nil {
				t.Fatal(err)
			}
			dispatcher, _ := NewDispatcher(registry)
			controller := &developerTestController{status: domain.TaskVerifying}
			feature, err := workflow.NewFeatureWorkflow(
				controller,
				dispatcher,
				workflow.FeatureOptions{CIRequired: test.ciRequired},
			)
			if err != nil {
				t.Fatal(err)
			}
			result, err := feature.Run(context.Background(), 7, 11)
			if err != nil {
				t.Fatal(err)
			}
			if result.FinalStatus != test.final ||
				len(result.ModesRun) != 1 ||
				result.ModesRun[0] != workflow.ModeVerifier {
				t.Fatalf("result = %#v", result)
			}
		})
	}
}

func TestVerifierFailedOutputRoutesThroughBoundedFixBudget(t *testing.T) {
	t.Parallel()
	verificationContext := validVerifierContext(t)
	verificationContext.CIRequired = true
	verificationContext.CIStatus = VerificationCIFailed
	verifier := mustVerifier(
		t,
		successfulVerifierEngine(verifierModeOutput(OutputFailed, false)),
		VerifierContextProviderFunc(func(context.Context, int64, int64) (*VerifierContext, error) {
			return verificationContext, nil
		}),
	)
	fixer := &registryTestMode{
		definition: mustBuiltinDefinition(t, workflow.ModeFixer),
		output:     validFixerOutput(OutputBlocked),
	}
	registry, err := NewRegistry(verifier, fixer)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, _ := NewDispatcher(registry)
	controller := &developerTestController{status: domain.TaskVerifying}
	feature, _ := workflow.NewFeatureWorkflow(
		controller,
		dispatcher,
		workflow.FeatureOptions{MaxReviewFixCycles: 2},
	)
	result, err := feature.Run(context.Background(), 7, 11)
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalStatus != domain.TaskBlocked ||
		len(result.ModesRun) != 2 ||
		result.ModesRun[0] != workflow.ModeVerifier ||
		result.ModesRun[1] != workflow.ModeFixer {
		t.Fatalf("result = %#v", result)
	}
}

func TestVerifierRejectsInvalidRequestsBeforeLoadingContext(t *testing.T) {
	t.Parallel()
	tests := []workflow.ModeRequest{
		{ProjectID: 7, TaskID: 11, Mode: workflow.ModeDeveloper, Status: domain.TaskVerifying},
		{TaskID: 11, Mode: workflow.ModeVerifier, Status: domain.TaskVerifying},
		{ProjectID: 7, Mode: workflow.ModeVerifier, Status: domain.TaskVerifying},
		{ProjectID: 7, TaskID: 11, Mode: workflow.ModeVerifier, Status: domain.TaskReviewing},
	}
	for index, request := range tests {
		index, request := index, request
		t.Run(string(rune('a'+index)), func(t *testing.T) {
			t.Parallel()
			loadCalls := 0
			verifier := mustVerifier(
				t,
				successfulVerifierEngine(verifierModeOutput(OutputCompleted, nil)),
				VerifierContextProviderFunc(func(context.Context, int64, int64) (*VerifierContext, error) {
					loadCalls++
					return validVerifierContext(t), nil
				}),
			)
			if _, err := verifier.Run(context.Background(), request); !errors.Is(err, ErrInvalidVerifierRequest) {
				t.Fatalf("Run error = %v", err)
			}
			if loadCalls != 0 {
				t.Fatalf("context loaded %d times", loadCalls)
			}
		})
	}
}

func TestVerifierRejectsInvalidContext(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*VerifierContext)
	}{
		{"nil project", func(value *VerifierContext) { value.Project = nil }},
		{"nil task", func(value *VerifierContext) { value.Task = nil }},
		{"project mismatch", func(value *VerifierContext) { value.Project.ID++ }},
		{"task mismatch", func(value *VerifierContext) { value.Task.ID++ }},
		{"cross project", func(value *VerifierContext) { value.Task.ProjectID++ }},
		{"wrong state", func(value *VerifierContext) { value.Task.Status = domain.TaskReviewing }},
		{"branch mismatch", func(value *VerifierContext) { value.CurrentBranch = "madar/other" }},
		{"missing PR", func(value *VerifierContext) { value.PRNumber = 0 }},
		{"task PR mismatch", func(value *VerifierContext) { value.Task.PRNumber++ }},
		{"PR head mismatch", func(value *VerifierContext) { value.PRHead = "madar/other" }},
		{"missing PR base", func(value *VerifierContext) { value.PRBase = "" }},
		{"same PR refs", func(value *VerifierContext) { value.PRBase = value.PRHead }},
		{"missing workdir", func(value *VerifierContext) { value.WorkDir = "" }},
		{"relative workdir", func(value *VerifierContext) { value.WorkDir = "relative" }},
		{"negative execution", func(value *VerifierContext) { value.ExecutionID = -1 }},
		{"invalid CI", func(value *VerifierContext) { value.CIStatus = "unknown" }},
		{"optional CI pending", func(value *VerifierContext) { value.CIStatus = VerificationCIPending }},
		{"required CI omitted", func(value *VerifierContext) { value.CIRequired = true }},
		{"missing plan", func(value *VerifierContext) { value.Plan = nil }},
		{"incomplete delivery", func(value *VerifierContext) { value.Delivery = validDeveloperOutput(OutputNeedsInput) }},
		{"blocking review", func(value *VerifierContext) { value.Review = validReviewerOutput(OutputCompleted, true) }},
		{"incomplete fix", func(value *VerifierContext) { value.Fixes[0] = validFixerOutput(OutputBlocked) }},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			verificationContext := validVerifierContext(t)
			test.mutate(verificationContext)
			provider := successfulVerifierEngine(verifierModeOutput(OutputCompleted, nil))
			verifier := mustVerifier(
				t,
				provider,
				VerifierContextProviderFunc(func(context.Context, int64, int64) (*VerifierContext, error) {
					return verificationContext, nil
				}),
			)
			if _, err := verifier.Run(context.Background(), verifierRequest()); !errors.Is(err, ErrInvalidVerifierContext) {
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

func TestVerifierRejectsContradictoryCompletedEvidence(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"empty criteria", func(output map[string]any) { output["acceptance_results"] = []any{} }},
		{"failed criterion", func(output map[string]any) {
			output["acceptance_results"].([]any)[0].(map[string]any)["passed"] = false
		}},
		{"missing criterion evidence", func(output map[string]any) {
			output["acceptance_results"].([]any)[0].(map[string]any)["evidence"] = ""
		}},
		{"empty commands", func(output map[string]any) { output["verification_commands"] = []any{} }},
		{"failed command", func(output map[string]any) {
			output["verification_commands"].([]any)[0].(map[string]any)["passed"] = false
		}},
		{"unexpected CI evidence", func(output map[string]any) { output["ci_passed"] = true }},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var output map[string]any
			if err := json.Unmarshal(verifierModeOutput(OutputCompleted, nil), &output); err != nil {
				t.Fatal(err)
			}
			test.mutate(output)
			verifier := mustVerifier(
				t,
				successfulVerifierEngine(mustJSON(output)),
				validVerifierContextProvider(t),
			)
			if _, err := verifier.Run(context.Background(), verifierRequest()); !errors.Is(err, ErrVerifierResult) {
				t.Fatalf("Run error = %v", err)
			}
		})
	}

	t.Run("passed CI requires true", func(t *testing.T) {
		verificationContext := validVerifierContext(t)
		verificationContext.CIRequired = true
		verificationContext.CIStatus = VerificationCIPassed
		verifier := mustVerifier(
			t,
			successfulVerifierEngine(verifierModeOutput(OutputCompleted, nil)),
			VerifierContextProviderFunc(func(context.Context, int64, int64) (*VerifierContext, error) {
				return verificationContext, nil
			}),
		)
		if _, err := verifier.Run(context.Background(), verifierRequest()); !errors.Is(err, ErrVerifierResult) {
			t.Fatalf("Run error = %v", err)
		}
	})

	t.Run("failed CI cannot complete", func(t *testing.T) {
		verificationContext := validVerifierContext(t)
		verificationContext.CIRequired = true
		verificationContext.CIStatus = VerificationCIFailed
		verifier := mustVerifier(
			t,
			successfulVerifierEngine(verifierModeOutput(OutputCompleted, false)),
			VerifierContextProviderFunc(func(context.Context, int64, int64) (*VerifierContext, error) {
				return verificationContext, nil
			}),
		)
		if _, err := verifier.Run(context.Background(), verifierRequest()); !errors.Is(err, ErrVerifierResult) {
			t.Fatalf("Run error = %v", err)
		}
	})
}

func TestVerifierAcceptsPassedCIEvidence(t *testing.T) {
	t.Parallel()
	verificationContext := validVerifierContext(t)
	verificationContext.CIRequired = true
	verificationContext.CIStatus = VerificationCIPassed
	verifier := mustVerifier(
		t,
		successfulVerifierEngine(verifierModeOutput(OutputCompleted, true)),
		VerifierContextProviderFunc(func(context.Context, int64, int64) (*VerifierContext, error) {
			return verificationContext, nil
		}),
	)
	if _, err := verifier.Run(context.Background(), verifierRequest()); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestVerifierEngineAndResultFailures(t *testing.T) {
	t.Parallel()
	contextError := errors.New("snapshot unavailable")
	engineError := errors.New("provider unavailable")
	tests := []struct {
		name      string
		provider  *plannerTestEngine
		contexts  VerifierContextProvider
		want      error
		cancelled bool
	}{
		{"capabilities", &plannerTestEngine{capabilityErr: engineError}, validVerifierContextProvider(t), engineError, false},
		{"unsupported", &plannerTestEngine{capabilities: engine.CapabilitySet{StructuredOutput: true}}, validVerifierContextProvider(t), ErrVerifierUnsupported, false},
		{
			"context",
			successfulVerifierEngine(verifierModeOutput(OutputCompleted, nil)),
			VerifierContextProviderFunc(func(context.Context, int64, int64) (*VerifierContext, error) {
				return nil, contextError
			}),
			contextError,
			false,
		},
		{"engine", &plannerTestEngine{capabilities: plannerCapabilities(), runErr: engineError}, validVerifierContextProvider(t), engineError, false},
		{"nil result", &plannerTestEngine{capabilities: plannerCapabilities()}, validVerifierContextProvider(t), ErrVerifierResult, false},
		{"failed", &plannerTestEngine{capabilities: plannerCapabilities(), result: &engine.Result{Status: engine.ResultFailed}}, validVerifierContextProvider(t), ErrVerifierResult, false},
		{"cancelled", &plannerTestEngine{capabilities: plannerCapabilities(), result: &engine.Result{Status: engine.ResultCancelled}}, validVerifierContextProvider(t), context.Canceled, true},
		{"missing output", &plannerTestEngine{capabilities: plannerCapabilities(), result: &engine.Result{Status: engine.ResultCompleted}}, validVerifierContextProvider(t), ErrVerifierResult, false},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			verifier := mustVerifier(t, test.provider, test.contexts)
			_, err := verifier.Run(context.Background(), verifierRequest())
			if !errors.Is(err, test.want) {
				t.Fatalf("Run error = %v, want %v", err, test.want)
			}
			if test.cancelled && engine.ClassOf(err) != engine.ErrorCancelled {
				t.Fatalf("error class = %q", engine.ClassOf(err))
			}
		})
	}
}

func TestVerifierConcurrentRuns(t *testing.T) {
	t.Parallel()
	provider := successfulVerifierEngine(verifierModeOutput(OutputCompleted, nil))
	verificationContext := validVerifierContext(t)
	verifier := mustVerifier(
		t,
		provider,
		VerifierContextProviderFunc(func(context.Context, int64, int64) (*VerifierContext, error) {
			return verificationContext, nil
		}),
	)
	const runCount = 32
	errs := make(chan error, runCount)
	var group sync.WaitGroup
	for index := 0; index < runCount; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := verifier.Run(context.Background(), verifierRequest())
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

func TestNewVerifierValidationAndCancellation(t *testing.T) {
	t.Parallel()
	contexts := validVerifierContextProvider(t)
	provider := successfulVerifierEngine(verifierModeOutput(OutputCompleted, nil))
	var nilProvider *plannerTestEngine
	var nilContexts *verifierNilContextProvider
	var nilContextFunc VerifierContextProviderFunc
	for _, test := range []struct {
		name     string
		provider engine.Engine
		contexts VerifierContextProvider
		options  VerifierOptions
	}{
		{"nil engine", nil, contexts, VerifierOptions{}},
		{"typed nil engine", nilProvider, contexts, VerifierOptions{}},
		{"nil contexts", provider, nil, VerifierOptions{}},
		{"typed nil contexts", provider, nilContexts, VerifierOptions{}},
		{"nil context function", provider, nilContextFunc, VerifierOptions{}},
		{"negative timeout", provider, contexts, VerifierOptions{Timeout: -time.Second}},
		{"negative turns", provider, contexts, VerifierOptions{MaxTurns: -1}},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if verifier, err := NewVerifier(test.provider, test.contexts, test.options); err == nil || verifier != nil {
				t.Fatalf("NewVerifier = %#v, %v", verifier, err)
			}
		})
	}

	loadCalls := 0
	verifier := mustVerifier(
		t,
		provider,
		VerifierContextProviderFunc(func(context.Context, int64, int64) (*VerifierContext, error) {
			loadCalls++
			return validVerifierContext(t), nil
		}),
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := verifier.Run(ctx, verifierRequest()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v", err)
	}
	if loadCalls != 0 {
		t.Fatalf("context loaded %d times", loadCalls)
	}
}

type verifierNilContextProvider struct{}

func (*verifierNilContextProvider) LoadVerifierContext(
	context.Context,
	int64,
	int64,
) (*VerifierContext, error) {
	panic("unexpected call")
}

func successfulVerifierEngine(raw json.RawMessage) *plannerTestEngine {
	return &plannerTestEngine{
		capabilities: plannerCapabilities(),
		result: &engine.Result{
			Status:     engine.ResultCompleted,
			OutputJSON: append(json.RawMessage(nil), raw...),
		},
	}
}

func validVerifierContextProvider(t *testing.T) VerifierContextProvider {
	t.Helper()
	return VerifierContextProviderFunc(func(context.Context, int64, int64) (*VerifierContext, error) {
		return validVerifierContext(t), nil
	})
}

func validVerifierContext(t *testing.T) *VerifierContext {
	t.Helper()
	project := domain.NewProject("owner/repo", "Madar", "Ship v2", "Sequential delivery")
	project.ID = 7
	project.State = domain.ProjectExecuting
	task := domain.NewTask(project.ID, "Implement verifier", "Require completion evidence")
	task.ID = 11
	task.IssueNumber = 133
	task.Status = domain.TaskVerifying
	task.Sequence = 6
	task.SelectedReason = "review accepted"
	task.BranchName = "madar/issue-133"
	task.PRNumber = 134
	task.DependencyState = "ready"
	return &VerifierContext{
		Project:       project,
		Task:          task,
		Plan:          withJSONWhitespace(validPlannerOutput(OutputCompleted)),
		Delivery:      withJSONWhitespace(validDeveloperOutput(OutputCompleted)),
		Review:        withJSONWhitespace(validReviewerOutput(OutputCompleted, false)),
		Fixes:         []json.RawMessage{withJSONWhitespace(validFixerOutput(OutputCompleted))},
		WorkDir:       t.TempDir(),
		CurrentBranch: task.BranchName,
		PRNumber:      task.PRNumber,
		PRHead:        task.BranchName,
		PRBase:        "main",
		CIStatus:      VerificationCINotRequired,
	}
}

func verifierRequest() workflow.ModeRequest {
	return workflow.ModeRequest{
		ProjectID: 7,
		TaskID:    11,
		Mode:      workflow.ModeVerifier,
		Status:    domain.TaskVerifying,
	}
}

func verifierModeOutput(status OutputStatus, ciPassed any) json.RawMessage {
	passed := status == OutputCompleted
	return mustJSON(map[string]any{
		"status":                  status,
		"summary":                 "Verification evidence collected.",
		"question":                nil,
		"discoveries":             []any{},
		"risks":                   []any{},
		"recommended_next_action": "Complete or repair the task.",
		"acceptance_results": []any{map[string]any{
			"criterion": "Behavior is covered.",
			"passed":    passed,
			"evidence":  "Focused unit test passed.",
		}},
		"verification_commands": []any{map[string]any{
			"command": "go test ./...",
			"passed":  passed,
			"output":  "ok",
		}},
		"branch_consistent":           passed,
		"pr_consistent":               passed,
		"working_tree_clean":          passed,
		"ci_passed":                   ciPassed,
		"blocking_findings_remaining": 0,
	})
}

func mustVerifier(
	t *testing.T,
	provider engine.Engine,
	contexts VerifierContextProvider,
) *Verifier {
	t.Helper()
	verifier, err := NewVerifier(provider, contexts, VerifierOptions{})
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return verifier
}

var _ VerifierContextProvider = (*verifierNilContextProvider)(nil)
