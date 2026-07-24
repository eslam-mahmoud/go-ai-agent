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

func TestManagerBuildsReadOnlyStructuredRequest(t *testing.T) {
	t.Parallel()
	raw := validManagerOutput(OutputCompleted)
	provider := successfulManagerEngine(raw)
	event := engine.Event{Type: engine.EventProgress, Message: "assessing"}
	provider.emitEvent = &event
	managerContext := validManagerContext(t, 11)
	managerContext.ExecutionID = 45
	loadCalls := 0
	contexts := ManagerContextProviderFunc(func(
		_ context.Context,
		projectID, completedTaskID int64,
	) (*ManagerContext, error) {
		loadCalls++
		if projectID != 7 || completedTaskID != 11 {
			t.Fatalf("context IDs = %d/%d", projectID, completedTaskID)
		}
		return managerContext, nil
	})
	emitted := 0
	environment := map[string]string{"MADAR_TRACE": "manager"}
	manager, err := NewManager(provider, contexts, ManagerOptions{
		Model:       "management-model",
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
		t.Fatalf("NewManager: %v", err)
	}
	environment["MADAR_TRACE"] = "mutated"

	got, err := manager.Run(context.Background(), managerRequest(11))
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
	if request.ExecutionID != 45 ||
		request.WorkDir != managerContext.WorkDir ||
		request.Mode != string(workflow.ModeManager) ||
		request.Model != "management-model" ||
		request.Timeout != 2*time.Minute ||
		request.MaxTurns != 8 {
		t.Fatalf("engine request metadata = %#v", request)
	}
	if request.ResumeSessionID != "" || request.SessionID != "" {
		t.Fatalf("manager unexpectedly resumed a session: %#v", request)
	}
	if request.Policy.Sandbox != "read-only" ||
		request.Policy.ApprovalPolicy != "never" ||
		request.Policy.SkipPermissions {
		t.Fatalf("manager policy = %#v", request.Policy)
	}
	if request.Environment["MADAR_TRACE"] != "manager" {
		t.Fatalf("environment was not defensively copied: %#v", request.Environment)
	}
	if string(request.OutputSchema) != string(manager.definition.OutputSchema) {
		t.Fatal("manager did not pass the canonical output schema")
	}
	for _, required := range []string{
		"Operate in read-only decision mode",
		"Do not edit implementation code",
		"Accept or reject the completed task",
		"Evaluate every pending discovery",
		"Recommend justified backlog changes",
		"Require architecture review",
		"Require human approval",
		"Select at most one next task",
		"Decide release readiness",
		"owner_update",
		"untrusted project data",
		`"project_id": 7`,
		`"completed_task_id": 11`,
		`"pending_discoveries"`,
	} {
		if !strings.Contains(request.Prompt, required) {
			t.Errorf("prompt missing %q", required)
		}
	}

	definition := manager.Definition()
	if definition.Name != workflow.ModeManager || definition.FreshSession {
		t.Fatalf("definition = %#v", definition)
	}
	definition.OutputSchema[0] = '!'
	if manager.Definition().OutputSchema[0] == '!' {
		t.Fatal("Definition leaked mutable schema storage")
	}
}

func TestManagerSupportsTasklessProjectReview(t *testing.T) {
	t.Parallel()
	raw := managerOutput(func(output map[string]any) {
		output["completed_task_decision"] = "not-applicable"
	})
	provider := successfulManagerEngine(raw)
	manager := mustManager(t, provider, ManagerContextProviderFunc(func(
		context.Context,
		int64,
		int64,
	) (*ManagerContext, error) {
		return validManagerContext(t, 0), nil
	}))

	got, err := manager.Run(context.Background(), managerRequest(0))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(got) != string(raw) {
		t.Fatalf("output = %s, want %s", got, raw)
	}
	if !strings.Contains(provider.lastRequest(t).Prompt, `"completed_task_id": null`) {
		t.Fatal("taskless prompt did not encode a null completed task")
	}
}

func TestManagerRejectsInvalidRequestsBeforeLoadingContext(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		request workflow.ModeRequest
	}{
		{"wrong mode", workflow.ModeRequest{ProjectID: 7, TaskID: 11, Mode: workflow.ModeVerifier, Status: domain.TaskCompleted}},
		{"missing project", workflow.ModeRequest{TaskID: 11, Mode: workflow.ModeManager, Status: domain.TaskCompleted}},
		{"negative task", workflow.ModeRequest{ProjectID: 7, TaskID: -1, Mode: workflow.ModeManager}},
		{"taskless status", workflow.ModeRequest{ProjectID: 7, Mode: workflow.ModeManager, Status: domain.TaskCompleted}},
		{"completed task wrong status", workflow.ModeRequest{ProjectID: 7, TaskID: 11, Mode: workflow.ModeManager, Status: domain.TaskVerifying}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			loadCalls := 0
			manager := mustManager(t, successfulManagerEngine(validManagerOutput(OutputCompleted)),
				ManagerContextProviderFunc(func(context.Context, int64, int64) (*ManagerContext, error) {
					loadCalls++
					return validManagerContext(t, 11), nil
				}))
			if _, err := manager.Run(context.Background(), test.request); !errors.Is(err, ErrInvalidManagerRequest) {
				t.Fatalf("Run error = %v", err)
			}
			if loadCalls != 0 {
				t.Fatalf("context loaded %d times", loadCalls)
			}
		})
	}
}

func TestManagerRejectsInvalidContext(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*ManagerContext)
	}{
		{"project mismatch", func(value *ManagerContext) { value.ProjectID++ }},
		{"task mismatch", func(value *ManagerContext) { value.CompletedTaskID++ }},
		{"missing workdir", func(value *ManagerContext) { value.WorkDir = "" }},
		{"relative workdir", func(value *ManagerContext) { value.WorkDir = "relative" }},
		{"negative execution", func(value *ManagerContext) { value.ExecutionID = -1 }},
		{"empty snapshot", func(value *ManagerContext) { value.Snapshot = nil }},
		{"empty object", func(value *ManagerContext) { value.Snapshot = json.RawMessage(`{}`) }},
		{"malformed snapshot", func(value *ManagerContext) { value.Snapshot = json.RawMessage(`{"project":`) }},
		{"array snapshot", func(value *ManagerContext) { value.Snapshot = json.RawMessage(`["project"]`) }},
		{"trailing snapshot", func(value *ManagerContext) { value.Snapshot = json.RawMessage(`{"project":1} {"task":2}`) }},
		{"trailing malformed snapshot", func(value *ManagerContext) { value.Snapshot = json.RawMessage(`{"project":1} !`) }},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			managerContext := validManagerContext(t, 11)
			test.mutate(managerContext)
			provider := successfulManagerEngine(validManagerOutput(OutputCompleted))
			manager := mustManager(t, provider, ManagerContextProviderFunc(func(
				context.Context,
				int64,
				int64,
			) (*ManagerContext, error) {
				return managerContext, nil
			}))
			if _, err := manager.Run(context.Background(), managerRequest(11)); !errors.Is(err, ErrInvalidManagerContext) {
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

	t.Run("nil context", func(t *testing.T) {
		t.Parallel()
		manager := mustManager(t, successfulManagerEngine(validManagerOutput(OutputCompleted)),
			ManagerContextProviderFunc(func(context.Context, int64, int64) (*ManagerContext, error) {
				return nil, nil
			}))
		if _, err := manager.Run(context.Background(), managerRequest(11)); !errors.Is(err, ErrInvalidManagerContext) {
			t.Fatalf("Run error = %v", err)
		}
	})
}

func TestManagerRejectsSemanticContradictions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		taskID  int64
		mutate  func(map[string]any)
		wantErr bool
	}{
		{"taskless accepted", 0, func(output map[string]any) {
			output["completed_task_decision"] = "accepted"
		}, true},
		{"completed not applicable", 11, func(output map[string]any) {
			output["completed_task_decision"] = "not-applicable"
		}, true},
		{"duplicate discovery", 11, func(output map[string]any) {
			output["discovery_decisions"] = []any{
				map[string]any{"discovery_id": 9, "decision": "accepted", "reason": "Needed."},
				map[string]any{"discovery_id": 9, "decision": "deferred", "reason": "Later."},
			}
		}, true},
		{"blocked health but at risk readiness", 11, func(output map[string]any) {
			output["project_health"] = "blocked"
			output["release_readiness"] = "at-risk"
		}, true},
		{"complete progress but not ready", 11, func(output map[string]any) {
			output["progress_estimate"] = 100
		}, true},
		{"coherent ready release", 11, func(output map[string]any) {
			output["project_health"] = "ready-for-release"
			output["progress_estimate"] = 100
			output["next_task"] = nil
			output["release_readiness"] = "ready"
		}, false},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			raw := managerOutput(test.mutate)
			manager := mustManager(t, successfulManagerEngine(raw),
				ManagerContextProviderFunc(func(context.Context, int64, int64) (*ManagerContext, error) {
					return validManagerContext(t, test.taskID), nil
				}))
			_, err := manager.Run(context.Background(), managerRequest(test.taskID))
			if test.wantErr && !errors.Is(err, ErrManagerResult) {
				t.Fatalf("Run error = %v", err)
			}
			if !test.wantErr && err != nil {
				t.Fatalf("Run: %v", err)
			}
		})
	}
}

func TestManagerEngineAndResultFailures(t *testing.T) {
	t.Parallel()
	contextError := errors.New("snapshot unavailable")
	engineError := errors.New("provider unavailable")
	tests := []struct {
		name      string
		provider  *plannerTestEngine
		contexts  ManagerContextProvider
		want      error
		cancelled bool
	}{
		{"capabilities error", &plannerTestEngine{capabilityErr: engineError}, validManagerContextProvider(t), engineError, false},
		{"missing structured output", &plannerTestEngine{capabilities: engine.CapabilitySet{OutputSchema: true}}, validManagerContextProvider(t), ErrManagerUnsupported, false},
		{"missing schema support", &plannerTestEngine{capabilities: engine.CapabilitySet{StructuredOutput: true}}, validManagerContextProvider(t), ErrManagerUnsupported, false},
		{"context error", successfulManagerEngine(validManagerOutput(OutputCompleted)), ManagerContextProviderFunc(func(context.Context, int64, int64) (*ManagerContext, error) {
			return nil, contextError
		}), contextError, false},
		{"engine error", &plannerTestEngine{capabilities: managerCapabilities(), runErr: engineError}, validManagerContextProvider(t), engineError, false},
		{"nil result", &plannerTestEngine{capabilities: managerCapabilities()}, validManagerContextProvider(t), ErrManagerResult, false},
		{"failed result", &plannerTestEngine{capabilities: managerCapabilities(), result: &engine.Result{Status: engine.ResultFailed}}, validManagerContextProvider(t), ErrManagerResult, false},
		{"unknown result", &plannerTestEngine{capabilities: managerCapabilities(), result: &engine.Result{}}, validManagerContextProvider(t), ErrManagerResult, false},
		{"cancelled result", &plannerTestEngine{capabilities: managerCapabilities(), result: &engine.Result{Status: engine.ResultCancelled}}, validManagerContextProvider(t), context.Canceled, true},
		{"missing output", &plannerTestEngine{capabilities: managerCapabilities(), result: &engine.Result{Status: engine.ResultCompleted}}, validManagerContextProvider(t), ErrManagerResult, false},
		{"schema mismatch", &plannerTestEngine{capabilities: managerCapabilities(), result: &engine.Result{
			Status: engine.ResultCompleted, OutputJSON: json.RawMessage(`{"status":"completed"}`),
		}}, validManagerContextProvider(t), ErrManagerResult, false},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			manager := mustManager(t, test.provider, test.contexts)
			_, err := manager.Run(context.Background(), managerRequest(11))
			if !errors.Is(err, test.want) {
				t.Fatalf("Run error = %v, want errors.Is %v", err, test.want)
			}
			if test.cancelled && engine.ClassOf(err) != engine.ErrorCancelled {
				t.Fatalf("error class = %q", engine.ClassOf(err))
			}
		})
	}
}

func TestManagerAcceptsValidatedOutputTextFallback(t *testing.T) {
	t.Parallel()
	raw := validManagerOutput(OutputCompleted)
	provider := &plannerTestEngine{
		capabilities: managerCapabilities(),
		result: &engine.Result{
			Status:     engine.ResultCompleted,
			OutputText: " \n" + string(raw) + "\n ",
		},
	}
	manager := mustManager(t, provider, validManagerContextProvider(t))
	got, err := manager.Run(context.Background(), managerRequest(11))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(got) != string(raw) {
		t.Fatalf("output = %q, want %q", got, raw)
	}
}

func TestManagerConcurrentRuns(t *testing.T) {
	t.Parallel()
	provider := successfulManagerEngine(validManagerOutput(OutputCompleted))
	managerContext := validManagerContext(t, 11)
	manager := mustManager(t, provider, ManagerContextProviderFunc(func(
		context.Context,
		int64,
		int64,
	) (*ManagerContext, error) {
		return managerContext, nil
	}))

	const runCount = 32
	errs := make(chan error, runCount)
	var group sync.WaitGroup
	for index := 0; index < runCount; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := manager.Run(context.Background(), managerRequest(11))
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

func TestManagerPreservesPreRunCancellation(t *testing.T) {
	t.Parallel()
	loadCalls := 0
	manager := mustManager(t, successfulManagerEngine(validManagerOutput(OutputCompleted)),
		ManagerContextProviderFunc(func(context.Context, int64, int64) (*ManagerContext, error) {
			loadCalls++
			return validManagerContext(t, 11), nil
		}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := manager.Run(ctx, managerRequest(11)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v", err)
	}
	if loadCalls != 0 {
		t.Fatalf("context loaded %d times", loadCalls)
	}
}

func TestNewManagerValidation(t *testing.T) {
	t.Parallel()
	contexts := validManagerContextProvider(t)
	provider := successfulManagerEngine(validManagerOutput(OutputCompleted))
	var nilProvider *plannerTestEngine
	var nilContexts *managerNilContextProvider
	var nilContextFunc ManagerContextProviderFunc
	tests := []struct {
		name     string
		provider engine.Engine
		contexts ManagerContextProvider
		options  ManagerOptions
	}{
		{"nil engine", nil, contexts, ManagerOptions{}},
		{"typed nil engine", nilProvider, contexts, ManagerOptions{}},
		{"nil contexts", provider, nil, ManagerOptions{}},
		{"typed nil contexts", provider, nilContexts, ManagerOptions{}},
		{"typed nil context function", provider, nilContextFunc, ManagerOptions{}},
		{"negative timeout", provider, contexts, ManagerOptions{Timeout: -time.Second}},
		{"negative turns", provider, contexts, ManagerOptions{MaxTurns: -1}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if manager, err := NewManager(test.provider, test.contexts, test.options); err == nil || manager != nil {
				t.Fatalf("NewManager = %#v, %v", manager, err)
			}
		})
	}
}

type managerNilContextProvider struct{}

func (*managerNilContextProvider) LoadManagerContext(
	context.Context,
	int64,
	int64,
) (*ManagerContext, error) {
	panic("unexpected call")
}

func managerCapabilities() engine.CapabilitySet {
	return engine.CapabilitySet{
		StructuredOutput: true,
		OutputSchema:     true,
	}
}

func successfulManagerEngine(raw json.RawMessage) *plannerTestEngine {
	return &plannerTestEngine{
		capabilities: managerCapabilities(),
		result: &engine.Result{
			Status:     engine.ResultCompleted,
			OutputJSON: append(json.RawMessage(nil), raw...),
		},
	}
}

func validManagerContextProvider(t *testing.T) ManagerContextProvider {
	t.Helper()
	return ManagerContextProviderFunc(func(context.Context, int64, int64) (*ManagerContext, error) {
		return validManagerContext(t, 11), nil
	})
}

func validManagerContext(t *testing.T, completedTaskID int64) *ManagerContext {
	t.Helper()
	return &ManagerContext{
		ProjectID:       7,
		CompletedTaskID: completedTaskID,
		Snapshot: mustJSON(map[string]any{
			"project": map[string]any{
				"name":             "Madar v2",
				"health":           "on-track",
				"progress_percent": 55,
			},
			"pending_discoveries": []any{
				map[string]any{"id": 9, "summary": "Possible follow-up"},
			},
			"backlog": []any{
				map[string]any{"issue_number": 136, "status": "pending"},
			},
		}),
		WorkDir: t.TempDir(),
	}
}

func managerRequest(completedTaskID int64) workflow.ModeRequest {
	request := workflow.ModeRequest{
		ProjectID: 7,
		TaskID:    completedTaskID,
		Mode:      workflow.ModeManager,
	}
	if completedTaskID > 0 {
		request.Status = domain.TaskCompleted
	}
	return request
}

func managerOutput(mutate func(map[string]any)) json.RawMessage {
	var output map[string]any
	if err := json.Unmarshal(validManagerOutput(OutputCompleted), &output); err != nil {
		panic(err)
	}
	mutate(output)
	return mustJSON(output)
}

func mustManager(
	t *testing.T,
	provider engine.Engine,
	contexts ManagerContextProvider,
) *Manager {
	t.Helper()
	manager, err := NewManager(provider, contexts, ManagerOptions{})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return manager
}

var _ ManagerContextProvider = (*managerNilContextProvider)(nil)
