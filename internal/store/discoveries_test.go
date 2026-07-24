package store

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

func TestCreateDiscoveriesPersistsBatchAndAudits(t *testing.T) {
	s := openTestStore(t)
	project, task, execution := discoveryFixture(t, s, "owner/main")

	first := domain.NewDiscovery(
		project.ID, task.ID, execution.ID,
		"Retry budget is unbounded", domain.DiscoveryBug, domain.SeverityHigh,
	)
	first.Description = "The client retries forever on 5xx."
	first.BlocksCurrent = true
	first.SuggestedAction = string(first.RecommendAction())
	second := domain.NewDiscovery(
		project.ID, task.ID, execution.ID,
		"No integration test for resume", domain.DiscoveryTesting, domain.SeverityMedium,
	)

	stored, err := s.CreateDiscoveries(
		project.ID,
		[]*domain.Discovery{first, second},
		"execution:1:discoveries",
	)
	if err != nil {
		t.Fatalf("CreateDiscoveries: %v", err)
	}
	if len(stored.Created) != 2 || stored.Created[0].ID <= 0 || stored.Created[1].ID <= 0 {
		t.Fatalf("stored = %#v", stored)
	}
	if len(stored.Duplicates) != 0 {
		t.Fatalf("duplicates = %#v", stored.Duplicates)
	}
	if stored.Created[0].Status != domain.DiscoveryUnevaluated ||
		stored.Created[0].SuggestedAction != string(domain.ActionFixInCurrentTask) ||
		!stored.Created[0].BlocksCurrent ||
		stored.Created[0].SourceTaskID != task.ID ||
		stored.Created[0].SourceExecutionID != execution.ID {
		t.Fatalf("first = %#v", stored.Created[0])
	}
	if stored.Created[0].CreatedAt.IsZero() || stored.Created[0].UpdatedAt.IsZero() {
		t.Fatal("timestamps were not set")
	}

	listed, err := s.ListDiscoveries(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 {
		t.Fatalf("listed %d discoveries", len(listed))
	}
	pending, err := s.ListUnevaluatedDiscoveries(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("pending = %d", len(pending))
	}

	events, err := s.ListWorkflowEvents(project.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != domain.WorkflowDiscoveriesRecorded {
		t.Fatalf("events = %#v", events)
	}
	var evidence struct {
		CreatedCount int     `json:"created_count"`
		DiscoveryIDs []int64 `json:"discovery_ids"`
	}
	if err := json.Unmarshal(events[0].Data, &evidence); err != nil {
		t.Fatal(err)
	}
	if evidence.CreatedCount != 2 || len(evidence.DiscoveryIDs) != 2 {
		t.Fatalf("evidence = %#v", evidence)
	}

	// The same execution reporting again must not duplicate the audit fact.
	if _, err := s.CreateDiscoveries(
		project.ID,
		[]*domain.Discovery{domain.NewDiscovery(
			project.ID, task.ID, execution.ID,
			"Repeat", domain.DiscoveryBug, domain.SeverityLow,
		)},
		"execution:1:discoveries",
	); err != nil {
		t.Fatal(err)
	}
	events, _ = s.ListWorkflowEvents(project.ID, 0, 100)
	if len(events) != 1 {
		t.Fatalf("idempotent batch emitted %d events", len(events))
	}
}

func TestCreateDiscoveriesEmptyBatchIsNoOp(t *testing.T) {
	s := openTestStore(t)
	project, _, _ := discoveryFixture(t, s, "owner/main")
	stored, err := s.CreateDiscoveries(project.ID, nil, "key")
	if err != nil || stored != nil {
		t.Fatalf("stored = %#v, err = %v", stored, err)
	}
	events, _ := s.ListWorkflowEvents(project.ID, 0, 100)
	if len(events) != 0 {
		t.Fatalf("empty batch emitted %d events", len(events))
	}
}

func TestCreateDiscoveriesRejectsInvalidBatchWithoutPartialWrites(t *testing.T) {
	s := openTestStore(t)
	project, task, execution := discoveryFixture(t, s, "owner/main")
	otherProject, otherTask, _ := discoveryFixture(t, s, "owner/other")

	valid := func() *domain.Discovery {
		return domain.NewDiscovery(
			project.ID, task.ID, execution.ID,
			"Valid", domain.DiscoveryBug, domain.SeverityLow,
		)
	}
	tests := []struct {
		name  string
		batch []*domain.Discovery
		want  error
	}{
		{
			"invalid member",
			[]*domain.Discovery{valid(), {ProjectID: project.ID, Title: ""}},
			domain.ErrInvalidDiscovery,
		},
		{
			"preset ID",
			[]*domain.Discovery{func() *domain.Discovery {
				discovery := valid()
				discovery.ID = 5
				return discovery
			}()},
			domain.ErrInvalidDiscovery,
		},
		{
			"foreign project",
			[]*domain.Discovery{valid(), domain.NewDiscovery(
				otherProject.ID, otherTask.ID, 0,
				"Foreign", domain.DiscoveryBug, domain.SeverityLow,
			)},
			ErrDiscoveryOwnership,
		},
		{
			"foreign task",
			[]*domain.Discovery{domain.NewDiscovery(
				project.ID, otherTask.ID, 0,
				"Foreign task", domain.DiscoveryBug, domain.SeverityLow,
			)},
			ErrDiscoveryOwnership,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			if _, err := s.CreateDiscoveries(project.ID, test.batch, ""); !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			listed, err := s.ListDiscoveries(project.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(listed) != 0 {
				t.Fatalf("rejected batch persisted %d discoveries", len(listed))
			}
		})
	}
	if _, err := s.CreateDiscoveries(0, []*domain.Discovery{valid()}, ""); !errors.Is(
		err, domain.ErrInvalidDiscovery,
	) {
		t.Fatalf("missing project error = %v", err)
	}
	if _, err := s.CreateDiscoveries(9999, []*domain.Discovery{
		domain.NewDiscovery(9999, 0, 0, "Orphan", domain.DiscoveryBug, domain.SeverityLow),
	}, ""); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("unknown project error = %v", err)
	}
}

func TestDiscoveryLookupsEnforceExistence(t *testing.T) {
	s := openTestStore(t)
	project, task, execution := discoveryFixture(t, s, "owner/main")
	stored, err := s.CreateDiscoveries(project.ID, []*domain.Discovery{
		domain.NewDiscovery(
			project.ID, task.ID, execution.ID,
			"Only", domain.DiscoveryBug, domain.SeverityLow,
		),
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	found, err := s.GetDiscoveryByID(stored.Created[0].ID)
	if err != nil || found == nil || found.Title != "Only" {
		t.Fatalf("found = %#v, err = %v", found, err)
	}
	if missing, err := s.GetDiscoveryByID(9999); err != nil || missing != nil {
		t.Fatalf("missing = %#v, err = %v", missing, err)
	}
	if _, err := s.GetDiscoveryByID(0); !errors.Is(err, ErrDiscoveryNotFound) {
		t.Fatalf("zero ID error = %v", err)
	}
	if _, err := s.ListDiscoveries(9999); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("unknown project list error = %v", err)
	}
}

func discoveryFixture(
	t *testing.T,
	s *Store,
	repo string,
) (*domain.Project, *domain.Task, *domain.Execution) {
	t.Helper()
	project, err := s.CreateProject(domain.NewProject(repo, "Madar", "Goal", ""))
	if err != nil {
		t.Fatal(err)
	}
	task := domain.NewTask(project.ID, "Task", "Goal")
	task.Status = domain.TaskQueued
	task, err = s.CreateProjectTask(task)
	if err != nil {
		t.Fatal(err)
	}
	execution, err := s.CreateExecution(
		domain.NewExecution(project.ID, task.ID, "developer", "codex", "gpt-test", 1),
	)
	if err != nil {
		t.Fatal(err)
	}
	return project, task, execution
}

func TestCreateDiscoveriesDeduplicatesWithinAndAcrossBatches(t *testing.T) {
	s := openTestStore(t)
	project, task, execution := discoveryFixture(t, s, "owner/main")

	sighting := func(title string) *domain.Discovery {
		return domain.NewDiscovery(
			project.ID, task.ID, execution.ID,
			title, domain.DiscoveryBug, domain.SeverityHigh,
		)
	}
	// The same finding twice in one batch, plus a distinct one.
	first, err := s.CreateDiscoveries(project.ID, []*domain.Discovery{
		sighting("Retry budget is unbounded"),
		sighting("  retry budget, is unbounded!  "),
		sighting("Token is logged"),
	}, "")
	if err != nil {
		t.Fatalf("CreateDiscoveries: %v", err)
	}
	if len(first.Created) != 2 || len(first.Duplicates) != 0 {
		t.Fatalf("first batch = %d created, %d duplicates",
			len(first.Created), len(first.Duplicates))
	}
	if first.Created[0].Occurrences != 2 {
		t.Fatalf("within-batch occurrences = %d", first.Created[0].Occurrences)
	}
	if first.Created[0].ExternalID == "" ||
		first.Created[0].ExternalID == first.Created[1].ExternalID {
		t.Fatalf("external IDs = %q, %q",
			first.Created[0].ExternalID, first.Created[1].ExternalID)
	}

	// A later execution reporting the same finding must not create a row.
	second, err := s.CreateDiscoveries(project.ID, []*domain.Discovery{
		sighting("RETRY BUDGET IS UNBOUNDED."),
	}, "")
	if err != nil {
		t.Fatalf("second CreateDiscoveries: %v", err)
	}
	if len(second.Created) != 0 || len(second.Duplicates) != 1 {
		t.Fatalf("second batch = %d created, %d duplicates",
			len(second.Created), len(second.Duplicates))
	}
	if second.Duplicates[0].ID != first.Created[0].ID {
		t.Fatalf("duplicate resolved to discovery %d", second.Duplicates[0].ID)
	}
	if second.Duplicates[0].Occurrences != 3 {
		t.Fatalf("occurrences = %d, want 3", second.Duplicates[0].Occurrences)
	}
	listed, _ := s.ListDiscoveries(project.ID)
	if len(listed) != 2 {
		t.Fatalf("stored %d discoveries, want 2", len(listed))
	}

	byExternal, err := s.GetDiscoveryByExternalID(project.ID, first.Created[0].ExternalID)
	if err != nil || byExternal == nil || byExternal.ID != first.Created[0].ID {
		t.Fatalf("GetDiscoveryByExternalID = %#v, err = %v", byExternal, err)
	}
	if missing, err := s.GetDiscoveryByExternalID(project.ID, "disc-absent"); err != nil ||
		missing != nil {
		t.Fatalf("missing external ID = %#v, err = %v", missing, err)
	}
}

func TestDuplicateDiscoveryPreservesRecordedDecision(t *testing.T) {
	s := openTestStore(t)
	project, task, execution := discoveryFixture(t, s, "owner/main")
	decided := domain.NewDiscovery(
		project.ID, task.ID, execution.ID,
		"Retry budget is unbounded", domain.DiscoveryBug, domain.SeverityHigh,
	)
	decided.Status = domain.DiscoveryRejected
	decided.DecisionReason = "Out of scope for this release"
	stored, err := s.CreateDiscoveries(project.ID, []*domain.Discovery{decided}, "")
	if err != nil {
		t.Fatal(err)
	}

	repeat := domain.NewDiscovery(
		project.ID, task.ID, execution.ID,
		"retry budget is unbounded", domain.DiscoveryBug, domain.SeverityCritical,
	)
	batch, err := s.CreateDiscoveries(project.ID, []*domain.Discovery{repeat}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Duplicates) != 1 {
		t.Fatalf("duplicates = %d", len(batch.Duplicates))
	}
	kept := batch.Duplicates[0]
	if kept.ID != stored.Created[0].ID ||
		kept.Status != domain.DiscoveryRejected ||
		kept.DecisionReason != "Out of scope for this release" ||
		kept.Severity != domain.SeverityHigh {
		t.Fatalf("duplicate overwrote the decision: %#v", kept)
	}
	if kept.Occurrences != 2 {
		t.Fatalf("occurrences = %d", kept.Occurrences)
	}
}

func TestCreateDiscoveriesLinksMatchingBacklogTask(t *testing.T) {
	s := openTestStore(t)
	project, task, execution := discoveryFixture(t, s, "owner/main")

	matching := domain.NewDiscovery(
		project.ID, task.ID, execution.ID,
		"  task!  ", domain.DiscoveryBug, domain.SeverityLow,
	)
	unmatched := domain.NewDiscovery(
		project.ID, task.ID, execution.ID,
		"Something else entirely", domain.DiscoveryBug, domain.SeverityLow,
	)
	batch, err := s.CreateDiscoveries(
		project.ID,
		[]*domain.Discovery{matching, unmatched},
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	if batch.Created[0].LinkedTaskID == nil || *batch.Created[0].LinkedTaskID != task.ID {
		t.Fatalf("linked task = %#v", batch.Created[0].LinkedTaskID)
	}
	if batch.Created[1].LinkedTaskID != nil {
		t.Fatalf("unrelated discovery linked to %#v", batch.Created[1].LinkedTaskID)
	}

	// A terminal task cannot absorb new work.
	task.Status = domain.TaskCompleted
	if _, err := s.UpdateProjectTask(task); err != nil {
		t.Fatal(err)
	}
	terminal := domain.NewDiscovery(
		project.ID, task.ID, execution.ID,
		"Task", domain.DiscoveryTesting, domain.SeverityLow,
	)
	later, err := s.CreateDiscoveries(project.ID, []*domain.Discovery{terminal}, "")
	if err != nil {
		t.Fatal(err)
	}
	if later.Created[0].LinkedTaskID != nil {
		t.Fatalf("linked to terminal task %#v", later.Created[0].LinkedTaskID)
	}
}

func TestDiscoveryExternalIDIsUniquePerProject(t *testing.T) {
	s := openTestStore(t)
	project, task, execution := discoveryFixture(t, s, "owner/main")
	other, otherTask, _ := discoveryFixture(t, s, "owner/other")

	shared := "disc-shared-identity"
	first := domain.NewDiscovery(
		project.ID, task.ID, execution.ID,
		"Shared", domain.DiscoveryBug, domain.SeverityLow,
	)
	first.ExternalID = shared
	if _, err := s.CreateDiscoveries(project.ID, []*domain.Discovery{first}, ""); err != nil {
		t.Fatal(err)
	}

	// The same identity in a different project is a different finding.
	second := domain.NewDiscovery(
		other.ID, otherTask.ID, 0,
		"Shared", domain.DiscoveryBug, domain.SeverityLow,
	)
	second.ExternalID = shared
	if _, err := s.CreateDiscoveries(other.ID, []*domain.Discovery{second}, ""); err != nil {
		t.Fatalf("cross-project identity rejected: %v", err)
	}

	listed, _ := s.ListDiscoveries(project.ID)
	if len(listed) != 1 {
		t.Fatalf("project has %d discoveries", len(listed))
	}
}
