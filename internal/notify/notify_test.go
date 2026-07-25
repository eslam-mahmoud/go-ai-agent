package notify

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/eslam-mahmoud/go-ai-agent/internal/telegram"
)

func TestNotifyRendersEveryKind(t *testing.T) {
	t.Parallel()
	for _, kind := range AllKinds() {
		kind := kind
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			sender := &fakeSender{}
			router := newTestRouter(t, sender, nil, Options{})
			outcome, err := router.Notify(context.Background(), Notification{
				ProjectID: 7,
				Kind:      kind,
				Project:   "Madar",
				Subject:   "#43 — Add the Codex adapter",
			})
			if err != nil {
				t.Fatalf("Notify: %v", err)
			}
			if !outcome.Delivered || len(sender.sent) != 1 {
				t.Fatalf("outcome = %#v", outcome)
			}
			text := sender.sent[0]
			if !strings.Contains(text, "Madar") ||
				!strings.Contains(text, "#43") {
				t.Fatalf("text = %q", text)
			}
			// Every kind renders a distinct headline, never the fallback.
			if strings.Contains(text, "Update\n") {
				t.Fatalf("kind %q fell through to the default headline", kind)
			}
		})
	}
}

func TestNotifyRendersBodyAndFields(t *testing.T) {
	t.Parallel()
	sender := &fakeSender{}
	router := newTestRouter(t, sender, nil, Options{})
	if _, err := router.Notify(context.Background(), Notification{
		ProjectID: 7,
		Kind:      KindApprovalRequest,
		Project:   "Madar",
		Subject:   "Force push to main",
		Body:      "The fixer wants to rewrite history.",
		Fields: []Field{
			{Label: "Task", Value: "#43"},
			{Label: "Risk", Value: "high"},
			{Label: "Empty", Value: "   "},
		},
	}); err != nil {
		t.Fatal(err)
	}
	text := sender.sent[0]
	for _, fragment := range []string{
		"Approval needed · Madar",
		"Force push to main",
		"rewrite history",
		"Task: #43",
		"Risk: high",
	} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("text missing %q:\n%s", fragment, text)
		}
	}
	if strings.Contains(text, "Empty") {
		t.Fatalf("an empty field was rendered:\n%s", text)
	}
}

func TestNotifyEscapesFormattingCharacters(t *testing.T) {
	t.Parallel()
	sender := &fakeSender{}
	router := newTestRouter(t, sender, nil, Options{})
	if _, err := router.Notify(context.Background(), Notification{
		ProjectID: 7,
		Kind:      KindQuestion,
		Project:   "Madar",
		Subject:   "Which *region* should _ship_ first?",
	}); err != nil {
		t.Fatal(err)
	}
	text := sender.sent[0]
	if strings.Contains(text, "*region*") || strings.Contains(text, "_ship_") {
		t.Fatalf("formatting characters were not escaped:\n%s", text)
	}
}

func TestNotifySuppressesRepeatsAndDisabledKinds(t *testing.T) {
	t.Parallel()
	t.Run("repeat", func(t *testing.T) {
		t.Parallel()
		sender := &fakeSender{}
		router := newTestRouter(t, sender, nil, Options{})
		notification := Notification{
			ProjectID: 7,
			Kind:      KindTaskCompleted,
			Subject:   "#43 done",
			Key:       "task:43:completed",
		}
		if _, err := router.Notify(context.Background(), notification); err != nil {
			t.Fatal(err)
		}
		outcome, err := router.Notify(context.Background(), notification)
		if err != nil {
			t.Fatal(err)
		}
		if !outcome.Suppressed || len(sender.sent) != 1 {
			t.Fatalf("outcome = %#v, sent = %d", outcome, len(sender.sent))
		}
	})

	t.Run("disabled kind", func(t *testing.T) {
		t.Parallel()
		sender := &fakeSender{}
		router := newTestRouter(t, sender, nil, Options{
			Enabled: []Kind{KindQuestion},
		})
		outcome, err := router.Notify(context.Background(), Notification{
			ProjectID: 7, Kind: KindProgress, Subject: "still going",
		})
		if err != nil {
			t.Fatal(err)
		}
		if !outcome.Suppressed || len(sender.sent) != 0 {
			t.Fatalf("outcome = %#v, sent = %d", outcome, len(sender.sent))
		}
		// The enabled kind still gets through.
		if _, err := router.Notify(context.Background(), Notification{
			ProjectID: 7, Kind: KindQuestion, Subject: "which region?",
		}); err != nil {
			t.Fatal(err)
		}
		if len(sender.sent) != 1 {
			t.Fatalf("enabled kind was not delivered")
		}
	})
}

// The plan requires that Telegram failure never blocks execution.
func TestNotifyNeverReturnsADeliveryFailure(t *testing.T) {
	t.Parallel()
	failure := errors.New("telegram is unreachable")
	sender := &fakeSender{err: failure}
	recorder := &fakeRecorder{}
	router := newTestRouter(t, sender, recorder, Options{})

	outcome, err := router.Notify(context.Background(), Notification{
		ProjectID: 7,
		Kind:      KindTaskCompleted,
		Subject:   "#43 done",
		Key:       "task:43:completed",
	})
	if err != nil {
		t.Fatalf("delivery failure was returned as an error: %v", err)
	}
	if outcome.Delivered || !errors.Is(outcome.Err, failure) {
		t.Fatalf("outcome = %#v", outcome)
	}
	// The failure is recorded, and the key is not consumed, so a later retry
	// can still deliver.
	if len(recorder.records) != 1 || recorder.records[0].delivered {
		t.Fatalf("records = %#v", recorder.records)
	}
	sender.err = nil
	retry, err := router.Notify(context.Background(), Notification{
		ProjectID: 7,
		Kind:      KindTaskCompleted,
		Subject:   "#43 done",
		Key:       "task:43:completed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !retry.Delivered {
		t.Fatalf("retry after failure was suppressed: %#v", retry)
	}
}

func TestNotifyUsesDurableHistoryForIdempotency(t *testing.T) {
	t.Parallel()
	sender := &fakeSender{}
	// A fresh router with history from a previous process.
	recorder := &fakeRecorder{delivered: map[string]bool{"task:43:completed": true}}
	router := newTestRouter(t, sender, recorder, Options{})

	outcome, err := router.Notify(context.Background(), Notification{
		ProjectID: 7,
		Kind:      KindTaskCompleted,
		Subject:   "#43 done",
		Key:       "task:43:completed",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Suppressed || len(sender.sent) != 0 {
		t.Fatalf("a restart re-sent a delivered notification: %#v", outcome)
	}

	recorder.err = errors.New("database is unavailable")
	if _, err := router.Notify(context.Background(), Notification{
		ProjectID: 7, Kind: KindQuestion, Subject: "which region?", Key: "q:1",
	}); err == nil {
		t.Fatal("an unreadable history was treated as no history")
	}
}

func TestNotifyRejectsUnusableNotifications(t *testing.T) {
	t.Parallel()
	sender := &fakeSender{}
	router := newTestRouter(t, sender, nil, Options{})
	cases := []Notification{
		{ProjectID: 0, Kind: KindQuestion, Subject: "x"},
		{ProjectID: 7, Kind: "nonsense", Subject: "x"},
		{ProjectID: 7, Kind: KindQuestion, Subject: "   "},
	}
	for _, notification := range cases {
		if _, err := router.Notify(
			context.Background(), notification,
		); !errors.Is(err, ErrInvalidNotification) {
			t.Fatalf("notification %#v error = %v", notification, err)
		}
	}
	if len(sender.sent) != 0 {
		t.Fatal("a rejected notification was delivered")
	}
	if _, err := NewRouter(nil, nil, Options{}); err == nil {
		t.Error("missing sender accepted")
	}
	if _, err := NewRouter(sender, nil, Options{
		Enabled: []Kind{"nonsense"},
	}); !errors.Is(err, ErrInvalidNotification) {
		t.Error("unknown enabled kind accepted")
	}
}

func TestEnabledKindsRejectsUnknownNames(t *testing.T) {
	t.Parallel()
	kinds, err := EnabledKinds([]string{
		"task.completed", " task.selected ", "task.completed",
	})
	if err != nil {
		t.Fatalf("EnabledKinds: %v", err)
	}
	if len(kinds) != 2 {
		t.Fatalf("kinds = %v", kinds)
	}
	// A typo must fail loudly rather than silently disabling a notification.
	if _, err := EnabledKinds([]string{"task.complete"}); !errors.Is(
		err, ErrInvalidNotification,
	) {
		t.Fatalf("typo error = %v", err)
	}
}

func newTestRouter(
	t *testing.T,
	sender Sender,
	recorder Recorder,
	options Options,
) *Router {
	t.Helper()
	router, err := NewRouter(sender, recorder, options)
	if err != nil {
		t.Fatal(err)
	}
	return router
}

type fakeSender struct {
	sent []string
	err  error
}

func (fake *fakeSender) Send(_ context.Context, text string) error {
	if fake.err != nil {
		return fake.err
	}
	fake.sent = append(fake.sent, text)
	return nil
}

type recordedNotification struct {
	kind      string
	key       string
	delivered bool
	detail    string
}

type fakeRecorder struct {
	records   []recordedNotification
	delivered map[string]bool
	err       error
}

func (fake *fakeRecorder) RecordNotification(
	_ int64,
	kind, key string,
	delivered bool,
	detail string,
) error {
	fake.records = append(fake.records, recordedNotification{kind, key, delivered, detail})
	if delivered {
		if fake.delivered == nil {
			fake.delivered = map[string]bool{}
		}
		fake.delivered[key] = true
	}
	return nil
}

func (fake *fakeRecorder) NotificationDelivered(_ int64, key string) (bool, error) {
	if fake.err != nil {
		return false, fake.err
	}
	return fake.delivered[key], nil
}

// The gateway must satisfy the router's sender contract, so the router is
// usable with real Telegram delivery rather than only with a test double.
func TestGatewaySatisfiesTheSenderContract(t *testing.T) {
	t.Parallel()
	var sender Sender = telegram.New("", nil)
	if sender == nil {
		t.Fatal("gateway does not satisfy Sender")
	}
	// An unconfigured gateway must not fail delivery, since Telegram problems
	// never block execution.
	if err := sender.Send(context.Background(), "hello"); err != nil {
		t.Fatalf("unconfigured gateway send: %v", err)
	}
}
