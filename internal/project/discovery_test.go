package project

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
)

func TestDiscoveryControllerAppliesEveryVerdict(t *testing.T) {
	t.Parallel()
	fixture := newReviewFixture(t)
	pending := fixture.recordDiscoveries(t, "Retry budget is unbounded", "Token is logged")
	review := fixture.review(t, []map[string]any{
		{
			"discovery_id": pending[0].ID,
			"decision":     "create-next-task",
			"reason":       "Blocks the MVP",
		},
		{
			"discovery_id": pending[1].ID,
			"decision":     "merge-into-existing-task",
			"task_id":      fixture.tasks[1].ID,
			"reason":       "Same area as the queued task",
		},
	})
	controller, err := NewDiscoveryController(fixture.store)
	if err != nil {
		t.Fatal(err)
	}
	result, err := controller.ApplyManagerReview(fixture.projectID, review.ID)
	if err != nil {
		t.Fatalf("ApplyManagerReview: %v", err)
	}
	if len(result.Decided) != 2 {
		t.Fatalf("decided %d discoveries", len(result.Decided))
	}
	if result.Decided[0].Status != domain.DiscoveryAccepted ||
		result.Decided[0].Decision != domain.DecisionCreateNextTask ||
		result.Decided[0].DecisionReason != "Blocks the MVP" {
		t.Fatalf("first = %#v", result.Decided[0])
	}
	merged := result.Decided[1]
	if merged.Status != domain.DiscoveryMerged ||
		merged.LinkedTaskID == nil ||
		*merged.LinkedTaskID != fixture.tasks[1].ID {
		t.Fatalf("second = %#v", merged)
	}
	if remaining, _ := fixture.store.ListUnevaluatedDiscoveries(fixture.projectID); len(
		remaining,
	) != 0 {
		t.Fatalf("%d discoveries remain unevaluated", len(remaining))
	}

	events, _ := fixture.store.ListWorkflowEvents(fixture.projectID, 0, 100)
	decided := 0
	for _, event := range events {
		if event.Type == domain.WorkflowDiscoveriesDecided {
			decided++
		}
	}
	if decided != 1 {
		t.Fatalf("emitted %d decision events", decided)
	}

	// Replaying the same review must not double-apply or duplicate the fact.
	again, err := controller.ApplyManagerReview(fixture.projectID, review.ID)
	if err != nil {
		t.Fatalf("replayed ApplyManagerReview: %v", err)
	}
	if len(again.Decided) != 0 {
		t.Fatalf("replay re-decided %d discoveries", len(again.Decided))
	}
}

func TestDiscoveryControllerRequiresEveryPendingDiscoveryToBeEvaluated(t *testing.T) {
	t.Parallel()
	fixture := newReviewFixture(t)
	pending := fixture.recordDiscoveries(t, "First finding", "Second finding")
	review := fixture.review(t, []map[string]any{
		{"discovery_id": pending[0].ID, "decision": "defer", "reason": "Later"},
	})
	controller, _ := NewDiscoveryController(fixture.store)
	if _, err := controller.ApplyManagerReview(fixture.projectID, review.ID); !errors.Is(
		err, ErrInvalidDiscoveryDecisions,
	) {
		t.Fatalf("error = %v", err)
	}
	// Nothing may be applied when the set is incomplete.
	remaining, _ := fixture.store.ListUnevaluatedDiscoveries(fixture.projectID)
	if len(remaining) != 2 {
		t.Fatalf("%d discoveries remain unevaluated, want 2", len(remaining))
	}
}

func TestDiscoveryControllerRejectsUnusableDecisions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		build   func(*reviewFixture, []*domain.Discovery) []map[string]any
		wantErr error
	}{
		{
			"unknown decision",
			func(_ *reviewFixture, pending []*domain.Discovery) []map[string]any {
				return []map[string]any{
					{"discovery_id": pending[0].ID, "decision": "vibes", "reason": "why"},
				}
			},
			ErrInvalidDiscoveryDecisions,
		},
		{
			"missing reason",
			func(_ *reviewFixture, pending []*domain.Discovery) []map[string]any {
				return []map[string]any{
					{"discovery_id": pending[0].ID, "decision": "defer", "reason": "   "},
				}
			},
			ErrInvalidDiscoveryDecisions,
		},
		{
			"unknown discovery",
			func(_ *reviewFixture, _ []*domain.Discovery) []map[string]any {
				return []map[string]any{
					{"discovery_id": 4040, "decision": "defer", "reason": "why"},
				}
			},
			ErrInvalidDiscoveryDecisions,
		},
		{
			"duplicate decisions",
			func(_ *reviewFixture, pending []*domain.Discovery) []map[string]any {
				return []map[string]any{
					{"discovery_id": pending[0].ID, "decision": "defer", "reason": "one"},
					{"discovery_id": pending[0].ID, "decision": "reject-out-of-scope", "reason": "two"},
				}
			},
			ErrInvalidDiscoveryDecisions,
		},
		{
			"merge without a task",
			func(_ *reviewFixture, pending []*domain.Discovery) []map[string]any {
				return []map[string]any{
					{
						"discovery_id": pending[0].ID,
						"decision":     "merge-into-existing-task",
						"reason":       "same area",
					},
				}
			},
			ErrInvalidDiscoveryDecisions,
		},
		{
			"merge into a terminal task",
			func(fixture *reviewFixture, pending []*domain.Discovery) []map[string]any {
				return []map[string]any{
					{
						"discovery_id": pending[0].ID,
						"decision":     "merge-into-existing-task",
						"task_id":      fixture.tasks[0].ID,
						"reason":       "same area",
					},
				}
			},
			store.ErrDiscoveryDecisionConflict,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			fixture := newReviewFixture(t)
			fixture.completeFirstTask(t)
			pending := fixture.recordDiscoveries(t, "Only finding")
			review := fixture.review(t, test.build(fixture, pending))
			controller, _ := NewDiscoveryController(fixture.store)
			if _, err := controller.ApplyManagerReview(
				fixture.projectID, review.ID,
			); !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
			remaining, _ := fixture.store.ListUnevaluatedDiscoveries(fixture.projectID)
			if len(remaining) != 1 {
				t.Fatalf("%d discoveries remain unevaluated", len(remaining))
			}
		})
	}
}

func TestDiscoveryControllerRejectsMalformedJSONAndStaleReviews(t *testing.T) {
	t.Parallel()
	t.Run("malformed", func(t *testing.T) {
		t.Parallel()
		fixture := newReviewFixture(t)
		fixture.recordDiscoveries(t, "Finding")
		review := fixture.reviewRaw(t, json.RawMessage(
			`[{"discovery_id":1,"decision":"defer","reason":"x","extra":true}]`,
		))
		controller, _ := NewDiscoveryController(fixture.store)
		if _, err := controller.ApplyManagerReview(
			fixture.projectID, review.ID,
		); !errors.Is(err, ErrInvalidDiscoveryDecisions) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("stale review", func(t *testing.T) {
		t.Parallel()
		fixture := newReviewFixture(t)
		pending := fixture.recordDiscoveries(t, "Finding")
		stale := fixture.review(t, []map[string]any{
			{"discovery_id": pending[0].ID, "decision": "defer", "reason": "later"},
		})
		fixture.review(t, nil)
		controller, _ := NewDiscoveryController(fixture.store)
		if _, err := controller.ApplyManagerReview(
			fixture.projectID, stale.ID,
		); !errors.Is(err, ErrStaleManagerReview) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("bad identifiers", func(t *testing.T) {
		t.Parallel()
		fixture := newReviewFixture(t)
		controller, _ := NewDiscoveryController(fixture.store)
		for _, ids := range [][2]int64{{0, 1}, {1, 0}} {
			if _, err := controller.ApplyManagerReview(ids[0], ids[1]); !errors.Is(
				err, ErrInvalidDiscoveryDecisions,
			) {
				t.Fatalf("ids %v error = %v", ids, err)
			}
		}
		if _, err := NewDiscoveryController(nil); err == nil {
			t.Fatal("nil store accepted")
		}
	})
}

func TestDiscoveryControllerWithNoPendingDiscoveriesIsANoOp(t *testing.T) {
	t.Parallel()
	fixture := newReviewFixture(t)
	review := fixture.review(t, nil)
	controller, _ := NewDiscoveryController(fixture.store)
	result, err := controller.ApplyManagerReview(fixture.projectID, review.ID)
	if err != nil {
		t.Fatalf("ApplyManagerReview: %v", err)
	}
	if len(result.Decided) != 0 {
		t.Fatalf("decided = %#v", result.Decided)
	}
	events, _ := fixture.store.ListWorkflowEvents(fixture.projectID, 0, 100)
	for _, event := range events {
		if event.Type == domain.WorkflowDiscoveriesDecided {
			t.Fatal("no-op emitted a decision event")
		}
	}
}
