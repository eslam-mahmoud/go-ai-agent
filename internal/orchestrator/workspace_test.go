package orchestrator

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/eslam-mahmoud/go-ai-agent/internal/config"
)

func TestGitEnvWithToken_injectsRewrite(t *testing.T) {
	env := gitEnvWithToken("ghp_secret")
	hasCount, hasKey, hasVal := false, false, false
	for _, e := range env {
		switch e {
		case "GIT_CONFIG_COUNT=1":
			hasCount = true
		case "GIT_CONFIG_VALUE_0=https://github.com/":
			hasVal = true
		}
		if len(e) > 16 && e[:16] == "GIT_CONFIG_KEY_0" {
			if !containsStr(e, "ghp_secret") {
				t.Errorf("GIT_CONFIG_KEY_0 should contain token, got: %s", e)
			}
			if !containsStr(e, "x-access-token") {
				t.Errorf("GIT_CONFIG_KEY_0 should use x-access-token, got: %s", e)
			}
			hasKey = true
		}
	}
	if !hasCount {
		t.Error("env missing GIT_CONFIG_COUNT=1")
	}
	if !hasKey {
		t.Error("env missing GIT_CONFIG_KEY_0")
	}
	if !hasVal {
		t.Error("env missing GIT_CONFIG_VALUE_0")
	}
}

func TestGitEnvWithToken_emptyToken(t *testing.T) {
	// Empty token should not inject config vars — plain os.Environ()
	env := gitEnvWithToken("")
	for _, e := range env {
		if containsStr(e, "GIT_CONFIG_COUNT") {
			t.Errorf("empty token should not inject GIT_CONFIG vars, found: %s", e)
		}
	}
}

func TestEnsureWorkspaces_alreadyExists(t *testing.T) {
	dir := t.TempDir()
	// Pre-create the workspace directory.
	existing := filepath.Join(dir, "owner", "repo")
	if err := os.MkdirAll(existing, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Repos:        []config.RepoConfig{{Name: "owner/repo"}},
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
		Repos:        []config.RepoConfig{{Name: "noslash"}},
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
		Repos:        []config.RepoConfig{},
		WorkspaceDir: t.TempDir(),
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := EnsureWorkspaces(context.Background(), cfg, log); err != nil {
		t.Fatalf("EnsureWorkspaces with no repos: %v", err)
	}
}

func TestPullWorkspace_nonExistentDir(t *testing.T) {
	cfg := testConfig()
	cfg.WorkspaceDir = t.TempDir() // workspace dir exists but repo subdir does not
	s := testStore(t)
	loop := testLoop(t, &fakeGitHub{}, &fakeRunner{result: nil}, &fakeTelegram{}, s)
	loop.cfg = cfg

	// Should not panic or error — missing workspace is silently skipped.
	loop.pullWorkspace(context.Background(), "owner", "repo")
}

func TestPullWorkspace_validGitRepo(t *testing.T) {
	// Create a minimal local bare repo and clone it, then test pull.
	dir := t.TempDir()
	bare := filepath.Join(dir, "bare.git")
	clone := filepath.Join(dir, "workspaces", "owner", "repo")

	// Init bare repo.
	if out, err := exec.Command("git", "init", "--bare", bare).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}
	// Clone it.
	if err := os.MkdirAll(filepath.Dir(clone), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "clone", bare, clone).CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v\n%s", err, out)
	}

	cfg := testConfig()
	cfg.WorkspaceDir = filepath.Join(dir, "workspaces")
	s := testStore(t)
	loop := testLoop(t, &fakeGitHub{}, &fakeRunner{result: nil}, &fakeTelegram{}, s)
	loop.cfg = cfg

	// Pull on an empty bare repo should succeed (already up to date).
	loop.pullWorkspace(context.Background(), "owner", "repo")
}
