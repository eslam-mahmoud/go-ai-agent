package engine

import (
	"context"
	"encoding/json"
	"time"
)

// Policy contains provider-neutral execution controls. Adapters translate
// these values into their provider-specific sandbox and approval flags.
type Policy struct {
	Sandbox         string
	ApprovalPolicy  string
	SkipPermissions bool
	// ToolRules are provider-neutral tool-permission patterns handed to the
	// provider, so a denied command or write is refused where the tool call
	// happens rather than noticed by us afterwards.
	ToolRules ToolRules
}

// ToolRules mirrors policy.ToolRules without the engine package depending on
// the policy package, keeping the provider boundary free of policy types.
type ToolRules struct {
	Allow []string
	Ask   []string
	Deny  []string
}

// Empty reports that these rules constrain nothing.
func (rules ToolRules) Empty() bool {
	return len(rules.Allow) == 0 && len(rules.Ask) == 0 && len(rules.Deny) == 0
}

// RunRequest is the complete input for a provider execution or resume.
type RunRequest struct {
	ExecutionID     int64
	WorkDir         string
	Prompt          string
	Mode            string
	Model           string
	SessionID       string
	ResumeSessionID string
	Timeout         time.Duration
	MaxTurns        int
	OutputSchema    json.RawMessage
	Environment     map[string]string
	Policy          Policy
}

type EventType string

const (
	EventSessionStarted EventType = "session.started"
	EventStepStarted    EventType = "step.started"
	EventToolStarted    EventType = "tool.started"
	EventToolCompleted  EventType = "tool.completed"
	EventFileChanged    EventType = "file.changed"
	EventProgress       EventType = "progress"
	EventQuestion       EventType = "question"
	EventUsage          EventType = "usage"
	EventCheckpoint     EventType = "checkpoint"
	EventCompleted      EventType = "completed"
	EventFailed         EventType = "failed"
)

// Event is a provider-neutral streaming event emitted during an execution.
type Event struct {
	Sequence  int64
	Type      EventType
	Timestamp time.Time
	SessionID string
	Message   string
	Data      json.RawMessage
}

type ResultStatus string

const (
	ResultCompleted ResultStatus = "completed"
	ResultFailed    ResultStatus = "failed"
	ResultCancelled ResultStatus = "cancelled"
)

// Usage records provider-reported consumption when available.
type Usage struct {
	InputTokens       int64
	OutputTokens      int64
	CachedInputTokens int64
	EstimatedCost     float64
}

// Result is the normalized terminal evidence for one provider execution.
type Result struct {
	SessionID   string
	Status      ResultStatus
	OutputJSON  json.RawMessage
	OutputText  string
	ExitCode    int
	StartedAt   time.Time
	CompletedAt time.Time
	Usage       Usage
}

// CapabilitySet lets startup validation reject configurations that require
// provider behavior an adapter cannot supply.
type CapabilitySet struct {
	Resume           bool
	StructuredOutput bool
	Streaming        bool
	Usage            bool
	Cancellation     bool
	OutputSchema     bool
}

// Engine is the replaceable execution-provider boundary used by workflow
// code. Implementations normalize their native streams and errors.
type Engine interface {
	Name() string
	Capabilities(context.Context) (CapabilitySet, error)
	Run(context.Context, RunRequest, func(Event) error) (*Result, error)
	Resume(context.Context, RunRequest, func(Event) error) (*Result, error)
	Cancel(context.Context, string) error
}
