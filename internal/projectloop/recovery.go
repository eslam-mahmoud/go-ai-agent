package projectloop

import (
	"fmt"
	"log/slog"

	"github.com/eslam-mahmoud/go-ai-agent/internal/config"
	"github.com/eslam-mahmoud/go-ai-agent/internal/project"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
)

// Recover repairs v2 state left behind by a restart, before anything reads
// it. It runs ahead of reconciliation and ahead of the first delivery tick, so
// the loop never builds on state a crash left half-written.
func Recover(
	cfg *config.Config, projectStore *store.Store, log *slog.Logger,
) error {
	if cfg == nil {
		return fmt.Errorf("%w: config is required for recovery", ErrInvalidLoop)
	}
	if projectStore == nil {
		return fmt.Errorf("%w: store is required for recovery", ErrInvalidLoop)
	}
	if log == nil {
		log = slog.Default()
	}
	controller, err := project.NewController(projectStore)
	if err != nil {
		return err
	}
	recovery, err := project.NewStartupRecovery(controller, projectStore)
	if err != nil {
		return err
	}
	report, err := recovery.Run()
	if err != nil {
		return err
	}
	// Saying what was repaired matters more than saying that recovery ran: a
	// restart that interrupted work should be visible in the log.
	if report != nil && len(report.InterruptedExecutions) > 0 {
		log.Info("v2 startup recovery repaired interrupted work",
			"executions", len(report.InterruptedExecutions),
			"projects", len(report.Projects))
	}
	return nil
}
