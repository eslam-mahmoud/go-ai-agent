package orchestrator

import (
	"strings"
	"testing"
)

func TestDetectClarification(t *testing.T) {
	needsInput, question := detectClarification(
		"  NEEDS_CLARIFICATION:   Which deployment region?  ",
	)
	if !needsInput || question != "Which deployment region?" {
		t.Errorf("detectClarification = (%v, %q)", needsInput, question)
	}
	if needsInput, _ := detectClarification("Task completed."); needsInput {
		t.Error("ordinary completion was classified as clarification")
	}
}

func TestBuildFirstRunPromptPreservesTaskProtocol(t *testing.T) {
	prompt := BuildFirstRunPrompt(
		"Add rate limiting",
		strings.Repeat("x", 200),
		"@alice: use per-IP",
		42,
		100,
	)
	for _, want := range []string{
		"Add rate limiting",
		"madar/issue-42",
		"PR: #",
		"NEEDS_CLARIFICATION:",
		"@alice: use per-IP",
		"[truncated",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt does not contain %q", want)
		}
	}
	if strings.Contains(prompt, strings.Repeat("x", 101)) {
		t.Error("prompt did not apply issue body limit")
	}
}

func TestBuildResumePromptPreservesAttribution(t *testing.T) {
	prompt := BuildResumePrompt([]ReplyEntry{
		{Author: "alice", Body: "Use per-IP", Timestamp: "2026-07-24T07:00:00Z"},
		{Author: "bob", Body: "Limit to 5/min", Timestamp: "2026-07-24T07:01:00Z"},
	})
	for _, want := range []string{"2 messages", "@alice", "Use per-IP", "@bob", "5/min", "Continue"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt does not contain %q", want)
		}
	}
}

func TestBuildCIFixPromptPreservesExistingDelivery(t *testing.T) {
	prompt := BuildCIFixPrompt("FAIL: TestRateLimit", "madar/issue-42", 43, 2, 3)
	for _, want := range []string{
		"attempt 2 of 3",
		"madar/issue-42",
		"PR #43",
		"Do NOT create a new branch",
		"FAIL: TestRateLimit",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt does not contain %q", want)
		}
	}
}
