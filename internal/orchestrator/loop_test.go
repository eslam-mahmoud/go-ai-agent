package orchestrator

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/claude"
	"github.com/eslam-mahmoud/go-ai-agent/internal/config"
	githubclient "github.com/eslam-mahmoud/go-ai-agent/internal/github"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
	"github.com/eslam-mahmoud/go-ai-agent/internal/telegram"
	"log/slog"
	"os"
)

// --- fakes ---

type fakeGitHub struct {
	issues       []*githubclient.Issue
	comments     []*githubclient.Comment
	postedComment string
	labelsSet    []string
}

func (f *fakeGitHub) ListReadyIssues(_ context.Context, _, _, _ string) ([]*githubclient.Issue, error) {
	return f.issues, nil
}
func (f *fakeGitHub) GetIssue(_ context.Context, _, _ string, number int) (*githubclient.Issue, error) {
	for _, i := range f.issues {
		if i.Number == number {
			return i, nil
		}
	}
	return &githubclient.Issue{Number: number, Labels: f.labelsSet}, nil
}
func (f *fakeGitHub) GetComments(_ context.Context, _, _ string, _ int, _ *time.Time) ([]*githubclient.Comment, error) {
	return f.comments, nil
}
func (f *fakeGitHub) PostComment(_ context.Context, _, _ string, _ int, body string) (*githubclient.Comment, error) {
	f.postedComment = body
	return &githubclient.Comment{HTMLURL: "https://github.com/x/y/issues/1#comment-1"}, nil
}
func (f *fakeGitHub) AddLabel(_ context.Context, _, _ string, _ int, label string) error {
	return nil
}
func (f *fakeGitHub) RemoveLabel(_ context.Context, _, _ string, _ int, label string) error {
	return nil
}
func (f *fakeGitHub) ReplaceLabels(_ context.Context, _, _ string, _ int, labels []string) error {
	f.labelsSet = labels
	return nil
}
func (f *fakeGitHub) CreateLabel(_ context.Context, _, _, _, _ string) error { return nil }
func (f *fakeGitHub) EnsureLabels(_ context.Context, _, _ string, _ map[string]string) error {
	return nil
}
func (f *fakeGitHub) GetCheckSuiteStatus(_ context.Context, _, _, _ string) (githubclient.CheckStatus, error) {
	return githubclient.CheckSuccess, nil
}
func (f *fakeGitHub) GetFailedStepOutput(_ context.Context, _, _, _ string) (string, error) {
	return "", nil
}
func (f *fakeGitHub) CloseIssue(_ context.Context, _, _ string, _ int) error { return nil }

type fakeRunner struct {
	result *claude.Result
	err    error
}

func (f *fakeRunner) Run(_ context.Context, _ claude.RunOptions) (*claude.Result, error) {
	return f.result, f.err
}

type fakeTelegram struct {
	clarificationCalled bool
	completionCalled    bool
	errorCalled         bool
}

func (f *fakeTelegram) NotifyClarification(_ context.Context, _, _ string) error {
	f.clarificationCalled = true
	return nil
}
func (f *fakeTelegram) NotifyCompletion(_ context.Context, _, _ string) error {
	f.completionCalled = true
	return nil
}
func (f *fakeTelegram) NotifyError(_ context.Context, _ string, _ error) error {
	f.errorCalled = true
	return nil
}

// --- helpers ---

func testConfig() *config.Config {
	return &config.Config{
		PollInterval: time.Second,
		Concurrency:  config.ConcurrencyConfig{Enabled: false, MaxParallel: 1},
		Labels: config.LabelsConfig{
			Ready:            "ready",
			InProgress:       "in-progress",
			AwaitingFeedback: "awaiting-feedback",
			Done:             "done",
		},
		Repos:        []string{"owner/repo"},
		ContextDir:   ".claude-context",
		WorkspaceDir: "/tmp/madar/workspaces",
		Claude: config.ClaudeConfig{
			MaxTurns:   40,
			RunTimeout: 30 * time.Minute,
		},
	}
}

func testStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func testLoop(t *testing.T, gh githubclient.Client, runner claude.Runner, tg telegram.Gateway, s *store.Store) *Loop {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return New(testConfig(), gh, runner, tg, s, log)
}

// --- tests ---

func TestTick_noReadyIssues(t *testing.T) {
	gh := &fakeGitHub{}
	runner := &fakeRunner{result: &claude.Result{Output: "done"}}
	tg := &fakeTelegram{}
	s := testStore(t)

	loop := testLoop(t, gh, runner, tg, s)
	if err := loop.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	// Nothing should have been claimed.
	count, _ := s.CountActive()
	if count != 0 {
		t.Errorf("active count = %d, want 0", count)
	}
}

func TestTick_claimsAndCompletes(t *testing.T) {
	gh := &fakeGitHub{
		issues: []*githubclient.Issue{
			{Number: 1, Title: "Fix bug", Body: "details", HTMLURL: "https://github.com/owner/repo/issues/1", Labels: []string{"ready"}},
		},
	}
	runner := &fakeRunner{result: &claude.Result{
		SessionID: "sess-test",
		Output:    "Fixed the bug by updating X.",
		IsError:   false,
		NumTurns:  3,
	}}
	tg := &fakeTelegram{}
	s := testStore(t)

	loop := testLoop(t, gh, runner, tg, s)
	if err := loop.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	// Task should now be done.
	task, err := s.GetTask("owner/repo", 1)
	if err != nil || task == nil {
		t.Fatalf("GetTask: %v, %v", err, task)
	}
	if task.State != store.StateDone {
		t.Errorf("task state = %q, want done", task.State)
	}

	// Telegram completion should have been called.
	if !tg.completionCalled {
		t.Error("NotifyCompletion should have been called")
	}

	// A completion comment should have been posted.
	if gh.postedComment == "" {
		t.Error("expected a comment to be posted on the issue")
	}
	if !containsStr(gh.postedComment, "Fixed the bug") {
		t.Errorf("comment should contain output, got: %q", gh.postedComment)
	}
}

func TestTick_handlesClarification(t *testing.T) {
	gh := &fakeGitHub{
		issues: []*githubclient.Issue{
			{Number: 2, Title: "Add feature", Body: "vague", HTMLURL: "https://github.com/owner/repo/issues/2", Labels: []string{"ready"}},
		},
	}
	runner := &fakeRunner{result: &claude.Result{
		SessionID:  "sess-clarify",
		Output:     "NEEDS_CLARIFICATION: Should I use A or B?",
		NeedsInput: true,
		Question:   "Should I use A or B?",
	}}
	tg := &fakeTelegram{}
	s := testStore(t)

	loop := testLoop(t, gh, runner, tg, s)
	if err := loop.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	task, _ := s.GetTask("owner/repo", 2)
	if task == nil {
		t.Fatal("task not found")
	}
	if task.State != store.StateAwaitingFeedback {
		t.Errorf("task state = %q, want awaiting-feedback", task.State)
	}
	if !tg.clarificationCalled {
		t.Error("NotifyClarification should have been called")
	}
	if !containsStr(gh.postedComment, "Should I use A or B?") {
		t.Errorf("comment should contain question, got: %q", gh.postedComment)
	}
}

func TestTick_capacityGuard(t *testing.T) {
	gh := &fakeGitHub{
		issues: []*githubclient.Issue{
			{Number: 3, Title: "Task", HTMLURL: "url", Labels: []string{"ready"}},
		},
	}
	runner := &fakeRunner{result: &claude.Result{Output: "done"}}
	tg := &fakeTelegram{}
	s := testStore(t)

	// Pre-load an active task so the guard kicks in.
	_, _ = s.UpsertTask("owner/repo", 99, store.StateInProgress, "existing-session")

	loop := testLoop(t, gh, runner, tg, s)
	if err := loop.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	// Issue #3 should NOT have been claimed.
	task, _ := s.GetTask("owner/repo", 3)
	if task != nil {
		t.Errorf("issue 3 should not have been claimed while another is active, state=%q", task.State)
	}
}

func TestCheckAwaitingFeedback_resumes(t *testing.T) {
	clarTime := time.Now().UTC().Add(-10 * time.Minute)
	gh := &fakeGitHub{
		comments: []*githubclient.Comment{
			{
				ID:        1,
				Body:      "Use per-IP, 5/min",
				Author:    "human-user",
				CreatedAt: clarTime.Add(5 * time.Minute),
			},
		},
	}
	runner := &fakeRunner{result: &claude.Result{
		Output:   "Implemented per-IP rate limiting at 5/min.",
		IsError:  false,
		NumTurns: 2,
	}}
	tg := &fakeTelegram{}
	s := testStore(t)

	// Set up a task in awaiting-feedback state.
	_, _ = s.UpsertTask("owner/repo", 10, store.StateAwaitingFeedback, "sess-abc")
	_ = s.SetClarificationTime("owner/repo", 10, clarTime)

	loop := testLoop(t, gh, runner, tg, s)
	if err := loop.checkAwaitingFeedback(context.Background()); err != nil {
		t.Fatalf("checkAwaitingFeedback: %v", err)
	}

	task, _ := s.GetTask("owner/repo", 10)
	if task == nil {
		t.Fatal("task not found")
	}
	if task.State != store.StateDone {
		t.Errorf("task state = %q after resume+completion, want done", task.State)
	}
	if !tg.completionCalled {
		t.Error("completion notification should have been sent after resume")
	}
}

func TestIsAgentComment(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{"🤔 **Madar needs your input", true},
		{"✅ **Madar completed this task", true},
		{"❌ **Madar error", true},
		{"Regular user comment", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isAgentComment(tc.body); got != tc.want {
			t.Errorf("isAgentComment(%q) = %v, want %v", tc.body, got, tc.want)
		}
	}
}

func TestFormatThread(t *testing.T) {
	comments := []*githubclient.Comment{
		{Author: "alice", Body: "first comment", CreatedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)},
		{Author: "bob", Body: "second comment", CreatedAt: time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)},
	}
	result := formatThread(comments)
	if !containsStr(result, "alice") || !containsStr(result, "first comment") {
		t.Errorf("thread missing alice: %q", result)
	}
	if !containsStr(result, "bob") || !containsStr(result, "second comment") {
		t.Errorf("thread missing bob: %q", result)
	}
}

func TestFormatThread_empty(t *testing.T) {
	if formatThread(nil) != "" {
		t.Error("expected empty string for nil comments")
	}
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || findStr(s, sub))
}

func findStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
