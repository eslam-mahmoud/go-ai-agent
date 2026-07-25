package store

import (
	"testing"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

func TestStatusMessageIdentitySurvivesRestart(t *testing.T) {
	s := openTestStore(t)
	project, err := s.CreateProject(domain.NewProject("owner/status", "Madar", "Goal", ""))
	if err != nil {
		t.Fatal(err)
	}

	if message, err := s.GetStatusMessage(project.ID); err != nil || message != nil {
		t.Fatalf("fresh project = %#v, err = %v", message, err)
	}
	if err := s.SaveStatusMessage(StatusMessage{
		ProjectID: project.ID, ChatID: 11, MessageID: 21, LastText: "first",
	}); err != nil {
		t.Fatal(err)
	}
	stored, err := s.GetStatusMessage(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ChatID != 11 || stored.MessageID != 21 || stored.LastText != "first" {
		t.Fatalf("stored = %#v", stored)
	}
	if stored.UpdatedAt.IsZero() {
		t.Fatal("timestamp was not recorded")
	}

	// Saving again replaces the identity rather than adding a second row,
	// which is what lets a recovered message be tracked.
	if err := s.SaveStatusMessage(StatusMessage{
		ProjectID: project.ID, ChatID: 11, MessageID: 22, LastText: "second",
	}); err != nil {
		t.Fatal(err)
	}
	stored, _ = s.GetStatusMessage(project.ID)
	if stored.MessageID != 22 || stored.LastText != "second" {
		t.Fatalf("replaced = %#v", stored)
	}

	for _, invalid := range []StatusMessage{
		{ProjectID: 0, ChatID: 1, MessageID: 1},
		{ProjectID: project.ID, ChatID: 0, MessageID: 1},
		{ProjectID: project.ID, ChatID: 1, MessageID: 0},
	} {
		if err := s.SaveStatusMessage(invalid); err == nil {
			t.Fatalf("accepted %#v", invalid)
		}
	}
	if _, err := s.GetStatusMessage(0); err == nil {
		t.Fatal("zero project accepted")
	}
}
