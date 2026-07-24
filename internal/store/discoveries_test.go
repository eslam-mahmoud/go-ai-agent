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
	if len(stored) != 2 || stored[0].ID <= 0 || stored[1].ID <= 0 {
		t.Fatalf("stored = %#v", stored)
	}
	if stored[0].Status != domain.DiscoveryUnevaluated ||
		stored[0].SuggestedAction != string(domain.ActionFixInCurrentTask) ||
		!stored[0].BlocksCurrent ||
		stored[0].SourceTaskID != task.ID ||
		stored[0].SourceExecutionID != execution.ID {
		t.Fatalf("first = %#v", stored[0])
	}
	if stored[0].CreatedAt.IsZero() || stored[0].UpdatedAt.IsZero() {
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
		Count        int     `json:"count"`
		DiscoveryIDs []int64 `json:"discovery_ids"`
	}
	if err := json.Unmarshal(events[0].Data, &evidence); err != nil {
		t.Fatal(err)
	}
	if evidence.Count != 2 || len(evidence.DiscoveryIDs) != 2 {
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
	found, err := s.GetDiscoveryByID(stored[0].ID)
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
