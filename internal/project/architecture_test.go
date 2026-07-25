package project

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/eslam-mahmoud/go-ai-agent/internal/architecturedocs"
	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
	"github.com/eslam-mahmoud/go-ai-agent/internal/workflow"
)

func TestArchitectureAssessmentFiresFromDiscoveriesAndManager(t *testing.T) {
	t.Parallel()
	t.Run("unresolved discovery", func(t *testing.T) {
		t.Parallel()
		fixture := newReviewFixture(t)
		fixture.architectureRiskDiscovery(t, "Cross-cutting cache change")
		controller := fixture.architectureController(t)
		assessment, err := controller.Assess(fixture.projectID)
		if err != nil {
			t.Fatalf("Assess: %v", err)
		}
		if !assessment.Required || assessment.ManagerRequested ||
			len(assessment.Discoveries) != 1 || !assessment.Recorded {
			t.Fatalf("assessment = %#v", assessment)
		}
		if assessment.Reason == "" {
			t.Fatal("assessment has no reason")
		}

		events, _ := fixture.store.ListWorkflowEvents(fixture.projectID, 0, 100)
		raised := 0
		for _, event := range events {
			if event.Type == domain.WorkflowArchitectureReviewRequired {
				raised++
				var evidence struct {
					ManagerRequested bool    `json:"manager_requested"`
					DiscoveryIDs     []int64 `json:"discovery_ids"`
				}
				if err := json.Unmarshal(event.Data, &evidence); err != nil {
					t.Fatal(err)
				}
				if evidence.ManagerRequested || len(evidence.DiscoveryIDs) != 1 {
					t.Fatalf("evidence = %#v", evidence)
				}
			}
		}
		if raised != 1 {
			t.Fatalf("raised %d obligations", raised)
		}

		// Re-assessing the same outstanding set must not record it again.
		second, err := controller.Assess(fixture.projectID)
		if err != nil {
			t.Fatal(err)
		}
		if !second.Required || second.Recorded {
			t.Fatalf("second assessment = %#v", second)
		}
	})

	t.Run("manager request", func(t *testing.T) {
		t.Parallel()
		fixture := newReviewFixture(t)
		fixture.architectureReview(t)
		controller := fixture.architectureController(t)
		assessment, err := controller.Assess(fixture.projectID)
		if err != nil {
			t.Fatalf("Assess: %v", err)
		}
		if !assessment.Required || !assessment.ManagerRequested ||
			len(assessment.Discoveries) != 0 {
			t.Fatalf("assessment = %#v", assessment)
		}
	})

	t.Run("nothing outstanding", func(t *testing.T) {
		t.Parallel()
		fixture := newReviewFixture(t)
		controller := fixture.architectureController(t)
		assessment, err := controller.Assess(fixture.projectID)
		if err != nil {
			t.Fatalf("Assess: %v", err)
		}
		if assessment.Required || assessment.Recorded || assessment.Reason != "" {
			t.Fatalf("assessment = %#v", assessment)
		}
		events, _ := fixture.store.ListWorkflowEvents(fixture.projectID, 0, 100)
		for _, event := range events {
			if event.Type == domain.WorkflowArchitectureReviewRequired {
				t.Fatal("quiet project raised an obligation")
			}
		}
	})

	t.Run("bad input", func(t *testing.T) {
		t.Parallel()
		fixture := newReviewFixture(t)
		controller := fixture.architectureController(t)
		if _, err := controller.Assess(0); !errors.Is(
			err, ErrInvalidArchitectureAssessment,
		) {
			t.Fatalf("error = %v", err)
		}
		if _, err := NewArchitectureController(nil); err == nil {
			t.Fatal("nil store accepted")
		}
	})
}

func TestUnresolvedArchitectureRiskBlocksNextTaskSelection(t *testing.T) {
	t.Parallel()
	fixture := newReviewFixture(t)
	risk := fixture.architectureRiskDiscovery(t, "Cross-cutting cache change")
	review := createSelectionReview(t, fixture.store, fixture.projectID, func(r *domain.ManagerReview) {
		r.NextTaskID = &fixture.tasks[1].ID
		r.NextTaskReason = "Next dependency"
	})
	controller, err := NewSelectionController(fixture.store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.SelectNextTask(fixture.projectID, review.ID); !errors.Is(
		err, workflow.ErrTransitionPrecondition,
	) {
		t.Fatalf("SelectNextTask error = %v", err)
	}
	blocked, _ := fixture.store.GetProjectTaskByID(fixture.tasks[1].ID)
	if blocked.Status != domain.TaskQueued {
		t.Fatalf("task status = %q", blocked.Status)
	}

	// Resolving the risk clears the obligation and allows work to start.
	fixture.resolveDiscovery(t, risk, domain.DecisionRejectOutOfScope)
	outstanding, err := fixture.store.ListArchitectureRiskDiscoveries(fixture.projectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(outstanding) != 0 {
		t.Fatalf("%d risks remain outstanding", len(outstanding))
	}
	cleared := createSelectionReview(t, fixture.store, fixture.projectID, func(r *domain.ManagerReview) {
		r.NextTaskID = &fixture.tasks[1].ID
		r.NextTaskReason = "Next dependency"
	})
	result, err := controller.SelectNextTask(fixture.projectID, cleared.ID)
	if err != nil {
		t.Fatalf("SelectNextTask after resolution: %v", err)
	}
	if !result.Applied || result.Task.Status != domain.TaskSelected {
		t.Fatalf("result = %#v", result)
	}
}

func TestListArchitectureRiskDiscoveriesTracksResolution(t *testing.T) {
	t.Parallel()
	fixture := newReviewFixture(t)
	// An escalated discovery stays outstanding even without the risk flag.
	escalated := fixture.decidedDiscovery(t,
		"Needs an architect", domain.DecisionRequestArchitecture, domain.SeverityHigh)
	if !escalated.RequiresArchitectureReview() {
		t.Fatal("escalated discovery is not outstanding")
	}
	outstanding, err := fixture.store.ListArchitectureRiskDiscoveries(fixture.projectID)
	if err != nil {
		t.Fatal(err)
	}
	if len(outstanding) != 1 || outstanding[0].ID != escalated.ID {
		t.Fatalf("outstanding = %#v", outstanding)
	}
}

func (fixture *reviewFixture) architectureController(
	t *testing.T,
) *ArchitectureController {
	t.Helper()
	controller, err := NewArchitectureController(fixture.store)
	if err != nil {
		t.Fatal(err)
	}
	return controller
}

// architectureRiskDiscovery records an unevaluated discovery carrying
// architecture risk, which is the state that raises the obligation.
func (fixture *reviewFixture) architectureRiskDiscovery(
	t *testing.T,
	title string,
) *domain.Discovery {
	t.Helper()
	discovery := domain.NewDiscovery(
		fixture.projectID, fixture.tasks[0].ID, 0,
		title, domain.DiscoveryArchitecture, domain.SeverityHigh,
	)
	discovery.ArchitectureRisk = true
	batch, err := fixture.store.CreateDiscoveries(
		fixture.projectID, []*domain.Discovery{discovery}, "",
	)
	if err != nil {
		t.Fatal(err)
	}
	return batch.Created[0]
}

func (fixture *reviewFixture) architectureReview(t *testing.T) *domain.ManagerReview {
	t.Helper()
	review := domain.NewManagerReview(fixture.projectID)
	review.ArchitectureReviewRequired = true
	review.ReleaseReadiness = "not-ready"
	review.OwnerUpdate = "Architecture review requested."
	created, err := fixture.store.CreateManagerReview(review)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func (fixture *reviewFixture) resolveDiscovery(
	t *testing.T,
	discovery *domain.Discovery,
	decision domain.DiscoveryDecision,
) {
	t.Helper()
	status, ok := decision.Status()
	if !ok {
		t.Fatalf("decision %q has no status", decision)
	}
	review := fixture.review(t, []map[string]any{
		{
			"discovery_id": discovery.ID,
			"decision":     string(decision),
			"reason":       "Resolved by the fixture",
		},
	})
	if _, err := fixture.store.ApplyDiscoveryDecisions(store.DiscoveryDecisionUpdate{
		ProjectID:       fixture.projectID,
		ManagerReviewID: review.ID,
		Decisions: []store.DiscoveryDecisionRecord{{
			DiscoveryID: discovery.ID,
			Decision:    decision,
			Status:      status,
			Reason:      "Resolved by the fixture",
		}},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestArchitectureControllerRunsArchitectOverTheTriggeringRisks(t *testing.T) {
	t.Parallel()
	fixture := newReviewFixture(t)
	risk := fixture.architectureRiskDiscovery(t, "Cross-cutting cache change")
	runner := &fakeArchitectRunner{
		output: []byte(`{"status":"completed","architecture_summary":"one writer"}`),
	}
	controller, err := NewArchitectureControllerWithRunner(fixture.store, runner)
	if err != nil {
		t.Fatal(err)
	}
	assessment, proposal, err := controller.RunArchitect(
		context.Background(), fixture.projectID,
	)
	if err != nil {
		t.Fatalf("RunArchitect: %v", err)
	}
	if !assessment.Required || len(proposal) == 0 {
		t.Fatalf("assessment = %#v, proposal = %s", assessment, proposal)
	}
	if runner.calls != 1 || len(runner.lastIDs) != 1 || runner.lastIDs[0] != risk.ID {
		t.Fatalf("runner calls=%d ids=%v", runner.calls, runner.lastIDs)
	}
}

func TestArchitectureControllerSkipsTheRunWhenNothingIsOwed(t *testing.T) {
	t.Parallel()
	fixture := newReviewFixture(t)
	runner := &fakeArchitectRunner{output: []byte(`{"status":"completed"}`)}
	controller, err := NewArchitectureControllerWithRunner(fixture.store, runner)
	if err != nil {
		t.Fatal(err)
	}
	assessment, proposal, err := controller.RunArchitect(
		context.Background(), fixture.projectID,
	)
	if err != nil {
		t.Fatalf("RunArchitect: %v", err)
	}
	if assessment.Required || proposal != nil || runner.calls != 0 {
		t.Fatalf("assessment = %#v, runner calls = %d", assessment, runner.calls)
	}
}

func TestArchitectureControllerSurfacesArchitectFailures(t *testing.T) {
	t.Parallel()
	fixture := newReviewFixture(t)
	fixture.architectureRiskDiscovery(t, "Cross-cutting cache change")
	runner := &fakeArchitectRunner{err: errors.New("engine unavailable")}
	controller, err := NewArchitectureControllerWithRunner(fixture.store, runner)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := controller.RunArchitect(
		context.Background(), fixture.projectID,
	); err == nil {
		t.Fatal("architect failure reported success")
	}
	// Without a runner the obligation is still raised and reported.
	plain := fixture.architectureController(t)
	assessment, proposal, err := plain.RunArchitect(context.Background(), fixture.projectID)
	if err != nil {
		t.Fatalf("RunArchitect without runner: %v", err)
	}
	if !assessment.Required || proposal != nil {
		t.Fatalf("assessment = %#v, proposal = %s", assessment, proposal)
	}
}

type fakeArchitectRunner struct {
	output  json.RawMessage
	err     error
	calls   int
	lastIDs []int64
}

func (fake *fakeArchitectRunner) RunArchitect(
	_ context.Context,
	_ int64,
	outstandingDiscoveryIDs []int64,
) (json.RawMessage, error) {
	fake.calls++
	fake.lastIDs = outstandingDiscoveryIDs
	if fake.err != nil {
		return nil, fake.err
	}
	return fake.output, nil
}

func TestArchitectRunWritesArchitectureDocuments(t *testing.T) {
	t.Parallel()
	fixture := newReviewFixture(t)
	fixture.architectureRiskDiscovery(t, "Cross-cutting cache change")
	proposal := `{"status":"completed","architecture_summary":"One writer.",
		"components":[{"name":"store","responsibility":"Own durable state."}],
		"decisions":[{"title":"Single writer","decision":"Serialize writes.","rationale":"Avoids contention."}]}`
	writer := &fakeDocumentWriter{}
	controller, err := NewArchitectureControllerWithDocuments(
		fixture.store,
		&fakeArchitectRunner{output: []byte(proposal)},
		writer,
	)
	if err != nil {
		t.Fatal(err)
	}
	assessment, raw, err := controller.RunArchitect(context.Background(), fixture.projectID)
	if err != nil {
		t.Fatalf("RunArchitect: %v", err)
	}
	if len(raw) == 0 || writer.calls != 1 {
		t.Fatalf("writer calls = %d", writer.calls)
	}
	if assessment.Documents == nil || len(assessment.Documents.Written) != 1 {
		t.Fatalf("documents = %#v", assessment.Documents)
	}
}

func TestArchitectRunSurfacesDocumentFailures(t *testing.T) {
	t.Parallel()
	fixture := newReviewFixture(t)
	fixture.architectureRiskDiscovery(t, "Cross-cutting cache change")
	controller, err := NewArchitectureControllerWithDocuments(
		fixture.store,
		&fakeArchitectRunner{output: []byte(`{"status":"completed"}`)},
		&fakeDocumentWriter{err: errors.New("disk is full")},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := controller.RunArchitect(
		context.Background(), fixture.projectID,
	); err == nil {
		t.Fatal("document failure reported success")
	}
}

type fakeDocumentWriter struct {
	calls int
	err   error
}

func (fake *fakeDocumentWriter) WriteArchitectureDocuments(
	_ int64,
	proposal json.RawMessage,
) (*architecturedocs.Result, error) {
	fake.calls++
	if fake.err != nil {
		return nil, fake.err
	}
	if len(proposal) == 0 {
		return nil, errors.New("empty proposal")
	}
	return &architecturedocs.Result{Written: []string{"AGENTS.md"}}, nil
}
