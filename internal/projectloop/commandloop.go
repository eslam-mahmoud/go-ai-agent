package projectloop

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/command"
	"github.com/eslam-mahmoud/go-ai-agent/internal/telegram"
)

// CommandGateway is the inbound half of Telegram. It is narrow so the loop
// cannot accidentally send notifications while polling for commands.
type CommandGateway interface {
	GetUpdates(ctx context.Context, offset int64) ([]telegram.Update, error)
	Reply(ctx context.Context, chatID int64, text string) error
}

// CommandLoop answers owner commands from Telegram.
//
// It inherited this job from the v1 orchestrator's poller when v1 was removed.
// There is exactly one poller by design: Telegram hands each update to whoever
// asks first, so a second one would silently steal messages from this one.
type CommandLoop struct {
	gateway CommandGateway
	router  *command.Router
	offset  int64
	log     *slog.Logger
}

func NewCommandLoop(
	gateway CommandGateway, router *command.Router, log *slog.Logger,
) (*CommandLoop, error) {
	if gateway == nil {
		return nil, errors.New("command loop gateway is required")
	}
	if router == nil {
		return nil, errors.New("command loop router is required")
	}
	if log == nil {
		log = slog.Default()
	}
	return &CommandLoop{gateway: gateway, router: router, log: log}, nil
}

// Poll consumes one batch of updates. The offset advances past every update it
// reads, including ones it does not act on, so a message that is not a command
// cannot wedge the loop by being fetched forever.
func (loop *CommandLoop) Poll(ctx context.Context) error {
	updates, err := loop.gateway.GetUpdates(ctx, loop.offset)
	if err != nil {
		return err
	}
	for _, update := range updates {
		loop.offset = update.UpdateID + 1
		if update.Message == nil || update.Message.Text == "" {
			continue
		}
		loop.handle(ctx, update)
	}
	return nil
}

func (loop *CommandLoop) handle(ctx context.Context, update telegram.Update) {
	// The message timestamp drives the freshness check, so a backlog of
	// updates delivered after an outage cannot act long after it was written.
	parsed, err := command.ParseAt(
		update.Message.Text,
		update.Message.Chat.ID,
		update.Message.From.ID,
		time.Unix(update.Message.Date, 0).UTC(),
	)
	if err != nil {
		// Most messages in a chat are not commands; that is not a problem.
		return
	}
	reply, err := loop.router.Dispatch(ctx, parsed)
	if err != nil {
		loop.log.Warn("command failed", "command", parsed.Name, "err", err)
		reply = "That command failed. Check the logs."
	}
	if err := loop.gateway.Reply(ctx, update.Message.Chat.ID, reply); err != nil {
		loop.log.Warn("command reply failed", "chat", update.Message.Chat.ID, "err", err)
	}
}

// Run polls until the context is cancelled. A failed poll is logged and
// retried: Telegram being briefly unreachable must not end the command
// surface for the life of the process.
func (loop *CommandLoop) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		return errors.New("command loop interval must be positive")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := loop.Poll(ctx); err != nil && ctx.Err() == nil {
			loop.log.Debug("telegram poll failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
