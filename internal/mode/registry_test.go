package mode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/eslam-mahmoud/go-ai-agent/internal/workflow"
)

type registryTestMode struct {
	definition Definition
	output     json.RawMessage
	err        error
	requests   []workflow.ModeRequest
	mu         sync.Mutex
}

func (mode *registryTestMode) Definition() Definition {
	return mode.definition
}

func (mode *registryTestMode) Run(
	_ context.Context,
	request workflow.ModeRequest,
) (json.RawMessage, error) {
	mode.mu.Lock()
	mode.requests = append(mode.requests, request)
	mode.mu.Unlock()
	return append(json.RawMessage(nil), mode.output...), mode.err
}

func TestRegistryZeroValueRegistersResolvesAndDefensivelyOwnsDefinitions(t *testing.T) {
	plannerDefinition := mustBuiltinDefinition(t, workflow.ModePlanner)
	planner := &registryTestMode{definition: plannerDefinition}
	developer := &registryTestMode{
		definition: mustBuiltinDefinition(t, workflow.ModeDeveloper),
	}
	var registry Registry
	if err := registry.Register(developer); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(planner); err != nil {
		t.Fatal(err)
	}
	if got, err := registry.Resolve(workflow.ModePlanner); err != nil || got != planner {
		t.Fatalf("resolved mode=%#v error=%v", got, err)
	}
	if names := registry.Names(); !reflect.DeepEqual(names, []workflow.ModeName{
		workflow.ModeDeveloper,
		workflow.ModePlanner,
	}) {
		t.Fatalf("names = %v", names)
	}

	planner.definition.OutputSchema[0] = '!'
	stored, err := registry.Definition(workflow.ModePlanner)
	if err != nil || !json.Valid(stored.OutputSchema) {
		t.Fatalf("stored definition=%#v error=%v", stored, err)
	}
	stored.OutputSchema[0] = '!'
	again, _ := registry.Definition(workflow.ModePlanner)
	if !json.Valid(again.OutputSchema) {
		t.Fatal("caller mutated the registry's schema")
	}
	names := registry.Names()
	names[0] = "changed"
	if registry.Names()[0] != workflow.ModeDeveloper {
		t.Fatal("caller mutated registry names")
	}
}

func TestRegistryRejectsNilInvalidDuplicateAndMalformedDefinitions(t *testing.T) {
	var typedNil *registryTestMode
	valid := mustBuiltinDefinition(t, workflow.ModePlanner)
	tests := []struct {
		name string
		mode Mode
		want error
	}{
		{"nil", nil, ErrNilMode},
		{"typed nil", typedNil, ErrNilMode},
		{"empty name", &registryTestMode{definition: Definition{
			OutputSchema: valid.OutputSchema,
		}}, ErrInvalidModeName},
		{"uppercase", &registryTestMode{definition: Definition{
			Name: "Planner", OutputSchema: valid.OutputSchema,
		}}, ErrInvalidModeName},
		{"leading digit", &registryTestMode{definition: Definition{
			Name: "2planner", OutputSchema: valid.OutputSchema,
		}}, ErrInvalidModeName},
		{"whitespace", &registryTestMode{definition: Definition{
			Name: "plan mode", OutputSchema: valid.OutputSchema,
		}}, ErrInvalidModeName},
		{"missing schema", &registryTestMode{definition: Definition{
			Name: "custom",
		}}, ErrInvalidModeDefinition},
		{"malformed schema", &registryTestMode{definition: Definition{
			Name: "custom", OutputSchema: json.RawMessage(`{`),
		}}, ErrInvalidModeDefinition},
		{"external schema", &registryTestMode{definition: Definition{
			Name: "custom",
			OutputSchema: json.RawMessage(
				`{"$ref":"https://example.com/schema.json"}`,
			),
		}}, ErrInvalidModeDefinition},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var registry Registry
			if err := registry.Register(test.mode); !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if len(registry.Names()) != 0 {
				t.Fatalf("rejected mode was registered: %v", registry.Names())
			}
		})
	}

	original := &registryTestMode{definition: valid}
	replacement := &registryTestMode{definition: valid}
	registry, err := NewRegistry(original)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(replacement); !errors.Is(err, ErrModeAlreadyRegistered) {
		t.Fatalf("duplicate error = %v", err)
	}
	if got, _ := registry.Resolve(valid.Name); got != original {
		t.Fatal("duplicate registration replaced original mode")
	}
	if partial, err := NewRegistry(original, replacement); partial != nil ||
		!errors.Is(err, ErrModeAlreadyRegistered) {
		t.Fatalf("partial registry=%#v error=%v", partial, err)
	}
}

func TestRegistryUnknownModeErrorsAreStable(t *testing.T) {
	var registry Registry
	if mode, err := registry.Resolve("missing"); mode != nil ||
		!errors.Is(err, ErrModeNotFound) {
		t.Fatalf("resolve mode=%#v error=%v", mode, err)
	}
	if definition, err := registry.Definition("missing"); definition.Name != "" ||
		!errors.Is(err, ErrModeNotFound) {
		t.Fatalf("definition=%#v error=%v", definition, err)
	}
	if output, err := registry.ValidateOutput("missing", json.RawMessage(`{}`)); output != nil ||
		!errors.Is(err, ErrModeNotFound) {
		t.Fatalf("output=%#v error=%v", output, err)
	}
}

func TestRegistryConcurrentRegistrationResolutionAndValidation(t *testing.T) {
	baseDefinition := mustBuiltinDefinition(t, workflow.ModePlanner)
	base := &registryTestMode{
		definition: baseDefinition,
		output:     validPlannerOutput(OutputCompleted),
	}
	registry, err := NewRegistry(base)
	if err != nil {
		t.Fatal(err)
	}
	const count = 100
	var wait sync.WaitGroup
	wait.Add(count + 4)
	for index := 0; index < count; index++ {
		index := index
		go func() {
			defer wait.Done()
			definition := baseDefinition
			definition.Name = workflow.ModeName(fmt.Sprintf("custom-%03d", index))
			if err := registry.Register(&registryTestMode{definition: definition}); err != nil {
				t.Errorf("register %s: %v", definition.Name, err)
			}
		}()
	}
	for index := 0; index < 4; index++ {
		go func() {
			defer wait.Done()
			for iteration := 0; iteration < count; iteration++ {
				if _, err := registry.Resolve(workflow.ModePlanner); err != nil {
					t.Errorf("resolve planner: %v", err)
					return
				}
				if _, err := registry.Definition(workflow.ModePlanner); err != nil {
					t.Errorf("definition planner: %v", err)
					return
				}
				if _, err := registry.ValidateOutput(
					workflow.ModePlanner,
					validPlannerOutput(OutputCompleted),
				); err != nil {
					t.Errorf("validate planner: %v", err)
					return
				}
				_ = registry.Names()
			}
		}()
	}
	wait.Wait()
	if got := len(registry.Names()); got != count+1 {
		t.Fatalf("registered modes = %d, want %d", got, count+1)
	}
}

func mustBuiltinDefinition(t *testing.T, name workflow.ModeName) Definition {
	t.Helper()
	definition, err := BuiltinDefinition(name)
	if err != nil {
		t.Fatal(err)
	}
	return definition
}
