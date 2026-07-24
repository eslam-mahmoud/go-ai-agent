package project

import (
	"errors"
	"fmt"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

type StartupRecoveryStore interface {
	InterruptRunningExecutionsForRecovery() ([]*domain.Execution, error)
	ListProjects() ([]*domain.Project, error)
}

type StartupRecoveryReport struct {
	InterruptedExecutions []*domain.Execution
	Projects              []*RecoveryResult
}

type StartupRecovery struct {
	controller *Controller
	store      StartupRecoveryStore
}

func NewStartupRecovery(
	controller *Controller,
	recoveryStore StartupRecoveryStore,
) (*StartupRecovery, error) {
	if controller == nil {
		return nil, errors.New("startup recovery controller is required")
	}
	if recoveryStore == nil {
		return nil, errors.New("startup recovery store is required")
	}
	return &StartupRecovery{controller: controller, store: recoveryStore}, nil
}

// Run first normalizes orphaned provider processes, then evaluates projects
// in stable creation order. Repeating Run against unchanged durable state is
// safe because interruption is terminal and decision events are idempotent.
func (recovery *StartupRecovery) Run() (*StartupRecoveryReport, error) {
	interrupted, err := recovery.store.InterruptRunningExecutionsForRecovery()
	if err != nil {
		return nil, err
	}
	projects, err := recovery.store.ListProjects()
	if err != nil {
		return nil, err
	}
	report := &StartupRecoveryReport{InterruptedExecutions: interrupted}
	for _, project := range projects {
		result, err := recovery.controller.Recover(project.ID)
		if err != nil {
			return nil, fmt.Errorf("recover project %d: %w", project.ID, err)
		}
		report.Projects = append(report.Projects, result)
	}
	return report, nil
}
