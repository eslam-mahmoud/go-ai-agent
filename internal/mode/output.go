package mode

import (
	"encoding/json"

	"github.com/eslam-mahmoud/go-ai-agent/internal/workflow"
)

type OutputStatus string

const (
	OutputCompleted  OutputStatus = "completed"
	OutputNeedsInput OutputStatus = "needs_input"
	OutputBlocked    OutputStatus = "blocked"
	OutputFailed     OutputStatus = "failed"
)

type Output struct {
	Status                OutputStatus      `json:"status"`
	Summary               string            `json:"summary"`
	Question              *string           `json:"question"`
	Discoveries           []json.RawMessage `json:"discoveries"`
	Risks                 []json.RawMessage `json:"risks"`
	RecommendedNextAction string            `json:"recommended_next_action"`
	BlockingFindings      []json.RawMessage `json:"blocking_findings,omitempty"`
	Raw                   json.RawMessage   `json:"-"`
}

func (status OutputStatus) Valid() bool {
	switch status {
	case OutputCompleted, OutputNeedsInput, OutputBlocked, OutputFailed:
		return true
	default:
		return false
	}
}

func (output *Output) WorkflowOutcome() workflow.ModeOutcome {
	if output == nil {
		return workflow.ModeOutcome{}
	}
	status := workflow.ModeStatus("")
	switch output.Status {
	case OutputCompleted:
		status = workflow.ModeCompleted
	case OutputNeedsInput:
		status = workflow.ModeNeedsInput
	case OutputBlocked:
		status = workflow.ModeBlocked
	case OutputFailed:
		status = workflow.ModeFailed
	}
	return workflow.ModeOutcome{
		Status:           status,
		BlockingFindings: len(output.BlockingFindings) > 0,
		Summary:          output.Summary,
	}
}
