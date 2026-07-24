package mode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

var ErrInvalidDiscoveryOutput = errors.New("invalid mode discovery output")

// MaxDiscoveriesPerExecution bounds one execution's contribution so a
// misbehaving provider cannot flood the backlog.
const MaxDiscoveriesPerExecution = 50

// discoveryOutput is the per-item contract every mode shares. Unknown fields
// are rejected so a silently renamed field never becomes a dropped signal.
type discoveryOutput struct {
	Title            string `json:"title"`
	Description      string `json:"description"`
	Category         string `json:"category"`
	Severity         string `json:"severity"`
	BlocksCurrent    bool   `json:"blocks_current"`
	ArchitectureRisk bool   `json:"architecture_risk"`
	SuggestedAction  string `json:"suggested_action"`
	ExternalID       string `json:"external_id"`
}

type modeDiscoveryEnvelope struct {
	Discoveries []json.RawMessage `json:"discoveries"`
}

// ExtractDiscoveries turns one mode's validated output into unevaluated
// Discovery records attributed to the task and execution that revealed them.
// Output carrying no discoveries yields none rather than an error.
func ExtractDiscoveries(
	raw json.RawMessage,
	projectID, sourceTaskID, sourceExecutionID int64,
) ([]*domain.Discovery, error) {
	if projectID <= 0 {
		return nil, fmt.Errorf(
			"%w: project ID must be positive",
			ErrInvalidDiscoveryOutput,
		)
	}
	if sourceTaskID < 0 || sourceExecutionID < 0 {
		return nil, fmt.Errorf(
			"%w: source task and execution IDs cannot be negative",
			ErrInvalidDiscoveryOutput,
		)
	}
	var envelope modeDiscoveryEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidDiscoveryOutput, err)
	}
	if len(envelope.Discoveries) == 0 {
		return nil, nil
	}
	if len(envelope.Discoveries) > MaxDiscoveriesPerExecution {
		return nil, fmt.Errorf(
			"%w: %d discoveries exceed the %d limit",
			ErrInvalidDiscoveryOutput,
			len(envelope.Discoveries),
			MaxDiscoveriesPerExecution,
		)
	}

	discoveries := make([]*domain.Discovery, 0, len(envelope.Discoveries))
	for index, item := range envelope.Discoveries {
		output, err := decodeDiscoveryItem(item, index)
		if err != nil {
			return nil, err
		}
		discovery := domain.NewDiscovery(
			projectID,
			sourceTaskID,
			sourceExecutionID,
			strings.TrimSpace(output.Title),
			domain.DiscoveryCategory(strings.TrimSpace(output.Category)),
			domain.DiscoverySeverity(strings.TrimSpace(output.Severity)),
		)
		discovery.Description = strings.TrimSpace(output.Description)
		discovery.BlocksCurrent = output.BlocksCurrent
		discovery.ArchitectureRisk = output.ArchitectureRisk
		discovery.ExternalID = strings.TrimSpace(output.ExternalID)
		if discovery.ExternalID == "" {
			// Derive the stable identity so independent executions reporting
			// the same finding converge on one discovery.
			discovery.ExternalID = discovery.ContentHash()
		}
		discovery.SuggestedAction = strings.TrimSpace(output.SuggestedAction)
		if discovery.SuggestedAction == "" {
			// Pre-classify deterministically so the manager always receives a
			// recommendation, which it may override with a reason.
			discovery.SuggestedAction = string(discovery.RecommendAction())
		}
		if err := discovery.Validate(); err != nil {
			return nil, fmt.Errorf("%w: discovery %d: %v", ErrInvalidDiscoveryOutput, index, err)
		}
		discoveries = append(discoveries, discovery)
	}
	return discoveries, nil
}

func decodeDiscoveryItem(item json.RawMessage, index int) (*discoveryOutput, error) {
	decoder := json.NewDecoder(bytes.NewReader(item))
	decoder.DisallowUnknownFields()
	var output discoveryOutput
	if err := decoder.Decode(&output); err != nil {
		return nil, fmt.Errorf(
			"%w: discovery %d: %v",
			ErrInvalidDiscoveryOutput,
			index,
			err,
		)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf(
			"%w: discovery %d has trailing JSON",
			ErrInvalidDiscoveryOutput,
			index,
		)
	}
	return &output, nil
}
