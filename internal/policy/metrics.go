package policy

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
)

var ErrMetrics = errors.New("metrics collection failed")

// Metrics is a structured snapshot of one project. Every value is derived
// from stored state, so metrics cannot drift from what actually happened.
type Metrics struct {
	ProjectID          int64          `json:"project_id"`
	Repo               string         `json:"repo"`
	Health             string         `json:"health"`
	State              string         `json:"state"`
	TasksByStatus      map[string]int `json:"tasks_by_status"`
	ExecutionsByMode   map[string]int `json:"executions_by_mode"`
	ExecutionsByStatus map[string]int `json:"executions_by_status"`
	TotalTasks         int            `json:"total_tasks"`
	TotalExecutions    int            `json:"total_executions"`
	// BudgetExhaustions counts how often each budget stopped work.
	BudgetExhaustions map[string]int `json:"budget_exhaustions"`
	// MeanExecutionSeconds covers executions that finished.
	MeanExecutionSeconds float64   `json:"mean_execution_seconds"`
	CollectedAt          time.Time `json:"collected_at"`
}

// MetricsSource is the durable state metrics are derived from.
type MetricsSource interface {
	ListProjectTasks(projectID int64) ([]*domain.Task, error)
	ListProjectExecutions(projectID int64) ([]*domain.Execution, error)
	ListWorkflowEvents(projectID, afterSequence int64, limit int) ([]*domain.WorkflowEvent, error)
}

// CollectMetrics builds a metrics snapshot for one project.
func CollectMetrics(
	source MetricsSource,
	project *domain.Project,
	now time.Time,
) (*Metrics, error) {
	if source == nil {
		return nil, fmt.Errorf("%w: metrics source is required", ErrMetrics)
	}
	if project == nil || project.ID <= 0 {
		return nil, fmt.Errorf("%w: a persisted project is required", ErrMetrics)
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tasks, err := source.ListProjectTasks(project.ID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMetrics, err)
	}
	executions, err := source.ListProjectExecutions(project.ID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMetrics, err)
	}
	events, err := source.ListWorkflowEvents(project.ID, 0, 1000)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMetrics, err)
	}

	metrics := &Metrics{
		ProjectID:          project.ID,
		Repo:               project.Repo,
		Health:             string(project.Health),
		State:              string(project.State),
		TasksByStatus:      map[string]int{},
		ExecutionsByMode:   map[string]int{},
		ExecutionsByStatus: map[string]int{},
		BudgetExhaustions:  map[string]int{},
		TotalTasks:         len(tasks),
		TotalExecutions:    len(executions),
		CollectedAt:        now,
	}
	for _, task := range tasks {
		if task != nil {
			metrics.TasksByStatus[string(task.Status)]++
		}
	}
	var totalSeconds float64
	var finished int
	for _, execution := range executions {
		if execution == nil {
			continue
		}
		metrics.ExecutionsByMode[execution.Mode]++
		metrics.ExecutionsByStatus[string(execution.Status)]++
		if execution.StartedAt != nil && execution.CompletedAt != nil {
			duration := execution.CompletedAt.Sub(*execution.StartedAt)
			if duration > 0 {
				totalSeconds += duration.Seconds()
				finished++
			}
		}
	}
	if finished > 0 {
		metrics.MeanExecutionSeconds = totalSeconds / float64(finished)
	}
	for _, event := range events {
		if event != nil && event.Type == domain.WorkflowBudgetExhausted {
			metrics.BudgetExhaustions[budgetKindFromEvent(event)]++
		}
	}
	return metrics, nil
}

// budgetKindFromEvent reads the budget name out of an exhaustion event,
// falling back to a generic bucket rather than dropping the count.
func budgetKindFromEvent(event *domain.WorkflowEvent) string {
	var data struct {
		Budget string `json:"budget"`
	}
	if err := json.Unmarshal(event.Data, &data); err == nil && data.Budget != "" {
		return data.Budget
	}
	return "unknown"
}

// SortedKeys gives metric maps a stable iteration order for reporting.
func SortedKeys(values map[string]int) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

var _ MetricsSource = (*store.Store)(nil)
