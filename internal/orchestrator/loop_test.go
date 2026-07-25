package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/config"
	"github.com/eslam-mahmoud/go-ai-agent/internal/engine"
	githubclient "github.com/eslam-mahmoud/go-ai-agent/internal/github"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
	"github.com/eslam-mahmoud/go-ai-agent/internal/telegram"
	"log/slog"
	"os"
)

// --- fakes ---

type fakeGitHub struct {
	issues        []*githubclient.Issue
	comments      []*githubclient.Comment
	postedComment string
	labelsSet     []string
}

func (f *fakeGitHub) GetAuthenticatedUsername(_ context.Context) (string, error) {
	return "madar-bot", nil
}
func (f *fakeGitHub) ListPullRequestsForBranch(
	_ context.Context,
	_, _, _ string,
) ([]*githubclient.PullRequest, error) {
	return nil, nil
}

func (f *fakeGitHub) ListOpenIssues(_ context.Context, _, _ string) ([]*githubclient.Issue, error) {
	return nil, nil
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
func (f *fakeGitHub) MergePullRequest(_ context.Context, _, _ string, _ int, _ string) error {
	return nil
}
func (f *fakeGitHub) CreateIssue(_ context.Context, _, _, _, _ string, _ []string) (*githubclient.Issue, error) {
	return &githubclient.Issue{Number: 99, HTMLURL: "https://github.com/owner/repo/issues/99"}, nil
}
func (f *fakeGitHub) UpdateIssueBody(_ context.Context, _, _ string, number int, body string) (*githubclient.Issue, error) {
	return &githubclient.Issue{Number: number, Body: body}, nil
}
func (f *fakeGitHub) CloseIssue(_ context.Context, _, _ string, _ int) error { return nil }

type fakeRunner struct {
	name           string
	result         *engine.Result
	err            error
	capturePrompt  *string
	captureOptions *engine.RunRequest
	onRun          func()
	operations     []string
}

func (f *fakeRunner) Name() string {
	if f.name != "" {
		return f.name
	}
	return "fake"
}

func (f *fakeRunner) Capabilities(context.Context) (engine.CapabilitySet, error) {
	return engine.CapabilitySet{Resume: true, Streaming: true}, nil
}

func (f *fakeRunner) Run(
	_ context.Context,
	request engine.RunRequest,
	_ func(engine.Event) error,
) (*engine.Result, error) {
	f.operations = append(f.operations, "run")
	return f.execute(request)
}

func (f *fakeRunner) Resume(
	_ context.Context,
	request engine.RunRequest,
	_ func(engine.Event) error,
) (*engine.Result, error) {
	f.operations = append(f.operations, "resume")
	return f.execute(request)
}

func (f *fakeRunner) Cancel(context.Context, string) error {
	return nil
}

func (f *fakeRunner) execute(opts engine.RunRequest) (*engine.Result, error) {
	if f.capturePrompt != nil {
		*f.capturePrompt = opts.Prompt
	}
	if f.captureOptions != nil {
		*f.captureOptions = opts
	}
	if f.onRun != nil {
		f.onRun()
	}
	if f.result != nil && f.result.Status == "" {
		f.result.Status = engine.ResultCompleted
	}
	return f.result, f.err
}

type fakeTelegram struct {
	clarificationCalled bool
	completionCalled    bool
	errorCalled         bool
	sent                []string
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
func (f *fakeTelegram) GetUpdates(_ context.Context, _ int64) ([]telegram.Update, error) {
	return nil, nil
}
func (f *fakeTelegram) Send(_ context.Context, text string) error {
	f.sent = append(f.sent, text)
	return nil
}

func (f *fakeTelegram) Reply(_ context.Context, _ int64, _ string) error { return nil }

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
		Repos:        []config.RepoConfig{{Name: "owner/repo"}},
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

func testLoop(t *testing.T, gh githubclient.Client, runner engine.Engine, tg telegram.Gateway, s *store.Store) *Loop {
	t.Helper()
	cfg := testConfig()
	// Create the workspace dir so the os.Stat check in runEngine passes.
	wsDir := filepath.Join(t.TempDir(), "workspaces", "owner", "repo")
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatalf("create workspace dir: %v", err)
	}
	cfg.WorkspaceDir = filepath.Join(filepath.Dir(filepath.Dir(wsDir)))
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	registry, err := engine.NewRegistry(runner)
	if err != nil {
		t.Fatalf("engine.NewRegistry: %v", err)
	}
	loop, err := New(cfg, gh, registry, runner.Name(), cfg.Claude.Model, tg, s, log)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return loop
}

// --- tests ---

func TestNewRejectsUnavailableDefaultEngine(t *testing.T) {
	registry, err := engine.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	_, err = New(
		testConfig(),
		&fakeGitHub{},
		registry,
		"missing",
		"",
		&fakeTelegram{},
		testStore(t),
		slog.Default(),
	)
	if !errors.Is(err, engine.ErrEngineNotFound) {
		t.Errorf("New error = %v", err)
	}
}

func TestTick_noReadyIssues(t *testing.T) {
	gh := &fakeGitHub{}
	runner := &fakeRunner{result: &engine.Result{OutputText: "done"}}
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
	var captured engine.RunRequest
	s := testStore(t)
	runner := &fakeRunner{
		result: &engine.Result{
			SessionID:  "sess-test",
			OutputText: "Fixed the bug by updating X.",
			Status:     engine.ResultCompleted,
		},
		captureOptions: &captured,
		onRun: func() {
			task, err := s.GetTask("owner/repo", 1)
			if err != nil {
				t.Errorf("GetTask during provider run: %v", err)
				return
			}
			if task == nil || task.Engine != "fake" || task.Model != "sonnet" ||
				task.SessionID == "" || task.SessionID == "sess-test" {
				t.Errorf("binding before provider launch = %#v", task)
			}
		},
	}
	tg := &fakeTelegram{}

	loop := testLoop(t, gh, runner, tg, s)
	loop.defaultModel = "sonnet"
	loop.cfg.Claude.SkipPermissions = true
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
	if task.SessionID != "sess-test" {
		t.Errorf("persisted session = %q, want sess-test", task.SessionID)
	}
	if task.Engine != "fake" || task.Model != "sonnet" {
		t.Errorf("persisted engine/model = %q/%q", task.Engine, task.Model)
	}
	if len(runner.operations) != 1 || runner.operations[0] != "run" {
		t.Errorf("operations = %v, want [run]", runner.operations)
	}
	if captured.SessionID == "" || captured.ResumeSessionID != "" {
		t.Errorf("first-run session fields = session %q resume %q", captured.SessionID, captured.ResumeSessionID)
	}
	if captured.ExecutionID != task.ID || captured.Model != "sonnet" ||
		captured.MaxTurns != 40 || captured.Timeout != 30*time.Minute ||
		!captured.Policy.SkipPermissions {
		t.Errorf("normalized request = %#v", captured)
	}
	if !strings.Contains(captured.Prompt, "Fix bug") ||
		!strings.Contains(captured.Prompt, "madar/issue-1") {
		t.Errorf("first-run prompt = %q", captured.Prompt)
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
	runner := &fakeRunner{result: &engine.Result{
		SessionID:  "sess-clarify",
		OutputText: "NEEDS_CLARIFICATION: Should I use A or B?",
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

func TestTick_engineRunError_transitionsToAwaitingFeedback(t *testing.T) {
	gh := &fakeGitHub{
		issues: []*githubclient.Issue{
			{Number: 7, Title: "Task", HTMLURL: "url", Labels: []string{"ready"}},
		},
	}
	runner := &fakeRunner{err: fmt.Errorf("timeout")}
	tg := &fakeTelegram{}
	s := testStore(t)

	loop := testLoop(t, gh, runner, tg, s)
	_ = loop.tick(context.Background()) // error is expected; don't fatal

	task, _ := s.GetTask("owner/repo", 7)
	if task == nil {
		t.Fatal("task not created")
	}
	if task.State != store.StateAwaitingFeedback {
		t.Errorf("task state = %q after engine error, want awaiting-feedback", task.State)
	}
	if !tg.errorCalled {
		t.Error("Telegram error notification should have been sent")
	}
	if !tg.clarificationCalled {
		t.Error("clarification should have been posted so human can intervene")
	}
}

func TestTick_engineResultError_transitionsToAwaitingFeedback(t *testing.T) {
	gh := &fakeGitHub{
		issues: []*githubclient.Issue{
			{Number: 8, Title: "Task", HTMLURL: "url", Labels: []string{"ready"}},
		},
	}
	runner := &fakeRunner{result: &engine.Result{Status: engine.ResultFailed, OutputText: "something went wrong"}}
	tg := &fakeTelegram{}
	s := testStore(t)

	loop := testLoop(t, gh, runner, tg, s)
	_ = loop.tick(context.Background())

	task, _ := s.GetTask("owner/repo", 8)
	if task == nil {
		t.Fatal("task not created")
	}
	if task.State != store.StateAwaitingFeedback {
		t.Errorf("task state = %q after result error, want awaiting-feedback", task.State)
	}
}

func TestTick_invalidResultStatusCannotComplete(t *testing.T) {
	gh := &fakeGitHub{
		issues: []*githubclient.Issue{
			{Number: 81, Title: "Task", HTMLURL: "url", Labels: []string{"ready"}},
		},
	}
	runner := &fakeRunner{result: &engine.Result{
		Status:     engine.ResultStatus("unexpected"),
		OutputText: "looks complete",
	}}
	tg := &fakeTelegram{}
	s := testStore(t)

	loop := testLoop(t, gh, runner, tg, s)
	_ = loop.tick(context.Background())

	task, _ := s.GetTask("owner/repo", 81)
	if task == nil || task.State != store.StateAwaitingFeedback {
		t.Errorf("task = %#v, want awaiting-feedback", task)
	}
	if tg.completionCalled {
		t.Error("invalid result status announced completion")
	}
}

func TestTick_nilResultCannotComplete(t *testing.T) {
	gh := &fakeGitHub{
		issues: []*githubclient.Issue{
			{Number: 83, Title: "Task", HTMLURL: "url", Labels: []string{"ready"}},
		},
	}
	tg := &fakeTelegram{}
	s := testStore(t)

	loop := testLoop(t, gh, &fakeRunner{}, tg, s)
	_ = loop.tick(context.Background())

	task, _ := s.GetTask("owner/repo", 83)
	if task == nil || task.State != store.StateAwaitingFeedback {
		t.Errorf("task = %#v, want awaiting-feedback", task)
	}
	if tg.completionCalled {
		t.Error("nil result announced completion")
	}
}

func TestTick_cancelledResultTransitionsToInterrupted(t *testing.T) {
	gh := &fakeGitHub{
		issues: []*githubclient.Issue{
			{Number: 82, Title: "Task", HTMLURL: "url", Labels: []string{"ready"}},
		},
	}
	runner := &fakeRunner{result: &engine.Result{Status: engine.ResultCancelled}}
	tg := &fakeTelegram{}
	s := testStore(t)

	loop := testLoop(t, gh, runner, tg, s)
	if err := loop.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	task, _ := s.GetTask("owner/repo", 82)
	if task == nil || task.State != store.StateInterrupted {
		t.Errorf("task = %#v, want interrupted", task)
	}
	if gh.postedComment != "" || tg.clarificationCalled || tg.completionCalled {
		t.Error("cancelled result notified completion or clarification")
	}
}

func TestTick_shutdownTransitionsToInterruptedWithoutClarification(t *testing.T) {
	gh := &fakeGitHub{
		issues: []*githubclient.Issue{
			{Number: 10, Title: "Task", HTMLURL: "url", Labels: []string{"ready"}},
		},
	}
	// Runner returns context.Canceled to simulate shutdown mid-run.
	runner := &fakeRunner{err: fmt.Errorf("signal: killed: %w", context.Canceled)}
	tg := &fakeTelegram{}
	s := testStore(t)

	// Use an already-cancelled context to simulate SIGTERM.
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	loop := testLoop(t, gh, runner, tg, s)
	_ = loop.tick(cancelledCtx)

	// Task should be durably interrupted without asking a routine restart
	// question on GitHub.
	task, _ := s.GetTask("owner/repo", 10)
	if task == nil {
		t.Fatal("task not found")
	}
	if task.State != store.StateInterrupted {
		t.Errorf("state = %q after shutdown, want interrupted", task.State)
	}
	if gh.postedComment != "" {
		t.Errorf("shutdown posted clarification comment: %q", gh.postedComment)
	}
	entries, _ := s.GetAuditLog("owner/repo", 10)
	if len(entries) == 0 || entries[len(entries)-1].Event != "interrupted" {
		t.Errorf("audit log = %#v, want final interrupted event", entries)
	}
}

func TestRecoverInterruptedResumesStoredSession(t *testing.T) {
	gh := &fakeGitHub{
		issues: []*githubclient.Issue{
			{
				Number:  42,
				Title:   "Recover me",
				HTMLURL: "https://github.com/owner/repo/issues/42",
				Labels:  []string{"in-progress"},
			},
		},
	}
	s := testStore(t)
	_, _ = s.UpsertTask("owner/repo", 42, store.StateInProgress, "session-42")

	var captured engine.RunRequest
	runner := &fakeRunner{
		result:         &engine.Result{OutputText: "recovered and completed"},
		captureOptions: &captured,
		onRun: func() {
			task, err := s.GetTask("owner/repo", 42)
			if err != nil {
				t.Errorf("GetTask during recovery: %v", err)
				return
			}
			if task.State != store.StateRecovering {
				t.Errorf("state during provider run = %q, want recovering", task.State)
			}
		},
	}
	loop := testLoop(t, gh, runner, &fakeTelegram{}, s)

	if err := loop.recoverInterrupted(context.Background()); err != nil {
		t.Fatalf("recoverInterrupted: %v", err)
	}

	if captured.ResumeSessionID != "session-42" {
		t.Errorf("ResumeSessionID = %q, want session-42", captured.ResumeSessionID)
	}
	if captured.SessionID != "" {
		t.Errorf("SessionID = %q, want empty for resume", captured.SessionID)
	}
	if len(runner.operations) != 1 || runner.operations[0] != "resume" {
		t.Errorf("operations = %v, want [resume]", runner.operations)
	}
	if !containsStr(captured.Prompt, "interrupted") {
		t.Errorf("recovery prompt = %q, want interruption context", captured.Prompt)
	}
	task, _ := s.GetTask("owner/repo", 42)
	if task.State != store.StateDone {
		t.Errorf("final state = %q, want done", task.State)
	}
	entries, _ := s.GetAuditLog("owner/repo", 42)
	events := make([]string, 0, len(entries))
	for _, entry := range entries {
		events = append(events, entry.Event)
	}
	if !containsStr(strings.Join(events, ","), "interrupted") ||
		!containsStr(strings.Join(events, ","), "recovering") {
		t.Errorf("audit events = %v, want interrupted and recovering", events)
	}
}

func TestRecoverInterruptedDoesNotResumeCIWaitingTask(t *testing.T) {
	s := testStore(t)
	_, _ = s.UpsertTask("owner/repo", 43, store.StateInProgress, "session-43")
	_ = s.SetCIState("owner/repo", 43, store.CIStateWaiting)
	runnerCalls := 0
	runner := &fakeRunner{
		result: &engine.Result{OutputText: "unexpected"},
		onRun:  func() { runnerCalls++ },
	}
	loop := testLoop(t, &fakeGitHub{}, runner, &fakeTelegram{}, s)

	if err := loop.recoverInterrupted(context.Background()); err != nil {
		t.Fatalf("recoverInterrupted: %v", err)
	}
	if runnerCalls != 0 {
		t.Errorf("runner calls = %d, want 0", runnerCalls)
	}
	task, _ := s.GetTask("owner/repo", 43)
	if task.State != store.StateInProgress || task.CIState != store.CIStateWaiting {
		t.Errorf("CI task after recovery = state %q, ci %q", task.State, task.CIState)
	}
}

func TestRecoverInterruptedUsesPinnedModelAfterDefaultChanges(t *testing.T) {
	gh := &fakeGitHub{issues: []*githubclient.Issue{{
		Number:  44,
		Title:   "Pinned execution",
		HTMLURL: "https://github.com/owner/repo/issues/44",
		Labels:  []string{"in-progress"},
	}}}
	s := testStore(t)
	if _, err := s.BindTaskExecution("owner/repo", 44, store.StateInProgress, store.ExecutionBinding{
		Engine:            "alternate",
		Model:             "pinned-model",
		ProviderSessionID: "session-44",
	}); err != nil {
		t.Fatal(err)
	}
	var captured engine.RunRequest
	defaultRunner := &fakeRunner{result: &engine.Result{OutputText: "must not run"}}
	runner := &fakeRunner{
		name:           "alternate",
		result:         &engine.Result{OutputText: "recovered"},
		captureOptions: &captured,
	}
	loop := testLoop(t, gh, defaultRunner, &fakeTelegram{}, s)
	if err := loop.engines.Register(runner); err != nil {
		t.Fatal(err)
	}
	loop.defaultModel = "new-default"

	if err := loop.recoverInterrupted(context.Background()); err != nil {
		t.Fatal(err)
	}
	if captured.Model != "pinned-model" || captured.ResumeSessionID != "session-44" {
		t.Errorf("resume request = %#v", captured)
	}
	if len(defaultRunner.operations) != 0 || len(runner.operations) != 1 ||
		runner.operations[0] != "resume" {
		t.Errorf("default operations=%v alternate operations=%v", defaultRunner.operations, runner.operations)
	}
}

func TestRecoverInterruptedUnavailableStoredEngineDoesNotFallback(t *testing.T) {
	gh := &fakeGitHub{issues: []*githubclient.Issue{{
		Number:  45,
		Title:   "Missing provider",
		HTMLURL: "https://github.com/owner/repo/issues/45",
		Labels:  []string{"in-progress"},
	}}}
	s := testStore(t)
	if _, err := s.BindTaskExecution("owner/repo", 45, store.StateInProgress, store.ExecutionBinding{
		Engine:            "unavailable",
		Model:             "pinned-model",
		ProviderSessionID: "session-45",
	}); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{result: &engine.Result{OutputText: "must not run"}}
	tg := &fakeTelegram{}
	loop := testLoop(t, gh, runner, tg, s)

	if err := loop.recoverInterrupted(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runner.operations) != 0 {
		t.Errorf("fallback provider operations = %v", runner.operations)
	}
	task, _ := s.GetTask("owner/repo", 45)
	if task.State != store.StateAwaitingFeedback || task.Engine != "unavailable" {
		t.Errorf("blocked task = %#v", task)
	}
	if !tg.clarificationCalled || !strings.Contains(gh.postedComment, "stored engine is unavailable") {
		t.Errorf("missing-provider clarification = %q, notified=%v", gh.postedComment, tg.clarificationCalled)
	}
}

func TestTick_missingWorkspace_skipsIssue(t *testing.T) {
	gh := &fakeGitHub{
		issues: []*githubclient.Issue{
			{Number: 9, Title: "Task", HTMLURL: "url", Labels: []string{"ready"}},
		},
	}
	tg := &fakeTelegram{}
	s := testStore(t)
	loop := testLoop(t, gh, &fakeRunner{result: &engine.Result{OutputText: "ok"}}, tg, s)
	// Remove the workspace dir so EnsureWorkspaces will attempt (and fail) to clone.
	// This simulates a missing workspace for a repo that can't be auto-cloned.
	wsDir := filepath.Join(loop.cfg.WorkspaceDir, "owner", "repo")
	if err := os.RemoveAll(wsDir); err != nil {
		t.Fatal(err)
	}
	// Point to a URL that won't be cloned (no network in unit tests);
	// EnsureWorkspaces clone failure causes pickAndRun to skip this repo.
	_ = loop.tick(context.Background())

	// Issue should NOT have been claimed — EnsureWorkspaces failure causes a skip.
	task, _ := s.GetTask("owner/repo", 9)
	if task != nil && task.State == store.StateInProgress {
		t.Error("issue should not be claimed when workspace clone fails")
	}
}

func TestTick_ciAndFeedbackRunBeforeCountActive(t *testing.T) {
	// Even if there are active tasks (concurrency guard would block pickAndRun),
	// CI checks and awaiting-feedback detection must still run.
	clarTime := time.Now().UTC().Add(-5 * time.Minute)
	gh := &fakeGitHub{
		comments: []*githubclient.Comment{
			{Body: "Use per-IP", Author: "human", CreatedAt: clarTime.Add(time.Minute)},
		},
	}
	runner := &fakeRunner{result: &engine.Result{OutputText: "done"}}
	tg := &fakeTelegram{}
	s := testStore(t)

	// Set up an awaiting-feedback task.
	_, _ = s.UpsertTask("owner/repo", 20, store.StateAwaitingFeedback, "sess-20")
	_ = s.SetClarificationTime("owner/repo", 20, clarTime)
	// Set up a second active task to trigger the capacity guard.
	_, _ = s.UpsertTask("owner/repo", 21, store.StateInProgress, "sess-21")

	loop := testLoop(t, gh, runner, tg, s)
	if err := loop.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	// Despite being at capacity (1 in-progress), issue 20 should have been
	// resumed because checkAwaitingFeedback runs before the capacity guard.
	task, _ := s.GetTask("owner/repo", 20)
	if task == nil {
		t.Fatal("task not found")
	}
	// After resume + completion the state should be done.
	if task.State != store.StateDone {
		t.Errorf("task 20 state = %q, want done (resumed despite capacity guard)", task.State)
	}
}

func TestTick_capacityGuard(t *testing.T) {
	gh := &fakeGitHub{
		issues: []*githubclient.Issue{
			{Number: 3, Title: "Task", HTMLURL: "url", Labels: []string{"ready"}},
		},
	}
	runner := &fakeRunner{result: &engine.Result{OutputText: "done"}}
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

func TestResumeIfReplied_collectsAllHumanReplies(t *testing.T) {
	clarTime := time.Now().UTC().Add(-10 * time.Minute)
	gh := &fakeGitHub{
		comments: []*githubclient.Comment{
			{Body: "First answer", Author: "human-user", CreatedAt: clarTime.Add(time.Minute)},
			{Body: "Correction to first answer", Author: "human-user", CreatedAt: clarTime.Add(2 * time.Minute)},
		},
	}
	var capturedPrompt string
	var capturedRequest engine.RunRequest
	runner := &fakeRunner{
		result:         &engine.Result{OutputText: "done"},
		capturePrompt:  &capturedPrompt,
		captureOptions: &capturedRequest,
	}
	tg := &fakeTelegram{}
	s := testStore(t)

	if _, err := s.BindTaskExecution("owner/repo", 55, store.StateAwaitingFeedback, store.ExecutionBinding{
		Engine:            "fake",
		Model:             "pinned-feedback-model",
		ProviderSessionID: "sess-55",
	}); err != nil {
		t.Fatal(err)
	}
	_ = s.SetClarificationTime("owner/repo", 55, clarTime)

	loop := testLoop(t, gh, runner, tg, s)
	loop.defaultModel = "changed-default"
	loop.botUsername = "madar-bot"

	if err := loop.checkAwaitingFeedback(context.Background()); err != nil {
		t.Fatalf("checkAwaitingFeedback: %v", err)
	}

	if !containsStr(capturedPrompt, "First answer") {
		t.Error("resume prompt should contain first reply")
	}
	if !containsStr(capturedPrompt, "Correction to first answer") {
		t.Error("resume prompt should contain second reply")
	}
	if capturedRequest.Model != "pinned-feedback-model" ||
		capturedRequest.ResumeSessionID != "sess-55" {
		t.Errorf("feedback resume request = %#v", capturedRequest)
	}
}

func TestResumeIfReplied_skipsBotCommentByUsername(t *testing.T) {
	clarTime := time.Now().UTC().Add(-10 * time.Minute)
	gh := &fakeGitHub{
		// First comment is from the bot itself (madar-bot), second from a human.
		comments: []*githubclient.Comment{
			{Body: "Some bot comment", Author: "madar-bot", CreatedAt: clarTime.Add(time.Minute)},
			{Body: "Human answer here", Author: "human-user", CreatedAt: clarTime.Add(2 * time.Minute)},
		},
	}
	runner := &fakeRunner{result: &engine.Result{OutputText: "done"}}
	tg := &fakeTelegram{}
	s := testStore(t)

	_, _ = s.UpsertTask("owner/repo", 50, store.StateAwaitingFeedback, "sess-50")
	_ = s.SetClarificationTime("owner/repo", 50, clarTime)

	loop := testLoop(t, gh, runner, tg, s)
	loop.botUsername = "madar-bot" // simulate resolved username

	if err := loop.checkAwaitingFeedback(context.Background()); err != nil {
		t.Fatalf("checkAwaitingFeedback: %v", err)
	}

	task, _ := s.GetTask("owner/repo", 50)
	if task == nil {
		t.Fatal("task not found")
	}
	// Should have resumed from the human comment, not the bot comment.
	if task.State != store.StateDone {
		t.Errorf("state = %q, want done (resumed from human reply)", task.State)
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
	runner := &fakeRunner{result: &engine.Result{
		OutputText: "Implemented per-IP rate limiting at 5/min.",
		Status:     engine.ResultCompleted,
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

func TestFormatHumanThread_filtersBotComments(t *testing.T) {
	s := testStore(t)
	loop := testLoop(t, &fakeGitHub{}, &fakeRunner{result: nil}, &fakeTelegram{}, s)
	loop.botUsername = "madar-bot"

	comments := []*githubclient.Comment{
		{Author: "human", Body: "please add rate limiting", CreatedAt: time.Now()},
		{Author: "madar-bot", Body: "🤔 **Madar needs your input", CreatedAt: time.Now()},
		{Author: "human", Body: "use per-IP 5/min", CreatedAt: time.Now()},
		{Author: "madar-bot", Body: "✅ **Madar completed", CreatedAt: time.Now()},
	}
	result := loop.formatHumanThread(comments)

	if !containsStr(result, "please add rate limiting") {
		t.Error("thread missing first human comment")
	}
	if !containsStr(result, "use per-IP 5/min") {
		t.Error("thread missing second human comment")
	}
	if containsStr(result, "Madar needs") || containsStr(result, "Madar completed") {
		t.Error("thread should not contain bot comments")
	}
}

func TestFormatHumanThread_emptyWhenAllBot(t *testing.T) {
	s := testStore(t)
	loop := testLoop(t, &fakeGitHub{}, &fakeRunner{result: nil}, &fakeTelegram{}, s)
	loop.botUsername = "madar-bot"

	comments := []*githubclient.Comment{
		{Author: "madar-bot", Body: "✅ **Madar completed this task:", CreatedAt: time.Now()},
	}
	if result := loop.formatHumanThread(comments); result != "" {
		t.Errorf("expected empty thread when all comments are from bot, got: %q", result)
	}
}

func TestFormatHumanThread_truncatesOldestFirst(t *testing.T) {
	s := testStore(t)
	loop := testLoop(t, &fakeGitHub{}, &fakeRunner{result: nil}, &fakeTelegram{}, s)
	// Allow enough chars for the most recent comment but not all three.
	// Each entry is roughly "@human (timestamp):\n<body>\n\n" = ~50+ chars.
	// Set limit to 120 — fits one entry but not three.
	loop.cfg.Claude.MaxThreadChars = 120

	now := time.Now()
	comments := []*githubclient.Comment{
		{Author: "human", Body: "first old comment that should be dropped", CreatedAt: now.Add(-2 * time.Hour)},
		{Author: "human", Body: "second older comment also dropped", CreatedAt: now.Add(-time.Hour)},
		{Author: "human", Body: "most recent comment", CreatedAt: now},
	}
	result := loop.formatHumanThread(comments)

	if !containsStr(result, "most recent comment") {
		t.Errorf("most recent comment should be included; got: %q", result)
	}
	if containsStr(result, "first old comment that should be dropped") {
		t.Error("oldest comment should have been truncated")
	}
	if !containsStr(result, "omitted") {
		t.Errorf("expected omission marker when comments were truncated; got: %q", result)
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
