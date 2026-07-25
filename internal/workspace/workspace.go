// Package workspace clones and refreshes the repository checkout that every
// delivery mode runs against.
//
// It moved out of the v1 orchestrator when v1 issue mode was removed: the
// checkout is not a v1 concern, it is what the agent works in.
package workspace

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"

	githubclient "github.com/eslam-mahmoud/go-ai-agent/internal/github"
)

var ErrInvalidWorkspace = errors.New("invalid workspace")

// Manager owns one repository's local checkout.
type Manager struct {
	root  string
	repo  string
	token string
	log   *slog.Logger
}

func New(root, repo, token string, log *slog.Logger) (*Manager, error) {
	if root == "" {
		return nil, fmt.Errorf("%w: workspace root is required", ErrInvalidWorkspace)
	}
	if _, _, err := githubclient.SplitRepo(repo); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidWorkspace, err)
	}
	if log == nil {
		log = slog.Default()
	}
	return &Manager{root: root, repo: repo, token: token, log: log}, nil
}

// Path is the checkout location, <root>/<owner>/<repo>, the layout the rest of
// Madar already resolves against.
func (manager *Manager) Path() (string, error) {
	owner, name, err := githubclient.SplitRepo(manager.repo)
	if err != nil {
		return "", err
	}
	return filepath.Join(manager.root, owner, name), nil
}

// Ensure clones the repository if it is not already present. It is idempotent,
// so a restart re-uses the existing checkout rather than re-cloning it.
//
// Credentials are supplied through the process environment, so the token never
// reaches .git/config or the process argument list.
func (manager *Manager) Ensure(ctx context.Context) error {
	destination, err := manager.Path()
	if err != nil {
		return err
	}
	if _, err := os.Stat(destination); err == nil {
		manager.log.Debug("workspace exists", "repo", manager.repo, "path", destination)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return fmt.Errorf("create workspace parent for %s: %w", manager.repo, err)
	}
	manager.log.Info("cloning workspace", "repo", manager.repo, "dest", destination)
	command := exec.CommandContext(
		ctx, "git", "clone", "https://github.com/"+manager.repo+".git", destination,
	)
	command.Env = gitEnvWithToken(manager.token)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("clone %s: %w: %s", manager.repo, err, output)
	}
	manager.log.Info("workspace ready", "repo", manager.repo, "path", destination)
	return nil
}

// Refresh fast-forwards the checkout. A failure is logged rather than
// returned: working from a slightly stale checkout beats not working at all,
// and the next delivery mode will still run.
func (manager *Manager) Refresh(ctx context.Context) {
	destination, err := manager.Path()
	if err != nil {
		return
	}
	if _, err := os.Stat(destination); err != nil {
		return
	}
	command := exec.CommandContext(ctx, "git", "-C", destination, "pull", "--ff-only")
	command.Env = gitEnvWithToken(manager.token)
	if output, err := command.CombinedOutput(); err != nil {
		manager.log.Warn("git pull failed, continuing with the existing workspace",
			"repo", manager.repo, "err", err, "output", string(output))
		return
	}
	manager.log.Debug("workspace refreshed", "repo", manager.repo)
}

// gitEnvWithToken rewrites https://github.com/ to an authenticated URL for
// this process only. Passing the token in the URL argument would leak it into
// the process list; writing it to .git/config would leave it on disk.
func gitEnvWithToken(token string) []string {
	if token == "" {
		return os.Environ()
	}
	authenticated := fmt.Sprintf("https://x-access-token:%s@github.com/", token)
	return append(os.Environ(),
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=url."+authenticated+".insteadOf",
		"GIT_CONFIG_VALUE_0=https://github.com/",
	)
}
