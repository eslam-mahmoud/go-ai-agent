package github

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"

	gh "github.com/google/go-github/v66/github"
)

func TestSplitRepo(t *testing.T) {
	cases := []struct {
		input     string
		wantOwner string
		wantRepo  string
		wantErr   bool
	}{
		{"owner/repo", "owner", "repo", false},
		{"acme/project-a", "acme", "project-a", false},
		{"noslash", "", "", true},
		{"", "", "", true},
		{"/repo", "", "", true},
		{"owner/", "", "", true},
	}

	for _, tc := range cases {
		owner, repo, err := SplitRepo(tc.input)
		if tc.wantErr {
			if err == nil {
				t.Errorf("SplitRepo(%q) expected error, got nil", tc.input)
			}
			continue
		}
		if err != nil {
			t.Errorf("SplitRepo(%q) unexpected error: %v", tc.input, err)
			continue
		}
		if owner != tc.wantOwner {
			t.Errorf("SplitRepo(%q) owner = %q, want %q", tc.input, owner, tc.wantOwner)
		}
		if repo != tc.wantRepo {
			t.Errorf("SplitRepo(%q) repo = %q, want %q", tc.input, repo, tc.wantRepo)
		}
	}
}

func TestUpdateIssueBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/repos/owner/repo/issues/42" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		var request struct {
			Body *string `json:"body"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if request.Body == nil || *request.Body != "updated dashboard" {
			t.Errorf("request body = %#v", request.Body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"number": 42,
			"title": "Project",
			"body": "updated dashboard",
			"html_url": "https://github.com/owner/repo/issues/42"
		}`))
	}))
	defer server.Close()

	api := gh.NewClient(server.Client())
	api.BaseURL = mustURL(t, server.URL+"/")
	client := &githubClient{
		gh:  api,
		log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	}
	issue, err := client.UpdateIssueBody(
		context.Background(),
		"owner",
		"repo",
		42,
		"updated dashboard",
	)
	if err != nil {
		t.Fatal(err)
	}
	if issue.Number != 42 ||
		issue.Body != "updated dashboard" ||
		issue.HTMLURL != "https://github.com/owner/repo/issues/42" {
		t.Errorf("issue = %#v", issue)
	}
}

func mustURL(t *testing.T, value string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
