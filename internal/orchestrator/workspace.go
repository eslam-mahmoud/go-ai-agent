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

// gitEnvWithToken returns os.Environ() plus git config vars that rewrite
// https://github.com/ to an authenticated URL in-process only — the token
// never reaches .git/config or the process argument list.
func gitEnvWithToken(token string) []string {
	if token == "" {
		return os.Environ()
	}
	authedPrefix := fmt.Sprintf("https://x-access-token:%s@github.com/", token)
	return append(os.Environ(),
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=url."+authedPrefix+".insteadOf",
		"GIT_CONFIG_VALUE_0=https://github.com/",
	)
}

// pullWorkspace runs git pull --ff-only on the workspace for owner/repo.
// Credentials are injected via process environment — never written to disk.
// Failures are logged as warnings and do not block task execution.
func (l *Loop) pullWorkspace(ctx context.Context, owner, repo string) {
	workDir := filepath.Join(l.cfg.WorkspaceDir, owner, repo)
	if _, err := os.Stat(workDir); err != nil {
		return // not cloned yet — EnsureWorkspaces will handle it
	}
	cmd := exec.CommandContext(ctx, "git", "-C", workDir, "pull", "--ff-only")
	cmd.Env = gitEnvWithToken(l.cfg.GitHub.Token)
	if out, err := cmd.CombinedOutput(); err != nil {
		l.log.Warn("git pull failed, continuing with existing workspace",
			"repo", owner+"/"+repo, "err", err, "output", string(out))
	} else {
		l.log.Debug("workspace pulled", "repo", owner+"/"+repo)
	}
}

// EnsureWorkspaces clones any repo listed in cfg.Repos whose local workspace
// directory does not yet exist. It is idempotent: already-cloned repos are skipped.
// Credentials are injected via process environment so the token is never
// written into the cloned repo's .git/config.
func EnsureWorkspaces(ctx context.Context, cfg *config.Config, log *slog.Logger) error {
	for _, repoCfg := range cfg.Repos {
		fullRepo := repoCfg.Name
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

		log.Info("cloning workspace", "repo", fullRepo, "dest", dest)

		// Clone with plain HTTPS URL; credentials are supplied via GIT_CONFIG_COUNT
		// env vars that rewrite the URL in-process only (never stored in .git/config).
		cmd := exec.CommandContext(ctx, "git", "clone", "https://github.com/"+fullRepo+".git", dest)
		cmd.Env = gitEnvWithToken(cfg.GitHub.Token)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("clone %s: %w", fullRepo, err)
		}
		log.Info("workspace ready", "repo", fullRepo, "path", dest)
	}
	return nil
}
