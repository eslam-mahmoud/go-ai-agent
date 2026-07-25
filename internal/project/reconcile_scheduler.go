package project

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
)

// ReconcileProjectLister supplies the projects a scheduled pass covers.
type ReconcileProjectLister interface {
	ListProjects() ([]*domain.Project, error)
}

// ReconcileScheduler runs reconciliation at startup and on an interval. A
// failing pass is logged and retried later: GitHub being unavailable is an
// expected condition, not a reason to stop delivering.
type ReconcileScheduler struct {
	reconciler *Reconciler
	projects   ReconcileProjectLister
	interval   time.Duration
	log        *slog.Logger
}

func NewReconcileScheduler(
	reconciler *Reconciler,
	projects ReconcileProjectLister,
	interval time.Duration,
	log *slog.Logger,
) (*ReconcileScheduler, error) {
	if reconciler == nil {
		return nil, errors.New("reconcile scheduler reconciler is required")
	}
	if projects == nil {
		return nil, errors.New("reconcile scheduler project lister is required")
	}
	if interval < 0 {
		return nil, errors.New("reconcile interval cannot be negative")
	}
	if log == nil {
		log = slog.Default()
	}
	return &ReconcileScheduler{
		reconciler: reconciler,
		projects:   projects,
		interval:   interval,
		log:        log,
	}, nil
}

// ReconcileOnce reconciles every project, returning the results it completed.
// A project that fails is logged and skipped so one broken repository cannot
// block the others.
func (scheduler *ReconcileScheduler) ReconcileOnce(
	ctx context.Context,
) ([]*ReconcileResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	projects, err := scheduler.projects.ListProjects()
	if err != nil {
		return nil, fmt.Errorf("list projects for reconciliation: %w", err)
	}
	results := make([]*ReconcileResult, 0, len(projects))
	for _, projectRecord := range projects {
		if projectRecord == nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			return results, err
		}
		result, err := scheduler.reconciler.Reconcile(ctx, projectRecord.ID)
		if err != nil {
			scheduler.log.Warn("reconciliation failed",
				"project", projectRecord.Repo, "error", err)
			continue
		}
		scheduler.report(projectRecord, result)
		results = append(results, result)
	}
	return results, nil
}

// report surfaces what a pass changed and what it refused to change. Ambiguity
// is logged at a level that demands attention, since it needs a human.
func (scheduler *ReconcileScheduler) report(
	projectRecord *domain.Project,
	result *ReconcileResult,
) {
	converged, drifted := 0, 0
	for _, task := range result.Tasks {
		if task.LabelsUpdated || task.IssueClosed || task.PullRequestBound > 0 {
			converged++
		}
		drifted += len(task.Drift)
	}
	if converged > 0 || drifted > 0 {
		scheduler.log.Info("reconciled project",
			"project", projectRecord.Repo,
			"converged", converged,
			"drift", drifted)
	}
	for _, ambiguous := range result.Ambiguous {
		scheduler.log.Warn("branch has multiple open pull requests",
			"project", projectRecord.Repo,
			"task", ambiguous.TaskID,
			"branch", ambiguous.Branch,
			"pull_requests", ambiguous.Numbers)
	}
	for _, task := range result.Tasks {
		for _, drift := range task.Drift {
			scheduler.log.Warn("reconciliation drift",
				"project", projectRecord.Repo, "task", task.TaskID, "drift", drift)
		}
	}
}

// Run reconciles once immediately, then on the interval until the context is
// cancelled. A zero interval means the startup pass only.
func (scheduler *ReconcileScheduler) Run(ctx context.Context) error {
	if _, err := scheduler.ReconcileOnce(ctx); err != nil && ctx.Err() == nil {
		scheduler.log.Warn("startup reconciliation failed", "error", err)
	}
	if scheduler.interval == 0 {
		return ctx.Err()
	}
	ticker := time.NewTicker(scheduler.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if _, err := scheduler.ReconcileOnce(ctx); err != nil && ctx.Err() == nil {
				scheduler.log.Warn("periodic reconciliation failed", "error", err)
			}
		}
	}
}

var _ ReconcileProjectLister = (*store.Store)(nil)
