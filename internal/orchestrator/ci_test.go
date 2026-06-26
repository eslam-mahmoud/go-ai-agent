package orchestrator

import (
	"context"
	"testing"
	"time"

	githubclient "github.com/eslam-mahmoud/go-ai-agent/internal/github"
	"github.com/eslam-mahmoud/go-ai-agent/internal/claude"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
)

// fakeGitHubCI extends fakeGitHub with CI check methods.
type fakeGitHubCI struct {
	fakeGitHub
	checkStatus    githubclient.CheckStatus
	checkStatusErr error
	failureOutput  string
}

func (f *fakeGitHubCI) GetCheckSuiteStatus(_ context.Context, _, _, _ string) (githubclient.CheckStatus, error) {
	return f.checkStatus, f.checkStatusErr
}
func (f *fakeGitHubCI) GetFailedStepOutput(_ context.Context, _, _, _ string) (string, error) {
	return f.failureOutput, nil
}

func TestCheckCIPending_disabled(t *testing.T) {
	cfg := testConfig()
	cfg.CI.Enabled = false
	s := testStore(t)
	_, _ = s.UpsertTask("owner/repo", 1, store.StateInProgress, "sess")
	_ = s.SetCIState("owner/repo", 1, store.CIStateWaiting)

	gh := &fakeGitHubCI{checkStatus: githubclient.CheckSuccess}
	loop := testLoop(t, gh, &fakeRunner{result: &claude.Result{Output: "ok"}}, &fakeTelegram{}, s)
	loop.cfg = cfg

	if err := loop.checkCIPending(context.Background()); err != nil {
		t.Fatalf("checkCIPending: %v", err)
	}
	// Should not have advanced state since CI is disabled.
	task, _ := s.GetTask("owner/repo", 1)
	if task.CIState != store.CIStateWaiting {
		t.Errorf("CI state changed to %q with CI disabled", task.CIState)
	}
}

func TestCheckCIPending_stillPending(t *testing.T) {
	cfg := testConfig()
	cfg.CI.Enabled = true
	cfg.CI.MaxRetries = 3
	s := testStore(t)
	_, _ = s.UpsertTask("owner/repo", 2, store.StateInProgress, "sess")
	_ = s.SetCIState("owner/repo", 2, store.CIStateWaiting)

	gh := &fakeGitHubCI{checkStatus: githubclient.CheckPending}
	loop := testLoop(t, gh, &fakeRunner{result: &claude.Result{Output: "ok"}}, &fakeTelegram{}, s)
	loop.cfg = cfg

	if err := loop.checkCIPending(context.Background()); err != nil {
		t.Fatalf("checkCIPending: %v", err)
	}
	task, _ := s.GetTask("owner/repo", 2)
	if task.CIState != store.CIStateWaiting {
		t.Errorf("CI state = %q, want waiting while CI is pending", task.CIState)
	}
}

func TestCheckCIPending_passes(t *testing.T) {
	cfg := testConfig()
	cfg.CI.Enabled = true
	cfg.CI.MaxRetries = 3
	s := testStore(t)
	_, _ = s.UpsertTask("owner/repo", 3, store.StateInProgress, "sess")
	_ = s.SetCIState("owner/repo", 3, store.CIStateWaiting)

	tg := &fakeTelegram{}
	gh := &fakeGitHubCI{
		fakeGitHub: fakeGitHub{
			issues: []*githubclient.Issue{
				{Number: 3, HTMLURL: "https://github.com/owner/repo/issues/3", Labels: []string{"in-progress"}},
			},
		},
		checkStatus: githubclient.CheckSuccess,
	}
	loop := testLoop(t, gh, &fakeRunner{result: &claude.Result{Output: "ok"}}, tg, s)
	loop.cfg = cfg

	if err := loop.checkCIPending(context.Background()); err != nil {
		t.Fatalf("checkCIPending: %v", err)
	}
	task, _ := s.GetTask("owner/repo", 3)
	if task.State != store.StateDone {
		t.Errorf("task state = %q after CI pass, want done", task.State)
	}
	if task.CIState != store.CIStatePassed {
		t.Errorf("CI state = %q after CI pass, want passed", task.CIState)
	}
	if !tg.completionCalled {
		t.Error("Telegram completion should have been sent on CI pass")
	}
}

func TestCheckCIPending_failsThenFixedAndPasses(t *testing.T) {
	cfg := testConfig()
	cfg.CI.Enabled = true
	cfg.CI.MaxRetries = 3
	s := testStore(t)
	_, _ = s.UpsertTask("owner/repo", 4, store.StateInProgress, "sess-abc")
	_ = s.SetCIState("owner/repo", 4, store.CIStateWaiting)

	tg := &fakeTelegram{}
	// First call: CI failed; Claude fixes it; second call: CI passes.
	callCount := 0
	gh := &fakeGitHubCI{
		fakeGitHub: fakeGitHub{
			issues: []*githubclient.Issue{
				{Number: 4, HTMLURL: "https://github.com/owner/repo/issues/4", Labels: []string{"in-progress"}},
			},
		},
	}

	runner := &fakeRunner{result: &claude.Result{Output: "fixed and pushed"}}
	loop := testLoop(t, gh, runner, tg, s)
	loop.cfg = cfg

	// Tick 1: CI fails → Claude re-invoked, ci_state stays waiting.
	gh.checkStatus = githubclient.CheckFailure
	gh.failureOutput = "FAIL: TestBar"
	callCount++
	if err := loop.checkCIPending(context.Background()); err != nil {
		t.Fatalf("tick 1 checkCIPending: %v", err)
	}
	task, _ := s.GetTask("owner/repo", 4)
	if task.CIState != store.CIStateWaiting {
		t.Errorf("after failure+fix ci_state = %q, want waiting", task.CIState)
	}
	if task.CIRetries != 1 {
		t.Errorf("ci_retries = %d, want 1", task.CIRetries)
	}

	// Tick 2: CI now passes → finalize.
	gh.checkStatus = githubclient.CheckSuccess
	callCount++
	if err := loop.checkCIPending(context.Background()); err != nil {
		t.Fatalf("tick 2 checkCIPending: %v", err)
	}
	task, _ = s.GetTask("owner/repo", 4)
	if task.State != store.StateDone {
		t.Errorf("task state = %q after CI pass, want done", task.State)
	}
	_ = callCount // suppress unused warning
}

func TestCheckCIPending_exhaustsRetries(t *testing.T) {
	cfg := testConfig()
	cfg.CI.Enabled = true
	cfg.CI.MaxRetries = 2
	s := testStore(t)
	_, _ = s.UpsertTask("owner/repo", 5, store.StateInProgress, "sess")
	_ = s.SetCIState("owner/repo", 5, store.CIStateWaiting)
	// Pre-set retries to max so next increment triggers give-up.
	_, _ = s.IncrementCIRetries("owner/repo", 5)
	_, _ = s.IncrementCIRetries("owner/repo", 5)

	tg := &fakeTelegram{}
	gh := &fakeGitHubCI{
		fakeGitHub: fakeGitHub{
			issues: []*githubclient.Issue{
				{Number: 5, HTMLURL: "url", Labels: []string{"in-progress"}},
			},
		},
		checkStatus:   githubclient.CheckFailure,
		failureOutput: "FAIL: still broken",
	}
	loop := testLoop(t, gh, &fakeRunner{result: &claude.Result{Output: "ok"}}, tg, s)
	loop.cfg = cfg

	if err := loop.checkCIPending(context.Background()); err != nil {
		t.Fatalf("checkCIPending: %v", err)
	}
	task, _ := s.GetTask("owner/repo", 5)
	if task.CIState != store.CIStateGaveUp {
		t.Errorf("CI state = %q after exhausted retries, want gave_up", task.CIState)
	}
	if task.State != store.StateAwaitingFeedback {
		t.Errorf("task state = %q after gave up, want awaiting-feedback", task.State)
	}
	if !tg.clarificationCalled {
		t.Error("human notification should be sent when retries exhausted")
	}
}

func TestCheckCIPending_waitTimeoutExceeded(t *testing.T) {
	cfg := testConfig()
	cfg.CI.Enabled = true
	cfg.CI.MaxRetries = 3
	cfg.CI.WaitTimeout = time.Millisecond // immediately expire
	s := testStore(t)

	_, _ = s.UpsertTask("owner/repo", 6, store.StateInProgress, "sess")
	_ = s.SetCIState("owner/repo", 6, store.CIStateWaiting)
	// Set CIWatchStartedAt in the past so timeout is exceeded.
	past := time.Now().UTC().Add(-10 * time.Millisecond)
	_ = s.SetCIWatchStartedAt("owner/repo", 6, past)
	time.Sleep(5 * time.Millisecond)

	tg := &fakeTelegram{}
	gh := &fakeGitHubCI{
		fakeGitHub: fakeGitHub{
			issues: []*githubclient.Issue{
				{Number: 6, HTMLURL: "url", Labels: []string{"in-progress"}},
			},
		},
		checkStatus: githubclient.CheckPending, // CI still pending but timeout exceeded
	}
	loop := testLoop(t, gh, &fakeRunner{result: &claude.Result{Output: "ok"}}, tg, s)
	loop.cfg = cfg

	if err := loop.checkCIPending(context.Background()); err != nil {
		t.Fatalf("checkCIPending: %v", err)
	}

	task, _ := s.GetTask("owner/repo", 6)
	if task.CIState != store.CIStateGaveUp {
		t.Errorf("CI state = %q after timeout, want gave_up", task.CIState)
	}
	if task.State != store.StateAwaitingFeedback {
		t.Errorf("task state = %q after timeout, want awaiting-feedback", task.State)
	}
	if !tg.clarificationCalled {
		t.Error("human should be notified on CI timeout")
	}
}

func TestExtractPRNumber(t *testing.T) {
	cases := []struct {
		output string
		want   int
	}{
		{"I opened PR: #42 for review", 42},
		{"Created PR #7 with the changes", 7},
		{"No PR mentioned here", 0},
		{"PR: #100\nSome other text", 100},
		{"", 0},
	}
	for _, tc := range cases {
		got := extractPRNumber(tc.output)
		if got != tc.want {
			t.Errorf("extractPRNumber(%q) = %d, want %d", tc.output, got, tc.want)
		}
	}
}

func TestStartCIWatch(t *testing.T) {
	s := testStore(t)
	_, _ = s.UpsertTask("owner/repo", 10, store.StateInProgress, "sess")

	loop := testLoop(t, &fakeGitHubCI{}, &fakeRunner{result: &claude.Result{}}, &fakeTelegram{}, s)
	loop.cfg = testConfig()

	if err := loop.StartCIWatch(context.Background(), "owner/repo", 10, 99); err != nil {
		t.Fatalf("StartCIWatch: %v", err)
	}
	task, _ := s.GetTask("owner/repo", 10)
	if task.PRNumber != 99 {
		t.Errorf("PRNumber = %d, want 99", task.PRNumber)
	}
	if task.CIState != store.CIStateWaiting {
		t.Errorf("CIState = %q, want waiting", task.CIState)
	}
}

// fakeGitHubCI must implement the full Client interface.
// Delegate everything else to fakeGitHub.
var _ githubclient.Client = (*fakeGitHubCI)(nil)

func (f *fakeGitHubCI) ListReadyIssues(ctx context.Context, o, r, l string) ([]*githubclient.Issue, error) {
	return f.fakeGitHub.ListReadyIssues(ctx, o, r, l)
}
func (f *fakeGitHubCI) GetIssue(ctx context.Context, o, r string, n int) (*githubclient.Issue, error) {
	return f.fakeGitHub.GetIssue(ctx, o, r, n)
}
func (f *fakeGitHubCI) GetComments(ctx context.Context, o, r string, n int, since *time.Time) ([]*githubclient.Comment, error) {
	return f.fakeGitHub.GetComments(ctx, o, r, n, since)
}
func (f *fakeGitHubCI) PostComment(ctx context.Context, o, r string, n int, b string) (*githubclient.Comment, error) {
	return f.fakeGitHub.PostComment(ctx, o, r, n, b)
}
func (f *fakeGitHubCI) AddLabel(ctx context.Context, o, r string, n int, l string) error {
	return f.fakeGitHub.AddLabel(ctx, o, r, n, l)
}
func (f *fakeGitHubCI) RemoveLabel(ctx context.Context, o, r string, n int, l string) error {
	return f.fakeGitHub.RemoveLabel(ctx, o, r, n, l)
}
func (f *fakeGitHubCI) ReplaceLabels(ctx context.Context, o, r string, n int, ls []string) error {
	return f.fakeGitHub.ReplaceLabels(ctx, o, r, n, ls)
}
func (f *fakeGitHubCI) CreateLabel(ctx context.Context, o, r, name, color string) error {
	return f.fakeGitHub.CreateLabel(ctx, o, r, name, color)
}
func (f *fakeGitHubCI) EnsureLabels(ctx context.Context, o, r string, m map[string]string) error {
	return f.fakeGitHub.EnsureLabels(ctx, o, r, m)
}
func (f *fakeGitHubCI) CloseIssue(_ context.Context, _, _ string, _ int) error { return nil }
