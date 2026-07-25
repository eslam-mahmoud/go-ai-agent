package execution

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

// fakeStore is an in-memory stand-in for the executions and artifacts tables.
type fakeStore struct {
	executions []*domain.Execution
	artifacts  map[int64]*domain.Artifact
	nextID     int64
	failCreate error
}

func newFakeStore() *fakeStore {
	return &fakeStore{artifacts: map[int64]*domain.Artifact{}}
}

func (fake *fakeStore) CreateExecution(
	record *domain.Execution,
) (*domain.Execution, error) {
	if fake.failCreate != nil {
		return nil, fake.failCreate
	}
	fake.nextID++
	stored := *record
	stored.ID = fake.nextID
	fake.executions = append(fake.executions, &stored)
	copied := stored
	return &copied, nil
}

func (fake *fakeStore) UpdateExecution(
	record *domain.Execution,
) (*domain.Execution, error) {
	for index, stored := range fake.executions {
		if stored.ID == record.ID {
			updated := *record
			fake.executions[index] = &updated
			copied := updated
			return &copied, nil
		}
	}
	return nil, errors.New("execution not found")
}

func (fake *fakeStore) ListTaskExecutions(taskID int64) ([]*domain.Execution, error) {
	matching := make([]*domain.Execution, 0, len(fake.executions))
	for _, stored := range fake.executions {
		if stored.TaskID == taskID {
			copied := *stored
			matching = append(matching, &copied)
		}
	}
	return matching, nil
}

func (fake *fakeStore) CreateArtifact(
	artifact *domain.Artifact,
) (*domain.Artifact, error) {
	fake.nextID++
	stored := *artifact
	stored.ID = fake.nextID
	fake.artifacts[stored.ID] = &stored
	copied := stored
	return &copied, nil
}

func (fake *fakeStore) GetArtifactByID(id int64) (*domain.Artifact, error) {
	artifact, ok := fake.artifacts[id]
	if !ok {
		return nil, nil
	}
	copied := *artifact
	return &copied, nil
}

func newRecorder(t *testing.T) (*Recorder, *fakeStore) {
	t.Helper()
	store := newFakeStore()
	recorder, err := New(store, Options{Root: t.TempDir(), Engine: "claude", Model: "opus"})
	if err != nil {
		t.Fatal(err)
	}
	return recorder, store
}

func TestNewRejectsIncompleteOptions(t *testing.T) {
	tests := []struct {
		name    string
		store   Store
		options Options
	}{
		{"no store", nil, Options{Root: "/tmp", Engine: "claude"}},
		{"no root", newFakeStore(), Options{Engine: "claude"}},
		{"relative root", newFakeStore(), Options{Root: "workspaces", Engine: "claude"}},
		{"no engine", newFakeStore(), Options{Root: "/tmp"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := New(test.store, test.options); err == nil {
				t.Fatal("expected construction to fail")
			}
		})
	}
}

// A taskless run — the bootstrap manager review that selects a project's
// first task — has no task to attribute an execution to. Begin must say so
// rather than let the store reject it with a less specific error.
func TestBeginRefusesATasklessRun(t *testing.T) {
	recorder, _ := newRecorder(t)
	if _, err := recorder.Begin(7, 0, "manager"); !errors.Is(err, ErrInvalidExecution) {
		t.Fatalf("err = %v, want ErrInvalidExecution", err)
	}
}

func TestCompleteStoresOutputAndLinksArtifact(t *testing.T) {
	recorder, store := newRecorder(t)
	record, err := recorder.Begin(7, 11, "planner")
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != domain.ExecutionRunning || record.Attempt != 1 {
		t.Fatalf("begin produced %#v", record)
	}

	output := json.RawMessage(`{"status":"completed"}`)
	completed, err := recorder.Complete(record, output)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != domain.ExecutionCompleted {
		t.Fatalf("status = %q", completed.Status)
	}
	if completed.OutputArtifactID == nil {
		t.Fatal("completed execution has no output artifact")
	}
	artifact := store.artifacts[*completed.OutputArtifactID]
	if artifact == nil {
		t.Fatal("artifact row was not created")
	}
	content, err := os.ReadFile(artifact.Path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != string(output) {
		t.Fatalf("stored output = %q", content)
	}
	if artifact.SizeBytes != int64(len(output)) {
		t.Fatalf("size = %d, want %d", artifact.SizeBytes, len(output))
	}
}

func TestLatestOutputReturnsMostRecentCompletedRun(t *testing.T) {
	recorder, _ := newRecorder(t)
	for _, body := range []string{`{"attempt":1}`, `{"attempt":2}`} {
		record, err := recorder.Begin(1, 2, "fixer")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := recorder.Complete(record, json.RawMessage(body)); err != nil {
			t.Fatal(err)
		}
	}
	latest, err := recorder.LatestOutput(2, "fixer")
	if err != nil {
		t.Fatal(err)
	}
	if string(latest) != `{"attempt":2}` {
		t.Fatalf("latest = %s", latest)
	}
	all, err := recorder.Outputs(2, "fixer")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || string(all[0]) != `{"attempt":1}` {
		t.Fatalf("outputs = %v", all)
	}
}

// A failed run must not become readable input for a later mode: the whole
// point of recording is that downstream modes see only real results.
func TestFailedRunIsNotReadableAsOutput(t *testing.T) {
	recorder, _ := newRecorder(t)
	record, err := recorder.Begin(1, 2, "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recorder.Fail(record, "provider-outage", "engine timed out"); err != nil {
		t.Fatal(err)
	}
	if _, err := recorder.LatestOutput(2, "reviewer"); !errors.Is(err, ErrNoOutput) {
		t.Fatalf("err = %v, want ErrNoOutput", err)
	}
}

func TestAttemptsContinueAcrossRuns(t *testing.T) {
	recorder, _ := newRecorder(t)
	for want := 1; want <= 3; want++ {
		record, err := recorder.Begin(1, 2, "developer")
		if err != nil {
			t.Fatal(err)
		}
		if record.Attempt != want {
			t.Fatalf("attempt = %d, want %d", record.Attempt, want)
		}
		if _, err := recorder.Complete(record, json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCompleteRunningClosesTheOpenExecution(t *testing.T) {
	recorder, store := newRecorder(t)
	if _, err := recorder.Begin(1, 2, "planner"); err != nil {
		t.Fatal(err)
	}
	if err := recorder.CompleteRunning(2, "planner", json.RawMessage(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	if store.executions[0].Status != domain.ExecutionCompleted {
		t.Fatalf("status = %q", store.executions[0].Status)
	}
	// Closing again is a no-op rather than an error: nothing is open.
	if err := recorder.CompleteRunning(2, "planner", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("second close = %v", err)
	}
}

func TestOutputFilesAreScopedByProjectAndTask(t *testing.T) {
	root := t.TempDir()
	store := newFakeStore()
	recorder, err := New(store, Options{Root: root, Engine: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	record, err := recorder.Begin(4, 9, "planner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recorder.Complete(record, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "project-4", "task-9")
	for _, artifact := range store.artifacts {
		if filepath.Dir(artifact.Path) != want {
			t.Fatalf("path = %q, want a file in %q", artifact.Path, want)
		}
	}
}
