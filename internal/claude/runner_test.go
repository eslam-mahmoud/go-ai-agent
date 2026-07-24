package claude

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLegacyRunnerUsesAdapterAndPreservesResult(t *testing.T) {
	argsPath := filepath.Join(t.TempDir(), "args")
	bin := writeLegacyFakeClaude(t, fmt.Sprintf(
		"printf '%%s\\n' \"$@\" > %s\n"+
			"printf '%%s\\n' '%s'\n",
		shellQuote(argsPath),
		`{"type":"result","is_error":false,"result":"NEEDS_CLARIFICATION: Which region?","session_id":"session-1","num_turns":4}`,
	))

	result, err := New(bin).Run(context.Background(), RunOptions{
		WorkDir:         t.TempDir(),
		ResumeID:        "session-1",
		MaxTurns:        8,
		// Generous: this asserts argument handling, not timeout behavior, and
		// a short budget flakes when the whole suite runs in parallel.
		Timeout:         60 * time.Second,
		Prompt:          "continue",
		SkipPermissions: true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.SessionID != "session-1" || result.Output == "" || result.IsError {
		t.Fatalf("result = %#v", result)
	}
	if result.NumTurns != 4 {
		t.Errorf("NumTurns = %d, want 4", result.NumTurns)
	}
	if !result.NeedsInput || result.Question != "Which region?" {
		t.Errorf("clarification = (%v, %q)", result.NeedsInput, result.Question)
	}

	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	got := string(args)
	for _, want := range []string{
		"--resume\nsession-1\n",
		"--max-turns\n8\n",
		"--dangerously-skip-permissions\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("args %q do not contain %q", got, want)
		}
	}
	if strings.Contains(got, "--session-id") {
		t.Errorf("resume args unexpectedly contain --session-id: %q", got)
	}
}

func TestDetectClarification(t *testing.T) {
	cases := []struct {
		output       string
		wantNeeds    bool
		wantQuestion string
	}{
		{"NEEDS_CLARIFICATION: Should I use per-IP?", true, "Should I use per-IP?"},
		{"I have completed the task.", false, ""},
		{" NEEDS_CLARIFICATION:   What timeout?  ", true, "What timeout?"},
		{"", false, ""},
	}
	for _, tc := range cases {
		needs, question := detectClarification(tc.output)
		if needs != tc.wantNeeds || question != tc.wantQuestion {
			t.Errorf("detectClarification(%q) = (%v, %q), want (%v, %q)",
				tc.output, needs, question, tc.wantNeeds, tc.wantQuestion)
		}
	}
}

func TestBuildFirstRunPrompt(t *testing.T) {
	prompt := BuildFirstRunPrompt("Add rate limiting", "Per-IP, 5/min", "some comments", 42, 0)
	for _, want := range []string{
		"Add rate limiting",
		"NEEDS_CLARIFICATION",
		"some comments",
		"madar/issue-42",
		"PR: #",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt does not contain %q", want)
		}
	}
}

func TestBuildFirstRunPromptTruncation(t *testing.T) {
	longBody := strings.Repeat("x", 5000)
	prompt := BuildFirstRunPrompt("Title", longBody, "", 1, 100)
	if strings.Contains(prompt, longBody) || !strings.Contains(prompt, "[truncated") {
		t.Errorf("body was not truncated: %q", prompt)
	}
	if !strings.Contains(prompt, strings.Repeat("x", 100)) {
		t.Error("prompt does not preserve the requested prefix")
	}

	unlimited := BuildFirstRunPrompt("Title", longBody, "", 1, 0)
	if !strings.Contains(unlimited, longBody) {
		t.Error("zero body limit should preserve the full body")
	}
}

func TestBuildResumePrompt(t *testing.T) {
	prompt := BuildResumePrompt([]ReplyEntry{
		{Author: "alice", Body: "Use per-IP", Timestamp: "2024-01-01T10:00:00Z"},
		{Author: "bob", Body: "Actually per-account", Timestamp: "2024-01-01T10:05:00Z"},
	})
	for _, want := range []string{"2 messages", "@alice", "Use per-IP", "@bob", "per-account", "Continue"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt does not contain %q", want)
		}
	}
}

func writeLegacyFakeClaude(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-claude")
	script := "#!/bin/sh\n" + body
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake Claude binary: %v", err)
	}
	return path
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
