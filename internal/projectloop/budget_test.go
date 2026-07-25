package projectloop

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	"github.com/eslam-mahmoud/go-ai-agent/internal/policy"
)

type fakeBudgetGuard struct {
	executions []*domain.Execution
	events     []*domain.WorkflowEvent
	listErr    error
}

func (fake *fakeBudgetGuard) ListTaskExecutions(int64) ([]*domain.Execution, error) {
	return fake.executions, fake.listErr
}

func (fake *fakeBudgetGuard) AppendWorkflowEvent(
	event *domain.WorkflowEvent,
) (*domain.WorkflowEvent, bool, error) {
	fake.events = append(fake.events, event)
	return event, true, nil
}

func fixerExecutions(count int) []*domain.Execution {
	executions := make([]*domain.Execution, 0, count)
	for index := 0; index < count; index++ {
		executions = append(executions, &domain.Execution{
			ID: int64(index + 1), Mode: "fixer", Attempt: index + 1,
			Status: domain.ExecutionCompleted,
		})
	}
	return executions
}

// A task that has spent its budget must stop rather than keep burning
// provider runs; the whole point of a budget is that it binds.
func TestExhaustedBudgetBlocksDeliveryAndIsAudited(t *testing.T) {
	current := &domain.Task{ID: 4, Status: domain.TaskSelected, CreatedAt: time.Now().UTC()}
	guard := &fakeBudgetGuard{executions: fixerExecutions(4)}
	delivery := &fakeDelivery{}
	loop := newLoop(t,
		&fakeController{snapshot: snapshotWith(domain.ProjectExecuting, current, current)},
		delivery, &fakeReviewer{},
		Options{
			Budgets:     policy.Budgets{MaxReviewFixCycles: 2},
			BudgetGuard: guard,
		})

	outcome, err := loop.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Action != ActionBlocked || outcome.TaskID != 4 {
		t.Fatalf("outcome = %#v", outcome)
	}
	if delivery.calls != 0 {
		t.Fatal("an exhausted budget must stop delivery")
	}
	if len(guard.events) != 1 || guard.events[0].Type != domain.WorkflowBudgetExhausted {
		t.Fatalf("events = %#v", guard.events)
	}
	if guard.events[0].IdempotencyKey == "" {
		t.Fatal("the budget event needs an idempotency key so ticks do not flood the trail")
	}
}

func TestBudgetWithinLimitsDoesNotBlock(t *testing.T) {
	current := &domain.Task{ID: 4, Status: domain.TaskSelected, CreatedAt: time.Now().UTC()}
	guard := &fakeBudgetGuard{executions: fixerExecutions(1)}
	delivery := &fakeDelivery{result: &workflowResultCompleted}
	loop := newLoop(t,
		&fakeController{snapshot: snapshotWith(domain.ProjectExecuting, current, current)},
		delivery, &fakeReviewer{},
		Options{
			Budgets:     policy.Budgets{MaxReviewFixCycles: 5},
			BudgetGuard: guard,
		})

	outcome, err := loop.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Action != ActionDeliver || delivery.calls != 1 {
		t.Fatalf("outcome = %#v, calls = %d", outcome, delivery.calls)
	}
	if len(guard.events) != 0 {
		t.Fatal("a budget within limits must not record exhaustion")
	}
}

// Unlimited budgets are the historical behaviour, so an upgrade that adds no
// budgets block must not start consulting the execution history.
func TestZeroBudgetsSkipTheGuardEntirely(t *testing.T) {
	current := &domain.Task{ID: 4, Status: domain.TaskSelected}
	guard := &fakeBudgetGuard{listErr: errors.New("must not be called")}
	loop := newLoop(t,
		&fakeController{snapshot: snapshotWith(domain.ProjectExecuting, current, current)},
		&fakeDelivery{result: &workflowResultCompleted}, &fakeReviewer{},
		Options{BudgetGuard: guard})

	if _, err := loop.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
}
