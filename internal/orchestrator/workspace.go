package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/eslam-mahmoud/go-ai-agent/internal/config"
	githubclient "github.com/eslam-mahmoud/go-ai-agent/internal/github"
)

// pullWorkspace runs git pull --ff-only on the workspace for owner/repo.
// Failures are logged as warnings and do not block task execution.
func (l *Loop) pullWorkspace(ctx context.Context, owner, repo string) {
	workDir := filepath.Join(l.cfg.WorkspaceDir, owner, repo)
	if _, err := os.Stat(workDir); err != nil {
		return // not cloned yet — EnsureWorkspaces will handle it
	}
	cmd := exec.CommandContext(ctx, "git", "-C", workDir, "pull", "--ff-only")
	if out, err := cmd.CombinedOutput(); err != nil {
		l.log.Warn("git pull failed, continuing with existing workspace",
			"repo", owner+"/"+repo, "err", err, "output", string(out))
	} else {
		l.log.Debug("workspace pulled", "repo", owner+"/"+repo)
	}
}

// EnsureWorkspaces clones any repo listed in cfg.Repos whose local workspace
// directory does not yet exist. It is idempotent: already-cloned repos are skipped.
func EnsureWorkspaces(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	for _, fullRepo := range cfg.Repos {
		owner, repo, err := githubclient.SplitRepo(fullRepo)
		if err != nil {
			log.Warn("invalid repo in config, skipping", "repo", fullRepo)
			continue
		}

		dest := filepath.Join(cfg.WorkspaceDir, owner, repo)
		if _, err := os.Stat(dest); err == nil {
			log.Debug("workspace exists, skipping clone", "repo", fullRepo, "path", dest)
			continue
		}

		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return fmt.Errorf("create workspace parent for %s: %w", fullRepo, err)
		}

		// Use authenticated HTTPS so private repos clone without SSH key setup.
		// Token is embedded in the URL; git does not log credential URLs by default.
		cloneURL := fmt.Sprintf("https://x-access-token:%s@github.com/%s.git", cfg.GitHub.Token, fullRepo)
		log.Info("cloning workspace", "repo", fullRepo, "dest", dest)

		// Full clone (no --depth) so subsequent git pull operations work reliably.
		cmd := exec.CommandContext(ctx, "git", "clone", cloneURL, dest)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("clone %s: %w", fullRepo, err)
		}
		log.Info("workspace ready", "repo", fullRepo, "path", dest)
	}
	return nil
}
