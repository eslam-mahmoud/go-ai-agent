package github

import (
	"context"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v66/github"
	"golang.org/x/oauth2"
)

type Issue struct {
	Number  int
	Title   string
	Body    string
	HTMLURL string
	Labels  []string
}

type Comment struct {
	ID        int64
	Body      string
	Author    string
	CreatedAt time.Time
	HTMLURL   string
}

type Client interface {
	GetAuthenticatedUsername(ctx context.Context) (string, error)
	ListReadyIssues(ctx context.Context, owner, repo, readyLabel string) ([]*Issue, error)
	GetIssue(ctx context.Context, owner, repo string, number int) (*Issue, error)
	GetComments(ctx context.Context, owner, repo string, number int, since *time.Time) ([]*Comment, error)
	PostComment(ctx context.Context, owner, repo string, number int, body string) (*Comment, error)
	AddLabel(ctx context.Context, owner, repo string, number int, label string) error
	RemoveLabel(ctx context.Context, owner, repo string, number int, label string) error
	ReplaceLabels(ctx context.Context, owner, repo string, number int, labels []string) error
	CreateLabel(ctx context.Context, owner, repo, name, color string) error
	EnsureLabels(ctx context.Context, owner, repo string, labels map[string]string) error
	GetCheckSuiteStatus(ctx context.Context, owner, repo, branch string) (CheckStatus, error)
	GetFailedStepOutput(ctx context.Context, owner, repo, branch string) (string, error)
	CloseIssue(ctx context.Context, owner, repo string, number int) error
}

type githubClient struct {
	gh *gh.Client
}

func New(token string) Client {
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	tc := oauth2.NewClient(context.Background(), ts)
	return &githubClient{gh: gh.NewClient(tc)}
}

func (c *githubClient) GetAuthenticatedUsername(ctx context.Context) (string, error) {
	user, _, err := c.gh.Users.Get(ctx, "")
	if err != nil {
		return "", fmt.Errorf("get authenticated user: %w", err)
	}
	return user.GetLogin(), nil
}

func (c *githubClient) ListReadyIssues(ctx context.Context, owner, repo, readyLabel string) ([]*Issue, error) {
	opts := &gh.IssueListByRepoOptions{
		State:  "open",
		Labels: []string{readyLabel},
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	var issues []*Issue
	for {
		ghIssues, resp, err := c.gh.Issues.ListByRepo(ctx, owner, repo, opts)
		if err != nil {
			return nil, fmt.Errorf("list issues: %w", err)
		}
		for _, i := range ghIssues {
			if i.PullRequestLinks != nil {
				continue // skip PRs
			}
			issues = append(issues, toIssue(i))
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return issues, nil
}

func (c *githubClient) GetIssue(ctx context.Context, owner, repo string, number int) (*Issue, error) {
	i, _, err := c.gh.Issues.Get(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("get issue %d: %w", number, err)
	}
	return toIssue(i), nil
}

func (c *githubClient) GetComments(ctx context.Context, owner, repo string, number int, since *time.Time) ([]*Comment, error) {
	opts := &gh.IssueListCommentsOptions{
		Sort:      gh.String("created"),
		Direction: gh.String("asc"),
		Since:     since,
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	var comments []*Comment
	for {
		ghComments, resp, err := c.gh.Issues.ListComments(ctx, owner, repo, number, opts)
		if err != nil {
			return nil, fmt.Errorf("list comments: %w", err)
		}
		for _, c := range ghComments {
			comments = append(comments, &Comment{
				ID:        c.GetID(),
				Body:      c.GetBody(),
				Author:    c.GetUser().GetLogin(),
				CreatedAt: c.GetCreatedAt().Time,
				HTMLURL:   c.GetHTMLURL(),
			})
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return comments, nil
}

func (c *githubClient) PostComment(ctx context.Context, owner, repo string, number int, body string) (*Comment, error) {
	cmt, _, err := c.gh.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{
		Body: gh.String(body),
	})
	if err != nil {
		return nil, fmt.Errorf("post comment: %w", err)
	}
	return &Comment{
		ID:        cmt.GetID(),
		Body:      cmt.GetBody(),
		Author:    cmt.GetUser().GetLogin(),
		CreatedAt: cmt.GetCreatedAt().Time,
		HTMLURL:   cmt.GetHTMLURL(),
	}, nil
}

func (c *githubClient) AddLabel(ctx context.Context, owner, repo string, number int, label string) error {
	_, _, err := c.gh.Issues.AddLabelsToIssue(ctx, owner, repo, number, []string{label})
	return err
}

func (c *githubClient) RemoveLabel(ctx context.Context, owner, repo string, number int, label string) error {
	_, err := c.gh.Issues.RemoveLabelForIssue(ctx, owner, repo, number, label)
	if err != nil {
		// Ignore "label not found" errors
		if strings.Contains(err.Error(), "404") {
			return nil
		}
		return err
	}
	return nil
}

func (c *githubClient) ReplaceLabels(ctx context.Context, owner, repo string, number int, labels []string) error {
	_, _, err := c.gh.Issues.ReplaceLabelsForIssue(ctx, owner, repo, number, labels)
	return err
}

func (c *githubClient) CreateLabel(ctx context.Context, owner, repo, name, color string) error {
	_, _, err := c.gh.Issues.CreateLabel(ctx, owner, repo, &gh.Label{
		Name:  gh.String(name),
		Color: gh.String(color),
	})
	return err
}

func (c *githubClient) CloseIssue(ctx context.Context, owner, repo string, number int) error {
	closed := "closed"
	_, _, err := c.gh.Issues.Edit(ctx, owner, repo, number, &gh.IssueRequest{State: &closed})
	return err
}

func (c *githubClient) EnsureLabels(ctx context.Context, owner, repo string, labels map[string]string) error {
	existingSet := make(map[string]bool)
	opts := &gh.ListOptions{PerPage: 100}
	for {
		page, resp, err := c.gh.Issues.ListLabels(ctx, owner, repo, opts)
		if err != nil {
			return fmt.Errorf("list labels: %w", err)
		}
		for _, l := range page {
			existingSet[l.GetName()] = true
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	for name, color := range labels {
		if !existingSet[name] {
			if err := c.CreateLabel(ctx, owner, repo, name, color); err != nil {
				return fmt.Errorf("create label %q: %w", name, err)
			}
		}
	}
	return nil
}

func toIssue(i *gh.Issue) *Issue {
	issue := &Issue{
		Number:  i.GetNumber(),
		Title:   i.GetTitle(),
		Body:    i.GetBody(),
		HTMLURL: i.GetHTMLURL(),
	}
	for _, l := range i.Labels {
		issue.Labels = append(issue.Labels, l.GetName())
	}
	return issue
}

func SplitRepo(fullRepo string) (owner, repo string, err error) {
	parts := strings.SplitN(fullRepo, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid repo format %q, expected owner/repo", fullRepo)
	}
	return parts[0], parts[1], nil
}
