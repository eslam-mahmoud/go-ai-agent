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

func projectConfig(allowed ...string) *config.Config {
	cfg := &config.Config{}
	cfg.Project.Repo = "owner/repo"
	cfg.Telegram.AllowedIDs = allowed
	return cfg
}

func TestCommandsRegisterBothSurfacesWhenEnabled(t *testing.T) {
	router, err := BuildCommands(projectConfig("42", " 43 "), openStore(t))
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
	router, err := BuildCommands(projectConfig(), openStore(t))
	if err != nil {
		t.Fatal(err)
	}
	if router != nil {
		t.Fatal("an empty allowlist must not produce a command surface")
	}
}

// Dropping an unparsable ID would silently lock the owner out of their agent.
func TestAMalformedAllowedIDIsAnError(t *testing.T) {
	if _, err := BuildCommands(projectConfig("not-a-number"), openStore(t)); err == nil {
		t.Fatal("expected a malformed Telegram ID to be reported")
	}
}

// Every collaborator the loop cannot work without must be reported by name.
// A loop built from an incomplete set would fail later, further from the cause.
func TestBuildReportsMissingCollaborators(t *testing.T) {
	tests := []struct {
		name         string
		dependencies Dependencies
	}{
		{"no config", Dependencies{Store: openStore(t)}},
		{"no store", Dependencies{Config: projectConfig("42"), Engine: stubEngine{}}},
		{"no engine", Dependencies{Config: projectConfig("42"), Store: openStore(t)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Build(test.dependencies); err == nil {
				t.Fatal("expected the missing collaborator to be reported")
			}
		})
	}
}

// Enabling project mode against a repository with no v2 project must say so
// rather than start a loop that can never do anything.
func TestBuildReportsAMissingProject(t *testing.T) {
	cfg := projectConfig("42")
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
