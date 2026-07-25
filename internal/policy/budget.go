package policy

import (
	"fmt"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

// Budgets bound how much work one task may consume. They are what stops a
// runaway loop without a human noticing it.
type Budgets struct {
	// MaxTaskDuration bounds wall-clock time on one task. Zero is unlimited.
	MaxTaskDuration time.Duration
	// MaxReviewFixCycles bounds review/fix rounds. Zero is unlimited.
	MaxReviewFixCycles int
	// MaxCIFixCycles bounds CI repair attempts. Zero is unlimited.
	MaxCIFixCycles int
	// MaxModeRetries bounds retries of one mode. Zero is unlimited.
	MaxModeRetries int
}

// BudgetKind names the budget that stopped work.
type BudgetKind string

const (
	BudgetTaskDuration BudgetKind = "max_task_duration"
	BudgetReviewFix    BudgetKind = "max_review_fix_cycles"
	BudgetCIFix        BudgetKind = "max_ci_fix_cycles"
	BudgetModeRetries  BudgetKind = "max_mode_retries"
)

// BudgetUsage is the durable evidence a budget decision is made from. Every
// field is derived from stored facts, never from an in-memory counter, so a
// restart cannot silently grant a fresh allowance.
type BudgetUsage struct {
	TaskStartedAt   time.Time
	Now             time.Time
	ReviewFixCycles int
	CIFixCycles     int
	ModeRetries     int
}

// BudgetResult reports whether work may continue, and what stopped it.
type BudgetResult struct {
	Exhausted bool
	Kind      BudgetKind
	Reason    string
}

// EvaluateBudgets reports the first exhausted budget, if any. Order is
// deliberate: duration first, since a task that has run too long should stop
// regardless of how few cycles it used.
func (budgets Budgets) Evaluate(usage BudgetUsage) BudgetResult {
	if budgets.MaxTaskDuration > 0 && !usage.TaskStartedAt.IsZero() {
		now := usage.Now
		if now.IsZero() {
			now = time.Now().UTC()
		}
		if elapsed := now.Sub(usage.TaskStartedAt); elapsed > budgets.MaxTaskDuration {
			return BudgetResult{
				Exhausted: true,
				Kind:      BudgetTaskDuration,
				Reason: fmt.Sprintf(
					"task has run for %s, over the %s budget",
					elapsed.Round(time.Second), budgets.MaxTaskDuration,
				),
			}
		}
	}
	checks := []struct {
		kind  BudgetKind
		limit int
		used  int
		unit  string
	}{
		{BudgetReviewFix, budgets.MaxReviewFixCycles, usage.ReviewFixCycles, "review/fix cycle"},
		{BudgetCIFix, budgets.MaxCIFixCycles, usage.CIFixCycles, "CI fix cycle"},
		{BudgetModeRetries, budgets.MaxModeRetries, usage.ModeRetries, "mode retry"},
	}
	for _, check := range checks {
		if check.limit > 0 && check.used >= check.limit {
			return BudgetResult{
				Exhausted: true,
				Kind:      check.kind,
				Reason: fmt.Sprintf(
					"used %d of %d %s(s)", check.used, check.limit, check.unit,
				),
			}
		}
	}
	return BudgetResult{}
}

// UsageFromExecutions derives budget usage from immutable execution history,
// which is what makes budgets survive a restart.
func UsageFromExecutions(
	executions []*domain.Execution,
	taskStartedAt time.Time,
	now time.Time,
) BudgetUsage {
	usage := BudgetUsage{TaskStartedAt: taskStartedAt, Now: now}
	attemptsByMode := make(map[string]int)
	for _, execution := range executions {
		if execution == nil {
			continue
		}
		switch execution.Mode {
		case "fixer":
			if execution.Status == domain.ExecutionCompleted {
				usage.ReviewFixCycles++
			}
		case "ci-fixer":
			if execution.Status == domain.ExecutionCompleted {
				usage.CIFixCycles++
			}
		}
		// An attempt beyond the first is a retry of that mode.
		attemptsByMode[execution.Mode]++
	}
	for _, attempts := range attemptsByMode {
		if attempts > 1 {
			usage.ModeRetries += attempts - 1
		}
	}
	return usage
}
