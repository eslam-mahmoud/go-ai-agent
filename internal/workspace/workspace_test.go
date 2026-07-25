package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewRejectsIncompleteConfiguration(t *testing.T) {
	tests := []struct {
		name string
		root string
		repo string
	}{
		{"no root", "", "owner/repo"},
		{"no repo", "/workspaces", ""},
		{"malformed repo", "/workspaces", "not-a-repo"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := New(test.root, test.repo, "", nil); err == nil {
				t.Fatal("expected construction to fail")
			}
		})
	}
}

func TestPathFollowsTheOwnerRepoLayout(t *testing.T) {
	manager, err := New("/workspaces", "owner/repo", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	path, err := manager.Path()
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join("/workspaces", "owner", "repo") {
		t.Fatalf("path = %q", path)
	}
}

// Ensure must not re-clone over an existing checkout: doing so would discard
// uncommitted work a delivery mode was in the middle of.
func TestEnsureLeavesAnExistingCheckoutAlone(t *testing.T) {
	root := t.TempDir()
	existing := filepath.Join(root, "owner", "repo")
	if err := os.MkdirAll(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(existing, "marker.txt")
	if err := os.WriteFile(marker, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	manager, err := New(root, "owner/repo", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(marker)
	if err != nil || string(content) != "keep me" {
		t.Fatalf("existing checkout was disturbed: %q, %v", content, err)
	}
}

// A missing checkout must be reported, not silently skipped, or the first mode
// to run would fail somewhere far less obvious.
func TestEnsureReportsACloneFailure(t *testing.T) {
	manager, err := New(t.TempDir(), "owner/definitely-not-a-real-repo", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, lookErr := exec.LookPath("git"); lookErr != nil {
		t.Skip("git is not installed")
	}
	if err := manager.Ensure(context.Background()); err == nil {
		t.Fatal("expected the clone failure to be reported")
	}
}

// Refresh is best-effort: a stale checkout still lets delivery proceed, so a
// pull failure must not propagate.
func TestRefreshToleratesAFailedPull(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "owner", "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	manager, err := New(root, "owner/repo", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Not a git repository, so the pull fails; Refresh must return anyway.
	manager.Refresh(context.Background())
}

// The token must never reach the process argument list or .git/config. It is
// injected through git's environment-variable config instead.
func TestTokenTravelsInTheEnvironmentNotTheURL(t *testing.T) {
	env := gitEnvWithToken("ghp_secret")
	var rewrite string
	for _, entry := range env {
		if strings.HasPrefix(entry, "GIT_CONFIG_KEY_0=") {
			rewrite = entry
		}
	}
	if !strings.Contains(rewrite, "ghp_secret") {
		t.Fatalf("token is not in the git config rewrite: %q", rewrite)
	}
	if count := countEntries(env, "GIT_CONFIG_COUNT=1"); count != 1 {
		t.Fatalf("GIT_CONFIG_COUNT appears %d times", count)
	}
}

func TestNoTokenLeavesTheEnvironmentUntouched(t *testing.T) {
	env := gitEnvWithToken("")
	if countEntries(env, "GIT_CONFIG_COUNT=1") != 0 {
		t.Fatal("an empty token must not add git config entries")
	}
}

func countEntries(env []string, want string) int {
	total := 0
	for _, entry := range env {
		if entry == want {
			total++
		}
	}
	return total
}
