package project

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/eslam-mahmoud/go-ai-agent/internal/architecturedocs"
	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
)

var ErrArchitectureDocuments = errors.New("architecture document generation failed")

type ArchitectureWriterStore interface {
	LoadProjectAggregate(projectID int64) (*store.ProjectAggregate, error)
	UpdateProject(project *domain.Project) (*domain.Project, error)
}

// WorkspaceArchitectureWriter renders a validated Architect proposal onto the
// project's workspace and versions the architecture it recorded.
type WorkspaceArchitectureWriter struct {
	store         ArchitectureWriterStore
	workspaceRoot string
}

func NewWorkspaceArchitectureWriter(
	writerStore ArchitectureWriterStore,
	workspaceRoot string,
) (*WorkspaceArchitectureWriter, error) {
	if writerStore == nil {
		return nil, errors.New("architecture writer store is required")
	}
	if strings.TrimSpace(workspaceRoot) == "" {
		return nil, errors.New("architecture writer workspace root is required")
	}
	return &WorkspaceArchitectureWriter{
		store:         writerStore,
		workspaceRoot: strings.TrimSpace(workspaceRoot),
	}, nil
}

// WriteArchitectureDocuments applies the proposal and bumps the architecture
// version only when something was actually written, so the version counts
// real changes rather than runs.
func (writer *WorkspaceArchitectureWriter) WriteArchitectureDocuments(
	projectID int64,
	proposal json.RawMessage,
) (*architecturedocs.Result, error) {
	if projectID <= 0 {
		return nil, fmt.Errorf("%w: project ID must be positive", ErrArchitectureDocuments)
	}
	aggregate, err := writer.store.LoadProjectAggregate(projectID)
	if err != nil {
		return nil, err
	}
	if aggregate == nil || aggregate.Project == nil {
		return nil, fmt.Errorf("%w: project aggregate is nil", ErrInconsistentState)
	}
	owner, repo, err := splitRepository(aggregate.Project.Repo)
	if err != nil {
		return nil, err
	}
	decoded, err := architecturedocs.Decode(proposal)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrArchitectureDocuments, err)
	}
	result, err := architecturedocs.Apply(
		filepath.Join(writer.workspaceRoot, owner, repo),
		architecturedocs.Project{
			Name: aggregate.Project.Name,
			Goal: aggregate.Project.Goal,
			Repo: aggregate.Project.Repo,
		},
		decoded,
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrArchitectureDocuments, err)
	}
	if len(result.Written) == 0 {
		return result, nil
	}
	updated := *aggregate.Project
	updated.ArchitectureVersion++
	if _, err := writer.store.UpdateProject(&updated); err != nil {
		return nil, fmt.Errorf("record architecture version: %w", err)
	}
	return result, nil
}

var (
	_ ArchitectureWriterStore    = (*store.Store)(nil)
	_ ArchitectureDocumentWriter = (*WorkspaceArchitectureWriter)(nil)
)
