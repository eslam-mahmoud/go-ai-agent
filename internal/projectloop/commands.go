package projectloop

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/eslam-mahmoud/go-ai-agent/internal/command"
	"github.com/eslam-mahmoud/go-ai-agent/internal/config"
	"github.com/eslam-mahmoud/go-ai-agent/internal/project"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
)

// BuildCommands assembles the owner command surface. It returns a nil router
// with a nil error when no Telegram ID is allowed to issue commands, since a
// surface that authorizes nobody would exist only to refuse everything.
//
// Read-only and mutating commands are registered together here because a
// deployment that runs the delivery loop is by definition one where the owner
// may control it; a reader-only deployment simply does not enable project mode.
func BuildCommands(
	cfg *config.Config, projectStore *store.Store,
) (*command.Router, error) {
	if cfg == nil {
		return nil, fmt.Errorf("%w: config is required for commands", ErrInvalidLoop)
	}
	if projectStore == nil {
		return nil, fmt.Errorf("%w: store is required for commands", ErrInvalidLoop)
	}
	allowed, err := allowedUserIDs(cfg.Telegram.AllowedIDs)
	if err != nil {
		return nil, err
	}
	if len(allowed) == 0 {
		// An empty allowlist authorizes nobody, so the surface would exist
		// and refuse everything. Say so rather than look broken.
		return nil, nil
	}
	authorizer := command.NewOwnerAuthorizer(command.OwnerAuthorizerOptions{
		AllowedUserIDs:      allowed,
		MaxAge:              cfg.Telegram.CommandMaxAge,
		Window:              cfg.Telegram.RateWindow,
		MaxPerWindow:        cfg.Telegram.MaxCommandsPerLimit,
		MaxControlPerWindow: cfg.Telegram.MaxControlPerLimit,
	}, projectStore)

	router, err := command.NewRouter(authorizer)
	if err != nil {
		return nil, err
	}
	if err := command.RegisterProjectCommands(router, projectStore); err != nil {
		return nil, err
	}
	controller, err := project.NewController(projectStore)
	if err != nil {
		return nil, err
	}
	if err := command.RegisterControlCommands(
		router, projectStore, controller, projectStore, projectStore,
	); err != nil {
		return nil, err
	}
	return router, nil
}

// allowedUserIDs converts the configured Telegram IDs. A malformed entry is an
// error rather than a skipped line: silently dropping an owner's ID would lock
// them out of their own agent.
func allowedUserIDs(configured []string) ([]int64, error) {
	ids := make([]int64, 0, len(configured))
	for _, raw := range configured {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		id, err := strconv.ParseInt(trimmed, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("telegram allowed ID %q is not a number: %w", trimmed, err)
		}
		ids = append(ids, id)
	}
	return ids, nil
}
