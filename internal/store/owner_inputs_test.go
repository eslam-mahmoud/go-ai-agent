package store

import (
	"testing"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

func TestOwnerInputsAreRecordedAndConsumedOnce(t *testing.T) {
	s := openTestStore(t)
	project, err := s.CreateProject(domain.NewProject("owner/input", "Madar", "Goal", ""))
	if err != nil {
		t.Fatal(err)
	}

	answer, err := s.RecordOwnerInput(OwnerInput{
		ProjectID: project.ID,
		Kind:      OwnerAnswer,
		Body:      "use eu-west",
		Author:    "99",
	})
	if err != nil {
		t.Fatal(err)
	}
	if answer.ID == 0 || answer.CreatedAt.IsZero() {
		t.Fatalf("answer = %#v", answer)
	}
	if _, err := s.RecordOwnerInput(OwnerInput{
		ProjectID: project.ID,
		Kind:      OwnerApproval,
		Subject:   "force push",
	}); err != nil {
		t.Fatal(err)
	}

	pending, err := s.PendingOwnerInputs(project.ID, OwnerAnswer)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Body != "use eu-west" {
		t.Fatalf("pending answers = %#v", pending)
	}

	// Consuming applies an input once and only once.
	if err := s.ConsumeOwnerInput(answer.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.ConsumeOwnerInput(answer.ID); err == nil {
		t.Fatal("an input was consumed twice")
	}
	pending, _ = s.PendingOwnerInputs(project.ID, OwnerAnswer)
	if len(pending) != 0 {
		t.Fatalf("consumed input is still pending: %#v", pending)
	}
	// The approval is untouched by consuming the answer.
	approvals, _ := s.PendingOwnerInputs(project.ID, OwnerApproval)
	if len(approvals) != 1 {
		t.Fatalf("approvals = %#v", approvals)
	}
}

func TestOwnerInputsRejectEmptyContent(t *testing.T) {
	s := openTestStore(t)
	project, err := s.CreateProject(domain.NewProject("owner/empty", "Madar", "Goal", ""))
	if err != nil {
		t.Fatal(err)
	}
	invalid := []OwnerInput{
		{ProjectID: 0, Kind: OwnerAnswer, Body: "x"},
		{ProjectID: project.ID, Kind: "nonsense", Body: "x"},
		{ProjectID: project.ID, Kind: OwnerAnswer, Body: "   "},
		{ProjectID: project.ID, Kind: OwnerApproval, Subject: "  "},
	}
	for _, input := range invalid {
		if _, err := s.RecordOwnerInput(input); err == nil {
			t.Fatalf("accepted %#v", input)
		}
	}
	if _, err := s.PendingOwnerInputs(0, OwnerAnswer); err == nil {
		t.Fatal("zero project accepted")
	}
	if err := s.ConsumeOwnerInput(0); err == nil {
		t.Fatal("zero ID accepted")
	}
}
