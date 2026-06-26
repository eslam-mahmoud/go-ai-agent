package claude

import (
	"strings"
	"testing"
)

func TestDetectClarification(t *testing.T) {
	cases := []struct {
		output      string
		wantNeeds   bool
		wantQuestion string
	}{
		{
			output:    "NEEDS_CLARIFICATION: Should I use per-IP or per-user rate limiting?",
			wantNeeds: true,
			wantQuestion: "Should I use per-IP or per-user rate limiting?",
		},
		{
			output:    "I have completed the task. The code is ready.",
			wantNeeds: false,
		},
		{
			output:    "NEEDS_CLARIFICATION:   What is the timeout value?  ",
			wantNeeds: true,
			wantQuestion: "What is the timeout value?",
		},
		{
			output:    "",
			wantNeeds: false,
		},
	}

	for _, tc := range cases {
		needs, question := detectClarification(tc.output)
		if needs != tc.wantNeeds {
			t.Errorf("detectClarification(%q) needs=%v, want %v", tc.output, needs, tc.wantNeeds)
		}
		if tc.wantNeeds && question != tc.wantQuestion {
			t.Errorf("detectClarification(%q) question=%q, want %q", tc.output, question, tc.wantQuestion)
		}
	}
}

func TestBuildArgs_newSession(t *testing.T) {
	opts := RunOptions{
		Prompt:    "do the thing",
		SessionID: "uuid-1234",
		MaxTurns:  20,
	}
	args := buildArgs(opts)

	assertContainsSeq(t, args, "-p", "do the thing")
	assertContainsSeq(t, args, "--session-id", "uuid-1234")
	assertContainsSeq(t, args, "--max-turns", "20")
	assertContains(t, args, "--output-format")
	assertContains(t, args, "--verbose")

	// Should NOT contain --resume when using --session-id
	for _, a := range args {
		if a == "--resume" {
			t.Error("args should not contain --resume for new session")
		}
	}
}

func TestBuildArgs_resume(t *testing.T) {
	opts := RunOptions{
		Prompt:   "continue",
		ResumeID: "uuid-5678",
		MaxTurns: 40,
	}
	args := buildArgs(opts)

	assertContainsSeq(t, args, "--resume", "uuid-5678")

	// Should NOT contain --session-id when resuming
	for _, a := range args {
		if a == "--session-id" {
			t.Error("args should not contain --session-id when resuming")
		}
	}
}

func TestBuildArgs_noMaxTurns(t *testing.T) {
	opts := RunOptions{Prompt: "test", MaxTurns: 0}
	args := buildArgs(opts)
	for i, a := range args {
		if a == "--max-turns" {
			t.Errorf("unexpected --max-turns at index %d", i)
		}
	}
}

func TestParseStream_successResult(t *testing.T) {
	jsonStream := `
{"type":"system","subtype":"init","session_id":"sess-abc"}
{"type":"assistant","message":{"content":[{"type":"text","text":"Working on it..."}]}}
{"type":"result","subtype":"success","is_error":false,"result":"Task completed successfully.","session_id":"sess-abc","num_turns":3}
`
	result, err := parseStream(strings.NewReader(jsonStream))
	if err != nil {
		t.Fatalf("parseStream: %v", err)
	}
	if result.SessionID != "sess-abc" {
		t.Errorf("SessionID = %q, want sess-abc", result.SessionID)
	}
	if result.Output != "Task completed successfully." {
		t.Errorf("Output = %q", result.Output)
	}
	if result.IsError {
		t.Error("IsError should be false")
	}
	if result.NeedsInput {
		t.Error("NeedsInput should be false")
	}
	if result.NumTurns != 3 {
		t.Errorf("NumTurns = %d, want 3", result.NumTurns)
	}
}

func TestParseStream_clarification(t *testing.T) {
	jsonStream := `
{"type":"system","subtype":"init","session_id":"sess-xyz"}
{"type":"result","subtype":"success","is_error":false,"result":"NEEDS_CLARIFICATION: Per-IP or per-user?","session_id":"sess-xyz","num_turns":1}
`
	result, err := parseStream(strings.NewReader(jsonStream))
	if err != nil {
		t.Fatalf("parseStream: %v", err)
	}
	if !result.NeedsInput {
		t.Error("NeedsInput should be true")
	}
	if result.Question != "Per-IP or per-user?" {
		t.Errorf("Question = %q", result.Question)
	}
}

func TestParseStream_errorResult(t *testing.T) {
	jsonStream := `
{"type":"system","subtype":"init","session_id":"sess-err"}
{"type":"result","subtype":"error","is_error":true,"result":"Something went wrong","session_id":"sess-err","num_turns":1}
`
	result, err := parseStream(strings.NewReader(jsonStream))
	if err != nil {
		t.Fatalf("parseStream: %v", err)
	}
	if !result.IsError {
		t.Error("IsError should be true")
	}
}

func TestParseStream_emptyOutput_usesAssistantText(t *testing.T) {
	// When result event has empty "result" field, use accumulated assistant texts
	jsonStream := `
{"type":"system","subtype":"init","session_id":"sess-1"}
{"type":"assistant","message":{"content":[{"type":"text","text":"Hello there."}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":" Done."}]}}
{"type":"result","subtype":"success","is_error":false,"result":"","session_id":"sess-1","num_turns":2}
`
	result, err := parseStream(strings.NewReader(jsonStream))
	if err != nil {
		t.Fatalf("parseStream: %v", err)
	}
	if !strings.Contains(result.Output, "Hello there.") {
		t.Errorf("Output should contain assistant text, got %q", result.Output)
	}
}

func TestParseStream_skipsGarbage(t *testing.T) {
	jsonStream := `
not json at all
{"type":"system","subtype":"init","session_id":"sess-clean"}
also not json
{"type":"result","is_error":false,"result":"OK","session_id":"sess-clean"}
`
	result, err := parseStream(strings.NewReader(jsonStream))
	if err != nil {
		t.Fatalf("parseStream: %v", err)
	}
	if result.SessionID != "sess-clean" {
		t.Errorf("SessionID = %q, want sess-clean", result.SessionID)
	}
}

func TestBuildFirstRunPrompt(t *testing.T) {
	prompt := BuildFirstRunPrompt("Add rate limiting", "Per-IP, 5/min", "some comments", 42, 0)
	if !strings.Contains(prompt, "Add rate limiting") {
		t.Error("prompt missing title")
	}
	if !strings.Contains(prompt, "NEEDS_CLARIFICATION") {
		t.Error("prompt missing clarification instruction")
	}
	if !strings.Contains(prompt, "some comments") {
		t.Error("prompt missing thread")
	}
	if !strings.Contains(prompt, "madar/issue-42") {
		t.Error("prompt missing branch name")
	}
	if !strings.Contains(prompt, "PR: #") {
		t.Error("prompt missing PR reporting instruction")
	}
}

func TestBuildFirstRunPrompt_truncatesBody(t *testing.T) {
	longBody := strings.Repeat("x", 5000)
	prompt := BuildFirstRunPrompt("Title", longBody, "", 1, 100)
	if strings.Contains(prompt, longBody) {
		t.Error("prompt should not contain full long body")
	}
	if !strings.Contains(prompt, "truncated") {
		t.Error("prompt should contain truncation marker")
	}
	if !strings.Contains(prompt, strings.Repeat("x", 100)) {
		t.Error("prompt should contain first 100 chars of body")
	}
}

func TestBuildFirstRunPrompt_noLimitWhenZero(t *testing.T) {
	body := strings.Repeat("y", 5000)
	prompt := BuildFirstRunPrompt("Title", body, "", 1, 0)
	if !strings.Contains(prompt, body) {
		t.Error("with limit=0, full body should be included")
	}
}

func TestBuildResumePrompt(t *testing.T) {
	entries := []ReplyEntry{
		{Author: "alice", Body: "Use per-IP, 5 req/min", Timestamp: "2024-01-01T10:00:00Z"},
	}
	prompt := BuildResumePrompt(entries)
	if !strings.Contains(prompt, "Use per-IP") {
		t.Error("resume prompt missing human reply")
	}
	if !strings.Contains(prompt, "alice") {
		t.Error("resume prompt missing author")
	}
	if !strings.Contains(prompt, "2024-01-01") {
		t.Error("resume prompt missing timestamp")
	}
	if !strings.Contains(prompt, "Continue") {
		t.Error("resume prompt missing continue instruction")
	}
}

func TestBuildResumePrompt_multipleReplies(t *testing.T) {
	entries := []ReplyEntry{
		{Author: "alice", Body: "Use per-IP", Timestamp: "2024-01-01T10:00:00Z"},
		{Author: "alice", Body: "Actually, per-account", Timestamp: "2024-01-01T10:05:00Z"},
	}
	prompt := BuildResumePrompt(entries)
	if !strings.Contains(prompt, "2 messages") {
		t.Error("multi-reply prompt should say how many messages")
	}
	if !strings.Contains(prompt, "per-IP") || !strings.Contains(prompt, "per-account") {
		t.Error("multi-reply prompt should contain both replies")
	}
}

// helpers

func assertContains(t *testing.T, args []string, want string) {
	t.Helper()
	for _, a := range args {
		if a == want {
			return
		}
	}
	t.Errorf("args %v does not contain %q", args, want)
}

func assertContainsSeq(t *testing.T, args []string, key, val string) {
	t.Helper()
	for i, a := range args {
		if a == key && i+1 < len(args) && args[i+1] == val {
			return
		}
	}
	t.Errorf("args %v does not contain sequence [%q, %q]", args, key, val)
}
