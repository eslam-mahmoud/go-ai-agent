package mode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"

	"github.com/eslam-mahmoud/go-ai-agent/internal/engine"
	"github.com/eslam-mahmoud/go-ai-agent/internal/workflow"
)

var (
	ErrNilMode               = errors.New("mode is nil")
	ErrInvalidModeName       = errors.New("invalid mode name")
	ErrInvalidModeDefinition = errors.New("invalid mode definition")
	ErrModeAlreadyRegistered = errors.New("mode already registered")
	ErrModeNotFound          = errors.New("mode not found")
	ErrInvalidModeOutput     = errors.New("invalid mode output")
)

// Definition is immutable registry metadata captured at registration time.
type Definition struct {
	Name         workflow.ModeName
	Description  string
	OutputSchema json.RawMessage
	FreshSession bool
}

// Mode is the provider-neutral delivery-mode contract. Implementations own
// their prompt/context construction and return the provider's structured JSON.
// Registry dispatch validates that JSON before workflow code can consume it.
type Mode interface {
	Definition() Definition
	Run(context.Context, workflow.ModeRequest) (json.RawMessage, error)
}

type registration struct {
	mode       Mode
	definition Definition
	validator  *engine.OutputValidator
}

// Registry stores delivery modes and compiled schemas. Its zero value is
// ready for concurrent use.
type Registry struct {
	mu    sync.RWMutex
	modes map[workflow.ModeName]registration
}

// NewRegistry constructs an all-or-nothing registry from initial modes.
func NewRegistry(initial ...Mode) (*Registry, error) {
	registry := &Registry{}
	for _, deliveryMode := range initial {
		if err := registry.Register(deliveryMode); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

// Register validates and compiles mode metadata before publishing it.
// Existing registrations cannot be replaced.
func (registry *Registry) Register(deliveryMode Mode) error {
	if isNilMode(deliveryMode) {
		return ErrNilMode
	}
	definition := cloneDefinition(deliveryMode.Definition())
	if !validModeName(definition.Name) {
		return fmt.Errorf(
			"%w %q: use a lowercase letter followed by lowercase letters, digits, hyphens, or underscores",
			ErrInvalidModeName,
			definition.Name,
		)
	}
	if len(definition.OutputSchema) == 0 {
		return fmt.Errorf(
			"%w: mode %q requires an output schema",
			ErrInvalidModeDefinition,
			definition.Name,
		)
	}
	validator, err := engine.CompileOutputSchema(definition.OutputSchema)
	if err != nil {
		return fmt.Errorf(
			"%w: mode %q schema: %v",
			ErrInvalidModeDefinition,
			definition.Name,
			err,
		)
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.modes == nil {
		registry.modes = make(map[workflow.ModeName]registration)
	}
	if _, exists := registry.modes[definition.Name]; exists {
		return fmt.Errorf("%w: %q", ErrModeAlreadyRegistered, definition.Name)
	}
	registry.modes[definition.Name] = registration{
		mode:       deliveryMode,
		definition: definition,
		validator:  validator,
	}
	return nil
}

func (registry *Registry) Resolve(name workflow.ModeName) (Mode, error) {
	registry.mu.RLock()
	entry, exists := registry.modes[name]
	registry.mu.RUnlock()
	if !exists {
		return nil, fmt.Errorf("%w: %q", ErrModeNotFound, name)
	}
	return entry.mode, nil
}

func (registry *Registry) Definition(name workflow.ModeName) (Definition, error) {
	registry.mu.RLock()
	entry, exists := registry.modes[name]
	registry.mu.RUnlock()
	if !exists {
		return Definition{}, fmt.Errorf("%w: %q", ErrModeNotFound, name)
	}
	return cloneDefinition(entry.definition), nil
}

func (registry *Registry) Names() []workflow.ModeName {
	registry.mu.RLock()
	names := make([]workflow.ModeName, 0, len(registry.modes))
	for name := range registry.modes {
		names = append(names, name)
	}
	registry.mu.RUnlock()
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
	return names
}

// ValidateOutput validates one raw mode result and returns its typed common
// envelope with compact JSON retained for mode-specific decoding.
func (registry *Registry) ValidateOutput(
	name workflow.ModeName,
	raw json.RawMessage,
) (*Output, error) {
	registry.mu.RLock()
	entry, exists := registry.modes[name]
	registry.mu.RUnlock()
	if !exists {
		return nil, fmt.Errorf("%w: %q", ErrModeNotFound, name)
	}
	result := &engine.Result{OutputJSON: append(json.RawMessage(nil), raw...)}
	if err := entry.validator.ValidateResult(result); err != nil {
		return nil, fmt.Errorf("%w: mode %q: %v", ErrInvalidModeOutput, name, err)
	}
	var output Output
	if err := json.Unmarshal(result.OutputJSON, &output); err != nil {
		return nil, fmt.Errorf("%w: mode %q: %v", ErrInvalidModeOutput, name, err)
	}
	if !output.Status.Valid() {
		return nil, fmt.Errorf(
			"%w: mode %q returned status %q",
			ErrInvalidModeOutput,
			name,
			output.Status,
		)
	}
	output.Raw = append(output.Raw[:0], result.OutputJSON...)
	return &output, nil
}

func cloneDefinition(definition Definition) Definition {
	definition.OutputSchema = append(json.RawMessage(nil), definition.OutputSchema...)
	return definition
}

func isNilMode(deliveryMode Mode) bool {
	if deliveryMode == nil {
		return true
	}
	value := reflect.ValueOf(deliveryMode)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func validModeName(name workflow.ModeName) bool {
	for index, character := range string(name) {
		if character >= 'a' && character <= 'z' {
			continue
		}
		if index > 0 &&
			((character >= '0' && character <= '9') ||
				character == '-' ||
				character == '_') {
			continue
		}
		return false
	}
	return name != ""
}
