package projectloop

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/eslam-mahmoud/go-ai-agent/internal/command"
	"github.com/eslam-mahmoud/go-ai-agent/internal/config"
	"github.com/eslam-mahmoud/go-ai-agent/internal/engine"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	opened, err := store.Open(filepath.Join(t.TempDir(), "madar.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { opened.Close() })
	return opened
}

func projectConfig(enabled bool, allowed ...string) *config.Config {
	cfg := &config.Config{}
	cfg.Project.Enabled = enabled
	cfg.Project.Repo = "owner/repo"
	cfg.Telegram.AllowedIDs = allowed
	return cfg
}

// v2 must never turn itself on: an existing installation upgrading to this
// build has to keep behaving exactly as it did.
func TestCommandsAreAbsentUnlessProjectModeIsEnabled(t *testing.T) {
	router, err := BuildCommands(projectConfig(false, "42"), openStore(t))
	if err != nil {
		t.Fatal(err)
	}
	if router != nil {
		t.Fatal("project commands must not exist when project mode is off")
	}
}

func TestCommandsRegisterBothSurfacesWhenEnabled(t *testing.T) {
	router, err := BuildCommands(projectConfig(true, "42", " 43 "), openStore(t))
	if err != nil {
		t.Fatal(err)
	}
	if router == nil {
		t.Fatal("project mode must expose the command surface")
	}
	for _, name := range []command.Name{
		command.NameStatus, command.NameProject, command.NameNext, command.NamePause,
	} {
		if !router.Knows(name) {
			t.Fatalf("/%s is not registered", name)
		}
	}
}

// A surface that authorizes nobody would refuse every message while looking
// like it works, so it is not built at all.
func TestCommandsAreAbsentWithAnEmptyAllowlist(t *testing.T) {
	router, err := BuildCommands(projectConfig(true), openStore(t))
	if err != nil {
		t.Fatal(err)
	}
	if router != nil {
		t.Fatal("an empty allowlist must not produce a command surface")
	}
}

// Dropping an unparsable ID would silently lock the owner out of their agent.
func TestAMalformedAllowedIDIsAnError(t *testing.T) {
	if _, err := BuildCommands(projectConfig(true, "not-a-number"), openStore(t)); err == nil {
		t.Fatal("expected a malformed Telegram ID to be reported")
	}
}

func TestBuildIsANoOpWhenProjectModeIsOff(t *testing.T) {
	loop, err := Build(Dependencies{Config: projectConfig(false), Store: openStore(t)})
	if err != nil {
		t.Fatal(err)
	}
	if loop != nil {
		t.Fatal("the delivery loop must not run when project mode is off")
	}
}

// Enabling project mode against a repository with no v2 project must say so
// rather than start a loop that can never do anything.
func TestBuildReportsAMissingProject(t *testing.T) {
	cfg := projectConfig(true, "42")
	cfg.WorkspaceDir = t.TempDir()
	_, err := Build(Dependencies{
		Config: cfg,
		Store:  openStore(t),
		Engine: stubEngine{},
	})
	if err == nil {
		t.Fatal("expected a missing project to be reported")
	}
}

// stubEngine satisfies the provider boundary without running anything: these
// tests are about wiring, not about provider behaviour.
type stubEngine struct{}

func (stubEngine) Name() string { return "stub" }

func (stubEngine) Capabilities(context.Context) (engine.CapabilitySet, error) {
	return engine.CapabilitySet{StructuredOutput: true, OutputSchema: true}, nil
}

func (stubEngine) Run(
	context.Context, engine.RunRequest, func(engine.Event) error,
) (*engine.Result, error) {
	return nil, errors.New("stub engine does not run")
}

func (stubEngine) Resume(
	context.Context, engine.RunRequest, func(engine.Event) error,
) (*engine.Result, error) {
	return nil, errors.New("stub engine does not resume")
}

func (stubEngine) Cancel(context.Context, string) error { return nil }
