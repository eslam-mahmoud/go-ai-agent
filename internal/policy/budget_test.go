package policy

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

func planBudgets() Budgets {
	// The budgets from the plan's example policy.
	return Budgets{
		MaxTaskDuration:    90 * time.Minute,
		MaxReviewFixCycles: 2,
		MaxCIFixCycles:     3,
		MaxModeRetries:     2,
	}
}

func TestBudgetsStopRunawayWork(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		usage BudgetUsage
		kind  BudgetKind
	}{
		{
			"over the duration budget",
			BudgetUsage{TaskStartedAt: now.Add(-91 * time.Minute), Now: now},
			BudgetTaskDuration,
		},
		{
			"review/fix cycles exhausted",
			BudgetUsage{Now: now, ReviewFixCycles: 2},
			BudgetReviewFix,
		},
		{
			"CI fix cycles exhausted",
			BudgetUsage{Now: now, CIFixCycles: 3},
			BudgetCIFix,
		},
		{
			"mode retries exhausted",
			BudgetUsage{Now: now, ModeRetries: 2},
			BudgetModeRetries,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			result := planBudgets().Evaluate(test.usage)
			if !result.Exhausted || result.Kind != test.kind {
				t.Fatalf("result = %#v, want %q", result, test.kind)
			}
			if result.Reason == "" {
				t.Fatal("an exhausted budget has no reason")
			}
		})
	}
}

func TestBudgetsAllowWorkInsideTheirLimits(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	inside := []BudgetUsage{
		{TaskStartedAt: now.Add(-89 * time.Minute), Now: now},
		{Now: now, ReviewFixCycles: 1},
		{Now: now, CIFixCycles: 2},
		{Now: now, ModeRetries: 1},
	}
	for _, usage := range inside {
		if result := planBudgets().Evaluate(usage); result.Exhausted {
			t.Fatalf("usage %#v was stopped: %#v", usage, result)
		}
	}
	// Exactly at the duration boundary is still allowed; over it is not.
	atLimit := BudgetUsage{TaskStartedAt: now.Add(-90 * time.Minute), Now: now}
	if planBudgets().Evaluate(atLimit).Exhausted {
		t.Fatal("a task exactly at the duration budget was stopped")
	}
}

// A zero budget must be an explicit "unlimited", not an accidental zero.
func TestZeroBudgetsAreUnlimited(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	result := Budgets{}.Evaluate(BudgetUsage{
		TaskStartedAt:   now.Add(-1000 * time.Hour),
		Now:             now,
		ReviewFixCycles: 99,
		CIFixCycles:     99,
		ModeRetries:     99,
	})
	if result.Exhausted {
		t.Fatalf("an unset budget stopped work: %#v", result)
	}
	// A task with no recorded start cannot be aged out.
	if planBudgets().Evaluate(BudgetUsage{Now: now}).Exhausted {
		t.Fatal("a task with no start time was stopped by the duration budget")
	}
}

// Duration is checked first: a task that has run too long should stop even if
// it used few cycles.
func TestDurationIsCheckedBeforeCycles(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	result := planBudgets().Evaluate(BudgetUsage{
		TaskStartedAt:   now.Add(-120 * time.Minute),
		Now:             now,
		ReviewFixCycles: 2,
	})
	if result.Kind != BudgetTaskDuration {
		t.Fatalf("kind = %q, want %q", result.Kind, BudgetTaskDuration)
	}
}

// Usage comes from immutable history, so a restart cannot grant a fresh
// allowance by resetting an in-memory counter.
func TestUsageIsDerivedFromDurableHistory(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, 7, 25, 11, 0, 0, 0, time.UTC)
	now := started.Add(30 * time.Minute)
	executions := []*domain.Execution{
		{Mode: "planner", Status: domain.ExecutionCompleted},
		{Mode: "developer", Status: domain.ExecutionCompleted},
		{Mode: "developer", Status: domain.ExecutionFailed},
		{Mode: "fixer", Status: domain.ExecutionCompleted},
		{Mode: "fixer", Status: domain.ExecutionCompleted},
		// A failed fixer attempt is a retry, not a consumed cycle.
		{Mode: "fixer", Status: domain.ExecutionFailed},
		{Mode: "ci-fixer", Status: domain.ExecutionCompleted},
		nil,
	}
	usage := UsageFromExecutions(executions, started, now)
	if usage.ReviewFixCycles != 2 {
		t.Fatalf("review/fix cycles = %d, want 2", usage.ReviewFixCycles)
	}
	if usage.CIFixCycles != 1 {
		t.Fatalf("CI fix cycles = %d, want 1", usage.CIFixCycles)
	}
	// developer ran twice (one retry) and fixer three times (two retries).
	if usage.ModeRetries != 3 {
		t.Fatalf("mode retries = %d, want 3", usage.ModeRetries)
	}
	if !usage.TaskStartedAt.Equal(started) || !usage.Now.Equal(now) {
		t.Fatalf("usage times = %#v", usage)
	}

	// The same history evaluated after a restart yields the same verdict.
	first := planBudgets().Evaluate(usage)
	second := planBudgets().Evaluate(UsageFromExecutions(executions, started, now))
	if first != second {
		t.Fatalf("verdict changed across a restart: %#v vs %#v", first, second)
	}
	if !first.Exhausted || first.Kind != BudgetReviewFix {
		t.Fatalf("verdict = %#v", first)
	}
}

func TestCollectMetricsDerivesFromStoredState(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	started := now.Add(-10 * time.Minute)
	completed := now.Add(-8 * time.Minute)
	source := &fakeMetricsSource{
		tasks: []*domain.Task{
			{Status: domain.TaskCompleted},
			{Status: domain.TaskCompleted},
			{Status: domain.TaskDeveloping},
			nil,
		},
		executions: []*domain.Execution{
			{
				Mode: "developer", Status: domain.ExecutionCompleted,
				StartedAt: &started, CompletedAt: &completed,
			},
			{Mode: "developer", Status: domain.ExecutionFailed},
			{Mode: "reviewer", Status: domain.ExecutionCompleted},
			nil,
		},
		events: []*domain.WorkflowEvent{
			{
				Type: domain.WorkflowBudgetExhausted,
				Data: json.RawMessage(`{"budget":"max_review_fix_cycles"}`),
			},
			{Type: domain.WorkflowBudgetExhausted, Data: json.RawMessage(`{}`)},
			{Type: domain.WorkflowTaskSelected, Data: json.RawMessage(`{}`)},
		},
	}
	project := &domain.Project{
		ID: 7, Repo: "owner/repo",
		Health: domain.HealthOnTrack, State: domain.ProjectExecuting,
	}
	metrics, err := CollectMetrics(source, project, now)
	if err != nil {
		t.Fatalf("CollectMetrics: %v", err)
	}
	if metrics.TotalTasks != 4 || metrics.TotalExecutions != 4 {
		t.Fatalf("totals = %d tasks, %d executions",
			metrics.TotalTasks, metrics.TotalExecutions)
	}
	if metrics.TasksByStatus["completed"] != 2 ||
		metrics.TasksByStatus["developing"] != 1 {
		t.Fatalf("tasks by status = %#v", metrics.TasksByStatus)
	}
	if metrics.ExecutionsByMode["developer"] != 2 ||
		metrics.ExecutionsByStatus["failed"] != 1 {
		t.Fatalf("executions = %#v / %#v",
			metrics.ExecutionsByMode, metrics.ExecutionsByStatus)
	}
	if metrics.MeanExecutionSeconds != 120 {
		t.Fatalf("mean seconds = %v", metrics.MeanExecutionSeconds)
	}
	if metrics.BudgetExhaustions["max_review_fix_cycles"] != 1 ||
		metrics.BudgetExhaustions["unknown"] != 1 {
		t.Fatalf("budget exhaustions = %#v", metrics.BudgetExhaustions)
	}
	// The snapshot must serialize, since that is how it leaves the process.
	if _, err := json.Marshal(metrics); err != nil {
		t.Fatalf("metrics do not serialize: %v", err)
	}
}

func TestCollectMetricsHandlesEmptyAndInvalidInput(t *testing.T) {
	t.Parallel()
	empty, err := CollectMetrics(&fakeMetricsSource{}, &domain.Project{ID: 7}, time.Time{})
	if err != nil {
		t.Fatalf("CollectMetrics: %v", err)
	}
	if empty.TotalTasks != 0 || empty.MeanExecutionSeconds != 0 {
		t.Fatalf("empty metrics = %#v", empty)
	}
	if empty.CollectedAt.IsZero() {
		t.Fatal("collection time was not set")
	}
	// Empty maps, not nil, so the serialized shape is stable.
	encoded, _ := json.Marshal(empty)
	if !strings.Contains(string(encoded), `"tasks_by_status":{}`) {
		t.Fatalf("encoded metrics = %s", encoded)
	}

	if _, err := CollectMetrics(nil, &domain.Project{ID: 7}, time.Time{}); !errors.Is(
		err, ErrMetrics,
	) {
		t.Fatalf("nil source error = %v", err)
	}
	if _, err := CollectMetrics(&fakeMetricsSource{}, nil, time.Time{}); !errors.Is(
		err, ErrMetrics,
	) {
		t.Fatalf("nil project error = %v", err)
	}
	failing := &fakeMetricsSource{err: errors.New("database is unavailable")}
	if _, err := CollectMetrics(
		failing, &domain.Project{ID: 7}, time.Time{},
	); !errors.Is(err, ErrMetrics) {
		t.Fatalf("source failure error = %v", err)
	}
}

func TestSortedKeysIsStable(t *testing.T) {
	t.Parallel()
	keys := SortedKeys(map[string]int{"c": 1, "a": 2, "b": 3})
	if len(keys) != 3 || keys[0] != "a" || keys[2] != "c" {
		t.Fatalf("keys = %v", keys)
	}
}

type fakeMetricsSource struct {
	tasks      []*domain.Task
	executions []*domain.Execution
	events     []*domain.WorkflowEvent
	err        error
}

func (fake *fakeMetricsSource) ListProjectTasks(int64) ([]*domain.Task, error) {
	return fake.tasks, fake.err
}

func (fake *fakeMetricsSource) ListProjectExecutions(int64) ([]*domain.Execution, error) {
	return fake.executions, fake.err
}

func (fake *fakeMetricsSource) ListWorkflowEvents(
	int64, int64, int,
) ([]*domain.WorkflowEvent, error) {
	return fake.events, fake.err
}
