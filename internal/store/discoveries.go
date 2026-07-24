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

// DiscoveryBatch separates newly recorded findings from repeat sightings of
// findings the project already knows about.
type DiscoveryBatch struct {
	Created    []*domain.Discovery
	Duplicates []*domain.Discovery
}

// CreateDiscoveries persists a batch atomically, collapsing repeat sightings
// onto the discovery that already represents them. An empty batch is a no-op.
func (s *Store) CreateDiscoveries(
	projectID int64,
	discoveries []*domain.Discovery,
	idempotencyKey string,
) (*DiscoveryBatch, error) {
	if projectID <= 0 {
		return nil, fmt.Errorf("%w: project ID must be positive", domain.ErrInvalidDiscovery)
	}
	if len(discoveries) == 0 {
		return nil, nil
	}
	prepared, err := prepareDiscoveryBatch(projectID, discoveries)
	if err != nil {
		return nil, err
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
	createdIDs := make([]int64, 0, len(prepared))
	duplicateIDs := make([]int64, 0)
	for index, discovery := range prepared {
		existingID, err := findDiscoveryByExternalID(tx, projectID, discovery.ExternalID)
		if err != nil {
			return nil, err
		}
		if existingID > 0 {
			if err := recordDiscoverySighting(tx, existingID, discovery, now); err != nil {
				return nil, err
			}
			duplicateIDs = append(duplicateIDs, existingID)
			continue
		}
		if discovery.LinkedTaskID == nil {
			linked, err := findBacklogTaskByTitle(tx, projectID, discovery.Title)
			if err != nil {
				return nil, err
			}
			discovery.LinkedTaskID = linked
		}
		id, err := insertDiscovery(tx, discovery, now)
		if err != nil {
			return nil, fmt.Errorf("insert discovery %d: %w", index, err)
		}
		createdIDs = append(createdIDs, id)
	}
	if err := touchProject(tx, projectID, now); err != nil {
		return nil, err
	}
	if err := appendDiscoveryBatchFact(
		tx, projectID, prepared[0], createdIDs, duplicateIDs, idempotencyKey,
	); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit create discoveries: %w", err)
	}

	batch := &DiscoveryBatch{}
	for _, id := range createdIDs {
		discovery, err := s.GetDiscoveryByID(id)
		if err != nil {
			return nil, err
		}
		batch.Created = append(batch.Created, discovery)
	}
	for _, id := range duplicateIDs {
		discovery, err := s.GetDiscoveryByID(id)
		if err != nil {
			return nil, err
		}
		batch.Duplicates = append(batch.Duplicates, discovery)
	}
	return batch, nil
}

// prepareDiscoveryBatch validates every member up front and collapses repeats
// inside the batch itself, so one execution reporting a finding twice records
// it once.
func prepareDiscoveryBatch(
	projectID int64,
	discoveries []*domain.Discovery,
) ([]*domain.Discovery, error) {
	prepared := make([]*domain.Discovery, 0, len(discoveries))
	seen := make(map[string]*domain.Discovery, len(discoveries))
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
		if strings.TrimSpace(discovery.ExternalID) == "" {
			discovery.ExternalID = discovery.ContentHash()
		}
		if earlier, duplicate := seen[discovery.ExternalID]; duplicate {
			earlier.Occurrences++
			continue
		}
		seen[discovery.ExternalID] = discovery
		prepared = append(prepared, discovery)
	}
	return prepared, nil
}

func findDiscoveryByExternalID(
	tx *sql.Tx,
	projectID int64,
	externalID string,
) (int64, error) {
	if strings.TrimSpace(externalID) == "" {
		return 0, nil
	}
	var id int64
	err := tx.QueryRow(`
		SELECT id FROM discoveries WHERE project_id = ? AND external_id = ?
	`, projectID, externalID).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("find discovery by external ID: %w", err)
	}
	return id, nil
}

// recordDiscoverySighting counts a repeat observation without touching the
// existing record's decision, status, or reason.
func recordDiscoverySighting(
	tx *sql.Tx,
	existingID int64,
	sighting *domain.Discovery,
	now time.Time,
) error {
	if _, err := tx.Exec(`
		UPDATE discoveries
		SET occurrences = occurrences + ?, updated_at = ?
		WHERE id = ?
	`, maxInt(sighting.Occurrences, 1), now, existingID); err != nil {
		return fmt.Errorf("record discovery sighting: %w", err)
	}
	return nil
}

func insertDiscovery(
	tx *sql.Tx,
	discovery *domain.Discovery,
	now time.Time,
) (int64, error) {
	result, err := tx.Exec(`
		INSERT INTO discoveries (
			project_id, source_task_id, source_execution_id, external_id,
			title, description, category, severity, blocks_current,
			architecture_risk, suggested_action, status, decision_reason,
			created_issue_number, backlog_position, occurrences, linked_task_id,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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
		maxInt(discovery.Occurrences, 1),
		nullableInt64(discovery.LinkedTaskID),
		now,
		now,
	)
	if err != nil {
		return 0, classifyDiscoveryConstraint(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("read discovery ID: %w", err)
	}
	return id, nil
}

// findBacklogTaskByTitle links a discovery to work the project already plans
// to do. Terminal tasks are ignored: they cannot absorb new work.
func findBacklogTaskByTitle(
	tx *sql.Tx,
	projectID int64,
	title string,
) (*int64, error) {
	normalized := domain.NormalizeDiscoveryTitle(title)
	if normalized == "" {
		return nil, nil
	}
	rows, err := tx.Query(`
		SELECT id, title FROM project_tasks
		WHERE project_id = ?
		AND status NOT IN ('completed', 'cancelled', 'deferred')
		ORDER BY sequence ASC, id ASC
	`, projectID)
	if err != nil {
		return nil, fmt.Errorf("search backlog for discovery: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var taskTitle string
		if err := rows.Scan(&id, &taskTitle); err != nil {
			return nil, fmt.Errorf("scan backlog for discovery: %w", err)
		}
		if domain.NormalizeDiscoveryTitle(taskTitle) == normalized {
			matched := id
			return &matched, nil
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate backlog for discovery: %w", err)
	}
	return nil, nil
}

func appendDiscoveryBatchFact(
	tx *sql.Tx,
	projectID int64,
	first *domain.Discovery,
	createdIDs, duplicateIDs []int64,
	idempotencyKey string,
) error {
	var taskID, executionID *int64
	if first.SourceTaskID > 0 {
		value := first.SourceTaskID
		taskID = &value
	}
	if first.SourceExecutionID > 0 {
		value := first.SourceExecutionID
		executionID = &value
	}
	return appendWorkflowFactTx(
		tx,
		projectID,
		taskID,
		executionID,
		domain.WorkflowSourceWorkflow,
		domain.WorkflowDiscoveriesRecorded,
		fmt.Sprintf(
			"Recorded %d new and %d repeat discovery/discoveries.",
			len(createdIDs),
			len(duplicateIDs),
		),
		map[string]any{
			"created_count":     len(createdIDs),
			"duplicate_count":   len(duplicateIDs),
			"discovery_ids":     createdIDs,
			"duplicate_ids":     duplicateIDs,
			"task_id":           first.SourceTaskID,
			"execution_id":      first.SourceExecutionID,
			"recorded_first":    first.Title,
			"first_external_id": first.ExternalID,
		},
		strings.TrimSpace(idempotencyKey),
	)
}

func (s *Store) GetDiscoveryByID(id int64) (*domain.Discovery, error) {
	if id <= 0 {
		return nil, fmt.Errorf("%w: ID %d", ErrDiscoveryNotFound, id)
	}
	return scanDiscovery(s.db.QueryRow(discoverySelect+` WHERE id = ?`, id))
}

// GetDiscoveryByExternalID resolves a discovery by its stable content identity.
func (s *Store) GetDiscoveryByExternalID(
	projectID int64,
	externalID string,
) (*domain.Discovery, error) {
	if projectID <= 0 || strings.TrimSpace(externalID) == "" {
		return nil, nil
	}
	return scanDiscovery(s.db.QueryRow(
		discoverySelect+` WHERE project_id = ? AND external_id = ?`,
		projectID,
		externalID,
	))
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
	case strings.Contains(message, "idx_discoveries_external_id"):
		return fmt.Errorf("%w: duplicate external ID: %v", domain.ErrInvalidDiscovery, err)
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

func maxInt(value, floor int) int {
	if value < floor {
		return floor
	}
	return value
}

const discoverySelect = `
	SELECT
		id, project_id, source_task_id, source_execution_id, external_id,
		title, description, category, severity, blocks_current,
		architecture_risk, suggested_action, status, decision_reason,
		created_issue_number, backlog_position, occurrences, linked_task_id,
		created_at, updated_at
	FROM discoveries
`

func scanDiscovery(row scanner) (*domain.Discovery, error) {
	var (
		discovery                    domain.Discovery
		sourceTaskID, sourceExecID   sql.NullInt64
		linkedTaskID                 sql.NullInt64
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
		&discovery.Occurrences,
		&linkedTaskID,
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
	discovery.LinkedTaskID = nullInt64Pointer(linkedTaskID)
	discovery.Category = domain.DiscoveryCategory(category)
	discovery.Severity = domain.DiscoverySeverity(severity)
	discovery.Status = domain.DiscoveryStatus(status)
	discovery.BlocksCurrent = blocksCurrent != 0
	discovery.ArchitectureRisk = architectural != 0
	discovery.CreatedAt = discovery.CreatedAt.UTC()
	discovery.UpdatedAt = discovery.UpdatedAt.UTC()
	return &discovery, nil
}
