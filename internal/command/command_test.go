package command

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

func TestParseAcceptsTheFormsTelegramSends(t *testing.T) {
	t.Parallel()
	tests := []struct {
		text string
		name Name
		args []string
	}{
		{"/status", NameStatus, nil},
		{"  /status  ", NameStatus, nil},
		{"/STATUS", NameStatus, nil},
		// Telegram addresses commands to a bot in group chats.
		{"/status@madarbot", NameStatus, nil},
		{"/answer use eu-west", "answer", []string{"use", "eu-west"}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.text, func(t *testing.T) {
			t.Parallel()
			command, err := Parse(test.text, 11, 22)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if command.Name != test.name {
				t.Fatalf("name = %q, want %q", command.Name, test.name)
			}
			if len(command.Args) != len(test.args) {
				t.Fatalf("args = %v, want %v", command.Args, test.args)
			}
			if command.ChatID != 11 || command.UserID != 22 {
				t.Fatalf("identity = %d/%d", command.ChatID, command.UserID)
			}
		})
	}

	// Ordinary chat messages are not errors, just not commands.
	for _, text := range []string{"hello", "", "   ", "/", "/@bot"} {
		if _, err := Parse(text, 11, 22); !errors.Is(err, ErrNotACommand) {
			t.Fatalf("Parse(%q) error = %v", text, err)
		}
	}
}

// An unauthorized sender must never reach a handler.
func TestDispatchRefusesUnauthorizedSenders(t *testing.T) {
	t.Parallel()
	called := false
	router := newTestRouter(t, authorizerFunc(func(context.Context, Command) error {
		return ErrUnauthorized
	}))
	if err := router.Register(NameStatus, "status", func(
		context.Context, Command,
	) (string, error) {
		called = true
		return "secret", nil
	}); err != nil {
		t.Fatal(err)
	}
	reply, err := router.Dispatch(context.Background(), Command{Name: NameStatus})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if called {
		t.Fatal("an unauthorized sender reached the handler")
	}
	if !strings.Contains(reply, "not authorized") || strings.Contains(reply, "secret") {
		t.Fatalf("reply = %q", reply)
	}

	// A router cannot exist without authorization at all.
	if _, err := NewRouter(nil); err == nil {
		t.Fatal("router without an authorizer accepted")
	}
}

func TestDispatchAnswersUnknownCommandsWithHelp(t *testing.T) {
	t.Parallel()
	router := newTestRouter(t, allowAll())
	if err := router.Register(NameStatus, "current status", func(
		context.Context, Command,
	) (string, error) {
		return "ok", nil
	}); err != nil {
		t.Fatal(err)
	}
	reply, err := router.Dispatch(context.Background(), Command{Name: "nonsense"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "Unknown command") || !strings.Contains(reply, "/status") {
		t.Fatalf("reply = %q", reply)
	}
	help, err := router.Dispatch(context.Background(), Command{Name: NameHelp})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(help, "/status — current status") {
		t.Fatalf("help = %q", help)
	}
}

func TestDispatchReportsHandlerFailureWithoutLeakingDetail(t *testing.T) {
	t.Parallel()
	router := newTestRouter(t, allowAll())
	if err := router.Register(NameStatus, "status", func(
		context.Context, Command,
	) (string, error) {
		return "", errors.New("read project: sql: connection refused at 10.0.0.5")
	}); err != nil {
		t.Fatal(err)
	}
	reply, err := router.Dispatch(context.Background(), Command{Name: NameStatus})
	if err != nil {
		t.Fatalf("Dispatch returned an error instead of a reply: %v", err)
	}
	if !strings.Contains(reply, "/status failed") {
		t.Fatalf("reply = %q", reply)
	}
	if strings.Contains(reply, "10.0.0.5") {
		t.Fatalf("reply leaked internal detail: %q", reply)
	}
}

func TestDispatchTruncatesLongReplies(t *testing.T) {
	t.Parallel()
	router := newTestRouter(t, allowAll())
	long := strings.Repeat("a backlog line that goes on\n", 500)
	if err := router.Register(NamePlan, "plan", func(
		context.Context, Command,
	) (string, error) {
		return long, nil
	}); err != nil {
		t.Fatal(err)
	}
	reply, err := router.Dispatch(context.Background(), Command{Name: NamePlan})
	if err != nil {
		t.Fatal(err)
	}
	if len(reply) > MaxReplyBytes+32 {
		t.Fatalf("reply is %d bytes", len(reply))
	}
	// A silently cut backlog reads as a complete one.
	if !strings.HasSuffix(reply, "truncated") {
		t.Fatalf("truncation was not marked: %q", reply[len(reply)-40:])
	}
}

func TestRegisterRejectsDuplicatesAndBadInput(t *testing.T) {
	t.Parallel()
	router := newTestRouter(t, allowAll())
	handler := func(context.Context, Command) (string, error) { return "", nil }
	if err := router.Register(NameStatus, "status", handler); err != nil {
		t.Fatal(err)
	}
	if err := router.Register(NameStatus, "status again", handler); err == nil {
		t.Error("duplicate registration accepted")
	}
	if err := router.Register("", "empty", handler); err == nil {
		t.Error("empty name accepted")
	}
	if err := router.Register(NamePlan, "plan", nil); err == nil {
		t.Error("nil handler accepted")
	}
}

func TestProjectCommandsAnswerFromDurableState(t *testing.T) {
	t.Parallel()
	reader := fixtureReader()
	router := newTestRouter(t, allowAll())
	if err := RegisterProjectCommands(router, reader); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	status, err := router.Dispatch(ctx, Command{Name: NameStatus})
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{"Madar", "Health: on-track", "#43", "developing"} {
		if !strings.Contains(status, fragment) {
			t.Fatalf("/status missing %q:\n%s", fragment, status)
		}
	}

	project, err := router.Dispatch(ctx, Command{Name: NameProject})
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{"owner/repo", "Goal: Ship v2", "1 completed of 3", "on track"} {
		if !strings.Contains(project, fragment) {
			t.Fatalf("/project missing %q:\n%s", fragment, project)
		}
	}

	plan, err := router.Dispatch(ctx, Command{Name: NamePlan})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "1. #41") || !strings.Contains(plan, "release blocker") {
		t.Fatalf("/plan = %s", plan)
	}

	next, err := router.Dispatch(ctx, Command{Name: NameNext})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(next, "#44") || !strings.Contains(next, "Position: 3") {
		t.Fatalf("/next = %s", next)
	}

	logs, err := router.Dispatch(ctx, Command{Name: NameLogs})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs, "task.selected") {
		t.Fatalf("/logs = %s", logs)
	}
}

func TestProjectCommandsHandleAnEmptyInstallation(t *testing.T) {
	t.Parallel()
	router := newTestRouter(t, allowAll())
	if err := RegisterProjectCommands(router, &fakeReader{}); err != nil {
		t.Fatal(err)
	}
	reply, err := router.Dispatch(context.Background(), Command{Name: NameStatus})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "failed") {
		t.Fatalf("reply = %q", reply)
	}

	// A project with no backlog answers plainly rather than erroring.
	empty := &fakeReader{projects: []*domain.Project{fixtureProject()}}
	emptyRouter := newTestRouter(t, allowAll())
	if err := RegisterProjectCommands(emptyRouter, empty); err != nil {
		t.Fatal(err)
	}
	plan, err := emptyRouter.Dispatch(context.Background(), Command{Name: NamePlan})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "no backlog yet") {
		t.Fatalf("/plan = %q", plan)
	}
	next, err := emptyRouter.Dispatch(context.Background(), Command{Name: NameNext})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(next, "nothing queued") {
		t.Fatalf("/next = %q", next)
	}
}

func TestRegisterProjectCommandsValidatesDependencies(t *testing.T) {
	t.Parallel()
	router := newTestRouter(t, allowAll())
	if err := RegisterProjectCommands(nil, fixtureReader()); err == nil {
		t.Error("missing router accepted")
	}
	if err := RegisterProjectCommands(router, nil); err == nil {
		t.Error("missing reader accepted")
	}
}

func newTestRouter(t *testing.T, authorizer Authorizer) *Router {
	t.Helper()
	router, err := NewRouter(authorizer)
	if err != nil {
		t.Fatal(err)
	}
	return router
}

type authorizerFunc func(context.Context, Command) error

func (authorize authorizerFunc) Authorize(ctx context.Context, command Command) error {
	return authorize(ctx, command)
}

func allowAll() Authorizer {
	return authorizerFunc(func(context.Context, Command) error { return nil })
}

func fixtureProject() *domain.Project {
	project := domain.NewProject("owner/repo", "Madar", "Ship v2", "")
	project.ID = 7
	project.Health = domain.HealthOnTrack
	project.State = domain.ProjectExecuting
	project.ReleaseReadiness = "not-ready"
	return project
}

func fixtureReader() *fakeReader {
	project := fixtureProject()
	current := int64(2)
	project.CurrentTaskID = &current

	first := domain.NewTask(7, "Completed work", "goal")
	first.ID = 1
	first.Sequence = 1
	first.IssueNumber = 41
	first.Status = domain.TaskCompleted
	first.BlocksRelease = true

	active := domain.NewTask(7, "Active work", "goal")
	active.ID = 2
	active.Sequence = 2
	active.IssueNumber = 43
	active.Status = domain.TaskDeveloping
	active.BranchName = "madar/issue-43"
	active.PRNumber = 51

	queued := domain.NewTask(7, "Queued work", "Make it work")
	queued.ID = 3
	queued.Sequence = 3
	queued.IssueNumber = 44
	queued.Status = domain.TaskQueued

	review := domain.NewManagerReview(7)
	review.OwnerUpdate = "Project remains on track."

	return &fakeReader{
		projects: []*domain.Project{project},
		tasks:    []*domain.Task{first, active, queued},
		review:   review,
		events: []*domain.WorkflowEvent{
			{Type: domain.WorkflowTaskSelected, Message: "Selected the next task."},
		},
	}
}

type fakeReader struct {
	projects []*domain.Project
	tasks    []*domain.Task
	review   *domain.ManagerReview
	events   []*domain.WorkflowEvent
	err      error
}

func (fake *fakeReader) ListProjects() ([]*domain.Project, error) {
	return fake.projects, fake.err
}

func (fake *fakeReader) ListProjectTasks(int64) ([]*domain.Task, error) {
	return fake.tasks, fake.err
}

func (fake *fakeReader) LatestManagerReview(int64) (*domain.ManagerReview, error) {
	return fake.review, fake.err
}

func (fake *fakeReader) ListWorkflowEvents(
	int64, int64, int,
) ([]*domain.WorkflowEvent, error) {
	return fake.events, fake.err
}
