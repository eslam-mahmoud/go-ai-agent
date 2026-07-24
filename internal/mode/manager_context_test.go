package mode

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
)

type managerContextTestLoader struct {
	mu        sync.Mutex
	aggregate *store.ManagerContextAggregate
	err       error
	calls     int
}

func (loader *managerContextTestLoader) LoadManagerContextAggregate(
	int64,
) (*store.ManagerContextAggregate, error) {
	loader.mu.Lock()
	defer loader.mu.Unlock()
	loader.calls++
	return loader.aggregate, loader.err
}

func TestDurableManagerContextProviderBuildsCompleteStableSnapshot(t *testing.T) {
	t.Parallel()
	aggregate := durableManagerAggregate()
	// Deliberately reverse durable collections; output order is canonical.
	aggregate.Tasks[0], aggregate.Tasks[1] = aggregate.Tasks[1], aggregate.Tasks[0]
	aggregate.Executions[0], aggregate.Executions[1] =
		aggregate.Executions[1], aggregate.Executions[0]
	loader := &managerContextTestLoader{aggregate: aggregate}
	workDir := t.TempDir()
	runtimeCalls := 0
	provider, err := NewDurableManagerContextProvider(
		loader,
		ManagerRuntimeContextProviderFunc(func(
			_ context.Context,
			projectID, completedTaskID int64,
		) (ManagerRuntimeContext, error) {
			runtimeCalls++
			if projectID != 7 || completedTaskID != 10 {
				t.Fatalf("runtime IDs = %d/%d", projectID, completedTaskID)
			}
			return ManagerRuntimeContext{WorkDir: workDir, ExecutionID: 99}, nil
		}),
	)
	if err != nil {
		t.Fatalf("NewDurableManagerContextProvider: %v", err)
	}

	got, err := provider.LoadManagerContext(context.Background(), 7, 10)
	if err != nil {
		t.Fatalf("LoadManagerContext: %v", err)
	}
	if got.ProjectID != 7 || got.CompletedTaskID != 10 ||
		got.WorkDir != filepath.Clean(workDir) || got.ExecutionID != 99 {
		t.Fatalf("context = %#v", got)
	}
	var snapshot durableManagerSnapshot
	if err := json.Unmarshal(got.Snapshot, &snapshot); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if snapshot.SchemaVersion != 1 ||
		snapshot.Project.Goal != "Ship v2" ||
		snapshot.Project.Scope != "Sequential delivery" ||
		snapshot.Assessment.Health != domain.HealthOnTrack ||
		snapshot.Assessment.ProgressPercent != 50 ||
		len(snapshot.Plan) != 2 ||
		snapshot.Plan[0].ID != 10 ||
		snapshot.Plan[1].ID != 11 ||
		snapshot.CompletedTask == nil ||
		snapshot.CompletedTask.ID != 10 ||
		snapshot.CurrentTask == nil ||
		snapshot.CurrentTask.ID != 11 ||
		len(snapshot.Dependencies) != 2 ||
		snapshot.LatestTaskResult == nil ||
		snapshot.LatestTaskResult.ID != 102 ||
		len(snapshot.ReviewAndCIResults) != 2 ||
		len(snapshot.CurrentArchitecture) != 1 ||
		len(snapshot.ManagerReviews) != 1 ||
		len(snapshot.HumanComments) != 1 ||
		len(snapshot.WorkflowEvents) != 2 ||
		len(snapshot.PendingDiscoveries) != 0 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if snapshot.RuntimeStatistics.ExecutionCount != 2 ||
		snapshot.RuntimeStatistics.CompletedRuns != 2 ||
		snapshot.RuntimeStatistics.InputTokens != 300 ||
		snapshot.RuntimeStatistics.OutputTokens != 120 ||
		snapshot.RuntimeStatistics.EstimatedCost != 1.5 ||
		snapshot.RuntimeStatistics.TotalDurationMilli != 90000 {
		t.Fatalf("runtime statistics = %#v", snapshot.RuntimeStatistics)
	}
	if len(snapshot.ReleaseRequirements.BlockingTaskIDs) != 1 ||
		snapshot.ReleaseRequirements.BlockingTaskIDs[0] != 11 ||
		len(snapshot.ReleaseRequirements.UnresolvedBlocks) != 1 {
		t.Fatalf("release requirements = %#v", snapshot.ReleaseRequirements)
	}
	if loader.calls != 1 || runtimeCalls != 1 {
		t.Fatalf("loader calls = %d, runtime calls = %d", loader.calls, runtimeCalls)
	}

	again, err := provider.LoadManagerContext(context.Background(), 7, 10)
	if err != nil {
		t.Fatalf("second LoadManagerContext: %v", err)
	}
	if string(got.Snapshot) != string(again.Snapshot) {
		t.Fatalf("snapshot is not deterministic:\n%s\n%s", got.Snapshot, again.Snapshot)
	}
}

func TestDurableManagerContextProviderSupportsTasklessAndEmptyProjects(t *testing.T) {
	t.Parallel()
	aggregate := durableManagerAggregate()
	aggregate.Project.CurrentTaskID = nil
	aggregate.Tasks = nil
	aggregate.Executions = nil
	aggregate.Artifacts = nil
	aggregate.ManagerReviews = nil
	aggregate.WorkflowEvents = nil
	provider := mustDurableManagerContextProvider(t, aggregate)
	got, err := provider.LoadManagerContext(context.Background(), 7, 0)
	if err != nil {
		t.Fatalf("LoadManagerContext: %v", err)
	}
	var snapshot durableManagerSnapshot
	if err := json.Unmarshal(got.Snapshot, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.CompletedTask != nil ||
		snapshot.Assessment.Health != domain.HealthOnTrack ||
		snapshot.Assessment.ProgressPercent != 0 ||
		snapshot.Plan == nil ||
		snapshot.Executions == nil ||
		snapshot.PendingDiscoveries == nil ||
		snapshot.HumanComments == nil {
		t.Fatalf("empty snapshot = %#v", snapshot)
	}
}

func TestDurableManagerContextProviderRejectsInconsistentSnapshots(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		completedTaskID int64
		mutate          func(*store.ManagerContextAggregate)
	}{
		{"nil aggregate", 10, func(aggregate *store.ManagerContextAggregate) {
			*aggregate = store.ManagerContextAggregate{}
		}},
		{"project without ID", 10, func(aggregate *store.ManagerContextAggregate) {
			aggregate.Project.ID = 0
		}},
		{"task ownership", 10, func(aggregate *store.ManagerContextAggregate) {
			aggregate.Tasks[0].ProjectID++
		}},
		{"completed task missing", 404, func(*store.ManagerContextAggregate) {}},
		{"completed task incomplete", 11, func(*store.ManagerContextAggregate) {}},
		{"artifact unknown task", 10, func(aggregate *store.ManagerContextAggregate) {
			unknown := int64(404)
			aggregate.Artifacts[0].TaskID = &unknown
		}},
		{"invalid review", 10, func(aggregate *store.ManagerContextAggregate) {
			aggregate.ManagerReviews[0].ReleaseReadiness = ""
		}},
		{"review ownership", 10, func(aggregate *store.ManagerContextAggregate) {
			aggregate.ManagerReviews[0].ProjectID++
		}},
		{"event ownership", 10, func(aggregate *store.ManagerContextAggregate) {
			aggregate.WorkflowEvents[0].ProjectID++
		}},
		{"event sequence collision", 10, func(aggregate *store.ManagerContextAggregate) {
			aggregate.WorkflowEvents[1].Sequence = aggregate.WorkflowEvents[0].Sequence
		}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			aggregate := durableManagerAggregate()
			test.mutate(aggregate)
			provider := mustDurableManagerContextProvider(t, aggregate)
			if _, err := provider.LoadManagerContext(
				context.Background(),
				7,
				test.completedTaskID,
			); !errors.Is(err, ErrInvalidManagerSnapshot) {
				t.Fatalf("LoadManagerContext error = %v", err)
			}
		})
	}
}

func TestDurableManagerContextProviderDependencyFailures(t *testing.T) {
	t.Parallel()
	loadError := errors.New("database unavailable")
	runtimeError := errors.New("runtime unavailable")
	tests := []struct {
		name    string
		loader  *managerContextTestLoader
		runtime ManagerRuntimeContextProvider
		want    error
	}{
		{
			"loader error",
			&managerContextTestLoader{err: loadError},
			validManagerRuntime(t),
			loadError,
		},
		{
			"runtime error",
			&managerContextTestLoader{aggregate: durableManagerAggregate()},
			ManagerRuntimeContextProviderFunc(func(context.Context, int64, int64) (ManagerRuntimeContext, error) {
				return ManagerRuntimeContext{}, runtimeError
			}),
			runtimeError,
		},
		{
			"empty workdir",
			&managerContextTestLoader{aggregate: durableManagerAggregate()},
			ManagerRuntimeContextProviderFunc(func(context.Context, int64, int64) (ManagerRuntimeContext, error) {
				return ManagerRuntimeContext{}, nil
			}),
			ErrInvalidManagerSnapshot,
		},
		{
			"relative workdir",
			&managerContextTestLoader{aggregate: durableManagerAggregate()},
			ManagerRuntimeContextProviderFunc(func(context.Context, int64, int64) (ManagerRuntimeContext, error) {
				return ManagerRuntimeContext{WorkDir: "relative"}, nil
			}),
			ErrInvalidManagerSnapshot,
		},
		{
			"negative execution",
			&managerContextTestLoader{aggregate: durableManagerAggregate()},
			ManagerRuntimeContextProviderFunc(func(context.Context, int64, int64) (ManagerRuntimeContext, error) {
				return ManagerRuntimeContext{WorkDir: t.TempDir(), ExecutionID: -1}, nil
			}),
			ErrInvalidManagerSnapshot,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			provider, err := NewDurableManagerContextProvider(test.loader, test.runtime)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := provider.LoadManagerContext(
				context.Background(),
				7,
				10,
			); !errors.Is(err, test.want) {
				t.Fatalf("LoadManagerContext error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestDurableManagerContextProviderValidatesRequestAndCancellation(t *testing.T) {
	t.Parallel()
	loader := &managerContextTestLoader{aggregate: durableManagerAggregate()}
	provider, err := NewDurableManagerContextProvider(loader, validManagerRuntime(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, IDs := range [][2]int64{{0, 0}, {7, -1}} {
		if _, err := provider.LoadManagerContext(
			context.Background(),
			IDs[0],
			IDs[1],
		); !errors.Is(err, ErrInvalidManagerSnapshot) {
			t.Fatalf("IDs %v error = %v", IDs, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := provider.LoadManagerContext(ctx, 7, 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled error = %v", err)
	}
	if loader.calls != 0 {
		t.Fatalf("loader called %d times", loader.calls)
	}
}

func TestDurableManagerContextProviderConcurrentReads(t *testing.T) {
	t.Parallel()
	loader := &managerContextTestLoader{aggregate: durableManagerAggregate()}
	provider, err := NewDurableManagerContextProvider(loader, validManagerRuntime(t))
	if err != nil {
		t.Fatal(err)
	}
	const count = 32
	results := make(chan string, count)
	errs := make(chan error, count)
	var group sync.WaitGroup
	for index := 0; index < count; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			contextValue, err := provider.LoadManagerContext(context.Background(), 7, 10)
			if err != nil {
				errs <- err
				return
			}
			results <- string(contextValue.Snapshot)
		}()
	}
	group.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatalf("LoadManagerContext: %v", err)
	}
	var first string
	for result := range results {
		if first == "" {
			first = result
		} else if result != first {
			t.Fatal("concurrent snapshots differ")
		}
	}
	if loader.calls != count {
		t.Fatalf("loader calls = %d, want %d", loader.calls, count)
	}
}

func TestNewDurableManagerContextProviderValidation(t *testing.T) {
	t.Parallel()
	loader := &managerContextTestLoader{aggregate: durableManagerAggregate()}
	runtime := validManagerRuntime(t)
	var nilLoader *managerContextTestLoader
	var nilRuntime *managerRuntimeNilProvider
	var nilRuntimeFunc ManagerRuntimeContextProviderFunc
	tests := []struct {
		name    string
		loader  ManagerContextAggregateLoader
		runtime ManagerRuntimeContextProvider
	}{
		{"nil loader", nil, runtime},
		{"typed nil loader", nilLoader, runtime},
		{"nil runtime", loader, nil},
		{"typed nil runtime", loader, nilRuntime},
		{"typed nil runtime function", loader, nilRuntimeFunc},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if provider, err := NewDurableManagerContextProvider(
				test.loader,
				test.runtime,
			); err == nil || provider != nil {
				t.Fatalf("provider = %#v, error = %v", provider, err)
			}
		})
	}
}

type managerRuntimeNilProvider struct{}

func (*managerRuntimeNilProvider) LoadManagerRuntimeContext(
	context.Context,
	int64,
	int64,
) (ManagerRuntimeContext, error) {
	panic("unexpected call")
}

func mustDurableManagerContextProvider(
	t *testing.T,
	aggregate *store.ManagerContextAggregate,
) *DurableManagerContextProvider {
	t.Helper()
	provider, err := NewDurableManagerContextProvider(
		&managerContextTestLoader{aggregate: aggregate},
		validManagerRuntime(t),
	)
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func validManagerRuntime(t *testing.T) ManagerRuntimeContextProvider {
	t.Helper()
	workDir := t.TempDir()
	return ManagerRuntimeContextProviderFunc(func(
		context.Context,
		int64,
		int64,
	) (ManagerRuntimeContext, error) {
		return ManagerRuntimeContext{WorkDir: workDir, ExecutionID: 99}, nil
	})
}

func durableManagerAggregate() *store.ManagerContextAggregate {
	now := time.Date(2026, 7, 24, 10, 0, 0, 0, time.UTC)
	projectRecord := domain.NewProject(
		"owner/repo",
		"Madar",
		"Ship v2",
		"Sequential delivery",
	)
	projectRecord.ID = 7
	projectRecord.ParentIssueNumber = 67
	projectRecord.State = domain.ProjectExecuting
	projectRecord.ReleaseTarget = "v2.0.0"
	projectRecord.ReleaseReadiness = "not-ready"
	currentTaskID := int64(11)
	projectRecord.CurrentTaskID = &currentTaskID

	completed := domain.NewTask(projectRecord.ID, "Foundation", "Build foundation")
	completed.ID = 10
	completed.IssueNumber = 135
	completed.Sequence = 1
	completed.Status = domain.TaskCompleted
	completed.DependencyState = "satisfied"
	current := domain.NewTask(projectRecord.ID, "Manager context", "Build context")
	current.ID = 11
	current.IssueNumber = 137
	current.Sequence = 2
	current.Status = domain.TaskReviewing
	current.DependencyState = "ready"
	current.BlocksRelease = true

	firstExecution := domain.NewExecution(7, 10, "reviewer", "codex", "gpt-test", 1)
	firstExecution.ID = 101
	firstExecution.Status = domain.ExecutionCompleted
	firstStarted := now.Add(-2 * time.Minute)
	firstCompleted := firstStarted.Add(time.Minute)
	firstExecution.StartedAt = &firstStarted
	firstExecution.CompletedAt = &firstCompleted
	firstExecution.InputTokens = 100
	firstExecution.OutputTokens = 40
	firstExecution.EstimatedCost = .5
	secondExecution := domain.NewExecution(7, 10, "verifier", "codex", "gpt-test", 1)
	secondExecution.ID = 102
	secondExecution.Status = domain.ExecutionCompleted
	secondStarted := now.Add(-time.Minute)
	secondCompleted := secondStarted.Add(30 * time.Second)
	secondExecution.StartedAt = &secondStarted
	secondExecution.CompletedAt = &secondCompleted
	secondExecution.InputTokens = 200
	secondExecution.OutputTokens = 80
	secondExecution.EstimatedCost = 1

	architecture := domain.NewArtifact(
		7,
		"architecture",
		"Architecture",
		"docs/architecture.md",
		"text/markdown",
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		256,
	)
	architecture.ID = 201
	architecture.CreatedAt = now
	architecture.TaskID = &completed.ID
	architecture.ExecutionID = &secondExecution.ID
	result := domain.NewArtifact(
		7,
		"verification",
		"Verification",
		"results/verification.json",
		"application/json",
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		128,
	)
	result.ID = 202
	result.CreatedAt = now
	result.TaskID = &completed.ID
	result.ExecutionID = &secondExecution.ID

	review := domain.NewManagerReview(7)
	review.ID = 301
	review.CompletedTaskID = &completed.ID
	review.ProjectHealth = domain.HealthOnTrack
	review.ProgressEstimate = 40
	review.CompletedTaskDecision = domain.TaskDecisionAccepted
	review.ReleaseReadiness = "not-ready"
	review.OwnerUpdate = "Continue."
	review.ReviewedAt = now.Add(-time.Hour)

	transition := domain.NewWorkflowEvent(
		7,
		domain.WorkflowSourceController,
		domain.WorkflowTaskTransitioned,
		"Task completed.",
	)
	transition.ID = 401
	transition.TaskID = &completed.ID
	transition.Sequence = 1
	transition.CreatedAt = now.Add(-time.Minute)
	comment := domain.NewWorkflowEvent(
		7,
		domain.WorkflowSourceExternal,
		domain.WorkflowEventType("human.comment"),
		"Preserve compatibility.",
	)
	comment.ID = 402
	comment.Sequence = 2
	comment.CreatedAt = now

	return &store.ManagerContextAggregate{
		Project:        projectRecord,
		Tasks:          []*domain.Task{completed, current},
		Executions:     []*domain.Execution{firstExecution, secondExecution},
		Artifacts:      []*domain.Artifact{architecture, result},
		ManagerReviews: []*domain.ManagerReview{review},
		WorkflowEvents: []*domain.WorkflowEvent{transition, comment},
	}
}

var _ ManagerContextAggregateLoader = (*managerContextTestLoader)(nil)
var _ ManagerRuntimeContextProvider = (*managerRuntimeNilProvider)(nil)
