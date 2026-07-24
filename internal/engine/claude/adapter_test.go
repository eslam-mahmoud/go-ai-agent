package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/engine"
)

func TestAdapterImplementsEngine(t *testing.T) {
	var provider engine.Engine = New("")
	if provider.Name() != "claude" {
		t.Errorf("Name = %q, want claude", provider.Name())
	}

	capabilities, err := provider.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	want := engine.CapabilitySet{
		Resume:       true,
		Streaming:    true,
		Usage:        true,
		Cancellation: false,
	}
	if !reflect.DeepEqual(capabilities, want) {
		t.Errorf("Capabilities = %#v, want %#v", capabilities, want)
	}
}

func TestBuildArgs(t *testing.T) {
	request := engine.RunRequest{
		Prompt:    "do the thing",
		Model:     "sonnet",
		SessionID: "new-session",
		MaxTurns:  20,
		Policy:    engine.Policy{SkipPermissions: true},
	}
	args := buildArgs(request, false)
	for _, pair := range [][2]string{
		{"-p", "do the thing"},
		{"--output-format", "stream-json"},
		{"--session-id", "new-session"},
		{"--model", "sonnet"},
		{"--max-turns", "20"},
	} {
		assertArgPair(t, args, pair[0], pair[1])
	}
	assertArg(t, args, "--verbose")
	assertArg(t, args, "--dangerously-skip-permissions")
	assertNoArg(t, args, "--resume")

	request.ResumeSessionID = "resume-session"
	resumeArgs := buildArgs(request, true)
	assertArgPair(t, resumeArgs, "--resume", "resume-session")
	assertNoArg(t, resumeArgs, "--session-id")
}

func TestParseStreamNormalizesEventsResultAndUsage(t *testing.T) {
	stream := `
{"type":"system","subtype":"init","session_id":"sess-abc"}
{"type":"assistant","message":{"content":[{"type":"text","text":"Working on it"}]}}
{"type":"result","is_error":false,"result":"Task completed","session_id":"sess-abc","num_turns":3,"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":2}}
`
	var events []engine.Event
	parsed, err := parseStream(strings.NewReader(stream), func(event engine.Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatalf("parseStream: %v", err)
	}
	if err := parsed.emitTerminal(func(event engine.Event) error {
		events = append(events, event)
		return nil
	}); err != nil {
		t.Fatalf("emitTerminal: %v", err)
	}
	result := parsed.result
	if result.SessionID != "sess-abc" || result.Status != engine.ResultCompleted ||
		result.OutputText != "Task completed" {
		t.Fatalf("result = %#v", result)
	}
	if result.Usage != (engine.Usage{InputTokens: 10, OutputTokens: 5, CachedInputTokens: 2}) {
		t.Errorf("Usage = %#v", result.Usage)
	}

	wantTypes := []engine.EventType{
		engine.EventSessionStarted,
		engine.EventProgress,
		engine.EventUsage,
		engine.EventCompleted,
	}
	if len(events) != len(wantTypes) {
		t.Fatalf("events = %#v, want %d", events, len(wantTypes))
	}
	for i, wantType := range wantTypes {
		if events[i].Type != wantType || events[i].Sequence != int64(i+1) {
			t.Errorf("event %d = %#v, want type %s sequence %d", i, events[i], wantType, i+1)
		}
		if events[i].Timestamp.IsZero() {
			t.Errorf("event %d has zero timestamp", i)
		}
	}
	var terminal struct {
		NumTurns int `json:"num_turns"`
	}
	if err := json.Unmarshal(events[len(events)-1].Data, &terminal); err != nil {
		t.Fatalf("decode terminal data: %v", err)
	}
	if terminal.NumTurns != 3 {
		t.Errorf("terminal num_turns = %d, want 3", terminal.NumTurns)
	}
}

func TestParseStreamNormalizesProviderFailure(t *testing.T) {
	stream := `{"type":"result","is_error":true,"result":"provider rejected task","session_id":"sess-err"}`
	var events []engine.Event
	parsed, err := parseStream(strings.NewReader(stream), func(event engine.Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatalf("parseStream: %v", err)
	}
	if err := parsed.emitTerminal(func(event engine.Event) error {
		events = append(events, event)
		return nil
	}); err != nil {
		t.Fatalf("emitTerminal: %v", err)
	}
	result := parsed.result
	if result.Status != engine.ResultFailed || result.OutputText != "provider rejected task" {
		t.Fatalf("result = %#v", result)
	}
	if len(events) != 1 || events[0].Type != engine.EventFailed {
		t.Errorf("events = %#v, want one failed event", events)
	}
}

func TestParseStreamRejectsMissingOrEmptySuccessWithoutTerminalEvent(t *testing.T) {
	cases := []struct {
		name   string
		stream string
		want   string
	}{
		{
			name:   "missing terminal",
			stream: `{"type":"assistant","message":{"content":[{"type":"text","text":"partial"}]}}`,
			want:   "terminal result",
		},
		{
			name:   "empty success",
			stream: `{"type":"result","is_error":false,"result":"","session_id":"sess-empty"}`,
			want:   "empty successful",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var events []engine.Event
			parsed, err := parseStream(strings.NewReader(tc.stream), func(event engine.Event) error {
				events = append(events, event)
				return nil
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parseStream error = %v, want %q", err, tc.want)
			}
			if parsed != nil {
				t.Errorf("parsed = %#v, want nil", parsed)
			}
			for _, event := range events {
				if event.Type == engine.EventCompleted || event.Type == engine.EventFailed {
					t.Errorf("invalid stream emitted terminal event: %#v", event)
				}
			}
		})
	}
}

func TestParseStreamUsesAssistantTextFallbackAndSkipsGarbage(t *testing.T) {
	stream := `
not json
{"type":"assistant","message":{"content":[{"type":"text","text":"Hello"},{"type":"tool_use"}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":"Done"}]}}
{"type":"result","is_error":false,"result":"","session_id":"sess-1"}
`
	parsed, err := parseStream(strings.NewReader(stream), nil)
	if err != nil {
		t.Fatalf("parseStream: %v", err)
	}
	if parsed.result.OutputText != "Hello\nDone" {
		t.Errorf("OutputText = %q, want assistant fallback", parsed.result.OutputText)
	}
}

func TestAdapterDoesNotEmitTerminalEventForNonZeroExit(t *testing.T) {
	bin := writeFakeClaude(t,
		`{"type":"result","is_error":false,"result":"looks successful"}`,
		"",
		9,
	)
	var events []engine.Event
	result, err := New(bin).Run(context.Background(), engine.RunRequest{
		WorkDir: t.TempDir(),
		Prompt:  "test",
	}, func(event engine.Event) error {
		events = append(events, event)
		return nil
	})
	if result != nil || engine.ClassOf(err) != engine.ErrorProcessExit {
		t.Fatalf("result = %#v, error = %v", result, err)
	}
	for _, event := range events {
		if event.Type == engine.EventCompleted || event.Type == engine.EventFailed {
			t.Errorf("non-zero process emitted terminal event: %#v", event)
		}
	}
}

func TestAdapterRunAndResumeUseProviderSpecificArguments(t *testing.T) {
	argsPath := filepath.Join(t.TempDir(), "args")
	bin := writeFakeClaudeScript(t,
		"printf '%s\\n' \"$@\" > \"$CAPTURE_ARGS\"\n"+
			"printf '%s\\n' '{\"type\":\"result\",\"is_error\":false,\"result\":\"completed\",\"session_id\":\"session-1\"}'\n",
	)
	provider := New(bin)

	result, err := provider.Run(context.Background(), engine.RunRequest{
		WorkDir:     t.TempDir(),
		Prompt:      "start",
		SessionID:   "new-session",
		Timeout:     5 * time.Second,
		Environment: map[string]string{"CAPTURE_ARGS": argsPath},
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != engine.ResultCompleted || result.StartedAt.IsZero() || result.CompletedAt.IsZero() {
		t.Errorf("Run result = %#v", result)
	}
	args := readFile(t, argsPath)
	if !strings.Contains(args, "--session-id\nnew-session\n") || strings.Contains(args, "--resume") {
		t.Errorf("Run args = %q", args)
	}

	_, err = provider.Resume(context.Background(), engine.RunRequest{
		WorkDir:         t.TempDir(),
		Prompt:          "continue",
		ResumeSessionID: "session-1",
		Timeout:         5 * time.Second,
		Environment:     map[string]string{"CAPTURE_ARGS": argsPath},
	}, nil)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	args = readFile(t, argsPath)
	if !strings.Contains(args, "--resume\nsession-1\n") || strings.Contains(args, "--session-id") {
		t.Errorf("Resume args = %q", args)
	}
}

func TestAdapterResumeRequiresSession(t *testing.T) {
	result, err := New("/bin/sh").Resume(context.Background(), engine.RunRequest{}, nil)
	if result != nil {
		t.Errorf("result = %#v, want nil", result)
	}
	if engine.ClassOf(err) != engine.ErrorSessionMissing {
		t.Errorf("error = %v, class = %s", err, engine.ClassOf(err))
	}
}

func TestAdapterRejectsNonZeroExitAndIncludesBoundedSanitizedStderr(t *testing.T) {
	stderr := "authentication\x00 failed\n" + strings.Repeat("x", maxStderrBytes*2)
	bin := writeFakeClaude(t,
		`{"type":"result","is_error":false,"result":"looks successful"}`,
		stderr,
		23,
	)
	result, err := New(bin).Run(context.Background(), engine.RunRequest{
		WorkDir: t.TempDir(),
		Prompt:  "test",
		Timeout: 5 * time.Second,
	}, nil)
	if result != nil || err == nil {
		t.Fatalf("result = %#v, error = %v", result, err)
	}
	if engine.ClassOf(err) != engine.ErrorProcessExit {
		t.Errorf("class = %s, want %s", engine.ClassOf(err), engine.ErrorProcessExit)
	}
	if !strings.Contains(err.Error(), "exit status 23") ||
		!strings.Contains(err.Error(), "authentication failed") ||
		!strings.Contains(err.Error(), "[truncated]") {
		t.Errorf("error does not preserve safe diagnostics: %q", err)
	}
	if strings.ContainsAny(err.Error(), "\x00\n\r\t") {
		t.Errorf("error contains unsanitized controls: %q", err)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Errorf("error %T does not wrap *exec.ExitError", err)
	}
}

func TestAdapterClassifiesExecutionFailures(t *testing.T) {
	t.Run("missing provider", func(t *testing.T) {
		result, err := New(filepath.Join(t.TempDir(), "missing")).Run(
			context.Background(),
			engine.RunRequest{WorkDir: t.TempDir(), Prompt: "test"},
			nil,
		)
		if result != nil || engine.ClassOf(err) != engine.ErrorProviderUnavailable {
			t.Errorf("result = %#v, error = %v, class = %s", result, err, engine.ClassOf(err))
		}
	})

	t.Run("invalid workspace", func(t *testing.T) {
		result, err := New("/bin/sh").Run(
			context.Background(),
			engine.RunRequest{WorkDir: filepath.Join(t.TempDir(), "missing"), Prompt: "test"},
			nil,
		)
		if result != nil || engine.ClassOf(err) != engine.ErrorWorkspaceInvalid {
			t.Errorf("result = %#v, error = %v, class = %s", result, err, engine.ClassOf(err))
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		result, err := New("/bin/sh").Run(ctx, engine.RunRequest{WorkDir: t.TempDir()}, nil)
		if result != nil || !errors.Is(err, context.Canceled) ||
			engine.ClassOf(err) != engine.ErrorCancelled {
			t.Errorf("result = %#v, error = %v, class = %s", result, err, engine.ClassOf(err))
		}
	})

	t.Run("timeout", func(t *testing.T) {
		bin := writeFakeClaudeScript(t, "while :; do :; done\n")
		result, err := New(bin).Run(context.Background(), engine.RunRequest{
			WorkDir: t.TempDir(),
			Prompt:  "test",
			Timeout: 50 * time.Millisecond,
		}, nil)
		if result != nil || !errors.Is(err, context.DeadlineExceeded) ||
			engine.ClassOf(err) != engine.ErrorTimeout {
			t.Errorf("result = %#v, error = %v, class = %s", result, err, engine.ClassOf(err))
		}
	})

	t.Run("invalid output", func(t *testing.T) {
		bin := writeFakeClaude(t, "", "", 0)
		result, err := New(bin).Run(context.Background(), engine.RunRequest{
			WorkDir: t.TempDir(),
			Prompt:  "test",
		}, nil)
		if result != nil || engine.ClassOf(err) != engine.ErrorInvalidOutput {
			t.Errorf("result = %#v, error = %v, class = %s", result, err, engine.ClassOf(err))
		}
	})
}

func TestAdapterRejectsUnsupportedSchemaAndCancel(t *testing.T) {
	result, err := New("/bin/sh").Run(context.Background(), engine.RunRequest{
		OutputSchema: json.RawMessage(`{"type":"object"}`),
	}, nil)
	if result != nil || engine.ClassOf(err) != engine.ErrorPolicyDenied {
		t.Errorf("result = %#v, error = %v", result, err)
	}
	if err := New("/bin/sh").Cancel(context.Background(), "execution-1"); err == nil {
		t.Fatal("Cancel returned nil for unsupported out-of-band cancellation")
	}
}

func TestLimitedStderrCaptureIsBounded(t *testing.T) {
	capture := &limitedBuffer{limit: 16}
	if _, err := capture.Write([]byte("bad\x00line\n" + strings.Repeat("x", 32))); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if capture.buf.Len() != 16 || !capture.truncated {
		t.Fatalf("capture = %d bytes, truncated=%v", capture.buf.Len(), capture.truncated)
	}
	got := sanitizeStderr(capture.String(), capture.truncated)
	if strings.ContainsAny(got, "\x00\n\r\t") || !strings.HasSuffix(got, "[truncated]") {
		t.Errorf("sanitized stderr = %q", got)
	}
}

func assertArg(t *testing.T, args []string, want string) {
	t.Helper()
	for _, arg := range args {
		if arg == want {
			return
		}
	}
	t.Errorf("args %v do not contain %q", args, want)
}

func assertNoArg(t *testing.T, args []string, want string) {
	t.Helper()
	for _, arg := range args {
		if arg == want {
			t.Errorf("args %v unexpectedly contain %q", args, want)
		}
	}
}

func assertArgPair(t *testing.T, args []string, key, value string) {
	t.Helper()
	for i := range args {
		if args[i] == key && i+1 < len(args) && args[i+1] == value {
			return
		}
	}
	t.Errorf("args %v do not contain [%q, %q]", args, key, value)
}

func writeFakeClaude(t *testing.T, stdout, stderr string, exitCode int) string {
	t.Helper()
	var script string
	if stdout != "" {
		script += fmt.Sprintf("printf '%%s\\n' %s\n", shellQuote(stdout))
	}
	if stderr != "" {
		script += fmt.Sprintf("printf '%%s\\n' %s >&2\n", shellQuote(stderr))
	}
	script += fmt.Sprintf("exit %d\n", exitCode)
	return writeFakeClaudeScript(t, script)
}

func writeFakeClaudeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatalf("write fake Claude binary: %v", err)
	}
	return path
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content)
}
