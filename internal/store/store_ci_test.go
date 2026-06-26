package store

import (
	"testing"
	"time"
)

func TestSetPRNumber(t *testing.T) {
	s := openTestStore(t)
	_, _ = s.UpsertTask("r", 1, StateInProgress, "sess")
	if err := s.SetPRNumber("r", 1, 42); err != nil {
		t.Fatalf("SetPRNumber: %v", err)
	}
	got, _ := s.GetTask("r", 1)
	if got.PRNumber != 42 {
		t.Errorf("PRNumber = %d, want 42", got.PRNumber)
	}
}

func TestSetCIState(t *testing.T) {
	s := openTestStore(t)
	_, _ = s.UpsertTask("r", 2, StateInProgress, "sess")
	if err := s.SetCIState("r", 2, CIStateWaiting); err != nil {
		t.Fatalf("SetCIState: %v", err)
	}
	got, _ := s.GetTask("r", 2)
	if got.CIState != CIStateWaiting {
		t.Errorf("CIState = %q, want waiting", got.CIState)
	}
}

func TestIncrementCIRetries(t *testing.T) {
	s := openTestStore(t)
	_, _ = s.UpsertTask("r", 3, StateInProgress, "sess")

	n, err := s.IncrementCIRetries("r", 3)
	if err != nil {
		t.Fatalf("IncrementCIRetries: %v", err)
	}
	if n != 1 {
		t.Errorf("retries after first increment = %d, want 1", n)
	}

	n, _ = s.IncrementCIRetries("r", 3)
	if n != 2 {
		t.Errorf("retries after second increment = %d, want 2", n)
	}
}

func TestListByCIState(t *testing.T) {
	s := openTestStore(t)
	_, _ = s.UpsertTask("r", 10, StateInProgress, "s1")
	_, _ = s.UpsertTask("r", 11, StateInProgress, "s2")
	_, _ = s.UpsertTask("r", 12, StateInProgress, "s3")

	_ = s.SetCIState("r", 10, CIStateWaiting)
	_ = s.SetCIState("r", 11, CIStateWaiting)
	_ = s.SetCIState("r", 12, CIStatePassed)

	waiting, err := s.ListByCIState(CIStateWaiting)
	if err != nil {
		t.Fatalf("ListByCIState: %v", err)
	}
	if len(waiting) != 2 {
		t.Errorf("waiting count = %d, want 2", len(waiting))
	}

	passed, _ := s.ListByCIState(CIStatePassed)
	if len(passed) != 1 {
		t.Errorf("passed count = %d, want 1", len(passed))
	}
}

func TestSetCIWatchStartedAt(t *testing.T) {
	s := openTestStore(t)
	_, _ = s.UpsertTask("r", 20, StateInProgress, "sess")
	now := time.Now().UTC().Truncate(time.Second)
	if err := s.SetCIWatchStartedAt("r", 20, now); err != nil {
		t.Fatalf("SetCIWatchStartedAt: %v", err)
	}
	got, _ := s.GetTask("r", 20)
	if got.CIWatchStartedAt == nil {
		t.Fatal("CIWatchStartedAt is nil")
	}
	diff := got.CIWatchStartedAt.Sub(now)
	if diff < 0 {
		diff = -diff
	}
	if diff > time.Second {
		t.Errorf("CIWatchStartedAt diff %v too large", diff)
	}
}

func TestCIStateConstants(t *testing.T) {
	// Ensure state strings are stable (used as DB values).
	cases := []struct{ state CIState; want string }{
		{CIStateNone, ""},
		{CIStateWaiting, "waiting"},
		{CIStatePassed, "passed"},
		{CIStateFailed, "failed"},
		{CIStateGaveUp, "gave_up"},
	}
	for _, tc := range cases {
		if string(tc.state) != tc.want {
			t.Errorf("CIState %q = %q, want %q", tc.state, string(tc.state), tc.want)
		}
	}
}
