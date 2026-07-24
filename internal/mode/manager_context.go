package mode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	"github.com/eslam-mahmoud/go-ai-agent/internal/project"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
)

var ErrInvalidManagerSnapshot = errors.New("invalid durable manager snapshot")

type ManagerContextAggregateLoader interface {
	LoadManagerContextAggregate(projectID int64) (*store.ManagerContextAggregate, error)
}

type ManagerRuntimeContext struct {
	WorkDir     string
	ExecutionID int64
}

type ManagerRuntimeContextProvider interface {
	LoadManagerRuntimeContext(
		ctx context.Context,
		projectID, completedTaskID int64,
	) (ManagerRuntimeContext, error)
}

type ManagerRuntimeContextProviderFunc func(
	context.Context,
	int64,
	int64,
) (ManagerRuntimeContext, error)

func (load ManagerRuntimeContextProviderFunc) LoadManagerRuntimeContext(
	ctx context.Context,
	projectID, completedTaskID int64,
) (ManagerRuntimeContext, error) {
	return load(ctx, projectID, completedTaskID)
}

// DurableManagerContextProvider adapts a transactionally consistent store
// aggregate into the opaque snapshot consumed by Manager.
type DurableManagerContextProvider struct {
	loader  ManagerContextAggregateLoader
	runtime ManagerRuntimeContextProvider
}

func NewDurableManagerContextProvider(
	loader ManagerContextAggregateLoader,
	runtime ManagerRuntimeContextProvider,
) (*DurableManagerContextProvider, error) {
	if isNilDependency(loader) {
		return nil, errors.New("manager context aggregate loader is required")
	}
	if isNilDependency(runtime) {
		return nil, errors.New("manager runtime context provider is required")
	}
	return &DurableManagerContextProvider{loader: loader, runtime: runtime}, nil
}

func (provider *DurableManagerContextProvider) LoadManagerContext(
	ctx context.Context,
	projectID, completedTaskID int64,
) (*ManagerContext, error) {
	if projectID <= 0 || completedTaskID < 0 {
		return nil, fmt.Errorf(
			"%w: project ID must be positive and completed task ID non-negative",
			ErrInvalidManagerSnapshot,
		)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	aggregate, err := provider.loader.LoadManagerContextAggregate(projectID)
	if err != nil {
		return nil, fmt.Errorf("load durable manager snapshot: %w", err)
	}
	snapshot, err := buildDurableManagerSnapshot(aggregate, completedTaskID)
	if err != nil {
		return nil, err
	}
	runtime, err := provider.runtime.LoadManagerRuntimeContext(
		ctx,
		projectID,
		completedTaskID,
	)
	if err != nil {
		return nil, fmt.Errorf("load manager runtime context: %w", err)
	}
	if strings.TrimSpace(runtime.WorkDir) == "" ||
		!filepath.IsAbs(runtime.WorkDir) ||
		runtime.ExecutionID < 0 {
		return nil, fmt.Errorf(
			"%w: runtime workspace must be absolute and execution ID non-negative",
			ErrInvalidManagerSnapshot,
		)
	}
	return &ManagerContext{
		ProjectID:       projectID,
		CompletedTaskID: completedTaskID,
		Snapshot:        snapshot,
		WorkDir:         filepath.Clean(runtime.WorkDir),
		ExecutionID:     runtime.ExecutionID,
	}, nil
}

type durableManagerSnapshot struct {
	SchemaVersion       int                         `json:"schema_version"`
	Project             managerProjectSnapshot      `json:"project"`
	Assessment          project.HealthAssessment    `json:"health_assessment"`
	Plan                []managerTaskSnapshot       `json:"ordered_backlog"`
	CompletedTask       *managerTaskSnapshot        `json:"completed_task"`
	CurrentTask         *managerTaskSnapshot        `json:"current_task"`
	Dependencies        []managerDependencySnapshot `json:"dependencies"`
	LatestTaskResult    *managerExecutionSnapshot   `json:"latest_task_result"`
	ReviewAndCIResults  []managerExecutionSnapshot  `json:"review_and_ci_results"`
	Executions          []managerExecutionSnapshot  `json:"executions"`
	Artifacts           []managerArtifactSnapshot   `json:"artifacts"`
	CurrentArchitecture []managerArtifactSnapshot   `json:"current_architecture"`
	ManagerReviews      []managerReviewSnapshot     `json:"manager_reviews"`
	PendingDiscoveries  []any                       `json:"pending_discoveries"`
	HumanComments       []managerEventSnapshot      `json:"human_comments"`
	WorkflowEvents      []managerEventSnapshot      `json:"workflow_events"`
	ReleaseRequirements managerReleaseSnapshot      `json:"release_requirements"`
	RuntimeStatistics   managerRuntimeStatistics    `json:"runtime_statistics"`
}

type managerProjectSnapshot struct {
	ID                  int64                `json:"id"`
	Repository          string               `json:"repository"`
	ParentIssueNumber   int                  `json:"parent_issue_number"`
	Name                string               `json:"name"`
	Goal                string               `json:"goal"`
	Scope               string               `json:"scope"`
	State               domain.ProjectState  `json:"state"`
	StoredHealth        domain.ProjectHealth `json:"stored_health"`
	CurrentTaskID       *int64               `json:"current_task_id"`
	CurrentPlanVersion  int                  `json:"current_plan_version"`
	ArchitectureVersion int                  `json:"architecture_version"`
	ReleaseTarget       string               `json:"release_target"`
	ReleaseReadiness    string               `json:"release_readiness"`
	LastManagerReviewAt *time.Time           `json:"last_manager_review_at"`
}

type managerTaskSnapshot struct {
	ID                int64             `json:"id"`
	IssueNumber       int               `json:"issue_number"`
	Title             string            `json:"title"`
	Goal              string            `json:"goal"`
	Status            domain.TaskStatus `json:"status"`
	Priority          int               `json:"priority"`
	Sequence          int               `json:"sequence"`
	TaskType          string            `json:"task_type"`
	Source            string            `json:"source"`
	SourceDiscoveryID *int64            `json:"source_discovery_id"`
	BlocksRelease     bool              `json:"blocks_release"`
	SelectedReason    string            `json:"selected_reason"`
	BranchName        string            `json:"branch_name"`
	PRNumber          int               `json:"pr_number"`
	DependencyState   string            `json:"dependency_state"`
}

type managerDependencySnapshot struct {
	TaskID int64  `json:"task_id"`
	State  string `json:"state"`
}

type managerExecutionSnapshot struct {
	ID                int64                  `json:"id"`
	TaskID            int64                  `json:"task_id"`
	Mode              string                 `json:"mode"`
	Engine            string                 `json:"engine"`
	Model             string                 `json:"model"`
	Attempt           int                    `json:"attempt"`
	Status            domain.ExecutionStatus `json:"status"`
	InputArtifactID   *int64                 `json:"input_artifact_id"`
	OutputArtifactID  *int64                 `json:"output_artifact_id"`
	StartedAt         *time.Time             `json:"started_at"`
	CompletedAt       *time.Time             `json:"completed_at"`
	ErrorClass        string                 `json:"error_class"`
	ErrorMessage      string                 `json:"error_message"`
	InputTokens       int64                  `json:"input_tokens"`
	OutputTokens      int64                  `json:"output_tokens"`
	EstimatedCost     float64                `json:"estimated_cost"`
	ProviderSessionID string                 `json:"provider_session_id"`
}

type managerArtifactSnapshot struct {
	ID          int64     `json:"id"`
	TaskID      *int64    `json:"task_id"`
	ExecutionID *int64    `json:"execution_id"`
	Kind        string    `json:"kind"`
	Name        string    `json:"name"`
	Path        string    `json:"path"`
	MediaType   string    `json:"media_type"`
	SHA256      string    `json:"sha256"`
	SizeBytes   int64     `json:"size_bytes"`
	CreatedAt   time.Time `json:"created_at"`
}

type managerReviewSnapshot struct {
	ID                         int64                        `json:"id"`
	CompletedTaskID            *int64                       `json:"completed_task_id"`
	ExecutionID                *int64                       `json:"execution_id"`
	ArtifactID                 *int64                       `json:"artifact_id"`
	ProjectHealth              domain.ProjectHealth         `json:"project_health"`
	ProgressEstimate           int                          `json:"progress_estimate"`
	CompletedTaskDecision      domain.CompletedTaskDecision `json:"completed_task_decision"`
	ArchitectureReviewRequired bool                         `json:"architecture_review_required"`
	HumanApprovalRequired      bool                         `json:"human_approval_required"`
	DiscoveryDecisions         json.RawMessage              `json:"discovery_decisions"`
	BacklogChanges             json.RawMessage              `json:"backlog_changes"`
	NextTaskID                 *int64                       `json:"next_task_id"`
	NextTaskIssueNumber        int                          `json:"next_task_issue_number"`
	NextTaskReason             string                       `json:"next_task_reason"`
	ReleaseReadiness           string                       `json:"release_readiness"`
	OwnerUpdate                string                       `json:"owner_update"`
	ReviewedAt                 time.Time                    `json:"reviewed_at"`
}

type managerEventSnapshot struct {
	ID          int64                      `json:"id"`
	TaskID      *int64                     `json:"task_id"`
	ExecutionID *int64                     `json:"execution_id"`
	Sequence    int64                      `json:"sequence"`
	Source      domain.WorkflowEventSource `json:"source"`
	Type        domain.WorkflowEventType   `json:"type"`
	Message     string                     `json:"message"`
	Data        json.RawMessage            `json:"data"`
	CreatedAt   time.Time                  `json:"created_at"`
}

type managerReleaseSnapshot struct {
	Target           string  `json:"target"`
	StoredReadiness  string  `json:"stored_readiness"`
	BlockingTaskIDs  []int64 `json:"blocking_task_ids"`
	UnresolvedBlocks []int64 `json:"unresolved_blocking_task_ids"`
}

type managerRuntimeStatistics struct {
	ExecutionCount     int     `json:"execution_count"`
	CompletedRuns      int     `json:"completed_runs"`
	FailedRuns         int     `json:"failed_runs"`
	InterruptedRuns    int     `json:"interrupted_runs"`
	RunningRuns        int     `json:"running_runs"`
	InputTokens        int64   `json:"input_tokens"`
	OutputTokens       int64   `json:"output_tokens"`
	EstimatedCost      float64 `json:"estimated_cost"`
	TotalDurationMilli int64   `json:"total_duration_ms"`
}

func buildDurableManagerSnapshot(
	aggregate *store.ManagerContextAggregate,
	completedTaskID int64,
) (json.RawMessage, error) {
	if aggregate == nil || aggregate.Project == nil {
		return nil, fmt.Errorf("%w: project aggregate is nil", ErrInvalidManagerSnapshot)
	}
	projectRecord := aggregate.Project
	if projectRecord.ID <= 0 {
		return nil, fmt.Errorf("%w: project ID must be positive", ErrInvalidManagerSnapshot)
	}
	taskRecords := append([]*domain.Task(nil), aggregate.Tasks...)
	sort.Slice(taskRecords, func(left, right int) bool {
		if taskRecords[left] == nil || taskRecords[right] == nil {
			return taskRecords[left] == nil && taskRecords[right] != nil
		}
		if taskRecords[left].Sequence != taskRecords[right].Sequence {
			return taskRecords[left].Sequence < taskRecords[right].Sequence
		}
		return taskRecords[left].ID < taskRecords[right].ID
	})
	executionRecords := append([]*domain.Execution(nil), aggregate.Executions...)
	sort.Slice(executionRecords, func(left, right int) bool {
		if executionRecords[left] == nil || executionRecords[right] == nil {
			return executionRecords[left] == nil && executionRecords[right] != nil
		}
		return executionRecords[left].ID < executionRecords[right].ID
	})
	artifactRecords := append([]*domain.Artifact(nil), aggregate.Artifacts...)
	sort.Slice(artifactRecords, func(left, right int) bool {
		if artifactRecords[left] == nil || artifactRecords[right] == nil {
			return artifactRecords[left] == nil && artifactRecords[right] != nil
		}
		return artifactRecords[left].ID < artifactRecords[right].ID
	})
	reviewRecords := append([]*domain.ManagerReview(nil), aggregate.ManagerReviews...)
	sort.Slice(reviewRecords, func(left, right int) bool {
		if reviewRecords[left] == nil || reviewRecords[right] == nil {
			return reviewRecords[left] == nil && reviewRecords[right] != nil
		}
		return reviewRecords[left].ID < reviewRecords[right].ID
	})
	eventRecords := append([]*domain.WorkflowEvent(nil), aggregate.WorkflowEvents...)
	sort.Slice(eventRecords, func(left, right int) bool {
		if eventRecords[left] == nil || eventRecords[right] == nil {
			return eventRecords[left] == nil && eventRecords[right] != nil
		}
		if eventRecords[left].Sequence != eventRecords[right].Sequence {
			return eventRecords[left].Sequence < eventRecords[right].Sequence
		}
		return eventRecords[left].ID < eventRecords[right].ID
	})
	assessment, err := project.AssessHealth(
		projectRecord,
		taskRecords,
		executionRecords,
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidManagerSnapshot, err)
	}
	tasks := make([]managerTaskSnapshot, 0, len(taskRecords))
	taskByID := make(map[int64]managerTaskSnapshot, len(taskRecords))
	dependencies := make([]managerDependencySnapshot, 0, len(taskRecords))
	release := managerReleaseSnapshot{
		Target:           projectRecord.ReleaseTarget,
		StoredReadiness:  projectRecord.ReleaseReadiness,
		BlockingTaskIDs:  []int64{},
		UnresolvedBlocks: []int64{},
	}
	for _, task := range taskRecords {
		value := snapshotTask(task)
		tasks = append(tasks, value)
		taskByID[task.ID] = value
		if strings.TrimSpace(task.DependencyState) != "" {
			dependencies = append(dependencies, managerDependencySnapshot{
				TaskID: task.ID,
				State:  task.DependencyState,
			})
		}
		if task.BlocksRelease {
			release.BlockingTaskIDs = append(release.BlockingTaskIDs, task.ID)
			if task.Status != domain.TaskCompleted {
				release.UnresolvedBlocks = append(release.UnresolvedBlocks, task.ID)
			}
		}
	}
	var completedTask *managerTaskSnapshot
	if completedTaskID > 0 {
		value, ok := taskByID[completedTaskID]
		if !ok || value.Status != domain.TaskCompleted {
			return nil, fmt.Errorf(
				"%w: completed task %d is missing or not completed",
				ErrInvalidManagerSnapshot,
				completedTaskID,
			)
		}
		completedTask = &value
	}
	var currentTask *managerTaskSnapshot
	if projectRecord.CurrentTaskID != nil {
		value := taskByID[*projectRecord.CurrentTaskID]
		currentTask = &value
	}

	executions := make([]managerExecutionSnapshot, 0, len(executionRecords))
	reviewResults := []managerExecutionSnapshot{}
	var latestTaskResult *managerExecutionSnapshot
	stats := managerRuntimeStatistics{ExecutionCount: len(executionRecords)}
	for _, execution := range executionRecords {
		value := snapshotExecution(execution)
		executions = append(executions, value)
		if execution.TaskID == completedTaskID &&
			(latestTaskResult == nil || value.ID > latestTaskResult.ID) {
			copy := value
			latestTaskResult = &copy
		}
		if execution.Mode == "reviewer" || execution.Mode == "verifier" {
			reviewResults = append(reviewResults, value)
		}
		accumulateManagerRuntime(&stats, execution)
	}
	artifacts := make([]managerArtifactSnapshot, 0, len(artifactRecords))
	architecture := []managerArtifactSnapshot{}
	for _, artifact := range artifactRecords {
		if err := validateManagerArtifact(artifact, projectRecord.ID, taskByID); err != nil {
			return nil, err
		}
		value := snapshotArtifact(artifact)
		artifacts = append(artifacts, value)
		kind := strings.ToLower(artifact.Kind + " " + artifact.Name)
		if strings.Contains(kind, "architecture") || strings.Contains(kind, "adr") {
			architecture = append(architecture, value)
		}
	}
	reviews := make([]managerReviewSnapshot, 0, len(reviewRecords))
	for _, review := range reviewRecords {
		if err := review.Validate(); err != nil || review.ProjectID != projectRecord.ID {
			return nil, fmt.Errorf(
				"%w: inconsistent manager review %d",
				ErrInvalidManagerSnapshot,
				review.ID,
			)
		}
		reviews = append(reviews, snapshotManagerReview(review))
	}
	events := make([]managerEventSnapshot, 0, len(eventRecords))
	humanComments := []managerEventSnapshot{}
	var lastSequence int64
	for _, event := range eventRecords {
		if err := event.Validate(); err != nil ||
			event.ProjectID != projectRecord.ID ||
			event.Sequence <= lastSequence {
			return nil, fmt.Errorf(
				"%w: inconsistent workflow event %d",
				ErrInvalidManagerSnapshot,
				event.ID,
			)
		}
		lastSequence = event.Sequence
		value := snapshotEvent(event)
		events = append(events, value)
		if event.Source == domain.WorkflowSourceExternal {
			humanComments = append(humanComments, value)
		}
	}
	snapshot := durableManagerSnapshot{
		SchemaVersion: 1,
		Project: managerProjectSnapshot{
			ID:                  projectRecord.ID,
			Repository:          projectRecord.Repo,
			ParentIssueNumber:   projectRecord.ParentIssueNumber,
			Name:                projectRecord.Name,
			Goal:                projectRecord.Goal,
			Scope:               projectRecord.Scope,
			State:               projectRecord.State,
			StoredHealth:        projectRecord.Health,
			CurrentTaskID:       cloneInt64Pointer(projectRecord.CurrentTaskID),
			CurrentPlanVersion:  projectRecord.CurrentPlanVersion,
			ArchitectureVersion: projectRecord.ArchitectureVersion,
			ReleaseTarget:       projectRecord.ReleaseTarget,
			ReleaseReadiness:    projectRecord.ReleaseReadiness,
			LastManagerReviewAt: cloneTimePointer(projectRecord.LastManagerReviewAt),
		},
		Assessment:          assessment,
		Plan:                tasks,
		CompletedTask:       completedTask,
		CurrentTask:         currentTask,
		Dependencies:        dependencies,
		LatestTaskResult:    latestTaskResult,
		ReviewAndCIResults:  reviewResults,
		Executions:          executions,
		Artifacts:           artifacts,
		CurrentArchitecture: architecture,
		ManagerReviews:      reviews,
		PendingDiscoveries:  []any{},
		HumanComments:       humanComments,
		WorkflowEvents:      events,
		ReleaseRequirements: release,
		RuntimeStatistics:   stats,
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return nil, fmt.Errorf("encode durable manager snapshot: %w", err)
	}
	return raw, nil
}

func snapshotTask(task *domain.Task) managerTaskSnapshot {
	return managerTaskSnapshot{
		ID:                task.ID,
		IssueNumber:       task.IssueNumber,
		Title:             task.Title,
		Goal:              task.Goal,
		Status:            task.Status,
		Priority:          task.Priority,
		Sequence:          task.Sequence,
		TaskType:          task.TaskType,
		Source:            task.Source,
		SourceDiscoveryID: cloneInt64Pointer(task.SourceDiscoveryID),
		BlocksRelease:     task.BlocksRelease,
		SelectedReason:    task.SelectedReason,
		BranchName:        task.BranchName,
		PRNumber:          task.PRNumber,
		DependencyState:   task.DependencyState,
	}
}

func snapshotExecution(execution *domain.Execution) managerExecutionSnapshot {
	return managerExecutionSnapshot{
		ID:                execution.ID,
		TaskID:            execution.TaskID,
		Mode:              execution.Mode,
		Engine:            execution.Engine,
		Model:             execution.Model,
		Attempt:           execution.Attempt,
		Status:            execution.Status,
		InputArtifactID:   cloneInt64Pointer(execution.InputArtifactID),
		OutputArtifactID:  cloneInt64Pointer(execution.OutputArtifactID),
		StartedAt:         cloneTimePointer(execution.StartedAt),
		CompletedAt:       cloneTimePointer(execution.CompletedAt),
		ErrorClass:        execution.ErrorClass,
		ErrorMessage:      execution.ErrorMessage,
		InputTokens:       execution.InputTokens,
		OutputTokens:      execution.OutputTokens,
		EstimatedCost:     execution.EstimatedCost,
		ProviderSessionID: execution.ProviderSessionID,
	}
}

func snapshotArtifact(artifact *domain.Artifact) managerArtifactSnapshot {
	return managerArtifactSnapshot{
		ID:          artifact.ID,
		TaskID:      cloneInt64Pointer(artifact.TaskID),
		ExecutionID: cloneInt64Pointer(artifact.ExecutionID),
		Kind:        artifact.Kind,
		Name:        artifact.Name,
		Path:        artifact.Path,
		MediaType:   artifact.MediaType,
		SHA256:      artifact.SHA256,
		SizeBytes:   artifact.SizeBytes,
		CreatedAt:   artifact.CreatedAt,
	}
}

func snapshotManagerReview(review *domain.ManagerReview) managerReviewSnapshot {
	return managerReviewSnapshot{
		ID:                         review.ID,
		CompletedTaskID:            cloneInt64Pointer(review.CompletedTaskID),
		ExecutionID:                cloneInt64Pointer(review.ExecutionID),
		ArtifactID:                 cloneInt64Pointer(review.ArtifactID),
		ProjectHealth:              review.ProjectHealth,
		ProgressEstimate:           review.ProgressEstimate,
		CompletedTaskDecision:      review.CompletedTaskDecision,
		ArchitectureReviewRequired: review.ArchitectureReviewRequired,
		HumanApprovalRequired:      review.HumanApprovalRequired,
		DiscoveryDecisions:         append(json.RawMessage(nil), review.DiscoveryDecisions...),
		BacklogChanges:             append(json.RawMessage(nil), review.BacklogChanges...),
		NextTaskID:                 cloneInt64Pointer(review.NextTaskID),
		NextTaskIssueNumber:        review.NextTaskIssueNumber,
		NextTaskReason:             review.NextTaskReason,
		ReleaseReadiness:           review.ReleaseReadiness,
		OwnerUpdate:                review.OwnerUpdate,
		ReviewedAt:                 review.ReviewedAt,
	}
}

func snapshotEvent(event *domain.WorkflowEvent) managerEventSnapshot {
	return managerEventSnapshot{
		ID:          event.ID,
		TaskID:      cloneInt64Pointer(event.TaskID),
		ExecutionID: cloneInt64Pointer(event.ExecutionID),
		Sequence:    event.Sequence,
		Source:      event.Source,
		Type:        event.Type,
		Message:     event.Message,
		Data:        append(json.RawMessage(nil), event.Data...),
		CreatedAt:   event.CreatedAt,
	}
}

func validateManagerArtifact(
	artifact *domain.Artifact,
	projectID int64,
	taskByID map[int64]managerTaskSnapshot,
) error {
	if err := artifact.Validate(); err != nil ||
		artifact.ID <= 0 ||
		artifact.ProjectID != projectID {
		return fmt.Errorf(
			"%w: inconsistent artifact",
			ErrInvalidManagerSnapshot,
		)
	}
	if artifact.TaskID != nil {
		if _, ok := taskByID[*artifact.TaskID]; !ok {
			return fmt.Errorf(
				"%w: artifact %d references unknown task",
				ErrInvalidManagerSnapshot,
				artifact.ID,
			)
		}
	}
	return nil
}

func accumulateManagerRuntime(
	stats *managerRuntimeStatistics,
	execution *domain.Execution,
) {
	switch execution.Status {
	case domain.ExecutionCompleted:
		stats.CompletedRuns++
	case domain.ExecutionFailed:
		stats.FailedRuns++
	case domain.ExecutionInterrupted:
		stats.InterruptedRuns++
	case domain.ExecutionRunning:
		stats.RunningRuns++
	}
	stats.InputTokens += execution.InputTokens
	stats.OutputTokens += execution.OutputTokens
	stats.EstimatedCost += execution.EstimatedCost
	if execution.StartedAt != nil && execution.CompletedAt != nil {
		stats.TotalDurationMilli += execution.CompletedAt.Sub(*execution.StartedAt).Milliseconds()
	}
}

func cloneInt64Pointer(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneTimePointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

var _ ManagerContextProvider = (*DurableManagerContextProvider)(nil)
var _ ManagerContextAggregateLoader = (*store.Store)(nil)
