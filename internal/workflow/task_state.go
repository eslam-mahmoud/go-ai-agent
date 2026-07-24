// Package workflow defines provider-neutral project workflow behavior.
package workflow

import (
	"errors"
	"fmt"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

var (
	ErrInvalidTaskTransition  = errors.New("invalid task transition")
	ErrTransitionPrecondition = errors.New("task transition precondition failed")
)

// TaskTransitionEvidence contains durable facts that gate a state change. The
// Project Controller is responsible for deriving these facts from persisted
// artifacts, reviews, CI state, and manager decisions.
type TaskTransitionEvidence struct {
	ManagerReviewCompleted  bool `json:"manager_review_completed"`
	ArchitectureRiskPending bool `json:"architecture_risk_pending"`
	PlanCompleted           bool `json:"plan_completed"`
	PlanningDisabled        bool `json:"planning_disabled"`
	InputProvided           bool `json:"input_provided"`
	BlockingReviewFindings  bool `json:"blocking_review_findings"`
	VerificationSucceeded   bool `json:"verification_succeeded"`
	VerificationFailed      bool `json:"verification_failed"`
	CIRequired              bool `json:"ci_required"`
	CIPassed                bool `json:"ci_passed"`
	CIFailed                bool `json:"ci_failed"`
	ReviewFixLimitReached   bool `json:"review_fix_limit_reached"`
	BlockerResolved         bool `json:"blocker_resolved"`
	Reprioritized           bool `json:"reprioritized"`
}

type TaskTransition struct {
	From     domain.TaskStatus
	To       domain.TaskStatus
	Evidence TaskTransitionEvidence
}

// TaskTransitionError gives callers stable classification plus the exact edge
// and missing requirement.
type TaskTransitionError struct {
	Kind        error
	From        domain.TaskStatus
	To          domain.TaskStatus
	Requirement string
}

func (err *TaskTransitionError) Error() string {
	if err.Requirement == "" {
		return fmt.Sprintf("%v: %s -> %s", err.Kind, err.From, err.To)
	}
	return fmt.Sprintf(
		"%v: %s -> %s requires %s",
		err.Kind,
		err.From,
		err.To,
		err.Requirement,
	)
}

func (err *TaskTransitionError) Unwrap() error {
	return err.Kind
}

var taskStatusOrder = []domain.TaskStatus{
	domain.TaskProposed,
	domain.TaskQueued,
	domain.TaskSelected,
	domain.TaskPlanning,
	domain.TaskWaitingInput,
	domain.TaskDeveloping,
	domain.TaskReviewing,
	domain.TaskFixing,
	domain.TaskVerifying,
	domain.TaskWaitingCI,
	domain.TaskCompleted,
	domain.TaskBlocked,
	domain.TaskCancelled,
	domain.TaskDeferred,
}

var allowedTaskTransitions = map[domain.TaskStatus]map[domain.TaskStatus]struct{}{
	domain.TaskProposed: targets(
		domain.TaskQueued,
		domain.TaskBlocked,
		domain.TaskCancelled,
		domain.TaskDeferred,
	),
	domain.TaskQueued: targets(
		domain.TaskSelected,
		domain.TaskBlocked,
		domain.TaskCancelled,
		domain.TaskDeferred,
	),
	domain.TaskSelected: targets(
		domain.TaskPlanning,
		domain.TaskBlocked,
		domain.TaskCancelled,
		domain.TaskDeferred,
	),
	domain.TaskPlanning: targets(
		domain.TaskWaitingInput,
		domain.TaskDeveloping,
		domain.TaskBlocked,
		domain.TaskCancelled,
		domain.TaskDeferred,
	),
	domain.TaskWaitingInput: targets(
		domain.TaskPlanning,
		domain.TaskDeveloping,
		domain.TaskBlocked,
		domain.TaskCancelled,
		domain.TaskDeferred,
	),
	domain.TaskDeveloping: targets(
		domain.TaskWaitingInput,
		domain.TaskReviewing,
		domain.TaskBlocked,
		domain.TaskCancelled,
		domain.TaskDeferred,
	),
	domain.TaskReviewing: targets(
		domain.TaskFixing,
		domain.TaskVerifying,
		domain.TaskBlocked,
		domain.TaskCancelled,
		domain.TaskDeferred,
	),
	domain.TaskFixing: targets(
		domain.TaskReviewing,
		domain.TaskBlocked,
		domain.TaskCancelled,
		domain.TaskDeferred,
	),
	domain.TaskVerifying: targets(
		domain.TaskFixing,
		domain.TaskWaitingCI,
		domain.TaskCompleted,
		domain.TaskBlocked,
		domain.TaskCancelled,
		domain.TaskDeferred,
	),
	domain.TaskWaitingCI: targets(
		domain.TaskFixing,
		domain.TaskCompleted,
		domain.TaskBlocked,
		domain.TaskCancelled,
		domain.TaskDeferred,
	),
	domain.TaskBlocked: targets(
		domain.TaskQueued,
		domain.TaskSelected,
		domain.TaskPlanning,
		domain.TaskWaitingInput,
		domain.TaskDeveloping,
		domain.TaskReviewing,
		domain.TaskFixing,
		domain.TaskVerifying,
		domain.TaskWaitingCI,
		domain.TaskCancelled,
		domain.TaskDeferred,
	),
	domain.TaskCompleted: {},
	domain.TaskCancelled: {},
	domain.TaskDeferred: targets(
		domain.TaskQueued,
		domain.TaskCancelled,
	),
}

func targets(statuses ...domain.TaskStatus) map[domain.TaskStatus]struct{} {
	result := make(map[domain.TaskStatus]struct{}, len(statuses))
	for _, status := range statuses {
		result[status] = struct{}{}
	}
	return result
}

// AllowedTaskTargets returns a new, canonical-order slice backed by the same
// transition table used by ValidateTaskTransition.
func AllowedTaskTargets(from domain.TaskStatus) ([]domain.TaskStatus, error) {
	if !from.Valid() {
		return nil, transitionError(
			ErrInvalidTaskTransition,
			from,
			"",
			"known source state",
		)
	}
	allowed := allowedTaskTransitions[from]
	result := make([]domain.TaskStatus, 0, len(allowed))
	for _, candidate := range taskStatusOrder {
		if _, ok := allowed[candidate]; ok {
			result = append(result, candidate)
		}
	}
	return result, nil
}

// ValidateTaskTransition is the single canonical validator for v2 task state
// changes. It does not mutate state.
func ValidateTaskTransition(transition TaskTransition) error {
	if !transition.From.Valid() {
		return transitionError(
			ErrInvalidTaskTransition,
			transition.From,
			transition.To,
			"known source state",
		)
	}
	if !transition.To.Valid() {
		return transitionError(
			ErrInvalidTaskTransition,
			transition.From,
			transition.To,
			"known target state",
		)
	}
	if transition.From == transition.To {
		return transitionError(
			ErrInvalidTaskTransition,
			transition.From,
			transition.To,
			"distinct source and target states",
		)
	}
	if _, ok := allowedTaskTransitions[transition.From][transition.To]; !ok {
		return transitionError(
			ErrInvalidTaskTransition,
			transition.From,
			transition.To,
			"an allowed lifecycle edge",
		)
	}
	return validateTaskTransitionEvidence(transition)
}

func validateTaskTransitionEvidence(transition TaskTransition) error {
	evidence := transition.Evidence
	require := func(condition bool, requirement string) error {
		if condition {
			return nil
		}
		return transitionError(
			ErrTransitionPrecondition,
			transition.From,
			transition.To,
			requirement,
		)
	}

	if transition.To == domain.TaskSelected {
		if err := require(
			evidence.ManagerReviewCompleted,
			"a completed manager review",
		); err != nil {
			return err
		}
		if err := require(
			!evidence.ArchitectureRiskPending,
			"resolved architecture risk",
		); err != nil {
			return err
		}
	}
	if transition.To == domain.TaskDeveloping {
		if err := require(
			evidence.PlanCompleted || evidence.PlanningDisabled,
			"a completed plan or explicit planning bypass",
		); err != nil {
			return err
		}
	}
	if transition.From == domain.TaskWaitingInput &&
		(transition.To == domain.TaskPlanning || transition.To == domain.TaskDeveloping) {
		if err := require(evidence.InputProvided, "provided human input"); err != nil {
			return err
		}
	}
	if transition.From == domain.TaskReviewing && transition.To == domain.TaskFixing {
		if err := require(
			evidence.BlockingReviewFindings,
			"blocking review findings",
		); err != nil {
			return err
		}
	}
	if transition.From == domain.TaskVerifying && transition.To == domain.TaskFixing {
		if err := require(
			evidence.VerificationFailed,
			"failed verification evidence",
		); err != nil {
			return err
		}
	}
	if transition.From == domain.TaskWaitingCI && transition.To == domain.TaskFixing {
		if err := require(evidence.CIFailed, "failed CI evidence"); err != nil {
			return err
		}
	}
	if transition.From == domain.TaskReviewing && transition.To == domain.TaskVerifying {
		if err := require(
			!evidence.BlockingReviewFindings,
			"no blocking review findings",
		); err != nil {
			return err
		}
	}
	if transition.From == domain.TaskVerifying && transition.To == domain.TaskWaitingCI {
		if err := require(
			evidence.VerificationSucceeded,
			"successful verification evidence",
		); err != nil {
			return err
		}
		if err := require(evidence.CIRequired, "configured CI verification"); err != nil {
			return err
		}
	}
	if transition.From == domain.TaskVerifying && transition.To == domain.TaskCompleted {
		if err := require(
			evidence.VerificationSucceeded,
			"successful verification evidence",
		); err != nil {
			return err
		}
		if err := require(!evidence.CIRequired, "CI to be disabled"); err != nil {
			return err
		}
	}
	if transition.From == domain.TaskWaitingCI && transition.To == domain.TaskCompleted {
		if err := require(
			evidence.VerificationSucceeded,
			"successful verification evidence",
		); err != nil {
			return err
		}
		if err := require(evidence.CIPassed, "passing CI evidence"); err != nil {
			return err
		}
	}
	if transition.From == domain.TaskBlocked &&
		transition.To != domain.TaskCancelled &&
		transition.To != domain.TaskDeferred {
		if err := require(evidence.BlockerResolved, "resolved blocker"); err != nil {
			return err
		}
	}
	if transition.From == domain.TaskDeferred && transition.To == domain.TaskQueued {
		if err := require(evidence.Reprioritized, "explicit reprioritization"); err != nil {
			return err
		}
	}
	return nil
}

func transitionError(
	kind error,
	from, to domain.TaskStatus,
	requirement string,
) error {
	return &TaskTransitionError{
		Kind:        kind,
		From:        from,
		To:          to,
		Requirement: requirement,
	}
}

// ManagerReviewRequired reports whether a terminal delivery outcome must be
// evaluated by the Engineering Manager before the project moves on.
//
// Success, blockage, and cancellation all end a delivery attempt and require a
// project-level decision. States that are still in flight, awaiting human
// input, or already the product of a manager decision do not.
func ManagerReviewRequired(status domain.TaskStatus) bool {
	switch status {
	case domain.TaskCompleted, domain.TaskBlocked, domain.TaskCancelled:
		return true
	default:
		return false
	}
}
