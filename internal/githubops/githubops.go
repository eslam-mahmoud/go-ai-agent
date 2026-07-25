// Package githubops makes GitHub writes safe to repeat. Retries, restarts,
// and reconciliation runs converge on the same state instead of accumulating
// duplicate comments, redundant label writes, and repeated edits.
package githubops

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	githubclient "github.com/eslam-mahmoud/go-ai-agent/internal/github"
)

var ErrInvalidOperation = errors.New("invalid GitHub operation")

// Each operation depends only on what it actually calls, so a caller holding
// a partial client can still use the parts that apply to it.
type CommentClient interface {
	GetComments(
		ctx context.Context,
		owner, repo string,
		number int,
		since *time.Time,
	) ([]*githubclient.Comment, error)
	PostComment(
		ctx context.Context,
		owner, repo string,
		number int,
		body string,
	) (*githubclient.Comment, error)
}

type IssueReader interface {
	GetIssue(ctx context.Context, owner, repo string, number int) (*githubclient.Issue, error)
}

type LabelClient interface {
	IssueReader
	ReplaceLabels(ctx context.Context, owner, repo string, number int, labels []string) error
}

type CloseClient interface {
	IssueReader
	CloseIssue(ctx context.Context, owner, repo string, number int) error
}

// Result reports whether an operation wrote anything, and why it did not.
type Result struct {
	Performed bool
	Reason    string
}

func skipped(reason string) *Result { return &Result{Reason: reason} }

func performed() *Result { return &Result{Performed: true} }

// CommentMarker renders the hidden marker that identifies a comment Madar
// already posted. It is exported so callers can recognize their own comments.
func CommentMarker(key string) string {
	return "<!-- madar:op:" + strings.TrimSpace(key) + " -->"
}

// EnsureComment posts a comment once per idempotency key. The key travels in a
// hidden marker appended to the body, so the record of what was posted lives
// on the issue itself rather than in Madar's database.
func EnsureComment(
	ctx context.Context,
	client CommentClient,
	owner, repo string,
	number int,
	key, body string,
) (*Result, error) {
	if client == nil {
		return nil, fmt.Errorf("%w: comment client is required", ErrInvalidOperation)
	}
	if err := requireTarget(owner, repo, number); err != nil {
		return nil, err
	}
	trimmedKey := strings.TrimSpace(key)
	if trimmedKey == "" {
		// Without a key a repeat is indistinguishable from a new comment.
		return nil, fmt.Errorf("%w: comment idempotency key is required", ErrInvalidOperation)
	}
	if strings.TrimSpace(body) == "" {
		return nil, fmt.Errorf("%w: comment body is required", ErrInvalidOperation)
	}
	marker := CommentMarker(trimmedKey)
	existing, err := client.GetComments(ctx, owner, repo, number, nil)
	if err != nil {
		return nil, fmt.Errorf("read comments: %w", err)
	}
	for _, comment := range existing {
		if comment != nil && strings.Contains(comment.Body, marker) {
			return skipped("comment already posted"), nil
		}
	}
	if _, err := client.PostComment(
		ctx, owner, repo, number, strings.TrimRight(body, "\n")+"\n\n"+marker,
	); err != nil {
		return nil, fmt.Errorf("post comment: %w", err)
	}
	return performed(), nil
}

// EnsureLabels writes only when the issue's label set actually differs.
// Comparison ignores order and duplicates, since GitHub returns neither
// stably nor uniquely.
func EnsureLabels(
	ctx context.Context,
	client LabelClient,
	owner, repo string,
	number int,
	labels []string,
) (*Result, error) {
	if client == nil {
		return nil, fmt.Errorf("%w: label client is required", ErrInvalidOperation)
	}
	if err := requireTarget(owner, repo, number); err != nil {
		return nil, err
	}
	desired := normalizeLabels(labels)
	issue, err := client.GetIssue(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("read issue labels: %w", err)
	}
	if issue == nil {
		return nil, fmt.Errorf("%w: issue #%d was not found", ErrInvalidOperation, number)
	}
	if equalLabels(normalizeLabels(issue.Labels), desired) {
		return skipped("labels already match"), nil
	}
	if err := client.ReplaceLabels(ctx, owner, repo, number, desired); err != nil {
		return nil, fmt.Errorf("replace labels: %w", err)
	}
	return performed(), nil
}

// EnsureIssueClosed closes an issue only when it is open, so a reconciliation
// pass does not reopen-and-close or spam the issue timeline.
func EnsureIssueClosed(
	ctx context.Context,
	client CloseClient,
	owner, repo string,
	number int,
) (*Result, error) {
	if client == nil {
		return nil, fmt.Errorf("%w: close client is required", ErrInvalidOperation)
	}
	if err := requireTarget(owner, repo, number); err != nil {
		return nil, err
	}
	issue, err := client.GetIssue(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("read issue state: %w", err)
	}
	if issue == nil {
		return nil, fmt.Errorf("%w: issue #%d was not found", ErrInvalidOperation, number)
	}
	if strings.EqualFold(strings.TrimSpace(issue.State), "closed") {
		return skipped("issue already closed"), nil
	}
	if err := client.CloseIssue(ctx, owner, repo, number); err != nil {
		return nil, fmt.Errorf("close issue: %w", err)
	}
	return performed(), nil
}

func requireTarget(owner, repo string, number int) error {
	switch {
	case strings.TrimSpace(owner) == "" || strings.TrimSpace(repo) == "":
		return fmt.Errorf("%w: owner and repository are required", ErrInvalidOperation)
	case number <= 0:
		return fmt.Errorf("%w: issue number must be positive", ErrInvalidOperation)
	default:
		return nil
	}
}

// normalizeLabels sorts and de-duplicates so two spellings of the same set
// compare equal.
func normalizeLabels(labels []string) []string {
	seen := make(map[string]struct{}, len(labels))
	normalized := make([]string, 0, len(labels))
	for _, label := range labels {
		trimmed := strings.TrimSpace(label)
		if trimmed == "" {
			continue
		}
		if _, duplicate := seen[trimmed]; duplicate {
			continue
		}
		seen[trimmed] = struct{}{}
		normalized = append(normalized, trimmed)
	}
	sort.Strings(normalized)
	return normalized
}

func equalLabels(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
