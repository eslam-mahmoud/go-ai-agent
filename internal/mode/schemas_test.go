package mode

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/eslam-mahmoud/go-ai-agent/internal/workflow"
)

func TestBuiltinDefinitionsCompileAndDeclareFreshReviewer(t *testing.T) {
	definitions, err := BuiltinDefinitions()
	if err != nil {
		t.Fatal(err)
	}
	want := []workflow.ModeName{
		workflow.ModePlanner,
		workflow.ModeDeveloper,
		workflow.ModeReviewer,
		workflow.ModeFixer,
		workflow.ModeVerifier,
		workflow.ModeManager,
		workflow.ModeArchitect,
	}
	if len(definitions) != len(want) {
		t.Fatalf("definitions = %d, want %d", len(definitions), len(want))
	}
	for index, definition := range definitions {
		if definition.Name != want[index] ||
			definition.Description == "" ||
			!json.Valid(definition.OutputSchema) ||
			definition.FreshSession != (definition.Name == workflow.ModeReviewer) {
			t.Fatalf("definition %d = %#v", index, definition)
		}
		registry, err := NewRegistry(&registryTestMode{definition: definition})
		if err != nil || len(registry.Names()) != 1 {
			t.Fatalf("register %s registry=%#v error=%v", definition.Name, registry, err)
		}
	}
	if definition, err := BuiltinDefinition("unknown"); definition.Name != "" ||
		!errors.Is(err, ErrModeNotFound) {
		t.Fatalf("unknown definition=%#v error=%v", definition, err)
	}
}

func TestBuiltinSchemasAcceptCanonicalOutputsAndRejectDrift(t *testing.T) {
	outputs := map[workflow.ModeName]json.RawMessage{
		workflow.ModePlanner:   validPlannerOutput(OutputCompleted),
		workflow.ModeDeveloper: validDeveloperOutput(OutputCompleted),
		workflow.ModeReviewer:  validReviewerOutput(OutputCompleted, true),
		workflow.ModeFixer:     validFixerOutput(OutputCompleted),
		workflow.ModeVerifier:  validVerifierOutput(OutputCompleted),
		workflow.ModeManager:   validManagerOutput(OutputCompleted),
	}
	for name, raw := range outputs {
		t.Run(string(name), func(t *testing.T) {
			definition := mustBuiltinDefinition(t, name)
			registry, err := NewRegistry(&registryTestMode{definition: definition})
			if err != nil {
				t.Fatal(err)
			}
			output, err := registry.ValidateOutput(name, raw)
			if err != nil {
				t.Fatal(err)
			}
			if output.Status != OutputCompleted ||
				output.Summary == "" ||
				len(output.Raw) == 0 {
				t.Fatalf("output = %#v", output)
			}
			var object map[string]any
			if err := json.Unmarshal(raw, &object); err != nil {
				t.Fatal(err)
			}
			delete(object, "summary")
			missing, _ := json.Marshal(object)
			if _, err := registry.ValidateOutput(name, missing); !errors.Is(
				err,
				ErrInvalidModeOutput,
			) {
				t.Fatalf("missing-field error = %v", err)
			}
			object["summary"] = "restored"
			object["unexpected"] = true
			extra, _ := json.Marshal(object)
			if _, err := registry.ValidateOutput(name, extra); !errors.Is(
				err,
				ErrInvalidModeOutput,
			) {
				t.Fatalf("extra-field error = %v", err)
			}
		})
	}
}

func TestCommonOutputStatusAndQuestionContract(t *testing.T) {
	definition := mustBuiltinDefinition(t, workflow.ModePlanner)
	registry, _ := NewRegistry(&registryTestMode{definition: definition})
	needsInput := validPlannerOutput(OutputNeedsInput)
	output, err := registry.ValidateOutput(workflow.ModePlanner, needsInput)
	if err != nil {
		t.Fatal(err)
	}
	if output.Status != OutputNeedsInput ||
		output.Question == nil ||
		output.WorkflowOutcome().Status != workflow.ModeNeedsInput {
		t.Fatalf("needs-input output = %#v outcome=%#v", output, output.WorkflowOutcome())
	}
	for _, raw := range []json.RawMessage{
		json.RawMessage(`{}`),
		validPlannerOutput(OutputStatus("unknown")),
		plannerOutputWithQuestion(OutputCompleted, "should be null"),
		plannerOutputWithQuestion(OutputNeedsInput, ""),
	} {
		if _, err := registry.ValidateOutput(
			workflow.ModePlanner,
			raw,
		); !errors.Is(err, ErrInvalidModeOutput) {
			t.Fatalf("invalid output %s error = %v", raw, err)
		}
	}
}

func validPlannerOutput(status OutputStatus) json.RawMessage {
	question := any(nil)
	if status == OutputNeedsInput {
		question = "Which API behavior should be preserved?"
	}
	return mustJSON(map[string]any{
		"status":                  status,
		"summary":                 "Plan prepared.",
		"question":                question,
		"discoveries":             []any{},
		"risks":                   []any{},
		"recommended_next_action": "Implement the plan.",
		"acceptance_criteria":     []string{"Behavior is covered."},
		"implementation_steps":    []string{"Add implementation."},
		"verification_commands":   []string{"go test ./..."},
		"split_recommended":       false,
	})
}

func plannerOutputWithQuestion(
	status OutputStatus,
	question string,
) json.RawMessage {
	var object map[string]any
	_ = json.Unmarshal(validPlannerOutput(status), &object)
	object["question"] = question
	return mustJSON(object)
}

func validDeveloperOutput(status OutputStatus) json.RawMessage {
	commit := any("abcdef1")
	if status != OutputCompleted {
		commit = nil
	}
	return mustJSON(map[string]any{
		"status":                  status,
		"summary":                 "Implementation complete.",
		"question":                nil,
		"discoveries":             []any{},
		"risks":                   []any{},
		"recommended_next_action": "Review the change.",
		"changed_files":           []string{"internal/example.go"},
		"commit_sha":              commit,
		"tests":                   []string{"go test ./..."},
		"pr_number":               42,
	})
}

func validReviewerOutput(
	status OutputStatus,
	blocking bool,
) json.RawMessage {
	findings := []any{}
	if blocking {
		findings = append(findings, map[string]any{
			"title":       "Missing rollback",
			"description": "The transaction can partially commit.",
			"severity":    "high",
		})
	}
	return mustJSON(map[string]any{
		"status":                  status,
		"summary":                 "Review complete.",
		"question":                nil,
		"discoveries":             []any{},
		"risks":                   []any{},
		"recommended_next_action": "Fix blocking findings.",
		"acceptance_criteria_met": !blocking,
		"blocking_findings":       findings,
		"future_improvements":     []string{},
	})
}

func validFixerOutput(status OutputStatus) json.RawMessage {
	return mustJSON(map[string]any{
		"status":                  status,
		"summary":                 "Findings addressed.",
		"question":                nil,
		"discoveries":             []any{},
		"risks":                   []any{},
		"recommended_next_action": "Review the fixes.",
		"addressed_findings":      []string{"Missing rollback"},
		"changed_files":           []string{"internal/example.go"},
		"commit_sha":              "abcdef1",
		"tests":                   []string{"go test ./..."},
	})
}

func validVerifierOutput(status OutputStatus) json.RawMessage {
	return mustJSON(map[string]any{
		"status":                  status,
		"summary":                 "Verification complete.",
		"question":                nil,
		"discoveries":             []any{},
		"risks":                   []any{},
		"recommended_next_action": "Complete the task.",
		"acceptance_results": []any{map[string]any{
			"criterion": "Behavior is covered.",
			"passed":    true,
			"evidence":  "Unit test passed.",
		}},
		"verification_commands": []any{map[string]any{
			"command": "go test ./...",
			"passed":  true,
			"output":  "ok",
		}},
		"branch_consistent":           true,
		"pr_consistent":               true,
		"working_tree_clean":          true,
		"ci_passed":                   true,
		"blocking_findings_remaining": 0,
	})
}

func validManagerOutput(status OutputStatus) json.RawMessage {
	return mustJSON(map[string]any{
		"status":                       status,
		"summary":                      "Project reviewed.",
		"question":                     nil,
		"discoveries":                  []any{},
		"risks":                        []any{},
		"recommended_next_action":      "Start the selected task.",
		"project_health":               "on-track",
		"progress_estimate":            50,
		"completed_task_decision":      "accepted",
		"architecture_review_required": false,
		"human_approval_required":      false,
		"discovery_decisions":          []any{},
		"backlog_changes":              []any{},
		"next_task": map[string]any{
			"issue_number": 136,
			"reason":       "Next dependency.",
		},
		"release_readiness": "not-ready",
		"owner_update":      "Project remains on track.",
	})
}

func mustJSON(value any) json.RawMessage {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return raw
}
