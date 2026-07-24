package mode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	"github.com/eslam-mahmoud/go-ai-agent/internal/reposcan"
)

var ErrInvalidArchitectSnapshot = errors.New("invalid architect snapshot")

// ArchitectProjectLoader supplies the durable half of the architect snapshot.
type ArchitectProjectLoader interface {
	LoadArchitectProject(projectID int64) (*ArchitectProject, error)
}

// ArchitectProject is the durable project state an architecture run needs. It
// deliberately excludes execution history: architecture is about shape, not
// about what ran last.
type ArchitectProject struct {
	Name                string
	Goal                string
	Scope               string
	Repo                string
	ArchitectureVersion int
	OutstandingRisks    []*domain.Discovery
}

type ArchitectRuntimeContext struct {
	WorkDir     string
	ExecutionID int64
}

type ArchitectRuntimeContextProvider interface {
	LoadArchitectRuntimeContext(
		ctx context.Context,
		projectID int64,
	) (ArchitectRuntimeContext, error)
}

type ArchitectRuntimeContextProviderFunc func(
	context.Context,
	int64,
) (ArchitectRuntimeContext, error)

func (load ArchitectRuntimeContextProviderFunc) LoadArchitectRuntimeContext(
	ctx context.Context,
	projectID int64,
) (ArchitectRuntimeContext, error) {
	return load(ctx, projectID)
}

// DurableArchitectContextProvider composes the durable project state with a
// read-only scan of the workspace, so the architect sees both what the project
// intends and what the repository already contains.
type DurableArchitectContextProvider struct {
	loader  ArchitectProjectLoader
	runtime ArchitectRuntimeContextProvider
}

func NewDurableArchitectContextProvider(
	loader ArchitectProjectLoader,
	runtime ArchitectRuntimeContextProvider,
) (*DurableArchitectContextProvider, error) {
	if isNilDependency(loader) {
		return nil, errors.New("architect project loader is required")
	}
	if isNilDependency(runtime) {
		return nil, errors.New("architect runtime context provider is required")
	}
	return &DurableArchitectContextProvider{loader: loader, runtime: runtime}, nil
}

func (provider *DurableArchitectContextProvider) LoadArchitectContext(
	ctx context.Context,
	projectID int64,
) (*ArchitectContext, error) {
	if projectID <= 0 {
		return nil, fmt.Errorf(
			"%w: project ID must be positive",
			ErrInvalidArchitectSnapshot,
		)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	projectState, err := provider.loader.LoadArchitectProject(projectID)
	if err != nil {
		return nil, fmt.Errorf("load architect project: %w", err)
	}
	if projectState == nil {
		return nil, fmt.Errorf("%w: project state is nil", ErrInvalidArchitectSnapshot)
	}
	runtime, err := provider.runtime.LoadArchitectRuntimeContext(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("load architect runtime context: %w", err)
	}
	if strings.TrimSpace(runtime.WorkDir) == "" ||
		!filepath.IsAbs(runtime.WorkDir) ||
		runtime.ExecutionID < 0 {
		return nil, fmt.Errorf(
			"%w: runtime workspace must be absolute and execution ID non-negative",
			ErrInvalidArchitectSnapshot,
		)
	}
	workDir := filepath.Clean(runtime.WorkDir)
	scan, err := reposcan.Scan(workDir)
	if err != nil {
		return nil, fmt.Errorf("scan workspace for architect: %w", err)
	}

	risks := make([]architectRiskSnapshot, 0, len(projectState.OutstandingRisks))
	ids := make([]int64, 0, len(projectState.OutstandingRisks))
	for _, discovery := range projectState.OutstandingRisks {
		if discovery == nil {
			continue
		}
		ids = append(ids, discovery.ID)
		risks = append(risks, architectRiskSnapshot{
			ID:          discovery.ID,
			Title:       discovery.Title,
			Description: discovery.Description,
			Category:    string(discovery.Category),
			Severity:    string(discovery.Severity),
			Decision:    string(discovery.Decision),
		})
	}
	snapshot, err := json.Marshal(architectSnapshot{
		Project: architectProjectSnapshot{
			Name:                projectState.Name,
			Goal:                projectState.Goal,
			Scope:               projectState.Scope,
			Repository:          projectState.Repo,
			ArchitectureVersion: projectState.ArchitectureVersion,
		},
		Repository:       scan,
		OutstandingRisks: risks,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: encode snapshot: %v", ErrInvalidArchitectSnapshot, err)
	}
	return &ArchitectContext{
		ProjectID:               projectID,
		OutstandingDiscoveryIDs: ids,
		Snapshot:                snapshot,
		WorkDir:                 workDir,
		ExecutionID:             runtime.ExecutionID,
	}, nil
}

type architectSnapshot struct {
	Project          architectProjectSnapshot `json:"project"`
	Repository       *reposcan.Report         `json:"repository"`
	OutstandingRisks []architectRiskSnapshot  `json:"outstanding_risks"`
}

type architectProjectSnapshot struct {
	Name                string `json:"name"`
	Goal                string `json:"goal"`
	Scope               string `json:"scope"`
	Repository          string `json:"repository"`
	ArchitectureVersion int    `json:"architecture_version"`
}

type architectRiskSnapshot struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Category    string `json:"category"`
	Severity    string `json:"severity"`
	Decision    string `json:"decision"`
}
