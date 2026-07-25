package store

import (
	"testing"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

func TestNotificationHistoryMakesIdempotencyDurable(t *testing.T) {
	s := openTestStore(t)
	project, err := s.CreateProject(domain.NewProject("owner/notify", "Madar", "Goal", ""))
	if err != nil {
		t.Fatal(err)
	}

	delivered, err := s.NotificationDelivered(project.ID, "task:1:completed")
	if err != nil || delivered {
		t.Fatalf("fresh key = %v, err = %v", delivered, err)
	}
	if err := s.RecordNotification(
		project.ID, "task.completed", "task:1:completed", true, "",
	); err != nil {
		t.Fatal(err)
	}
	delivered, err = s.NotificationDelivered(project.ID, "task:1:completed")
	if err != nil || !delivered {
		t.Fatalf("recorded key = %v, err = %v", delivered, err)
	}

	// Recording the same delivered key again must not fail a delivery step.
	if err := s.RecordNotification(
		project.ID, "task.completed", "task:1:completed", true, "",
	); err != nil {
		t.Fatalf("repeat record: %v", err)
	}

	// Failures are recorded without claiming the key.
	if err := s.RecordNotification(
		project.ID, "task.question", "task:2:question", false, "telegram down",
	); err != nil {
		t.Fatal(err)
	}
	delivered, err = s.NotificationDelivered(project.ID, "task:2:question")
	if err != nil || delivered {
		t.Fatalf("failed key = %v, err = %v", delivered, err)
	}

	history, err := s.ListNotifications(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The repeated delivered key is suppressed by the unique index, so it
	// adds no duplicate row: two entries, not three.
	if len(history) != 2 {
		t.Fatalf("history = %d entries", len(history))
	}
	if history[0].Kind != "task.completed" || !history[0].Delivered {
		t.Fatalf("first entry = %#v", history[0])
	}
	if history[1].Delivered || history[1].Detail != "telegram down" {
		t.Fatalf("failure entry = %#v", history[1])
	}

	// An empty key is never treated as delivered.
	if delivered, err := s.NotificationDelivered(project.ID, "  "); err != nil || delivered {
		t.Fatalf("blank key = %v, err = %v", delivered, err)
	}
	if err := s.RecordNotification(0, "kind", "", false, ""); err == nil {
		t.Fatal("missing project accepted")
	}
	if err := s.RecordNotification(project.ID, "  ", "", false, ""); err == nil {
		t.Fatal("missing kind accepted")
	}
}
