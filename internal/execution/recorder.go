// Package execution records what a mode run did and keeps its output durable.
//
// The schema always reserved room for this — executions carry an
// OutputArtifactID and artifacts carry a path — but nothing wrote to it, so a
// mode's output vanished the moment the run returned. Every later mode in the
// delivery chain needs the earlier outputs (the developer needs the plan, the
// fixer needs the review), which is only possible once runs are recorded.
package execution

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

var (
	ErrInvalidRecorder  = errors.New("invalid execution recorder")
	ErrInvalidExecution = errors.New("invalid execution")
	// ErrNoOutput reports that a mode has not produced a stored output yet.
	ErrNoOutput = errors.New("no recorded mode output")
)

// Store is the narrow persistence surface the recorder needs.
type Store interface {
	CreateExecution(execution *domain.Execution) (*domain.Execution, error)
	UpdateExecution(execution *domain.Execution) (*domain.Execution, error)
	ListTaskExecutions(taskID int64) ([]*domain.Execution, error)
	CreateArtifact(artifact *domain.Artifact) (*domain.Artifact, error)
	GetArtifactByID(id int64) (*domain.Artifact, error)
}

type Options struct {
	// Root is where output documents are written. One file per execution
	// keeps the database small and the payloads inspectable by a human
	// debugging a run.
	Root   string
	Engine string
	Model  string
	Now    func() time.Time
}

// Recorder opens an execution before a mode runs and closes it afterwards.
type Recorder struct {
	store   Store
	root    string
	engine  string
	model   string
	nowFunc func() time.Time
}

func New(store Store, options Options) (*Recorder, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: store is required", ErrInvalidRecorder)
	}
	root := strings.TrimSpace(options.Root)
	if root == "" || !filepath.IsAbs(root) {
		return nil, fmt.Errorf("%w: root must be an absolute path", ErrInvalidRecorder)
	}
	engineName := strings.TrimSpace(options.Engine)
	if engineName == "" {
		return nil, fmt.Errorf("%w: engine is required", ErrInvalidRecorder)
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Recorder{
		store:   store,
		root:    filepath.Clean(root),
		engine:  engineName,
		model:   strings.TrimSpace(options.Model),
		nowFunc: now,
	}, nil
}

// Begin opens a running execution for one mode run. The attempt number is
// derived from the durable history so a restart continues the count instead of
// restarting it.
func (recorder *Recorder) Begin(
	projectID, taskID int64, mode string,
) (*domain.Execution, error) {
	if projectID <= 0 {
		return nil, fmt.Errorf("%w: project ID must be positive", ErrInvalidExecution)
	}
	if taskID <= 0 {
		// Executions are keyed by task, so there is nothing to attribute a
		// taskless run to. Callers that have no task record nothing.
		return nil, fmt.Errorf("%w: task ID must be positive", ErrInvalidExecution)
	}
	if strings.TrimSpace(mode) == "" {
		return nil, fmt.Errorf("%w: mode is required", ErrInvalidExecution)
	}
	attempt, err := recorder.nextAttempt(taskID, mode)
	if err != nil {
		return nil, err
	}
	record := domain.NewExecution(projectID, taskID, mode, recorder.engine, recorder.model, attempt)
	started := recorder.nowFunc().UTC()
	record.Status = domain.ExecutionRunning
	record.StartedAt = &started
	return recorder.store.CreateExecution(record)
}

// Complete stores the mode output and closes the execution. The output file is
// written before the database row points at it, so a crash between the two
// leaves an orphaned file rather than a row referencing nothing.
func (recorder *Recorder) Complete(
	record *domain.Execution, output json.RawMessage,
) (*domain.Execution, error) {
	if record == nil || record.ID <= 0 {
		return nil, fmt.Errorf("%w: execution must be persisted first", ErrInvalidExecution)
	}
	if len(output) > 0 {
		artifact, err := recorder.writeOutput(record, output)
		if err != nil {
			return nil, err
		}
		record.OutputArtifactID = &artifact.ID
	}
	completed := recorder.nowFunc().UTC()
	record.Status = domain.ExecutionCompleted
	record.CompletedAt = &completed
	return recorder.store.UpdateExecution(record)
}

// Fail closes an execution that did not produce usable output. The taxonomy
// class is kept so a later run can tell a provider outage from a bad prompt.
func (recorder *Recorder) Fail(
	record *domain.Execution, errorClass, message string,
) (*domain.Execution, error) {
	if record == nil || record.ID <= 0 {
		return nil, fmt.Errorf("%w: execution must be persisted first", ErrInvalidExecution)
	}
	completed := recorder.nowFunc().UTC()
	record.Status = domain.ExecutionFailed
	record.CompletedAt = &completed
	record.ErrorClass = strings.TrimSpace(errorClass)
	record.ErrorMessage = strings.TrimSpace(message)
	return recorder.store.UpdateExecution(record)
}

// CompleteRunning closes the open execution for a task and mode. The mode
// itself opens its execution while building context, so whatever runs the mode
// closes it by (task, mode) rather than by holding the record across the call.
func (recorder *Recorder) CompleteRunning(
	taskID int64, mode string, output json.RawMessage,
) error {
	record, err := recorder.latestRunning(taskID, mode)
	if err != nil || record == nil {
		return err
	}
	_, err = recorder.Complete(record, output)
	return err
}

// FailRunning closes the open execution for a task and mode as failed.
func (recorder *Recorder) FailRunning(
	taskID int64, mode, errorClass, message string,
) error {
	record, err := recorder.latestRunning(taskID, mode)
	if err != nil || record == nil {
		return err
	}
	_, err = recorder.Fail(record, errorClass, message)
	return err
}

// latestRunning returns the open execution for a task and mode, or nil when
// there is none. A missing record is not an error: a mode that never opened an
// execution still produced a usable result.
func (recorder *Recorder) latestRunning(
	taskID int64, mode string,
) (*domain.Execution, error) {
	records, err := recorder.store.ListTaskExecutions(taskID)
	if err != nil {
		return nil, err
	}
	var latest *domain.Execution
	for _, record := range records {
		if record == nil || record.Mode != mode || record.Status != domain.ExecutionRunning {
			continue
		}
		if latest == nil || record.ID > latest.ID {
			latest = record
		}
	}
	return latest, nil
}

// LatestOutput returns the most recent completed output for a task and mode.
// It reports ErrNoOutput when the mode has not run, which callers treat as
// "not available yet" rather than as a failure.
func (recorder *Recorder) LatestOutput(
	taskID int64, mode string,
) (json.RawMessage, error) {
	outputs, err := recorder.Outputs(taskID, mode)
	if err != nil {
		return nil, err
	}
	if len(outputs) == 0 {
		return nil, fmt.Errorf("%w: task %d mode %s", ErrNoOutput, taskID, mode)
	}
	return outputs[len(outputs)-1], nil
}

// Outputs returns every completed output for a task and mode, oldest first.
// The fixer's history matters to the verifier, so the order is part of the
// contract rather than an accident of the query.
func (recorder *Recorder) Outputs(
	taskID int64, mode string,
) ([]json.RawMessage, error) {
	records, err := recorder.store.ListTaskExecutions(taskID)
	if err != nil {
		return nil, err
	}
	matching := make([]*domain.Execution, 0, len(records))
	for _, record := range records {
		if record == nil ||
			record.Mode != mode ||
			record.Status != domain.ExecutionCompleted ||
			record.OutputArtifactID == nil {
			continue
		}
		matching = append(matching, record)
	}
	sort.SliceStable(matching, func(first, second int) bool {
		return matching[first].ID < matching[second].ID
	})
	outputs := make([]json.RawMessage, 0, len(matching))
	for _, record := range matching {
		output, err := recorder.readOutput(*record.OutputArtifactID)
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, output)
	}
	return outputs, nil
}

func (recorder *Recorder) nextAttempt(taskID int64, mode string) (int, error) {
	records, err := recorder.store.ListTaskExecutions(taskID)
	if err != nil {
		return 0, err
	}
	attempt := 1
	for _, record := range records {
		if record != nil && record.Mode == mode && record.Attempt >= attempt {
			attempt = record.Attempt + 1
		}
	}
	return attempt, nil
}

func (recorder *Recorder) writeOutput(
	record *domain.Execution, output json.RawMessage,
) (*domain.Artifact, error) {
	directory := filepath.Join(
		recorder.root,
		fmt.Sprintf("project-%d", record.ProjectID),
		fmt.Sprintf("task-%d", record.TaskID),
	)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return nil, fmt.Errorf("create execution output directory: %w", err)
	}
	name := fmt.Sprintf("%s-attempt-%d-execution-%d.json", record.Mode, record.Attempt, record.ID)
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, output, 0o600); err != nil {
		return nil, fmt.Errorf("write execution output: %w", err)
	}
	digest := sha256.Sum256(output)
	artifact := domain.NewArtifact(
		record.ProjectID,
		"mode-output",
		name,
		path,
		"application/json",
		hex.EncodeToString(digest[:]),
		int64(len(output)),
	)
	taskID := record.TaskID
	executionID := record.ID
	artifact.TaskID = &taskID
	artifact.ExecutionID = &executionID
	return recorder.store.CreateArtifact(artifact)
}

func (recorder *Recorder) readOutput(artifactID int64) (json.RawMessage, error) {
	artifact, err := recorder.store.GetArtifactByID(artifactID)
	if err != nil {
		return nil, err
	}
	if artifact == nil {
		return nil, fmt.Errorf("%w: artifact %d is missing", ErrNoOutput, artifactID)
	}
	content, err := os.ReadFile(artifact.Path)
	if err != nil {
		return nil, fmt.Errorf("read execution output %q: %w", artifact.Path, err)
	}
	return json.RawMessage(content), nil
}
