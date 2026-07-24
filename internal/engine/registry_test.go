package engine

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

type registryTestEngine struct {
	name string
}

var _ Engine = (*registryTestEngine)(nil)

func (e *registryTestEngine) Name() string {
	return e.name
}

func (e *registryTestEngine) Capabilities(context.Context) (CapabilitySet, error) {
	return CapabilitySet{}, nil
}

func (e *registryTestEngine) Run(
	context.Context,
	RunRequest,
	func(Event) error,
) (*Result, error) {
	return &Result{}, nil
}

func (e *registryTestEngine) Resume(
	context.Context,
	RunRequest,
	func(Event) error,
) (*Result, error) {
	return &Result{}, nil
}

func (e *registryTestEngine) Cancel(context.Context, string) error {
	return nil
}

func TestRegistryZeroValueRegistersAndResolvesEngines(t *testing.T) {
	var registry Registry
	claude := &registryTestEngine{name: "claude"}
	codex := &registryTestEngine{name: "codex"}

	if err := registry.Register(claude); err != nil {
		t.Fatalf("Register claude: %v", err)
	}
	if err := registry.Register(codex); err != nil {
		t.Fatalf("Register codex: %v", err)
	}

	got, err := registry.Resolve("claude")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != claude {
		t.Errorf("Resolve returned %p, want %p", got, claude)
	}
	if names := registry.Names(); !reflect.DeepEqual(names, []string{"claude", "codex"}) {
		t.Errorf("Names = %v, want [claude codex]", names)
	}
}

func TestNewRegistryRegistersInitialEngines(t *testing.T) {
	claude := &registryTestEngine{name: "claude"}
	codex := &registryTestEngine{name: "codex"}

	registry, err := NewRegistry(codex, claude)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if names := registry.Names(); !reflect.DeepEqual(names, []string{"claude", "codex"}) {
		t.Errorf("Names = %v, want sorted names", names)
	}
}

func TestNewRegistryReturnsNoPartialRegistryOnInvalidInitialEngine(t *testing.T) {
	registry, err := NewRegistry(
		&registryTestEngine{name: "claude"},
		&registryTestEngine{name: "claude"},
	)
	if registry != nil {
		t.Errorf("registry = %#v, want nil after constructor failure", registry)
	}
	if !errors.Is(err, ErrEngineAlreadyRegistered) {
		t.Errorf("error = %v, want ErrEngineAlreadyRegistered", err)
	}
}

func TestRegistryRejectsDuplicateWithoutReplacingEngine(t *testing.T) {
	var registry Registry
	original := &registryTestEngine{name: "claude"}
	replacement := &registryTestEngine{name: "claude"}
	if err := registry.Register(original); err != nil {
		t.Fatalf("Register original: %v", err)
	}

	err := registry.Register(replacement)
	if !errors.Is(err, ErrEngineAlreadyRegistered) {
		t.Fatalf("Register duplicate error = %v, want ErrEngineAlreadyRegistered", err)
	}
	if !containsAll(err.Error(), "claude", "already registered") {
		t.Errorf("duplicate error lacks diagnostics: %q", err)
	}
	got, resolveErr := registry.Resolve("claude")
	if resolveErr != nil {
		t.Fatalf("Resolve: %v", resolveErr)
	}
	if got != original {
		t.Error("duplicate registration replaced the original engine")
	}
}

func TestRegistryRejectsNilAndInvalidEngines(t *testing.T) {
	var typedNil *registryTestEngine
	cases := []struct {
		name     string
		provider Engine
		want     error
	}{
		{name: "nil interface", provider: nil, want: ErrNilEngine},
		{name: "typed nil", provider: typedNil, want: ErrNilEngine},
		{name: "empty name", provider: &registryTestEngine{}, want: ErrInvalidEngineName},
		{name: "leading digit", provider: &registryTestEngine{name: "2claude"}, want: ErrInvalidEngineName},
		{name: "uppercase", provider: &registryTestEngine{name: "Claude"}, want: ErrInvalidEngineName},
		{name: "whitespace", provider: &registryTestEngine{name: "claude code"}, want: ErrInvalidEngineName},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var registry Registry
			err := registry.Register(tc.provider)
			if !errors.Is(err, tc.want) {
				t.Errorf("Register error = %v, want %v", err, tc.want)
			}
			if names := registry.Names(); len(names) != 0 {
				t.Errorf("Names = %v after rejected registration", names)
			}
		})
	}
}

func TestRegistryUnknownEngineIsDistinguishable(t *testing.T) {
	var registry Registry
	got, err := registry.Resolve("missing")
	if got != nil {
		t.Errorf("Resolve = %#v, want nil", got)
	}
	if !errors.Is(err, ErrEngineNotFound) {
		t.Errorf("error = %v, want ErrEngineNotFound", err)
	}
	if !containsAll(err.Error(), "missing", "not found") {
		t.Errorf("not-found error lacks requested name: %q", err)
	}
}

func TestRegistryNamesReturnsDefensiveSortedSnapshot(t *testing.T) {
	registry, err := NewRegistry(
		&registryTestEngine{name: "zeta"},
		&registryTestEngine{name: "alpha"},
	)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	names := registry.Names()
	names[0] = "changed"
	if got := registry.Names(); !reflect.DeepEqual(got, []string{"alpha", "zeta"}) {
		t.Errorf("Names after caller mutation = %v", got)
	}
}

func TestRegistryConcurrentRegistrationAndReads(t *testing.T) {
	registry, err := NewRegistry(&registryTestEngine{name: "base"})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	const engineCount = 100
	var wait sync.WaitGroup
	wait.Add(engineCount + 4)
	for i := 0; i < engineCount; i++ {
		i := i
		go func() {
			defer wait.Done()
			name := fmt.Sprintf("engine-%03d", i)
			if err := registry.Register(&registryTestEngine{name: name}); err != nil {
				t.Errorf("Register %s: %v", name, err)
			}
		}()
	}
	for i := 0; i < 4; i++ {
		go func() {
			defer wait.Done()
			for j := 0; j < engineCount; j++ {
				if _, err := registry.Resolve("base"); err != nil {
					t.Errorf("Resolve base: %v", err)
					return
				}
				_ = registry.Names()
			}
		}()
	}
	wait.Wait()

	if got := len(registry.Names()); got != engineCount+1 {
		t.Errorf("registered engines = %d, want %d", got, engineCount+1)
	}
}

func containsAll(value string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(value, part) {
			return false
		}
	}
	return true
}
