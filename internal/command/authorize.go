package command

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	// ErrExpired refuses a command that is too old to act on safely.
	ErrExpired = errors.New("command is too old")
	// ErrRateLimited refuses a sender who is issuing commands too quickly.
	ErrRateLimited = errors.New("too many commands")
)

// RefusalRecorder persists refusals. Repeated unauthorized attempts are worth
// noticing, so they are recorded rather than only refused.
type RefusalRecorder interface {
	RecordCommandRefusal(userID int64, command string, reason string) error
}

// OwnerAuthorizerOptions configures the three independent gates.
type OwnerAuthorizerOptions struct {
	// AllowedUserIDs is the set of Telegram user IDs permitted to issue
	// commands. An empty set authorizes nobody.
	AllowedUserIDs []int64
	// MaxAge refuses commands older than this. Zero disables the check.
	MaxAge time.Duration
	// Window is the rate-limit window. Zero disables rate limiting.
	Window time.Duration
	// MaxPerWindow bounds read-only commands per sender per window.
	MaxPerWindow int
	// MaxControlPerWindow bounds mutating commands, which deserve a tighter
	// limit than inspection.
	MaxControlPerWindow int
	// Now is injectable for tests.
	Now func() time.Time
}

// OwnerAuthorizer enforces the plan's rule that only configured Telegram IDs
// may execute commands, plus expiry and rate limiting.
type OwnerAuthorizer struct {
	allowed             map[int64]struct{}
	maxAge              time.Duration
	window              time.Duration
	maxPerWindow        int
	maxControlPerWindow int
	now                 func() time.Time
	recorder            RefusalRecorder

	mu      sync.Mutex
	history map[int64][]time.Time
}

func NewOwnerAuthorizer(
	options OwnerAuthorizerOptions,
	recorder RefusalRecorder,
) *OwnerAuthorizer {
	allowed := make(map[int64]struct{}, len(options.AllowedUserIDs))
	for _, id := range options.AllowedUserIDs {
		if id != 0 {
			allowed[id] = struct{}{}
		}
	}
	now := options.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &OwnerAuthorizer{
		allowed:             allowed,
		maxAge:              options.MaxAge,
		window:              options.Window,
		maxPerWindow:        options.MaxPerWindow,
		maxControlPerWindow: options.MaxControlPerWindow,
		now:                 now,
		recorder:            recorder,
		history:             make(map[int64][]time.Time),
	}
}

// Authorize applies the three gates in order: identity, freshness, then rate.
// Identity comes first so an unlisted sender learns nothing about the others.
func (authorizer *OwnerAuthorizer) Authorize(
	_ context.Context,
	command Command,
) error {
	if _, permitted := authorizer.allowed[command.UserID]; !permitted {
		return authorizer.refuse(command, ErrUnauthorized, "sender is not on the allowlist")
	}
	if authorizer.maxAge > 0 && !command.SentAt.IsZero() {
		age := authorizer.now().Sub(command.SentAt)
		if age > authorizer.maxAge {
			return authorizer.refuse(command, ErrExpired,
				fmt.Sprintf("command is %s old", age.Round(time.Second)))
		}
	}
	if err := authorizer.consumeRate(command); err != nil {
		return err
	}
	return nil
}

// consumeRate applies a sliding window per sender, with a tighter budget for
// commands that change delivery.
func (authorizer *OwnerAuthorizer) consumeRate(command Command) error {
	limit := authorizer.maxPerWindow
	if IsControlCommand(command.Name) && authorizer.maxControlPerWindow > 0 {
		limit = authorizer.maxControlPerWindow
	}
	if authorizer.window <= 0 || limit <= 0 {
		return nil
	}
	now := authorizer.now()
	cutoff := now.Add(-authorizer.window)

	authorizer.mu.Lock()
	recent := authorizer.history[command.UserID][:0:0]
	for _, stamp := range authorizer.history[command.UserID] {
		if stamp.After(cutoff) {
			recent = append(recent, stamp)
		}
	}
	if len(recent) >= limit {
		authorizer.history[command.UserID] = recent
		authorizer.mu.Unlock()
		return authorizer.refuse(command, ErrRateLimited,
			fmt.Sprintf("%d commands within %s", len(recent), authorizer.window))
	}
	authorizer.history[command.UserID] = append(recent, now)
	authorizer.mu.Unlock()
	return nil
}

// refuse records the attempt and returns the classified reason. Recording
// failure never turns a refusal into an acceptance.
func (authorizer *OwnerAuthorizer) refuse(
	command Command,
	reason error,
	detail string,
) error {
	if authorizer.recorder != nil {
		_ = authorizer.recorder.RecordCommandRefusal(
			command.UserID, string(command.Name), detail,
		)
	}
	return fmt.Errorf("%w: %s", reason, detail)
}

// IsControlCommand reports whether a command changes delivery, which is what
// earns it the tighter rate limit.
func IsControlCommand(name Name) bool {
	switch name {
	case NamePause, NameResume, NameCancel, NameRetry,
		NameAnswer, NameApprove, NameReject:
		return true
	default:
		return false
	}
}

var _ Authorizer = (*OwnerAuthorizer)(nil)
