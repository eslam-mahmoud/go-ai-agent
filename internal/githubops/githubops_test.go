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
	operations := newOperations(t, client)

	result, err := operations.EnsureComment(
		context.Background(), "owner", "repo", 7, "task:1:completed", "All done.",
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
	repeat, err := operations.EnsureComment(
		context.Background(), "owner", "repo", 7, "task:1:completed", "Different text.",
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
	other, err := operations.EnsureComment(
		context.Background(), "owner", "repo", 7, "task:1:blocked", "Now blocked.",
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
	operations := newOperations(t, client)

	// Same set, different order, with a duplicate and padding.
	same, err := operations.EnsureLabels(
		context.Background(), "owner", "repo", 7,
		[]string{"type:bug", " madar:developing ", "type:bug", ""},
	)
	if err != nil {
		t.Fatalf("EnsureLabels: %v", err)
	}
	if same.Performed || client.labelWrites != 0 {
		t.Fatalf("same set wrote labels: %#v, writes = %d", same, client.labelWrites)
	}

	changed, err := operations.EnsureLabels(
		context.Background(), "owner", "repo", 7,
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

func TestEnsureIssueBodyWritesOnlyOnChange(t *testing.T) {
	t.Parallel()
	client := &fakeClient{issue: &githubclient.Issue{
		Number: 7,
		State:  "open",
		Body:   "current body",
	}}
	operations := newOperations(t, client)

	same, err := operations.EnsureIssueBody(
		context.Background(), "owner", "repo", 7, "current body",
	)
	if err != nil {
		t.Fatalf("EnsureIssueBody: %v", err)
	}
	if same.Performed || client.bodyWrites != 0 {
		t.Fatalf("unchanged body wrote: %#v", same)
	}

	changed, err := operations.EnsureIssueBody(
		context.Background(), "owner", "repo", 7, "new body",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !changed.Performed || client.bodyWrites != 1 {
		t.Fatalf("changed = %#v, writes = %d", changed, client.bodyWrites)
	}
}

func TestEnsureIssueClosedSkipsAnAlreadyClosedIssue(t *testing.T) {
	t.Parallel()
	client := &fakeClient{issue: &githubclient.Issue{Number: 7, State: "open"}}
	operations := newOperations(t, client)

	closed, err := operations.EnsureIssueClosed(context.Background(), "owner", "repo", 7)
	if err != nil {
		t.Fatalf("EnsureIssueClosed: %v", err)
	}
	if !closed.Performed || client.closes != 1 {
		t.Fatalf("close = %#v, closes = %d", closed, client.closes)
	}

	client.issue.State = "CLOSED"
	repeat, err := operations.EnsureIssueClosed(context.Background(), "owner", "repo", 7)
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
	operations := newOperations(t, client)
	ctx := context.Background()

	if _, err := operations.EnsureComment(ctx, "owner", "repo", 7, "  ", "body"); !errors.Is(
		err, ErrInvalidOperation,
	) {
		t.Fatalf("missing key error = %v", err)
	}
	if _, err := operations.EnsureComment(ctx, "owner", "repo", 7, "key", "   "); !errors.Is(
		err, ErrInvalidOperation,
	) {
		t.Fatalf("blank body error = %v", err)
	}
	for _, target := range [][2]string{{"", "repo"}, {"owner", ""}} {
		if _, err := operations.EnsureLabels(
			ctx, target[0], target[1], 7, nil,
		); !errors.Is(err, ErrInvalidOperation) {
			t.Fatalf("target %v error = %v", target, err)
		}
	}
	if _, err := operations.EnsureIssueClosed(ctx, "owner", "repo", 0); !errors.Is(
		err, ErrInvalidOperation,
	) {
		t.Fatalf("zero issue error = %v", err)
	}
	if client.posted+client.labelWrites+client.bodyWrites+client.closes != 0 {
		t.Fatal("a rejected operation still wrote")
	}
	if _, err := New(nil); err == nil {
		t.Fatal("nil client accepted")
	}

	// A missing issue is an error, not a silent skip.
	missing := newOperations(t, &fakeClient{})
	if _, err := missing.EnsureIssueClosed(ctx, "owner", "repo", 7); !errors.Is(
		err, ErrInvalidOperation,
	) {
		t.Fatalf("missing issue error = %v", err)
	}
}

func TestOperationsPropagateClientFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	failure := errors.New("github is unavailable")

	readFailure := newOperations(t, &fakeClient{getErr: failure})
	if _, err := readFailure.EnsureLabels(ctx, "owner", "repo", 7, nil); !errors.Is(
		err, failure,
	) {
		t.Fatalf("read failure = %v", err)
	}

	commentFailure := newOperations(t, &fakeClient{
		issue:      &githubclient.Issue{Number: 7, State: "open"},
		commentErr: failure,
	})
	if _, err := commentFailure.EnsureComment(
		ctx, "owner", "repo", 7, "key", "body",
	); !errors.Is(err, failure) {
		t.Fatalf("comment failure = %v", err)
	}

	writeFailure := newOperations(t, &fakeClient{
		issue:    &githubclient.Issue{Number: 7, State: "open"},
		writeErr: failure,
	})
	if _, err := writeFailure.EnsureLabels(
		ctx, "owner", "repo", 7, []string{"type:bug"},
	); !errors.Is(err, failure) {
		t.Fatalf("label write failure = %v", err)
	}
	if _, err := writeFailure.EnsureIssueClosed(ctx, "owner", "repo", 7); !errors.Is(
		err, failure,
	) {
		t.Fatalf("close failure = %v", err)
	}
}

func newOperations(t *testing.T, client Client) *Operations {
	t.Helper()
	operations, err := New(client)
	if err != nil {
		t.Fatal(err)
	}
	return operations
}

type fakeClient struct {
	issue       *githubclient.Issue
	comments    []*githubclient.Comment
	posted      int
	labelWrites int
	bodyWrites  int
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

func (fake *fakeClient) UpdateIssueBody(
	_ context.Context,
	_, _ string,
	_ int,
	body string,
) (*githubclient.Issue, error) {
	if fake.writeErr != nil {
		return nil, fake.writeErr
	}
	fake.bodyWrites++
	if fake.issue != nil {
		fake.issue.Body = body
	}
	return fake.issue, nil
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
