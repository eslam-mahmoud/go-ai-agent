package githubops

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	githubclient "github.com/eslam-mahmoud/go-ai-agent/internal/github"
)

func TestEnsureCommentPostsOncePerKey(t *testing.T) {
	t.Parallel()
	client := &fakeClient{issue: &githubclient.Issue{Number: 7, State: "open"}}

	result, err := EnsureComment(
		context.Background(), client, "owner", "repo", 7, "task:1:completed", "All done.",
	)
	if err != nil {
		t.Fatalf("EnsureComment: %v", err)
	}
	if !result.Performed || client.posted != 1 {
		t.Fatalf("result = %#v, posted = %d", result, client.posted)
	}
	if !strings.Contains(client.lastComment, CommentMarker("task:1:completed")) {
		t.Fatalf("comment carries no marker: %q", client.lastComment)
	}
	if !strings.HasPrefix(client.lastComment, "All done.") {
		t.Fatalf("comment body = %q", client.lastComment)
	}

	// The same key is a no-op, even though the visible body differs.
	repeat, err := EnsureComment(
		context.Background(), client, "owner", "repo", 7, "task:1:completed", "Different text.",
	)
	if err != nil {
		t.Fatalf("repeat EnsureComment: %v", err)
	}
	if repeat.Performed || client.posted != 1 {
		t.Fatalf("repeat = %#v, posted = %d", repeat, client.posted)
	}
	if repeat.Reason == "" {
		t.Fatal("skip has no reason")
	}

	// A different key posts again.
	other, err := EnsureComment(
		context.Background(), client, "owner", "repo", 7, "task:1:blocked", "Now blocked.",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !other.Performed || client.posted != 2 {
		t.Fatalf("other = %#v, posted = %d", other, client.posted)
	}
}

func TestEnsureLabelsWritesOnlyOnRealDifference(t *testing.T) {
	t.Parallel()
	client := &fakeClient{issue: &githubclient.Issue{
		Number: 7,
		State:  "open",
		Labels: []string{"madar:developing", "type:bug"},
	}}

	// Same set, different order, with a duplicate and padding.
	same, err := EnsureLabels(
		context.Background(), client, "owner", "repo", 7,
		[]string{"type:bug", " madar:developing ", "type:bug", ""},
	)
	if err != nil {
		t.Fatalf("EnsureLabels: %v", err)
	}
	if same.Performed || client.labelWrites != 0 {
		t.Fatalf("same set wrote labels: %#v, writes = %d", same, client.labelWrites)
	}

	changed, err := EnsureLabels(
		context.Background(), client, "owner", "repo", 7,
		[]string{"madar:reviewing", "type:bug"},
	)
	if err != nil {
		t.Fatalf("EnsureLabels: %v", err)
	}
	if !changed.Performed || client.labelWrites != 1 {
		t.Fatalf("changed = %#v, writes = %d", changed, client.labelWrites)
	}
	// The written set is normalized.
	if len(client.lastLabels) != 2 ||
		client.lastLabels[0] != "madar:reviewing" ||
		client.lastLabels[1] != "type:bug" {
		t.Fatalf("written labels = %v", client.lastLabels)
	}
}

func TestEnsureIssueClosedSkipsAnAlreadyClosedIssue(t *testing.T) {
	t.Parallel()
	client := &fakeClient{issue: &githubclient.Issue{Number: 7, State: "open"}}

	closed, err := EnsureIssueClosed(context.Background(), client, "owner", "repo", 7)
	if err != nil {
		t.Fatalf("EnsureIssueClosed: %v", err)
	}
	if !closed.Performed || client.closes != 1 {
		t.Fatalf("close = %#v, closes = %d", closed, client.closes)
	}

	client.issue.State = "CLOSED"
	repeat, err := EnsureIssueClosed(context.Background(), client, "owner", "repo", 7)
	if err != nil {
		t.Fatal(err)
	}
	if repeat.Performed || client.closes != 1 {
		t.Fatalf("repeat = %#v, closes = %d", repeat, client.closes)
	}
}

func TestOperationsValidateTheirInputs(t *testing.T) {
	t.Parallel()
	client := &fakeClient{issue: &githubclient.Issue{Number: 7, State: "open"}}
	ctx := context.Background()

	if _, err := EnsureComment(ctx, client, "owner", "repo", 7, "  ", "body"); !errors.Is(
		err, ErrInvalidOperation,
	) {
		t.Fatalf("missing key error = %v", err)
	}
	if _, err := EnsureComment(ctx, client, "owner", "repo", 7, "key", "   "); !errors.Is(
		err, ErrInvalidOperation,
	) {
		t.Fatalf("blank body error = %v", err)
	}
	for _, target := range [][2]string{{"", "repo"}, {"owner", ""}} {
		if _, err := EnsureLabels(
			ctx, client, target[0], target[1], 7, nil,
		); !errors.Is(err, ErrInvalidOperation) {
			t.Fatalf("target %v error = %v", target, err)
		}
	}
	if _, err := EnsureIssueClosed(ctx, client, "owner", "repo", 0); !errors.Is(
		err, ErrInvalidOperation,
	) {
		t.Fatalf("zero issue error = %v", err)
	}
	if client.posted+client.labelWrites+client.closes != 0 {
		t.Fatal("a rejected operation still wrote")
	}

	// A missing issue is an error, not a silent skip.
	if _, err := EnsureIssueClosed(
		ctx, &fakeClient{}, "owner", "repo", 7,
	); !errors.Is(err, ErrInvalidOperation) {
		t.Fatalf("missing issue error = %v", err)
	}
	// A nil client is rejected rather than panicking.
	if _, err := EnsureComment(ctx, nil, "owner", "repo", 7, "key", "body"); !errors.Is(
		err, ErrInvalidOperation,
	) {
		t.Fatalf("nil client error = %v", err)
	}
}

func TestOperationsPropagateClientFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	failure := errors.New("github is unavailable")

	readFailure := &fakeClient{getErr: failure}
	if _, err := EnsureLabels(ctx, readFailure, "owner", "repo", 7, nil); !errors.Is(
		err, failure,
	) {
		t.Fatalf("read failure = %v", err)
	}

	commentFailure := &fakeClient{
		issue:      &githubclient.Issue{Number: 7, State: "open"},
		commentErr: failure,
	}
	if _, err := EnsureComment(
		ctx, commentFailure, "owner", "repo", 7, "key", "body",
	); !errors.Is(err, failure) {
		t.Fatalf("comment failure = %v", err)
	}

	writeFailure := &fakeClient{
		issue:    &githubclient.Issue{Number: 7, State: "open"},
		writeErr: failure,
	}
	if _, err := EnsureLabels(
		ctx, writeFailure, "owner", "repo", 7, []string{"type:bug"},
	); !errors.Is(err, failure) {
		t.Fatalf("label write failure = %v", err)
	}
	if _, err := EnsureIssueClosed(ctx, writeFailure, "owner", "repo", 7); !errors.Is(
		err, failure,
	) {
		t.Fatalf("close failure = %v", err)
	}
}

type fakeClient struct {
	issue       *githubclient.Issue
	comments    []*githubclient.Comment
	posted      int
	labelWrites int
	closes      int
	lastComment string
	lastLabels  []string
	getErr      error
	commentErr  error
	writeErr    error
}

func (fake *fakeClient) GetIssue(
	context.Context, string, string, int,
) (*githubclient.Issue, error) {
	if fake.getErr != nil {
		return nil, fake.getErr
	}
	return fake.issue, nil
}

func (fake *fakeClient) GetComments(
	context.Context, string, string, int, *time.Time,
) ([]*githubclient.Comment, error) {
	if fake.getErr != nil {
		return nil, fake.getErr
	}
	return fake.comments, nil
}

func (fake *fakeClient) PostComment(
	_ context.Context,
	_, _ string,
	_ int,
	body string,
) (*githubclient.Comment, error) {
	if fake.commentErr != nil {
		return nil, fake.commentErr
	}
	fake.posted++
	fake.lastComment = body
	fake.comments = append(fake.comments, &githubclient.Comment{Body: body})
	return &githubclient.Comment{Body: body}, nil
}

func (fake *fakeClient) ReplaceLabels(
	_ context.Context,
	_, _ string,
	_ int,
	labels []string,
) error {
	if fake.writeErr != nil {
		return fake.writeErr
	}
	fake.labelWrites++
	fake.lastLabels = labels
	if fake.issue != nil {
		fake.issue.Labels = labels
	}
	return nil
}

func (fake *fakeClient) CloseIssue(context.Context, string, string, int) error {
	if fake.writeErr != nil {
		return fake.writeErr
	}
	fake.closes++
	if fake.issue != nil {
		fake.issue.State = "closed"
	}
	return nil
}
