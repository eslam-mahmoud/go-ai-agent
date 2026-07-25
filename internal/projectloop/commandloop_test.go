package projectloop

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/command"
	"github.com/eslam-mahmoud/go-ai-agent/internal/telegram"
)

type fakeGateway struct {
	batches [][]telegram.Update
	offsets []int64
	replies []string
	err     error
}

func (fake *fakeGateway) GetUpdates(
	_ context.Context, offset int64,
) ([]telegram.Update, error) {
	fake.offsets = append(fake.offsets, offset)
	if fake.err != nil {
		return nil, fake.err
	}
	if len(fake.batches) == 0 {
		return nil, nil
	}
	batch := fake.batches[0]
	fake.batches = fake.batches[1:]
	return batch, nil
}

func (fake *fakeGateway) Reply(_ context.Context, _ int64, text string) error {
	fake.replies = append(fake.replies, text)
	return nil
}

// allowAll authorizes everything, so these tests exercise the loop rather than
// re-testing the authorizer.
type allowAll struct{}

func (allowAll) Authorize(context.Context, command.Command) error { return nil }

func newTestRouter(t *testing.T) *command.Router {
	t.Helper()
	router, err := command.NewRouter(allowAll{})
	if err != nil {
		t.Fatal(err)
	}
	if err := router.Register(
		command.NameStatus, "status",
		func(context.Context, command.Command) (string, error) { return "all good", nil },
	); err != nil {
		t.Fatal(err)
	}
	return router
}

func update(id int64, text string) telegram.Update {
	message := &telegram.Message{Text: text, Date: time.Now().Unix()}
	message.Chat.ID = 99
	message.From.ID = 42
	return telegram.Update{UpdateID: id, Message: message}
}

func TestPollDispatchesAKnownCommand(t *testing.T) {
	gateway := &fakeGateway{batches: [][]telegram.Update{{update(1, "/status")}}}
	loop, err := NewCommandLoop(gateway, newTestRouter(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := loop.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(gateway.replies) != 1 || gateway.replies[0] != "all good" {
		t.Fatalf("replies = %v", gateway.replies)
	}
}

// The offset must advance past every update read, including ones the loop does
// not act on. Otherwise one ordinary chat message wedges the loop by being
// fetched forever.
func TestOffsetAdvancesPastMessagesThatAreNotCommands(t *testing.T) {
	gateway := &fakeGateway{batches: [][]telegram.Update{
		{update(7, "just chatting"), update(8, "/status")},
		{},
	}}
	loop, err := NewCommandLoop(gateway, newTestRouter(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := loop.Poll(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(gateway.offsets) != 2 || gateway.offsets[1] != 9 {
		t.Fatalf("offsets = %v, want the second poll to resume at 9", gateway.offsets)
	}
	if len(gateway.replies) != 1 {
		t.Fatalf("replies = %v, want only the command answered", gateway.replies)
	}
}

// Expiry is measured against Telegram's own timestamp. Without it the
// command_max_age setting could never fire, which is how it behaved before the
// Date field existed.
func TestMessageTimestampReachesTheAuthorizer(t *testing.T) {
	var seen time.Time
	router, err := command.NewRouter(authorizerFunc(func(cmd command.Command) error {
		seen = cmd.SentAt
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := router.Register(command.NameStatus, "status",
		func(context.Context, command.Command) (string, error) { return "ok", nil },
	); err != nil {
		t.Fatal(err)
	}
	sent := time.Now().Add(-time.Hour).Truncate(time.Second)
	message := &telegram.Message{Text: "/status", Date: sent.Unix()}
	message.Chat.ID = 99
	message.From.ID = 42
	gateway := &fakeGateway{batches: [][]telegram.Update{
		{{UpdateID: 1, Message: message}},
	}}
	loop, err := NewCommandLoop(gateway, router, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := loop.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !seen.Equal(sent.UTC()) {
		t.Fatalf("SentAt = %v, want %v", seen, sent.UTC())
	}
}

type authorizerFunc func(command.Command) error

func (fn authorizerFunc) Authorize(_ context.Context, cmd command.Command) error {
	return fn(cmd)
}

// Telegram being briefly unreachable must not end the command surface for the
// life of the process.
func TestRunKeepsPollingAfterAFailure(t *testing.T) {
	gateway := &fakeGateway{err: errors.New("telegram unreachable")}
	loop, err := NewCommandLoop(gateway, newTestRouter(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	if err := loop.Run(ctx, 10*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if len(gateway.offsets) < 2 {
		t.Fatalf("polls = %d, want the loop to retry", len(gateway.offsets))
	}
}

func TestNewCommandLoopRequiresItsCollaborators(t *testing.T) {
	if _, err := NewCommandLoop(nil, newTestRouter(t), nil); err == nil {
		t.Fatal("expected a missing gateway to be reported")
	}
	if _, err := NewCommandLoop(&fakeGateway{}, nil, nil); err == nil {
		t.Fatal("expected a missing router to be reported")
	}
}
