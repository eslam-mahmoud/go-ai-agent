package mode

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/engine"
	"github.com/eslam-mahmoud/go-ai-agent/internal/workflow"
)

func TestArchitectBuildsReadOnlyStructuredRequest(t *testing.T) {
	t.Parallel()
	raw := validArchitectOutput(OutputCompleted, nil)
	provider := successfulArchitectEngine(raw)
	architectContext := validArchitectContext(t)
	architectContext.ExecutionID = 51
	loadCalls := 0
	contexts := ArchitectContextProviderFunc(func(
		_ context.Context,
		projectID int64,
	) (*ArchitectContext, error) {
		loadCalls++
		if projectID != 7 {
			t.Fatalf("context project ID = %d", projectID)
		}
		return architectContext, nil
	})
	architect, err := NewArchitect(provider, contexts, ArchitectOptions{
		Model:    "architecture-model",
		Timeout:  2 * time.Minute,
		MaxTurns: 6,
	})
	if err != nil {
		t.Fatal(err)
	}
	output, err := architect.Run(context.Background(), workflow.ModeRequest{
		ProjectID: 7,
		Mode:      workflow.ModeArchitect,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if loadCalls != 1 {
		t.Fatalf("context loaded %d times", loadCalls)
	}
	if string(output) != string(raw) {
		t.Fatalf("output = %s", output)
	}
	request := provider.lastRequest(t)
	if request.Mode != string(workflow.ModeArchitect) ||
		request.Model != "architecture-model" ||
		request.ExecutionID != 51 {
		t.Fatalf("request = %#v", request)
	}
	if request.Policy.Sandbox != "read-only" || request.Policy.ApprovalPolicy != "never" {
		t.Fatalf("policy = %#v", request.Policy)
	}
	if len(request.OutputSchema) == 0 {
		t.Fatal("request carried no output schema")
	}
	for _, fragment := range []string{
		"read-only decision mode",
		"untrusted project data",
		"outstanding_discovery_id",
		`"outstanding_discovery_ids": [`,
	} {
		if !strings.Contains(request.Prompt, fragment) {
			t.Fatalf("prompt missing %q:\n%s", fragment, request.Prompt)
		}
	}
}

func TestArchitectRejectsInvalidRequestsAndContexts(t *testing.T) {
	t.Parallel()
	t.Run("requests", func(t *testing.T) {
		t.Parallel()
		architect := newTestArchitect(t, successfulArchitectEngine(
			validArchitectOutput(OutputCompleted, nil),
		))
		requests := []workflow.ModeRequest{
			{ProjectID: 7, Mode: workflow.ModeManager},
			{ProjectID: 0, Mode: workflow.ModeArchitect},
			{ProjectID: 7, Mode: workflow.ModeArchitect, TaskID: -1},
		}
		for _, request := range requests {
			if _, err := architect.Run(context.Background(), request); !errors.Is(
				err, ErrInvalidArchitectRequest,
			) {
				t.Fatalf("request %#v error = %v", request, err)
			}
		}
	})

	t.Run("contexts", func(t *testing.T) {
		t.Parallel()
		cases := []struct {
			name   string
			mutate func(*ArchitectContext)
		}{
			{"wrong project", func(c *ArchitectContext) { c.ProjectID = 9 }},
			{"relative workdir", func(c *ArchitectContext) { c.WorkDir = "relative/path" }},
			{"missing workdir", func(c *ArchitectContext) { c.WorkDir = "  " }},
			{"negative execution", func(c *ArchitectContext) { c.ExecutionID = -1 }},
			{"empty snapshot", func(c *ArchitectContext) { c.Snapshot = json.RawMessage(`{}`) }},
			{"snapshot not an object", func(c *ArchitectContext) {
				c.Snapshot = json.RawMessage(`[]`)
			}},
			{"trailing snapshot JSON", func(c *ArchitectContext) {
				c.Snapshot = json.RawMessage(`{"a":1} {}`)
			}},
			{"bad discovery ID", func(c *ArchitectContext) {
				c.OutstandingDiscoveryIDs = []int64{0}
			}},
		}
		for _, test := range cases {
			test := test
			t.Run(test.name, func(t *testing.T) {
				t.Parallel()
				architectContext := validArchitectContext(t)
				test.mutate(architectContext)
				architect, err := NewArchitect(
					successfulArchitectEngine(validArchitectOutput(OutputCompleted, nil)),
					ArchitectContextProviderFunc(func(
						context.Context, int64,
					) (*ArchitectContext, error) {
						return architectContext, nil
					}),
					ArchitectOptions{},
				)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := architect.Run(context.Background(), workflow.ModeRequest{
					ProjectID: 7,
					Mode:      workflow.ModeArchitect,
				}); !errors.Is(err, ErrInvalidArchitectContext) {
					t.Fatalf("error = %v", err)
				}
			})
		}
	})
}

func TestArchitectRequiresStructuredOutputCapabilities(t *testing.T) {
	t.Parallel()
	provider := successfulArchitectEngine(validArchitectOutput(OutputCompleted, nil))
	provider.capabilities = engine.CapabilitySet{StructuredOutput: false}
	architect := newTestArchitect(t, provider)
	if _, err := architect.Run(context.Background(), workflow.ModeRequest{
		ProjectID: 7,
		Mode:      workflow.ModeArchitect,
	}); !errors.Is(err, ErrArchitectUnsupported) {
		t.Fatalf("error = %v", err)
	}
}

func TestArchitectEnforcesSemanticsTheSchemaCannot(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		output json.RawMessage
	}{
		{
			"addresses a discovery that was not outstanding",
			validArchitectOutput(OutputCompleted, map[string]any{
				"addressed_discovery_ids": []int64{404},
			}),
		},
		{
			"duplicate component",
			validArchitectOutput(OutputCompleted, map[string]any{
				"components": []map[string]any{
					{"name": "store", "responsibility": "persistence"},
					{"name": "store", "responsibility": "persistence again"},
				},
			}),
		},
		{
			"dependency on an unknown component",
			validArchitectOutput(OutputCompleted, map[string]any{
				"dependencies": []map[string]any{
					{"from": "store", "to": "nowhere", "reason": "unclear"},
				},
			}),
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			architect := newTestArchitect(t, successfulArchitectEngine(test.output))
			if _, err := architect.Run(context.Background(), workflow.ModeRequest{
				ProjectID: 7,
				Mode:      workflow.ModeArchitect,
			}); !errors.Is(err, ErrArchitectResult) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestArchitectPassesThroughNonCompletedStatuses(t *testing.T) {
	t.Parallel()
	needsInput := validArchitectOutput(OutputNeedsInput, map[string]any{
		"question": "Which service owns scheduling?",
	})
	architect := newTestArchitect(t, successfulArchitectEngine(needsInput))
	output, err := architect.Run(context.Background(), workflow.ModeRequest{
		ProjectID: 7,
		Mode:      workflow.ModeArchitect,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(string(output), "Which service owns scheduling?") {
		t.Fatalf("output = %s", output)
	}
}

func TestArchitectSurfacesEngineFailures(t *testing.T) {
	t.Parallel()
	for _, status := range []engine.ResultStatus{
		engine.ResultFailed,
		engine.ResultStatus("nonsense"),
	} {
		provider := successfulArchitectEngine(validArchitectOutput(OutputCompleted, nil))
		provider.result.Status = status
		architect := newTestArchitect(t, provider)
		if _, err := architect.Run(context.Background(), workflow.ModeRequest{
			ProjectID: 7,
			Mode:      workflow.ModeArchitect,
		}); !errors.Is(err, ErrArchitectResult) {
			t.Fatalf("status %q error = %v", status, err)
		}
	}
	cancelled := successfulArchitectEngine(validArchitectOutput(OutputCompleted, nil))
	cancelled.result.Status = engine.ResultCancelled
	architect := newTestArchitect(t, cancelled)
	if _, err := architect.Run(context.Background(), workflow.ModeRequest{
		ProjectID: 7,
		Mode:      workflow.ModeArchitect,
	}); err == nil {
		t.Fatal("cancelled run reported success")
	}
}

func TestNewArchitectValidatesDependencies(t *testing.T) {
	t.Parallel()
	provider := successfulArchitectEngine(validArchitectOutput(OutputCompleted, nil))
	contexts := ArchitectContextProviderFunc(func(
		context.Context, int64,
	) (*ArchitectContext, error) {
		return nil, nil
	})
	if _, err := NewArchitect(nil, contexts, ArchitectOptions{}); err == nil {
		t.Error("missing engine accepted")
	}
	if _, err := NewArchitect(provider, nil, ArchitectOptions{}); err == nil {
		t.Error("missing context provider accepted")
	}
	if _, err := NewArchitect(provider, contexts, ArchitectOptions{
		Timeout: -time.Second,
	}); err == nil {
		t.Error("negative timeout accepted")
	}
	if _, err := NewArchitect(provider, contexts, ArchitectOptions{
		MaxTurns: -1,
	}); err == nil {
		t.Error("negative max turns accepted")
	}
	architect := newTestArchitect(t, provider)
	if architect.Definition().Name != workflow.ModeArchitect {
		t.Fatalf("definition = %#v", architect.Definition())
	}
}

func newTestArchitect(t *testing.T, provider *plannerTestEngine) *Architect {
	t.Helper()
	architectContext := validArchitectContext(t)
	architect, err := NewArchitect(
		provider,
		ArchitectContextProviderFunc(func(
			context.Context, int64,
		) (*ArchitectContext, error) {
			return architectContext, nil
		}),
		ArchitectOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	return architect
}

func successfulArchitectEngine(raw json.RawMessage) *plannerTestEngine {
	return &plannerTestEngine{
		capabilities: managerCapabilities(),
		result: &engine.Result{
			Status:     engine.ResultCompleted,
			OutputJSON: append(json.RawMessage(nil), raw...),
		},
	}
}

func validArchitectContext(t *testing.T) *ArchitectContext {
	t.Helper()
	return &ArchitectContext{
		ProjectID:               7,
		OutstandingDiscoveryIDs: []int64{9},
		Snapshot: mustJSON(map[string]any{
			"project": map[string]any{"name": "Madar v2", "health": "at-risk"},
			"architecture_risks": []any{
				map[string]any{"id": 9, "title": "Cross-cutting cache change"},
			},
		}),
		WorkDir: t.TempDir(),
	}
}

func validArchitectOutput(
	status OutputStatus,
	overrides map[string]any,
) json.RawMessage {
	output := map[string]any{
		"status":                  string(status),
		"summary":                 "Defined the delivery boundaries.",
		"question":                nil,
		"discoveries":             []any{},
		"risks":                   []any{},
		"recommended_next_action": "Apply the recorded decisions.",
		"components": []map[string]any{
			{"name": "store", "responsibility": "Own durable state."},
		},
		"decisions": []map[string]any{
			{
				"title":     "Keep SQLite single-writer",
				"decision":  "Serialize all writes through one connection.",
				"rationale": "Avoids cross-process write contention.",
			},
		},
		"dependencies":            []any{},
		"recommended_tasks":       []any{},
		"addressed_discovery_ids": []int64{9},
		"architecture_summary":    "One binary, one writer, provider-neutral engines.",
	}
	for key, value := range overrides {
		output[key] = value
	}
	return mustJSON(output)
}
