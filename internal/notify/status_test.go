package notify

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
)

func TestStatusRendersThePlanLayout(t *testing.T) {
	t.Parallel()
	status := fixtureStatus(domain.TaskDeveloping)
	text := RenderStatus(status)
	for _, fragment := range []string{
		"Madar · Madar v2",
		"Health: on-track",
		"Issue: #43 — Add the Codex adapter",
		"Mode: developer",
		"Elapsed: 14m",
		"✅ Manager selected task",
		"✅ Planner",
		"🟡 Developer",
		"⬜ Review",
		"⬜ Verification",
		"⬜ Manager review",
	} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("status missing %q:\n%s", fragment, text)
		}
	}
}

func TestStatusChecklistFollowsTaskStatus(t *testing.T) {
	t.Parallel()
	tests := []struct {
		status  domain.TaskStatus
		running string
		done    []string
	}{
		{domain.TaskSelected, "Manager selected task", nil},
		{domain.TaskPlanning, "Planner", []string{"Manager selected task"}},
		{domain.TaskDeveloping, "Developer", []string{"Planner"}},
		{domain.TaskReviewing, "Review", []string{"Developer"}},
		{domain.TaskFixing, "Review", []string{"Developer"}},
		{domain.TaskVerifying, "Verification", []string{"Review"}},
		{domain.TaskWaitingCI, "Verification", []string{"Review"}},
		{domain.TaskCompleted, "Manager review", []string{"Verification"}},
	}
	for _, test := range tests {
		test := test
		t.Run(string(test.status), func(t *testing.T) {
			t.Parallel()
			text := RenderStatus(fixtureStatus(test.status))
			if !strings.Contains(text, "🟡 "+test.running) {
				t.Fatalf("%q is not running:\n%s", test.running, text)
			}
			for _, done := range test.done {
				if !strings.Contains(text, "✅ "+done) {
					t.Fatalf("%q is not done:\n%s", done, text)
				}
			}
		})
	}

	// Work outside the delivery lane claims no progress.
	blocked := fixtureStatus(domain.TaskBlocked)
	text := RenderStatus(blocked)
	if strings.Contains(text, "✅") || strings.Contains(text, "🟡 Developer") {
		t.Fatalf("a blocked task claimed progress:\n%s", text)
	}
}

func TestStatusElapsedFormatting(t *testing.T) {
	t.Parallel()
	base := fixtureStatus(domain.TaskDeveloping)
	base.Since = base.Now.Add(-95 * time.Minute)
	if !strings.Contains(RenderStatus(base), "Elapsed: 1h35m") {
		t.Fatalf("long elapsed:\n%s", RenderStatus(base))
	}
	// An idle project has no elapsed time to report.
	idle := base
	idle.CurrentTask = nil
	if !strings.Contains(RenderStatus(idle), "Elapsed: —") {
		t.Fatalf("idle elapsed:\n%s", RenderStatus(idle))
	}
	// A clock that went backwards must not render a negative duration.
	skewed := base
	skewed.Since = base.Now.Add(time.Hour)
	if !strings.Contains(RenderStatus(skewed), "Elapsed: —") {
		t.Fatalf("skewed elapsed:\n%s", RenderStatus(skewed))
	}
}

func TestPublishSendsOnceThenEditsInPlace(t *testing.T) {
	t.Parallel()
	sender := &fakeStatusSender{chatID: 11, messageID: 21}
	statusStore := newFakeStatusStore()
	publisher := newTestStatusPublisher(t, sender, statusStore)

	first, err := publisher.Publish(context.Background(), fixtureStatus(domain.TaskDeveloping))
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !first.Sent || sender.sends != 1 || sender.edits != 0 {
		t.Fatalf("first = %#v, sends = %d", first, sender.sends)
	}

	// Same state renders identically, so nothing is sent at all.
	unchanged, err := publisher.Publish(context.Background(), fixtureStatus(domain.TaskDeveloping))
	if err != nil {
		t.Fatal(err)
	}
	if !unchanged.Unchanged || sender.sends != 1 || sender.edits != 0 {
		t.Fatalf("unchanged = %#v, edits = %d", unchanged, sender.edits)
	}

	// A real change edits the same message rather than posting another.
	moved, err := publisher.Publish(context.Background(), fixtureStatus(domain.TaskReviewing))
	if err != nil {
		t.Fatal(err)
	}
	if !moved.Edited || sender.sends != 1 || sender.edits != 1 {
		t.Fatalf("moved = %#v, sends = %d, edits = %d", moved, sender.sends, sender.edits)
	}
	if sender.lastChatID != 11 || sender.lastMessageID != 21 {
		t.Fatalf("edited the wrong message: %d/%d", sender.lastChatID, sender.lastMessageID)
	}
}

// A restart must keep editing the same message rather than posting a second.
func TestPublishContinuesEditingAfterARestart(t *testing.T) {
	t.Parallel()
	sender := &fakeStatusSender{chatID: 11, messageID: 21}
	statusStore := newFakeStatusStore()
	if _, err := newTestStatusPublisher(t, sender, statusStore).Publish(
		context.Background(), fixtureStatus(domain.TaskDeveloping),
	); err != nil {
		t.Fatal(err)
	}
	// A fresh publisher, as after a process restart.
	restarted := newTestStatusPublisher(t, sender, statusStore)
	outcome, err := restarted.Publish(context.Background(), fixtureStatus(domain.TaskReviewing))
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Edited || sender.sends != 1 {
		t.Fatalf("restart posted a second message: %#v, sends = %d", outcome, sender.sends)
	}
}

// If the owner deleted the message, editing fails and a new one must appear.
func TestPublishRecoversWhenTheMessageIsGone(t *testing.T) {
	t.Parallel()
	sender := &fakeStatusSender{chatID: 11, messageID: 21}
	statusStore := newFakeStatusStore()
	publisher := newTestStatusPublisher(t, sender, statusStore)
	if _, err := publisher.Publish(
		context.Background(), fixtureStatus(domain.TaskDeveloping),
	); err != nil {
		t.Fatal(err)
	}

	sender.editErr = errors.New("message to edit not found")
	sender.messageID = 22
	outcome, err := publisher.Publish(context.Background(), fixtureStatus(domain.TaskReviewing))
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !outcome.Sent || sender.sends != 2 {
		t.Fatalf("outcome = %#v, sends = %d", outcome, sender.sends)
	}
	// The new identity is remembered, so the next edit targets the new message.
	saved, _ := statusStore.GetStatusMessage(7)
	if saved.MessageID != 22 {
		t.Fatalf("saved message = %d", saved.MessageID)
	}
}

func TestPublishToleratesDeliveryFailureAndMissingConfiguration(t *testing.T) {
	t.Parallel()
	failure := errors.New("telegram is unreachable")
	sender := &fakeStatusSender{sendErr: failure}
	publisher := newTestStatusPublisher(t, sender, newFakeStatusStore())
	outcome, err := publisher.Publish(
		context.Background(), fixtureStatus(domain.TaskDeveloping),
	)
	if err != nil {
		t.Fatalf("delivery failure was returned as an error: %v", err)
	}
	if !errors.Is(outcome.Err, failure) || outcome.Sent {
		t.Fatalf("outcome = %#v", outcome)
	}

	// An unconfigured gateway returns no identity and must not be recorded.
	silent := &fakeStatusSender{}
	silentStore := newFakeStatusStore()
	quiet, err := newTestStatusPublisher(t, silent, silentStore).Publish(
		context.Background(), fixtureStatus(domain.TaskDeveloping),
	)
	if err != nil {
		t.Fatal(err)
	}
	if quiet.Sent || quiet.Edited {
		t.Fatalf("unconfigured outcome = %#v", quiet)
	}
	if saved, _ := silentStore.GetStatusMessage(7); saved != nil {
		t.Fatalf("unconfigured gateway recorded %#v", saved)
	}
}

func TestPublishRejectsUnusableInput(t *testing.T) {
	t.Parallel()
	publisher := newTestStatusPublisher(t, &fakeStatusSender{}, newFakeStatusStore())
	if _, err := publisher.Publish(context.Background(), Status{}); !errors.Is(
		err, ErrInvalidStatus,
	) {
		t.Fatalf("missing project error = %v", err)
	}
	if _, err := NewStatusPublisher(nil, newFakeStatusStore()); err == nil {
		t.Error("missing sender accepted")
	}
	if _, err := NewStatusPublisher(&fakeStatusSender{}, nil); err == nil {
		t.Error("missing store accepted")
	}
}

func fixtureStatus(taskStatus domain.TaskStatus) Status {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	project := domain.NewProject("owner/repo", "Madar v2", "Ship v2", "")
	project.ID = 7
	project.Health = domain.HealthOnTrack
	task := domain.NewTask(7, "Add the Codex adapter", "Adapter goal")
	task.ID = 3
	task.IssueNumber = 43
	task.Status = taskStatus
	return Status{
		Project:     project,
		CurrentTask: task,
		Mode:        modeForStatus(taskStatus),
		Since:       now.Add(-14 * time.Minute),
		Now:         now,
	}
}

func modeForStatus(status domain.TaskStatus) string {
	switch status {
	case domain.TaskDeveloping:
		return "developer"
	case domain.TaskReviewing:
		return "reviewer"
	default:
		return ""
	}
}

func newTestStatusPublisher(
	t *testing.T,
	sender StatusSender,
	statusStore StatusStore,
) *StatusPublisher {
	t.Helper()
	publisher, err := NewStatusPublisher(sender, statusStore)
	if err != nil {
		t.Fatal(err)
	}
	return publisher
}

type fakeStatusSender struct {
	chatID        int64
	messageID     int64
	sends         int
	edits         int
	lastChatID    int64
	lastMessageID int64
	sendErr       error
	editErr       error
}

func (fake *fakeStatusSender) SendStatus(
	_ context.Context,
	_ string,
) (int64, int64, error) {
	if fake.sendErr != nil {
		return 0, 0, fake.sendErr
	}
	fake.sends++
	return fake.chatID, fake.messageID, nil
}

func (fake *fakeStatusSender) EditStatus(
	_ context.Context,
	chatID, messageID int64,
	_ string,
) error {
	if fake.editErr != nil {
		return fake.editErr
	}
	fake.edits++
	fake.lastChatID = chatID
	fake.lastMessageID = messageID
	return nil
}

type fakeStatusStore struct {
	messages map[int64]store.StatusMessage
}

func newFakeStatusStore() *fakeStatusStore {
	return &fakeStatusStore{messages: map[int64]store.StatusMessage{}}
}

func (fake *fakeStatusStore) GetStatusMessage(projectID int64) (*store.StatusMessage, error) {
	message, ok := fake.messages[projectID]
	if !ok {
		return nil, nil
	}
	copied := message
	return &copied, nil
}

func (fake *fakeStatusStore) SaveStatusMessage(message store.StatusMessage) error {
	fake.messages[message.ProjectID] = message
	return nil
}
