package mode

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

type fakeDeliveryStore struct {
	project *domain.Project
	task    *domain.Task
	backlog []*domain.Task
}

func (fake *fakeDeliveryStore) GetProjectByID(id int64) (*domain.Project, error) {
	if fake.project == nil || fake.project.ID != id {
		return nil, nil
	}
	return fake.project, nil
}

func (fake *fakeDeliveryStore) GetProjectTaskByID(id int64) (*domain.Task, error) {
	if fake.task == nil || fake.task.ID != id {
		return nil, nil
	}
	return fake.task, nil
}

func (fake *fakeDeliveryStore) ListProjectTasks(int64) ([]*domain.Task, error) {
	return fake.backlog, nil
}

// fakeOutputs records only what has been explicitly stored, so a mode reading
// an output that was never produced fails the same way it would in production.
type fakeOutputs struct {
	stored map[string][]json.RawMessage
}

func (fake *fakeOutputs) LatestOutput(_ int64, mode string) (json.RawMessage, error) {
	queued := fake.stored[mode]
	if len(queued) == 0 {
		return nil, errors.New("no recorded mode output")
	}
	return queued[len(queued)-1], nil
}

func (fake *fakeOutputs) Outputs(_ int64, mode string) ([]json.RawMessage, error) {
	return fake.stored[mode], nil
}

type fakeExecutions struct {
	opened []string
	nextID int64
}

func (fake *fakeExecutions) Begin(
	projectID, taskID int64, mode string,
) (*domain.Execution, error) {
	fake.opened = append(fake.opened, mode)
	fake.nextID++
	return &domain.Execution{
		ID: fake.nextID, ProjectID: projectID, TaskID: taskID, Mode: mode,
	}, nil
}

type fakeWorkspaces struct{ path string }

func (fake fakeWorkspaces) ProjectWorkspace(string) (string, error) {
	return fake.path, nil
}

type fakeCI struct{ status VerificationCIStatus }

func (fake fakeCI) TaskCIStatus(
	context.Context, *domain.Task,
) (VerificationCIStatus, error) {
	return fake.status, nil
}

func newDeliveryProvider(
	t *testing.T, stored map[string][]json.RawMessage, options DeliveryContextOptions,
) (*DurableDeliveryContextProvider, *fakeExecutions) {
	t.Helper()
	if stored == nil {
		stored = map[string][]json.RawMessage{}
	}
	store := &fakeDeliveryStore{
		project: &domain.Project{ID: 1, Repo: "owner/repo", Name: "Madar"},
		task:    &domain.Task{ID: 2, ProjectID: 1, Title: "Ship it", BranchName: "madar/issue-2", PRNumber: 5},
	}
	store.backlog = []*domain.Task{store.task}
	executions := &fakeExecutions{}
	provider, err := NewDurableDeliveryContextProvider(
		store, &fakeOutputs{stored: stored}, executions,
		fakeWorkspaces{path: "/workspaces/owner/repo"}, options,
	)
	if err != nil {
		t.Fatal(err)
	}
	return provider, executions
}

func TestPlannerContextNeedsNoPriorOutput(t *testing.T) {
	provider, executions := newDeliveryProvider(t, nil, DeliveryContextOptions{})
	loaded, err := provider.LoadPlannerContext(context.Background(), 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.WorkDir != "/workspaces/owner/repo" || loaded.ExecutionID != 1 {
		t.Fatalf("context = %#v", loaded)
	}
	if len(loaded.Backlog) != 1 {
		t.Fatalf("backlog = %v", loaded.Backlog)
	}
	if len(executions.opened) != 1 || executions.opened[0] != "planner" {
		t.Fatalf("opened = %v", executions.opened)
	}
}

// Each mode must refuse to run when an input it depends on was never
// produced. Silently substituting an empty plan would let the developer
// invent work the planner never approved.
func TestDeliveryModesRequireTheirInputs(t *testing.T) {
	plan := json.RawMessage(`{"plan":true}`)
	delivery := json.RawMessage(`{"delivery":true}`)
	review := json.RawMessage(`{"review":true}`)

	tests := []struct {
		name    string
		stored  map[string][]json.RawMessage
		load    func(*DurableDeliveryContextProvider) error
		wantErr bool
	}{
		{
			name:   "developer without a plan",
			stored: nil,
			load: func(provider *DurableDeliveryContextProvider) error {
				_, err := provider.LoadDeveloperContext(context.Background(), 1, 2)
				return err
			},
			wantErr: true,
		},
		{
			name:   "developer with a plan",
			stored: map[string][]json.RawMessage{"planner": {plan}},
			load: func(provider *DurableDeliveryContextProvider) error {
				_, err := provider.LoadDeveloperContext(context.Background(), 1, 2)
				return err
			},
		},
		{
			name:   "reviewer without the delivery",
			stored: map[string][]json.RawMessage{"planner": {plan}},
			load: func(provider *DurableDeliveryContextProvider) error {
				_, err := provider.LoadReviewerContext(context.Background(), 1, 2)
				return err
			},
			wantErr: true,
		},
		{
			name: "fixer without the review",
			stored: map[string][]json.RawMessage{
				"planner": {plan}, "developer": {delivery},
			},
			load: func(provider *DurableDeliveryContextProvider) error {
				_, err := provider.LoadFixerContext(context.Background(), 1, 2)
				return err
			},
			wantErr: true,
		},
		{
			name: "verifier with no fixes is a clean run",
			stored: map[string][]json.RawMessage{
				"planner": {plan}, "developer": {delivery}, "reviewer": {review},
			},
			load: func(provider *DurableDeliveryContextProvider) error {
				loaded, err := provider.LoadVerifierContext(context.Background(), 1, 2)
				if err == nil && len(loaded.Fixes) != 0 {
					return errors.New("expected no recorded fixes")
				}
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider, _ := newDeliveryProvider(t, test.stored, DeliveryContextOptions{})
			err := test.load(provider)
			if test.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr = %v", err, test.wantErr)
			}
		})
	}
}

func TestVerifierContextCarriesCIStatusWhenRequired(t *testing.T) {
	stored := map[string][]json.RawMessage{
		"planner":   {json.RawMessage(`{}`)},
		"developer": {json.RawMessage(`{}`)},
		"reviewer":  {json.RawMessage(`{}`)},
		"fixer":     {json.RawMessage(`{"fix":1}`), json.RawMessage(`{"fix":2}`)},
	}
	provider, _ := newDeliveryProvider(t, stored, DeliveryContextOptions{
		CIRequired: true,
		CI:         fakeCI{status: VerificationCIPassed},
	})
	loaded, err := provider.LoadVerifierContext(context.Background(), 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CIStatus != VerificationCIPassed || !loaded.CIRequired {
		t.Fatalf("ci = %q required = %v", loaded.CIStatus, loaded.CIRequired)
	}
	if len(loaded.Fixes) != 2 {
		t.Fatalf("fixes = %d, want 2 in order", len(loaded.Fixes))
	}
	if loaded.PRNumber != 5 || loaded.PRHead != "madar/issue-2" || loaded.PRBase != "main" {
		t.Fatalf("pull request context = %#v", loaded)
	}
}

// Requiring CI without a reader would report "not-required" to the verifier,
// which is a silent downgrade of the release gate.
func TestRequiringCIWithoutAReaderIsRefused(t *testing.T) {
	_, err := NewDurableDeliveryContextProvider(
		&fakeDeliveryStore{}, &fakeOutputs{}, &fakeExecutions{},
		fakeWorkspaces{path: "/workspaces"},
		DeliveryContextOptions{CIRequired: true},
	)
	if err == nil {
		t.Fatal("expected construction to fail without a CI reader")
	}
}

func TestTaskFromAnotherProjectIsRefused(t *testing.T) {
	provider, _ := newDeliveryProvider(t, nil, DeliveryContextOptions{})
	if _, err := provider.LoadPlannerContext(context.Background(), 99, 2); err == nil {
		t.Fatal("expected a project mismatch to fail")
	}
}
