package githubops

import (
	"context"
	"errors"
	"testing"
	"time"

	githubclient "github.com/eslam-mahmoud/go-ai-agent/internal/github"
)

func TestDiscoverPullRequestResolvesABranch(t *testing.T) {
	t.Parallel()
	earlier := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	later := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name          string
		pulls         []*githubclient.PullRequest
		wantNumber    int
		wantAmbiguous bool
	}{
		{"no pull requests", nil, 0, false},
		{
			"one open",
			[]*githubclient.PullRequest{
				{Number: 5, State: "open", HeadBranch: "madar/issue-1"},
			},
			5, false,
		},
		{
			// Branches get reused, so a closed predecessor is history.
			"one open beside closed ones",
			[]*githubclient.PullRequest{
				{Number: 9, State: "open", HeadBranch: "madar/issue-1"},
				{Number: 4, State: "closed", HeadBranch: "madar/issue-1"},
				{Number: 2, State: "closed", Merged: true, HeadBranch: "madar/issue-1", MergedAt: &earlier},
			},
			9, false,
		},
		{
			"more than one open is ambiguous",
			[]*githubclient.PullRequest{
				{Number: 9, State: "open", HeadBranch: "madar/issue-1"},
				{Number: 11, State: "open", HeadBranch: "madar/issue-1"},
			},
			0, true,
		},
		{
			"merged only resolves to the most recent merge",
			[]*githubclient.PullRequest{
				{Number: 3, State: "closed", Merged: true, HeadBranch: "madar/issue-1", MergedAt: &earlier},
				{Number: 7, State: "closed", Merged: true, HeadBranch: "madar/issue-1", MergedAt: &later},
			},
			7, false,
		},
		{
			"closed without a merge resolves to nothing",
			[]*githubclient.PullRequest{
				{Number: 3, State: "closed", HeadBranch: "madar/issue-1"},
			},
			0, false,
		},
		{
			"a draft is still the current pull request",
			[]*githubclient.PullRequest{
				{Number: 6, State: "open", Draft: true, HeadBranch: "madar/issue-1"},
			},
			6, false,
		},
		{
			// A mismatched head would bind work to the wrong review.
			"a mismatched head branch is ignored",
			[]*githubclient.PullRequest{
				{Number: 8, State: "open", HeadBranch: "someone-else/branch"},
			},
			0, false,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			client := &fakePullRequestClient{pulls: test.pulls}
			match, err := DiscoverPullRequest(
				context.Background(), client, "owner", "repo", "madar/issue-1",
			)
			if err != nil {
				t.Fatalf("DiscoverPullRequest: %v", err)
			}
			if match.Ambiguous != test.wantAmbiguous {
				t.Fatalf("ambiguous = %v, want %v", match.Ambiguous, test.wantAmbiguous)
			}
			if test.wantNumber == 0 {
				if match.Current != nil {
					t.Fatalf("current = %#v, want none", match.Current)
				}
				return
			}
			if match.Current == nil || match.Current.Number != test.wantNumber {
				t.Fatalf("current = %#v, want #%d", match.Current, test.wantNumber)
			}
		})
	}
}

func TestDiscoverPullRequestKeepsTheFullMatchSet(t *testing.T) {
	t.Parallel()
	client := &fakePullRequestClient{pulls: []*githubclient.PullRequest{
		{Number: 9, State: "open", HeadBranch: "madar/issue-1"},
		{Number: 4, State: "closed", HeadBranch: "madar/issue-1"},
		{Number: 8, State: "open", HeadBranch: "other/branch"},
	}}
	match, err := DiscoverPullRequest(
		context.Background(), client, "owner", "repo", "madar/issue-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	// The mismatched head is excluded from the match set entirely.
	if len(match.Matches) != 2 {
		t.Fatalf("matches = %#v", match.Matches)
	}
}

func TestDiscoverPullRequestValidatesInputAndPropagatesFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client := &fakePullRequestClient{}
	if _, err := DiscoverPullRequest(ctx, nil, "owner", "repo", "branch"); !errors.Is(
		err, ErrInvalidOperation,
	) {
		t.Fatalf("nil client error = %v", err)
	}
	for _, args := range [][3]string{
		{"", "repo", "branch"},
		{"owner", "", "branch"},
		{"owner", "repo", "  "},
	} {
		if _, err := DiscoverPullRequest(
			ctx, client, args[0], args[1], args[2],
		); !errors.Is(err, ErrInvalidOperation) {
			t.Fatalf("args %v error = %v", args, err)
		}
	}
	failure := errors.New("github is unavailable")
	if _, err := DiscoverPullRequest(
		ctx, &fakePullRequestClient{err: failure}, "owner", "repo", "branch",
	); !errors.Is(err, failure) {
		t.Fatalf("client failure = %v", err)
	}
}

type fakePullRequestClient struct {
	pulls []*githubclient.PullRequest
	err   error
}

func (fake *fakePullRequestClient) ListPullRequestsForBranch(
	context.Context,
	string, string, string,
) ([]*githubclient.PullRequest, error) {
	if fake.err != nil {
		return nil, fake.err
	}
	return fake.pulls, nil
}
