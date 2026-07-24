package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

type DiscoveryCategory string

const (
	DiscoveryBug                DiscoveryCategory = "bug"
	DiscoveryMissingRequirement DiscoveryCategory = "missing-requirement"
	DiscoveryTechnicalDebt      DiscoveryCategory = "technical-debt"
	DiscoverySecurity           DiscoveryCategory = "security"
	DiscoveryArchitecture       DiscoveryCategory = "architecture"
	DiscoveryTesting            DiscoveryCategory = "testing"
	DiscoveryDocumentation      DiscoveryCategory = "documentation"
	DiscoveryObservability      DiscoveryCategory = "observability"
	DiscoveryPerformance        DiscoveryCategory = "performance"
	DiscoveryDependency         DiscoveryCategory = "dependency"
	DiscoveryScopeChange        DiscoveryCategory = "scope-change"
)

type DiscoverySeverity string

const (
	SeverityLow      DiscoverySeverity = "low"
	SeverityMedium   DiscoverySeverity = "medium"
	SeverityHigh     DiscoverySeverity = "high"
	SeverityCritical DiscoverySeverity = "critical"
)

type DiscoveryStatus string

const (
	// DiscoveryUnevaluated is the only status extraction may produce; every
	// other status is the result of a manager decision.
	DiscoveryUnevaluated DiscoveryStatus = "unevaluated"
	DiscoveryAccepted    DiscoveryStatus = "accepted"
	DiscoveryRejected    DiscoveryStatus = "rejected"
	DiscoveryDeferred    DiscoveryStatus = "deferred"
	DiscoveryMerged      DiscoveryStatus = "merged"
)

// DiscoveryAction is the deterministic recommendation produced before the
// manager evaluates a discovery. The manager may override it with a reason.
type DiscoveryAction string

const (
	ActionFixInCurrentTask       DiscoveryAction = "fix-in-current-task"
	ActionCreateNextTask         DiscoveryAction = "create-next-task"
	ActionCreateReleaseBlocker   DiscoveryAction = "create-release-blocker"
	ActionRequestArchitectureRvw DiscoveryAction = "request-architecture-review"
	ActionEvaluatePriority       DiscoveryAction = "evaluate-priority"
)

var ErrInvalidDiscovery = errors.New("invalid discovery")

// Discovery is one durable unit of work revealed by implementation. It is
// persisted before evaluation so nothing observed is silently dropped.
type Discovery struct {
	ID                 int64
	ProjectID          int64
	SourceTaskID       int64
	SourceExecutionID  int64
	ExternalID         string
	Title              string
	Description        string
	Category           DiscoveryCategory
	Severity           DiscoverySeverity
	BlocksCurrent      bool
	ArchitectureRisk   bool
	SuggestedAction    string
	Status             DiscoveryStatus
	DecisionReason     string
	CreatedIssueNumber int
	BacklogPosition    int
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// NewDiscovery constructs an unevaluated discovery attributed to the task and
// execution that revealed it.
func NewDiscovery(
	projectID, sourceTaskID, sourceExecutionID int64,
	title string,
	category DiscoveryCategory,
	severity DiscoverySeverity,
) *Discovery {
	return &Discovery{
		ProjectID:         projectID,
		SourceTaskID:      sourceTaskID,
		SourceExecutionID: sourceExecutionID,
		Title:             title,
		Category:          category,
		Severity:          severity,
		Status:            DiscoveryUnevaluated,
	}
}

func (category DiscoveryCategory) Valid() bool {
	switch category {
	case DiscoveryBug,
		DiscoveryMissingRequirement,
		DiscoveryTechnicalDebt,
		DiscoverySecurity,
		DiscoveryArchitecture,
		DiscoveryTesting,
		DiscoveryDocumentation,
		DiscoveryObservability,
		DiscoveryPerformance,
		DiscoveryDependency,
		DiscoveryScopeChange:
		return true
	default:
		return false
	}
}

func (severity DiscoverySeverity) Valid() bool {
	switch severity {
	case SeverityLow, SeverityMedium, SeverityHigh, SeverityCritical:
		return true
	default:
		return false
	}
}

// AtLeast compares severity by rank, not lexically.
func (severity DiscoverySeverity) AtLeast(other DiscoverySeverity) bool {
	return severityRank(severity) >= severityRank(other)
}

func severityRank(severity DiscoverySeverity) int {
	switch severity {
	case SeverityLow:
		return 1
	case SeverityMedium:
		return 2
	case SeverityHigh:
		return 3
	case SeverityCritical:
		return 4
	default:
		return 0
	}
}

func (status DiscoveryStatus) Valid() bool {
	switch status {
	case DiscoveryUnevaluated,
		DiscoveryAccepted,
		DiscoveryRejected,
		DiscoveryDeferred,
		DiscoveryMerged:
		return true
	default:
		return false
	}
}

// Evaluated reports whether a manager decision has been recorded.
func (status DiscoveryStatus) Evaluated() bool {
	return status.Valid() && status != DiscoveryUnevaluated
}

// RecommendAction applies the plan's deterministic pre-classification. Order
// matters: a discovery blocking current work outranks everything else.
func (discovery *Discovery) RecommendAction() DiscoveryAction {
	switch {
	case discovery == nil:
		return ActionEvaluatePriority
	case discovery.BlocksCurrent:
		return ActionFixInCurrentTask
	case discovery.Severity == SeverityCritical:
		return ActionCreateNextTask
	case discovery.ArchitectureRisk:
		return ActionRequestArchitectureRvw
	case discovery.Category == DiscoverySecurity && discovery.Severity.AtLeast(SeverityHigh):
		return ActionCreateReleaseBlocker
	default:
		return ActionEvaluatePriority
	}
}

func (discovery *Discovery) Validate() error {
	if discovery == nil {
		return fmt.Errorf("%w: discovery is nil", ErrInvalidDiscovery)
	}
	switch {
	case discovery.ProjectID <= 0:
		return fmt.Errorf("%w: project ID must be positive", ErrInvalidDiscovery)
	case discovery.SourceTaskID < 0:
		return fmt.Errorf("%w: source task ID cannot be negative", ErrInvalidDiscovery)
	case discovery.SourceExecutionID < 0:
		return fmt.Errorf("%w: source execution ID cannot be negative", ErrInvalidDiscovery)
	case strings.TrimSpace(discovery.Title) == "":
		return fmt.Errorf("%w: title is required", ErrInvalidDiscovery)
	case !discovery.Category.Valid():
		return fmt.Errorf("%w: unknown category %q", ErrInvalidDiscovery, discovery.Category)
	case !discovery.Severity.Valid():
		return fmt.Errorf("%w: unknown severity %q", ErrInvalidDiscovery, discovery.Severity)
	case !discovery.Status.Valid():
		return fmt.Errorf("%w: unknown status %q", ErrInvalidDiscovery, discovery.Status)
	case discovery.Status.Evaluated() && strings.TrimSpace(discovery.DecisionReason) == "":
		return fmt.Errorf("%w: an evaluated discovery requires a reason", ErrInvalidDiscovery)
	case discovery.CreatedIssueNumber < 0:
		return fmt.Errorf("%w: created issue number cannot be negative", ErrInvalidDiscovery)
	case discovery.BacklogPosition < 0:
		return fmt.Errorf("%w: backlog position cannot be negative", ErrInvalidDiscovery)
	case strings.TrimSpace(discovery.ExternalID) != discovery.ExternalID:
		return fmt.Errorf(
			"%w: external ID cannot have surrounding whitespace",
			ErrInvalidDiscovery,
		)
	default:
		return nil
	}
}
