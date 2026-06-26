package orchestrator

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/eslam-mahmoud/go-ai-agent/internal/config"
)

func TestEnsureWorkspaces_alreadyExists(t *testing.T) {
	dir := t.TempDir()
	// Pre-create the workspace directory.
	existing := filepath.Join(dir, "owner", "repo")
	if err := os.MkdirAll(existing, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Repos:        []string{"owner/repo"},
		WorkspaceDir: dir,
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Should not error or attempt to clone since the dir exists.
	if err := EnsureWorkspaces(context.Background(), cfg, log); err != nil {
		t.Fatalf("EnsureWorkspaces: %v", err)
	}
}

func TestEnsureWorkspaces_invalidRepo(t *testing.T) {
	cfg := &config.Config{
		Repos:        []string{"noslash"},
		WorkspaceDir: t.TempDir(),
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	// Invalid repos are warned and skipped, not fatal.
	if err := EnsureWorkspaces(context.Background(), cfg, log); err != nil {
		t.Fatalf("EnsureWorkspaces should skip invalid repo, got: %v", err)
	}
}

func TestEnsureWorkspaces_emptyRepos(t *testing.T) {
	cfg := &config.Config{
		Repos:        []string{},
		WorkspaceDir: t.TempDir(),
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := EnsureWorkspaces(context.Background(), cfg, log); err != nil {
		t.Fatalf("EnsureWorkspaces with no repos: %v", err)
	}
}
