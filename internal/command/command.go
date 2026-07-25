// Package command implements Madar's owner command surface. Authorization is
// part of dispatch rather than a layer around it: an unauthorized sender can
// never reach a handler.
package command

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

var (
	ErrNotACommand  = errors.New("message is not a command")
	ErrUnauthorized = errors.New("sender is not authorized")
)

// Name is one supported command, without its leading slash.
type Name string

const (
	NameStatus  Name = "status"
	NameProject Name = "project"
	NamePlan    Name = "plan"
	NameNext    Name = "next"
	NameLogs    Name = "logs"
	NameHelp    Name = "help"
)

// Command is one parsed request from the owner.
type Command struct {
	Name   Name
	Args   []string
	ChatID int64
	UserID int64
	// Raw is the original message, kept for auditing.
	Raw string
	// SentAt is when Telegram says the message was written. A zero value
	// disables the freshness check for that command.
	SentAt time.Time
}

// Parse turns a message into a command. Text that is not a command is
// reported as such rather than treated as an error condition, since most
// messages in a chat are not commands.
func Parse(text string, chatID, userID int64) (Command, error) {
	return ParseAt(text, chatID, userID, time.Time{})
}

// ParseAt is Parse with the message timestamp, which the expiry check uses.
func ParseAt(text string, chatID, userID int64, sentAt time.Time) (Command, error) {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "/") {
		return Command{}, ErrNotACommand
	}
	fields := strings.Fields(trimmed)
	if len(fields) == 0 {
		return Command{}, ErrNotACommand
	}
	name := strings.TrimPrefix(fields[0], "/")
	// Telegram addresses commands as /status@madarbot in group chats.
	if at := strings.Index(name, "@"); at >= 0 {
		name = name[:at]
	}
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return Command{}, ErrNotACommand
	}
	return Command{
		Name:   Name(name),
		Args:   fields[1:],
		ChatID: chatID,
		UserID: userID,
		Raw:    trimmed,
		SentAt: sentAt,
	}, nil
}

// Authorizer decides whether a sender may run a command. Item 55a extends the
// implementation with expiry and rate limiting; the seam lives here so those
// cannot be bypassed by a handler added later.
type Authorizer interface {
	Authorize(ctx context.Context, command Command) error
}

// Handler answers one command with the text to reply.
type Handler func(ctx context.Context, command Command) (string, error)

type registration struct {
	handler     Handler
	description string
}

type Router struct {
	authorizer Authorizer
	handlers   map[Name]registration
}

func NewRouter(authorizer Authorizer) (*Router, error) {
	if authorizer == nil {
		// A router without authorization would expose project control to
		// anyone who can message the bot.
		return nil, errors.New("command authorizer is required")
	}
	return &Router{
		authorizer: authorizer,
		handlers:   make(map[Name]registration),
	}, nil
}

func (router *Router) Register(name Name, description string, handler Handler) error {
	if strings.TrimSpace(string(name)) == "" {
		return errors.New("command name is required")
	}
	if handler == nil {
		return fmt.Errorf("handler for /%s is required", name)
	}
	if _, exists := router.handlers[name]; exists {
		return fmt.Errorf("command /%s is already registered", name)
	}
	router.handlers[name] = registration{handler: handler, description: description}
	return nil
}

// Dispatch authorizes then runs a command, returning the reply text. An
// unauthorized sender never reaches a handler.
func (router *Router) Dispatch(
	ctx context.Context,
	command Command,
) (string, error) {
	if err := router.authorizer.Authorize(ctx, command); err != nil {
		// Each refusal reads differently, so the owner knows whether they
		// were unauthorized, too late, or too fast.
		switch {
		case errors.Is(err, ErrUnauthorized):
			return "You are not authorized to control this project.", nil
		case errors.Is(err, ErrExpired):
			return "That command is too old to act on. Send it again.", nil
		case errors.Is(err, ErrRateLimited):
			return "Too many commands just now. Try again shortly.", nil
		}
		return "", err
	}
	if command.Name == NameHelp {
		return router.help(), nil
	}
	registered, known := router.handlers[command.Name]
	if !known {
		// Answering with help beats silence: the owner learns what exists.
		return fmt.Sprintf("Unknown command /%s.\n\n%s", command.Name, router.help()), nil
	}
	reply, err := registered.handler(ctx, command)
	if err != nil {
		// A handler failure must read as a message, not as a leaked error.
		return fmt.Sprintf("/%s failed: %s", command.Name, readableError(err)), nil
	}
	return Truncate(reply), nil
}

func (router *Router) help() string {
	names := make([]Name, 0, len(router.handlers))
	for name := range router.handlers {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
	var help strings.Builder
	help.WriteString("Commands:")
	for _, name := range names {
		fmt.Fprintf(&help, "\n/%s — %s", name, router.handlers[name].description)
	}
	fmt.Fprintf(&help, "\n/%s — this message", NameHelp)
	return help.String()
}

// MaxReplyBytes keeps replies inside Telegram's message limit with room for
// the truncation marker.
const MaxReplyBytes = 3800

// Truncate bounds a reply and says so, since a silently cut backlog reads as
// a complete one.
func Truncate(text string) string {
	if len(text) <= MaxReplyBytes {
		return text
	}
	cut := text[:MaxReplyBytes]
	if index := strings.LastIndex(cut, "\n"); index > MaxReplyBytes/2 {
		cut = cut[:index]
	}
	return cut + "\n…truncated"
}

// readableError keeps replies free of wrapped internal detail.
func readableError(err error) string {
	message := err.Error()
	if index := strings.Index(message, ":"); index > 0 && index < len(message)-1 {
		return strings.TrimSpace(message[:index])
	}
	return message
}
