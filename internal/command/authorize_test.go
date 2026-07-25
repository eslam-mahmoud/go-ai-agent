package command

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// The plan requires that only configured Telegram IDs may execute commands.
func TestAuthorizeAllowsOnlyConfiguredSenders(t *testing.T) {
	t.Parallel()
	recorder := &fakeRefusalRecorder{}
	authorizer := NewOwnerAuthorizer(OwnerAuthorizerOptions{
		AllowedUserIDs: []int64{99},
	}, recorder)

	if err := authorizer.Authorize(context.Background(), Command{
		Name: NameStatus, UserID: 99,
	}); err != nil {
		t.Fatalf("allowed sender was refused: %v", err)
	}
	err := authorizer.Authorize(context.Background(), Command{
		Name: NameStatus, UserID: 1234,
	})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unlisted sender error = %v", err)
	}
	if len(recorder.refusals) != 1 || recorder.refusals[0].userID != 1234 {
		t.Fatalf("refusals = %#v", recorder.refusals)
	}
}

// An empty allowlist must authorize nobody, not everybody.
func TestAuthorizeDefaultsToRefusal(t *testing.T) {
	t.Parallel()
	authorizer := NewOwnerAuthorizer(OwnerAuthorizerOptions{}, nil)
	for _, userID := range []int64{0, 1, 99} {
		if err := authorizer.Authorize(context.Background(), Command{
			Name: NameStatus, UserID: userID,
		}); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("empty allowlist authorized %d: %v", userID, err)
		}
	}
	// A zero ID in the configured set must not become a wildcard.
	zeroConfigured := NewOwnerAuthorizer(OwnerAuthorizerOptions{
		AllowedUserIDs: []int64{0},
	}, nil)
	if err := zeroConfigured.Authorize(context.Background(), Command{
		Name: NameStatus, UserID: 0,
	}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("a zero ID was treated as configured: %v", err)
	}
}

func TestAuthorizeRefusesStaleCommands(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	authorizer := NewOwnerAuthorizer(OwnerAuthorizerOptions{
		AllowedUserIDs: []int64{99},
		MaxAge:         10 * time.Minute,
		Now:            func() time.Time { return now },
	}, nil)

	fresh := Command{Name: NameStatus, UserID: 99, SentAt: now.Add(-9 * time.Minute)}
	if err := authorizer.Authorize(context.Background(), fresh); err != nil {
		t.Fatalf("fresh command refused: %v", err)
	}
	stale := Command{Name: NameStatus, UserID: 99, SentAt: now.Add(-11 * time.Minute)}
	if err := authorizer.Authorize(context.Background(), stale); !errors.Is(err, ErrExpired) {
		t.Fatalf("stale command error = %v", err)
	}
	// A command with no timestamp is not aged out, since freshness is unknown.
	if err := authorizer.Authorize(context.Background(), Command{
		Name: NameStatus, UserID: 99,
	}); err != nil {
		t.Fatalf("timestampless command refused: %v", err)
	}
}

func TestAuthorizeRateLimitsPerSenderAndRecovers(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	authorizer := NewOwnerAuthorizer(OwnerAuthorizerOptions{
		AllowedUserIDs: []int64{99, 100},
		Window:         time.Minute,
		MaxPerWindow:   3,
		Now:            func() time.Time { return now },
	}, nil)
	command := Command{Name: NameStatus, UserID: 99}

	for attempt := range 3 {
		if err := authorizer.Authorize(context.Background(), command); err != nil {
			t.Fatalf("attempt %d refused: %v", attempt, err)
		}
	}
	if err := authorizer.Authorize(context.Background(), command); !errors.Is(
		err, ErrRateLimited,
	) {
		t.Fatalf("over-limit error = %v", err)
	}
	// The limit is per sender, so another owner is unaffected.
	if err := authorizer.Authorize(context.Background(), Command{
		Name: NameStatus, UserID: 100,
	}); err != nil {
		t.Fatalf("a second sender was rate limited: %v", err)
	}
	// The window slides, so the sender recovers.
	now = now.Add(61 * time.Second)
	if err := authorizer.Authorize(context.Background(), command); err != nil {
		t.Fatalf("sender did not recover: %v", err)
	}
}

// Inspection is cheap; control is not.
func TestControlCommandsCarryATighterLimit(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	authorizer := NewOwnerAuthorizer(OwnerAuthorizerOptions{
		AllowedUserIDs:      []int64{99},
		Window:              time.Minute,
		MaxPerWindow:        10,
		MaxControlPerWindow: 2,
		Now:                 func() time.Time { return now },
	}, nil)
	control := Command{Name: NamePause, UserID: 99}

	for attempt := range 2 {
		if err := authorizer.Authorize(context.Background(), control); err != nil {
			t.Fatalf("control attempt %d refused: %v", attempt, err)
		}
	}
	if err := authorizer.Authorize(context.Background(), control); !errors.Is(
		err, ErrRateLimited,
	) {
		t.Fatalf("control over-limit error = %v", err)
	}
}

func TestIsControlCommandCoversEveryMutatingCommand(t *testing.T) {
	t.Parallel()
	for _, name := range []Name{
		NamePause, NameResume, NameCancel, NameRetry,
		NameAnswer, NameApprove, NameReject,
	} {
		if !IsControlCommand(name) {
			t.Errorf("/%s is not treated as control", name)
		}
	}
	for _, name := range []Name{
		NameStatus, NameProject, NamePlan, NameNext, NameLogs, NameHelp,
	} {
		if IsControlCommand(name) {
			t.Errorf("/%s is treated as control", name)
		}
	}
}

// Each refusal reads differently, so the owner knows what to do next.
func TestDispatchDistinguishesRefusalReasons(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	authorizer := NewOwnerAuthorizer(OwnerAuthorizerOptions{
		AllowedUserIDs: []int64{99},
		MaxAge:         time.Minute,
		Window:         time.Minute,
		MaxPerWindow:   1,
		Now:            func() time.Time { return now },
	}, nil)
	router := newTestRouter(t, authorizer)
	if err := router.Register(NameStatus, "status", func(
		context.Context, Command,
	) (string, error) {
		return "ok", nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	unauthorized, err := router.Dispatch(ctx, Command{Name: NameStatus, UserID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(unauthorized, "not authorized") {
		t.Fatalf("unauthorized reply = %q", unauthorized)
	}
	expired, err := router.Dispatch(ctx, Command{
		Name: NameStatus, UserID: 99, SentAt: now.Add(-2 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(expired, "too old") {
		t.Fatalf("expired reply = %q", expired)
	}
	if _, err := router.Dispatch(ctx, Command{Name: NameStatus, UserID: 99}); err != nil {
		t.Fatal(err)
	}
	limited, err := router.Dispatch(ctx, Command{Name: NameStatus, UserID: 99})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(limited, "Too many commands") {
		t.Fatalf("rate limited reply = %q", limited)
	}
}

func TestAuthorizeToleratesAMissingRecorder(t *testing.T) {
	t.Parallel()
	authorizer := NewOwnerAuthorizer(OwnerAuthorizerOptions{
		AllowedUserIDs: []int64{99},
	}, nil)
	if err := authorizer.Authorize(context.Background(), Command{
		Name: NameStatus, UserID: 1,
	}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("error = %v", err)
	}
}

type recordedRefusal struct {
	userID  int64
	command string
	reason  string
}

type fakeRefusalRecorder struct {
	refusals []recordedRefusal
}

func (fake *fakeRefusalRecorder) RecordCommandRefusal(
	userID int64,
	command, reason string,
) error {
	fake.refusals = append(fake.refusals, recordedRefusal{userID, command, reason})
	return nil
}
