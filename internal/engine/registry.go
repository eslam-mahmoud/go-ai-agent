package engine

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
)

var (
	ErrNilEngine               = errors.New("engine is nil")
	ErrInvalidEngineName       = errors.New("invalid engine name")
	ErrEngineAlreadyRegistered = errors.New("engine already registered")
	ErrEngineNotFound          = errors.New("engine not found")
)

// Registry stores provider implementations behind the provider-neutral Engine
// contract. Its zero value is ready for use.
type Registry struct {
	mu      sync.RWMutex
	engines map[string]Engine
}

// NewRegistry constructs a registry containing all initial engines. It returns
// no partial registry if any engine cannot be registered.
func NewRegistry(initial ...Engine) (*Registry, error) {
	registry := &Registry{}
	for _, provider := range initial {
		if err := registry.Register(provider); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

// Register adds an engine under its stable Name. Existing registrations cannot
// be replaced, so provider selection remains deterministic after startup.
func (r *Registry) Register(provider Engine) error {
	if isNilEngine(provider) {
		return ErrNilEngine
	}
	name := provider.Name()
	if !validEngineName(name) {
		return fmt.Errorf(
			"%w %q: use a lowercase letter followed by lowercase letters, digits, hyphens, or underscores",
			ErrInvalidEngineName,
			name,
		)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.engines == nil {
		r.engines = make(map[string]Engine)
	}
	if _, exists := r.engines[name]; exists {
		return fmt.Errorf("%w: %q", ErrEngineAlreadyRegistered, name)
	}
	r.engines[name] = provider
	return nil
}

// Resolve returns the engine registered under name.
func (r *Registry) Resolve(name string) (Engine, error) {
	r.mu.RLock()
	provider, exists := r.engines[name]
	r.mu.RUnlock()
	if !exists {
		return nil, fmt.Errorf("%w: %q", ErrEngineNotFound, name)
	}
	return provider, nil
}

// Names returns a sorted snapshot of all registered engine names.
func (r *Registry) Names() []string {
	r.mu.RLock()
	names := make([]string, 0, len(r.engines))
	for name := range r.engines {
		names = append(names, name)
	}
	r.mu.RUnlock()
	sort.Strings(names)
	return names
}

func isNilEngine(provider Engine) bool {
	if provider == nil {
		return true
	}
	value := reflect.ValueOf(provider)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Ptr, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func validEngineName(name string) bool {
	for index, character := range name {
		if character >= 'a' && character <= 'z' {
			continue
		}
		if index > 0 && ((character >= '0' && character <= '9') || character == '-' || character == '_') {
			continue
		}
		return false
	}
	return name != ""
}
