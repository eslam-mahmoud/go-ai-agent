package mode

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
)

func TestExtractDiscoveriesAttributesAndPreClassifies(t *testing.T) {
	raw := json.RawMessage(`{
		"status": "completed",
		"summary": "Implemented the retry budget.",
		"discoveries": [
			{
				"title": "  Retry budget is unbounded  ",
				"description": " The client retries forever on 5xx. ",
				"category": "bug",
				"severity": "critical",
				"blocks_current": false,
				"architecture_risk": false
			},
			{
				"title": "Token is logged at debug level",
				"category": "security",
				"severity": "high",
				"suggested_action": "create-release-blocker",
				"external_id": "sec-1"
			}
		]
	}`)
	discoveries, err := ExtractDiscoveries(raw, 7, 3, 9)
	if err != nil {
		t.Fatalf("ExtractDiscoveries: %v", err)
	}
	if len(discoveries) != 2 {
		t.Fatalf("extracted %d discoveries", len(discoveries))
	}
	first := discoveries[0]
	if first.Title != "Retry budget is unbounded" ||
		first.Description != "The client retries forever on 5xx." {
		t.Fatalf("first = %#v", first)
	}
	if first.ProjectID != 7 || first.SourceTaskID != 3 || first.SourceExecutionID != 9 {
		t.Fatalf("attribution = %#v", first)
	}
	if first.Status != domain.DiscoveryUnevaluated {
		t.Fatalf("status = %q", first.Status)
	}
	// Critical severity pre-classifies to a next task when nothing is supplied.
	if first.SuggestedAction != string(domain.ActionCreateNextTask) {
		t.Fatalf("suggested action = %q", first.SuggestedAction)
	}
	if discoveries[1].SuggestedAction != "create-release-blocker" ||
		discoveries[1].ExternalID != "sec-1" {
		t.Fatalf("second = %#v", discoveries[1])
	}
}

func TestExtractDiscoveriesWithoutDiscoveriesYieldsNone(t *testing.T) {
	for _, raw := range []json.RawMessage{
		json.RawMessage(`{"status":"completed","discoveries":[]}`),
		json.RawMessage(`{"status":"completed"}`),
		json.RawMessage(`{"status":"completed","discoveries":null}`),
	} {
		discoveries, err := ExtractDiscoveries(raw, 7, 3, 9)
		if err != nil {
			t.Fatalf("ExtractDiscoveries(%s): %v", raw, err)
		}
		if len(discoveries) != 0 {
			t.Fatalf("ExtractDiscoveries(%s) = %d discoveries", raw, len(discoveries))
		}
	}
}

func TestExtractDiscoveriesRejectsMalformedOutput(t *testing.T) {
	tests := []struct {
		name string
		raw  json.RawMessage
	}{
		{"malformed json", json.RawMessage(`{`)},
		{"discoveries not an array", json.RawMessage(`{"discoveries":{}}`)},
		{"item not an object", json.RawMessage(`{"discoveries":["text"]}`)},
		{
			"unknown field",
			json.RawMessage(`{"discoveries":[{"title":"x","category":"bug","severity":"low","urgency":"now"}]}`),
		},
		{
			"missing title",
			json.RawMessage(`{"discoveries":[{"title":"   ","category":"bug","severity":"low"}]}`),
		},
		{
			"unknown category",
			json.RawMessage(`{"discoveries":[{"title":"x","category":"vibes","severity":"low"}]}`),
		},
		{
			"unknown severity",
			json.RawMessage(`{"discoveries":[{"title":"x","category":"bug","severity":"spicy"}]}`),
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			if _, err := ExtractDiscoveries(test.raw, 7, 3, 9); !errors.Is(
				err, ErrInvalidDiscoveryOutput,
			) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestExtractDiscoveriesRejectsBadAttributionAndOversizedBatches(t *testing.T) {
	valid := json.RawMessage(`{"discoveries":[{"title":"x","category":"bug","severity":"low"}]}`)
	for _, ids := range [][3]int64{{0, 3, 9}, {7, -1, 9}, {7, 3, -1}} {
		if _, err := ExtractDiscoveries(valid, ids[0], ids[1], ids[2]); !errors.Is(
			err, ErrInvalidDiscoveryOutput,
		) {
			t.Fatalf("ids %v error = %v", ids, err)
		}
	}

	items := make([]string, 0, MaxDiscoveriesPerExecution+1)
	for index := range MaxDiscoveriesPerExecution + 1 {
		items = append(items, fmt.Sprintf(
			`{"title":"finding %d","category":"bug","severity":"low"}`, index,
		))
	}
	oversized := json.RawMessage(
		`{"discoveries":[` + strings.Join(items, ",") + `]}`,
	)
	if _, err := ExtractDiscoveries(oversized, 7, 3, 9); !errors.Is(
		err, ErrInvalidDiscoveryOutput,
	) {
		t.Fatalf("oversized error = %v", err)
	}

	atLimit := json.RawMessage(
		`{"discoveries":[` + strings.Join(items[:MaxDiscoveriesPerExecution], ",") + `]}`,
	)
	discoveries, err := ExtractDiscoveries(atLimit, 7, 3, 9)
	if err != nil || len(discoveries) != MaxDiscoveriesPerExecution {
		t.Fatalf("at-limit batch = %d, err = %v", len(discoveries), err)
	}
}

func TestExtractDiscoveriesAcceptsTasklessAndExecutionlessSources(t *testing.T) {
	raw := json.RawMessage(`{"discoveries":[{"title":"x","category":"bug","severity":"low"}]}`)
	discoveries, err := ExtractDiscoveries(raw, 7, 0, 0)
	if err != nil {
		t.Fatalf("ExtractDiscoveries: %v", err)
	}
	if len(discoveries) != 1 ||
		discoveries[0].SourceTaskID != 0 ||
		discoveries[0].SourceExecutionID != 0 {
		t.Fatalf("discoveries = %#v", discoveries)
	}
}

func TestExtractDiscoveriesAssignsStableExternalIDs(t *testing.T) {
	raw := json.RawMessage(`{"discoveries":[
		{"title":"Retry budget is unbounded","category":"bug","severity":"high"},
		{"title":"Token is logged","category":"security","severity":"high","external_id":"sec-1"}
	]}`)
	first, err := ExtractDiscoveries(raw, 7, 3, 9)
	if err != nil {
		t.Fatal(err)
	}
	if first[0].ExternalID == "" || !strings.HasPrefix(first[0].ExternalID, "disc-") {
		t.Fatalf("derived external ID = %q", first[0].ExternalID)
	}
	if first[1].ExternalID != "sec-1" {
		t.Fatalf("supplied external ID = %q", first[1].ExternalID)
	}

	// A different execution reporting the same finding must converge.
	cosmetic := json.RawMessage(`{"discoveries":[
		{"title":"  RETRY budget, is unbounded!  ","category":"bug","severity":"low"}
	]}`)
	second, err := ExtractDiscoveries(cosmetic, 7, 4, 11)
	if err != nil {
		t.Fatal(err)
	}
	if second[0].ExternalID != first[0].ExternalID {
		t.Fatalf("external IDs diverged: %q vs %q",
			second[0].ExternalID, first[0].ExternalID)
	}
}
