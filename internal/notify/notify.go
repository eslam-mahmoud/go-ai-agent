// Package notify routes project events to the owner. Delivery is best-effort
// by design: the plan requires that Telegram failure never blocks execution.
package notify

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

var ErrInvalidNotification = errors.New("invalid notification")

// Kind is one meaningful project event, from the plan's notification list.
type Kind string

const (
	KindProjectInitialized Kind = "project.initialized"
	KindTaskSelected       Kind = "task.selected"
	KindProgress           Kind = "task.progress"
	KindQuestion           Kind = "task.question"
	KindApprovalRequest    Kind = "approval.requested"
	KindPlanChanged        Kind = "plan.changed"
	KindReleaseBlocker     Kind = "release.blocker"
	KindCIRepairExhausted  Kind = "ci.repair-exhausted"
	KindTaskCompleted      Kind = "task.completed"
	KindHealthChanged      Kind = "project.health-changed"
	KindProjectCompleted   Kind = "project.completed"
)

// AllKinds is the full vocabulary, in a stable order.
func AllKinds() []Kind {
	return []Kind{
		KindProjectInitialized,
		KindTaskSelected,
		KindProgress,
		KindQuestion,
		KindApprovalRequest,
		KindPlanChanged,
		KindReleaseBlocker,
		KindCIRepairExhausted,
		KindTaskCompleted,
		KindHealthChanged,
		KindProjectCompleted,
	}
}

func (kind Kind) Valid() bool {
	for _, known := range AllKinds() {
		if kind == known {
			return true
		}
	}
	return false
}

// Notification is one event to deliver.
type Notification struct {
	ProjectID int64
	Kind      Kind
	// Project is the human-facing project name.
	Project string
	// Subject is the headline: an issue reference, a task title, a health value.
	Subject string
	// Body carries the detail the owner needs to act.
	Body string
	// Fields are rendered as a short labelled list, in the given order.
	Fields []Field
	// Key makes delivery idempotent. Repeats with the same key are dropped.
	Key string
}

type Field struct {
	Label string
	Value string
}

// Outcome reports what happened to one notification.
type Outcome struct {
	Delivered  bool
	Suppressed bool
	Reason     string
	// Err is the delivery failure, if any. It is reported, never returned as
	// the router's own error, so a caller cannot fail on a Telegram outage.
	Err error
}

// Sender delivers rendered text. It is the only part that touches Telegram.
type Sender interface {
	Send(ctx context.Context, text string) error
}

// Recorder persists the notification history so deliveries, suppressions, and
// failures are all inspectable, and so idempotency survives a restart.
type Recorder interface {
	RecordNotification(
		projectID int64,
		kind string,
		key string,
		delivered bool,
		detail string,
	) error
	// NotificationDelivered reports whether this key was already delivered.
	// Without it, idempotency would only last as long as the process.
	NotificationDelivered(projectID int64, key string) (bool, error)
}

type Router struct {
	sender  Sender
	record  Recorder
	enabled map[Kind]struct{}
	sent    map[string]struct{}
}

// Options configures which kinds are routed. An empty Enabled set enables the
// full vocabulary, since a router that silently sends nothing is a trap.
type Options struct {
	Enabled []Kind
}

func NewRouter(sender Sender, recorder Recorder, options Options) (*Router, error) {
	if sender == nil {
		return nil, errors.New("notification sender is required")
	}
	enabled := make(map[Kind]struct{}, len(options.Enabled))
	if len(options.Enabled) == 0 {
		for _, kind := range AllKinds() {
			enabled[kind] = struct{}{}
		}
	} else {
		for _, kind := range options.Enabled {
			if !kind.Valid() {
				return nil, fmt.Errorf("%w: unknown kind %q", ErrInvalidNotification, kind)
			}
			enabled[kind] = struct{}{}
		}
	}
	return &Router{
		sender:  sender,
		record:  recorder,
		enabled: enabled,
		sent:    make(map[string]struct{}),
	}, nil
}

// Notify delivers one notification. It never returns a delivery failure as an
// error: the plan requires Telegram problems not to block execution, so the
// failure is reported in the outcome and recorded instead.
func (router *Router) Notify(
	ctx context.Context,
	notification Notification,
) (*Outcome, error) {
	if err := validate(notification); err != nil {
		return nil, err
	}
	if _, ok := router.enabled[notification.Kind]; !ok {
		return router.suppress(notification, "kind is not enabled"), nil
	}
	if key := strings.TrimSpace(notification.Key); key != "" {
		if _, repeated := router.sent[key]; repeated {
			return router.suppress(notification, "already delivered"), nil
		}
		if router.record != nil {
			delivered, err := router.record.NotificationDelivered(notification.ProjectID, key)
			if err != nil {
				return nil, fmt.Errorf("check notification history: %w", err)
			}
			if delivered {
				router.sent[key] = struct{}{}
				return router.suppress(notification, "already delivered"), nil
			}
		}
	}
	text := Render(notification)
	if err := router.sender.Send(ctx, text); err != nil {
		outcome := &Outcome{Reason: "delivery failed", Err: err}
		router.recordOutcome(notification, false, err.Error())
		return outcome, nil
	}
	if key := strings.TrimSpace(notification.Key); key != "" {
		router.sent[key] = struct{}{}
	}
	router.recordOutcome(notification, true, "")
	return &Outcome{Delivered: true}, nil
}

func (router *Router) suppress(notification Notification, reason string) *Outcome {
	router.recordOutcome(notification, false, reason)
	return &Outcome{Suppressed: true, Reason: reason}
}

// recordOutcome never fails the notification: an unusable history is a smaller
// problem than a blocked delivery step.
func (router *Router) recordOutcome(
	notification Notification,
	delivered bool,
	detail string,
) {
	if router.record == nil {
		return
	}
	_ = router.record.RecordNotification(
		notification.ProjectID,
		string(notification.Kind),
		notification.Key,
		delivered,
		detail,
	)
}

func validate(notification Notification) error {
	switch {
	case notification.ProjectID <= 0:
		return fmt.Errorf("%w: project ID must be positive", ErrInvalidNotification)
	case !notification.Kind.Valid():
		return fmt.Errorf("%w: unknown kind %q", ErrInvalidNotification, notification.Kind)
	case strings.TrimSpace(notification.Subject) == "":
		return fmt.Errorf("%w: subject is required", ErrInvalidNotification)
	default:
		return nil
	}
}

// Render produces the delivered text. It is deterministic so the same event
// always reads the same way.
func Render(notification Notification) string {
	var text strings.Builder
	fmt.Fprintf(&text, "%s %s", icon(notification.Kind), headline(notification.Kind))
	if project := inline(notification.Project); project != "" {
		fmt.Fprintf(&text, " · %s", project)
	}
	text.WriteString("\n\n")
	text.WriteString(inline(notification.Subject))
	if body := strings.TrimSpace(escape(notification.Body)); body != "" {
		text.WriteString("\n\n")
		text.WriteString(body)
	}
	for _, field := range notification.Fields {
		label := inline(field.Label)
		value := inline(field.Value)
		if label == "" || value == "" {
			continue
		}
		fmt.Fprintf(&text, "\n%s: %s", label, value)
	}
	return text.String()
}

func headline(kind Kind) string {
	switch kind {
	case KindProjectInitialized:
		return "Project initialized"
	case KindTaskSelected:
		return "Task selected"
	case KindProgress:
		return "Progress"
	case KindQuestion:
		return "Question"
	case KindApprovalRequest:
		return "Approval needed"
	case KindPlanChanged:
		return "Plan changed"
	case KindReleaseBlocker:
		return "Release blocker"
	case KindCIRepairExhausted:
		return "CI repair exhausted"
	case KindTaskCompleted:
		return "Task completed"
	case KindHealthChanged:
		return "Health changed"
	case KindProjectCompleted:
		return "Project completed"
	default:
		return "Update"
	}
}

func icon(kind Kind) string {
	switch kind {
	case KindQuestion, KindApprovalRequest:
		return "❓"
	case KindReleaseBlocker, KindCIRepairExhausted:
		return "🔴"
	case KindTaskCompleted, KindProjectCompleted, KindProjectInitialized:
		return "✅"
	case KindHealthChanged, KindPlanChanged:
		return "🟡"
	default:
		return "🔵"
	}
}

// EnabledKinds parses configured names into the routing set, rejecting
// anything unknown so a typo disables nothing silently.
func EnabledKinds(names []string) ([]Kind, error) {
	kinds := make([]Kind, 0, len(names))
	seen := make(map[Kind]struct{}, len(names))
	for _, name := range names {
		kind := Kind(strings.TrimSpace(name))
		if !kind.Valid() {
			return nil, fmt.Errorf("%w: unknown kind %q", ErrInvalidNotification, name)
		}
		if _, duplicate := seen[kind]; duplicate {
			continue
		}
		seen[kind] = struct{}{}
		kinds = append(kinds, kind)
	}
	sort.Slice(kinds, func(i, j int) bool { return kinds[i] < kinds[j] })
	return kinds, nil
}

func inline(value string) string {
	return strings.Join(strings.Fields(escape(value)), " ")
}

// escape neutralizes the characters Telegram's legacy Markdown treats as
// formatting, matching the escaping the existing gateway applies.
func escape(value string) string {
	replacer := strings.NewReplacer("_", "\\_", "*", "\\*", "`", "\\`", "[", "\\[")
	return replacer.Replace(value)
}
