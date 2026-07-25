package mode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	"github.com/eslam-mahmoud/go-ai-agent/internal/workflow"
)

// ArchitectStore is the durable state an architecture run reads. Like the
// delivery store it is read-only: the architect proposes a shape, it never
// writes one.
type ArchitectStore interface {
	GetProjectByID(id int64) (*domain.Project, error)
	ListArchitectureRiskDiscoveries(projectID int64) ([]*domain.Discovery, error)
}

// DurableArchitectProjectLoader supplies the durable half of the architect
// snapshot from the store. Without it NewDurableArchitectContextProvider
// cannot be constructed at all, which is why Architect mode was unreachable.
type DurableArchitectProjectLoader struct {
	store ArchitectStore
}

func NewDurableArchitectProjectLoader(
	store ArchitectStore,
) (*DurableArchitectProjectLoader, error) {
	if isNilDependency(store) {
		return nil, errors.New("architect project loader store is required")
	}
	return &DurableArchitectProjectLoader{store: store}, nil
}

func (loader *DurableArchitectProjectLoader) LoadArchitectProject(
	projectID int64,
) (*ArchitectProject, error) {
	if projectID <= 0 {
		return nil, fmt.Errorf(
			"%w: project ID must be positive", ErrInvalidArchitectSnapshot,
		)
	}
	record, err := loader.store.GetProjectByID(projectID)
	if err != nil {
		return nil, err
	}
	if record == nil {
		return nil, fmt.Errorf(
			"%w: project %d not found", ErrInvalidArchitectSnapshot, projectID,
		)
	}
	risks, err := loader.store.ListArchitectureRiskDiscoveries(projectID)
	if err != nil {
		return nil, err
	}
	return &ArchitectProject{
		Name:                record.Name,
		Goal:                record.Goal,
		Scope:               record.Scope,
		Repo:                record.Repo,
		ArchitectureVersion: record.ArchitectureVersion,
		OutstandingRisks:    risks,
	}, nil
}

var _ ArchitectProjectLoader = (*DurableArchitectProjectLoader)(nil)

// RunArchitect adapts Architect to the architecture controller's runner
// boundary. It lives here so the project package never learns engine or mode
// types, the same arrangement the manager uses.
//
// The outstanding discovery IDs are not passed through: the context provider
// derives them from the same store the controller read, so a single source
// decides which risks the run covers.
func (architect *Architect) RunArchitect(
	ctx context.Context,
	projectID int64,
	_ []int64,
) (json.RawMessage, error) {
	return architect.Run(ctx, workflow.ModeRequest{
		ProjectID: projectID,
		Mode:      workflow.ModeArchitect,
	})
}
