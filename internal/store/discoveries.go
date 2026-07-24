package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

var (
	ErrDiscoveryNotFound  = errors.New("discovery not found")
	ErrDiscoveryOwnership = errors.New("discovery ownership conflict")
)

// CreateDiscoveries persists a batch atomically and records how many an
// execution produced. An empty batch is a no-op with no audit noise.
func (s *Store) CreateDiscoveries(
	projectID int64,
	discoveries []*domain.Discovery,
	idempotencyKey string,
) ([]*domain.Discovery, error) {
	if projectID <= 0 {
		return nil, fmt.Errorf("%w: project ID must be positive", domain.ErrInvalidDiscovery)
	}
	if len(discoveries) == 0 {
		return nil, nil
	}
	for index, discovery := range discoveries {
		if err := discovery.Validate(); err != nil {
			return nil, fmt.Errorf("create discovery %d: %w", index, err)
		}
		if discovery.ID != 0 {
			return nil, fmt.Errorf(
				"%w: new discovery %d ID must be zero",
				domain.ErrInvalidDiscovery,
				index,
			)
		}
		if discovery.ProjectID != projectID {
			return nil, fmt.Errorf(
				"%w: discovery %d belongs to project %d, not %d",
				ErrDiscoveryOwnership,
				index,
				discovery.ProjectID,
				projectID,
			)
		}
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin create discoveries: %w", err)
	}
	defer tx.Rollback()
	if err := requireProject(tx, projectID); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	ids := make([]int64, 0, len(discoveries))
	for index, discovery := range discoveries {
		result, err := tx.Exec(`
			INSERT INTO discoveries (
				project_id, source_task_id, source_execution_id, external_id,
				title, description, category, severity, blocks_current,
				architecture_risk, suggested_action, status, decision_reason,
				created_issue_number, backlog_position, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		`,
			discovery.ProjectID,
			nullablePositive(discovery.SourceTaskID),
			nullablePositive(discovery.SourceExecutionID),
			discovery.ExternalID,
			discovery.Title,
			discovery.Description,
			string(discovery.Category),
			string(discovery.Severity),
			discovery.BlocksCurrent,
			discovery.ArchitectureRisk,
			discovery.SuggestedAction,
			string(discovery.Status),
			discovery.DecisionReason,
			discovery.CreatedIssueNumber,
			discovery.BacklogPosition,
			now,
			now,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"insert discovery %d: %w",
				index,
				classifyDiscoveryConstraint(err),
			)
		}
		id, err := result.LastInsertId()
		if err != nil {
			return nil, fmt.Errorf("read discovery %d ID: %w", index, err)
		}
		ids = append(ids, id)
	}
	if err := touchProject(tx, projectID, now); err != nil {
		return nil, err
	}
	first := discoveries[0]
	var taskID, executionID *int64
	if first.SourceTaskID > 0 {
		value := first.SourceTaskID
		taskID = &value
	}
	if first.SourceExecutionID > 0 {
		value := first.SourceExecutionID
		executionID = &value
	}
	if err := appendWorkflowFactTx(
		tx,
		projectID,
		taskID,
		executionID,
		domain.WorkflowSourceWorkflow,
		domain.WorkflowDiscoveriesRecorded,
		fmt.Sprintf("Recorded %d discovery/discoveries.", len(discoveries)),
		map[string]any{
			"count":          len(discoveries),
			"discovery_ids":  ids,
			"task_id":        first.SourceTaskID,
			"execution_id":   first.SourceExecutionID,
			"recorded_first": first.Title,
		},
		strings.TrimSpace(idempotencyKey),
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit create discoveries: %w", err)
	}

	stored := make([]*domain.Discovery, 0, len(ids))
	for _, id := range ids {
		discovery, err := s.GetDiscoveryByID(id)
		if err != nil {
			return nil, err
		}
		stored = append(stored, discovery)
	}
	return stored, nil
}

func (s *Store) GetDiscoveryByID(id int64) (*domain.Discovery, error) {
	if id <= 0 {
		return nil, fmt.Errorf("%w: ID %d", ErrDiscoveryNotFound, id)
	}
	return scanDiscovery(s.db.QueryRow(discoverySelect+` WHERE id = ?`, id))
}

// ListDiscoveries returns the project's discoveries in insertion order.
func (s *Store) ListDiscoveries(projectID int64) ([]*domain.Discovery, error) {
	return s.queryDiscoveries(projectID, discoverySelect+` WHERE project_id = ? ORDER BY id ASC`)
}

// ListUnevaluatedDiscoveries returns the discoveries still awaiting a manager
// decision, which the manager context and review cycle consume.
func (s *Store) ListUnevaluatedDiscoveries(projectID int64) ([]*domain.Discovery, error) {
	return s.queryDiscoveries(
		projectID,
		discoverySelect+` WHERE project_id = ? AND status = 'unevaluated' ORDER BY id ASC`,
	)
}

func (s *Store) queryDiscoveries(projectID int64, query string) ([]*domain.Discovery, error) {
	if project, err := s.GetProjectByID(projectID); err != nil {
		return nil, err
	} else if project == nil {
		return nil, fmt.Errorf("%w: ID %d", ErrProjectNotFound, projectID)
	}
	rows, err := s.db.Query(query, projectID)
	if err != nil {
		return nil, fmt.Errorf("list discoveries: %w", err)
	}
	defer rows.Close()
	var discoveries []*domain.Discovery
	for rows.Next() {
		discovery, err := scanDiscovery(rows)
		if err != nil {
			return nil, err
		}
		discoveries = append(discoveries, discovery)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate discoveries: %w", err)
	}
	return discoveries, nil
}

func classifyDiscoveryConstraint(err error) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	switch {
	case strings.Contains(message, "discovery task must belong"),
		strings.Contains(message, "discovery execution must belong"),
		strings.Contains(message, "FOREIGN KEY constraint failed"):
		return fmt.Errorf("%w: %v", ErrDiscoveryOwnership, err)
	case strings.Contains(message, "CHECK constraint failed"):
		return fmt.Errorf("%w: %v", domain.ErrInvalidDiscovery, err)
	default:
		return err
	}
}

func nullablePositive(value int64) any {
	if value <= 0 {
		return nil
	}
	return value
}

const discoverySelect = `
	SELECT
		id, project_id, source_task_id, source_execution_id, external_id,
		title, description, category, severity, blocks_current,
		architecture_risk, suggested_action, status, decision_reason,
		created_issue_number, backlog_position, created_at, updated_at
	FROM discoveries
`

func scanDiscovery(row scanner) (*domain.Discovery, error) {
	var (
		discovery                    domain.Discovery
		sourceTaskID, sourceExecID   sql.NullInt64
		category, severity, status   string
		blocksCurrent, architectural int
	)
	if err := row.Scan(
		&discovery.ID,
		&discovery.ProjectID,
		&sourceTaskID,
		&sourceExecID,
		&discovery.ExternalID,
		&discovery.Title,
		&discovery.Description,
		&category,
		&severity,
		&blocksCurrent,
		&architectural,
		&discovery.SuggestedAction,
		&status,
		&discovery.DecisionReason,
		&discovery.CreatedIssueNumber,
		&discovery.BacklogPosition,
		&discovery.CreatedAt,
		&discovery.UpdatedAt,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("scan discovery: %w", err)
	}
	discovery.SourceTaskID = sourceTaskID.Int64
	discovery.SourceExecutionID = sourceExecID.Int64
	discovery.Category = domain.DiscoveryCategory(category)
	discovery.Severity = domain.DiscoverySeverity(severity)
	discovery.Status = domain.DiscoveryStatus(status)
	discovery.BlocksCurrent = blocksCurrent != 0
	discovery.ArchitectureRisk = architectural != 0
	discovery.CreatedAt = discovery.CreatedAt.UTC()
	discovery.UpdatedAt = discovery.UpdatedAt.UTC()
	return &discovery, nil
}
