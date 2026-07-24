package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/engine"
)

func TestAdapterContractAndCapabilities(t *testing.T) {
	var provider engine.Engine = New("")
	if provider.Name() != "codex" {
		t.Errorf("Name = %q", provider.Name())
	}
	got, err := provider.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := engine.CapabilitySet{
		Resume:           true,
		StructuredOutput: true,
		Streaming:        true,
		Usage:            true,
		OutputSchema:     true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Capabilities = %#v, want %#v", got, want)
	}
}

func TestBuildArgsRunAndResume(t *testing.T) {
	request := engine.RunRequest{
		Prompt: "do work", Model: "gpt-test", ResumeSessionID: "thread-1",
		Policy: engine.Policy{Sandbox: "workspace-write", ApprovalPolicy: "never"},
	}
	run := buildArgs(request, false, "")
	assertSequence(t, run, "exec", "--json")
	assertSequence(t, run, "--model", "gpt-test")
	assertSequence(t, run, "--sandbox", "workspace-write")
	assertContains(t, run, `approval_policy="never"`)
	if run[len(run)-1] != "do work" {
		t.Errorf("run args = %v", run)
	}

	resume := buildArgs(request, true, "")
	assertSequence(t, resume, "exec", "resume")
	assertContains(t, resume, `sandbox_mode="workspace-write"`)
	assertSequence(t, resume, "thread-1", "do work")

	request.Policy.SkipPermissions = true
	bypass := buildArgs(request, false, "")
	assertContains(t, bypass, "--dangerously-bypass-approvals-and-sandbox")
	if contains(bypass, "--sandbox") || contains(bypass, `approval_policy="never"`) {
		t.Errorf("bypass args include conflicting policy flags: %v", bypass)
	}
}

func TestParseStreamNormalizesDocumentedEvents(t *testing.T) {
	stream := `
{"type":"thread.started","thread_id":"thread-1"}
{"type":"turn.started"}
{"type":"item.started","item":{"id":"1","type":"command_execution","command":"go test","status":"in_progress"}}
{"type":"item.completed","item":{"id":"1","type":"command_execution","command":"go test","status":"completed"}}
{"type":"item.completed","item":{"id":"2","type":"file_change","changes":[{"path":"a.go"}]}}
{"type":"item.completed","item":{"id":"3","type":"agent_message","text":"Task complete"}}
{"type":"turn.completed","usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":20}}
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
		t.Fatal(err)
	}
	result := parsed.result
	if result.SessionID != "thread-1" || result.OutputText != "Task complete" ||
		result.Status != engine.ResultCompleted {
		t.Errorf("result = %#v", result)
	}
	if result.Usage != (engine.Usage{InputTokens: 100, CachedInputTokens: 40, OutputTokens: 20}) {
		t.Errorf("usage = %#v", result.Usage)
	}
	want := []engine.EventType{
		engine.EventSessionStarted, engine.EventStepStarted, engine.EventToolStarted,
		engine.EventToolCompleted, engine.EventFileChanged, engine.EventProgress,
		engine.EventUsage, engine.EventCompleted,
	}
	if len(events) != len(want) {
		t.Fatalf("events = %#v", events)
	}
	for index := range want {
		if events[index].Type != want[index] || events[index].Sequence != int64(index+1) {
			t.Errorf("event %d = %#v", index, events[index])
		}
	}
}

func TestParseStreamFailureAndInvalidOutput(t *testing.T) {
	failed, err := parseStream(strings.NewReader(
		`{"type":"turn.failed","error":{"message":"model failed"}}`,
	), nil)
	if err != nil || failed.result.Status != engine.ResultFailed ||
		failed.result.OutputText != "model failed" {
		t.Fatalf("failed = %#v, err = %v", failed, err)
	}
	for _, tc := range []struct{ stream, want string }{
		{`{"type":"turn.started"}`, "terminal"},
		{`{"type":"turn.completed","usage":{}}`, "empty successful"},
	} {
		if _, err := parseStream(strings.NewReader(tc.stream), nil); err == nil ||
			!strings.Contains(err.Error(), tc.want) {
			t.Errorf("error = %v, want %q", err, tc.want)
		}
	}
}

func TestAdapterProcessGuarantees(t *testing.T) {
	t.Run("success and resume", func(t *testing.T) {
		argsPath := filepath.Join(t.TempDir(), "args")
		bin := fakeScript(t, fmt.Sprintf(
			"printf '%%s\\n' \"$@\" > %s\nprintf '%%s\\n' '%s'\n",
			shellQuote(argsPath),
			`{"type":"thread.started","thread_id":"thread-1"}
{"type":"item.completed","item":{"type":"agent_message","text":"done"}}
{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":2}}`,
		))
		result, err := New(bin).Resume(context.Background(), engine.RunRequest{
			WorkDir: t.TempDir(), Prompt: "continue", ResumeSessionID: "thread-1",
		}, nil)
		if err != nil || result.OutputText != "done" {
			t.Fatalf("result = %#v, err = %v", result, err)
		}
		if result.SessionID != "thread-1" || result.StartedAt.IsZero() ||
			result.CompletedAt.IsZero() || result.Usage.InputTokens != 1 ||
			result.Usage.OutputTokens != 2 {
			t.Errorf("normalized result = %#v", result)
		}
		args, _ := os.ReadFile(argsPath)
		if !strings.Contains(string(args), "resume\n") ||
			!strings.Contains(string(args), "thread-1\ncontinue\n") {
			t.Errorf("args = %q", args)
		}
	})

	t.Run("nonzero emits no terminal", func(t *testing.T) {
		bin := fakeScript(t, "printf '%s\\n' '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"done\"}}' '{\"type\":\"turn.completed\"}'\nexit 9\n")
		var events []engine.Event
		result, err := New(bin).Run(context.Background(), engine.RunRequest{
			WorkDir:      t.TempDir(),
			OutputSchema: json.RawMessage(`{"type":"object"}`),
		}, func(event engine.Event) error {
			events = append(events, event)
			return nil
		})
		if result != nil || engine.ClassOf(err) != engine.ErrorProcessExit {
			t.Fatalf("result = %#v, err = %v", result, err)
		}
		for _, event := range events {
			if event.Type == engine.EventCompleted || event.Type == engine.EventFailed {
				t.Errorf("terminal event = %#v", event)
			}
		}
	})

	t.Run("provider failure emits failed after zero exit", func(t *testing.T) {
		bin := fakeScript(t, "printf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"thread-2\"}' '{\"type\":\"turn.failed\",\"error\":{\"message\":\"model failed\"}}'\n")
		var events []engine.Event
		result, err := New(bin).Run(context.Background(), engine.RunRequest{
			WorkDir:      t.TempDir(),
			OutputSchema: json.RawMessage(`{"type":"object"}`),
		}, func(event engine.Event) error {
			events = append(events, event)
			return nil
		})
		if err != nil || result.Status != engine.ResultFailed || result.OutputText != "model failed" {
			t.Fatalf("result = %#v, error = %v", result, err)
		}
		if len(events) != 2 || events[0].Type != engine.EventSessionStarted ||
			events[1].Type != engine.EventFailed {
			t.Errorf("events = %#v", events)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		result, err := New(fakeScript(t, "while :; do :; done\n")).Run(
			context.Background(),
			engine.RunRequest{WorkDir: t.TempDir(), Timeout: 50 * time.Millisecond},
			nil,
		)
		if result != nil || !errors.Is(err, context.DeadlineExceeded) ||
			engine.ClassOf(err) != engine.ErrorTimeout {
			t.Errorf("result = %#v, err = %v", result, err)
		}
	})
}

func TestAdapterClassifiesEventCallbackFailure(t *testing.T) {
	bin := fakeScript(t, `
printf '%s\n' '{"type":"thread.started","thread_id":"thread-1"}'
i=0
while [ "$i" -lt 20000 ]; do
  printf '%s\n' '{"type":"item.completed","item":{"type":"reasoning","text":"keep draining"}}'
  i=$((i + 1))
done
printf '%s\n' '{"type":"turn.failed","message":"failed"}'
`)
	callbackErr := errors.New("event store unavailable")
	result, err := New(bin).Run(context.Background(), engine.RunRequest{
		WorkDir: t.TempDir(),
		Timeout: 5 * time.Second,
	}, func(engine.Event) error {
		return callbackErr
	})
	if result != nil || !errors.Is(err, callbackErr) || engine.ClassOf(err) != engine.ErrorUnknown {
		t.Errorf("result = %#v, error = %v, class = %s", result, err, engine.ClassOf(err))
	}
}

func TestAdapterValidationAndClassification(t *testing.T) {
	if _, err := New("/bin/sh").Resume(context.Background(), engine.RunRequest{}, nil); engine.ClassOf(err) != engine.ErrorSessionMissing {
		t.Errorf("resume class = %s", engine.ClassOf(err))
	}
	if _, err := New(filepath.Join(t.TempDir(), "missing")).Run(context.Background(), engine.RunRequest{WorkDir: t.TempDir()}, nil); engine.ClassOf(err) != engine.ErrorProviderUnavailable {
		t.Errorf("missing class = %s", engine.ClassOf(err))
	}
	if _, err := New("/bin/sh").Run(context.Background(), engine.RunRequest{WorkDir: filepath.Join(t.TempDir(), "missing")}, nil); engine.ClassOf(err) != engine.ErrorWorkspaceInvalid {
		t.Errorf("workspace class = %s", engine.ClassOf(err))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New("/bin/sh").Run(ctx, engine.RunRequest{WorkDir: t.TempDir()}, nil); !errors.Is(err, context.Canceled) || engine.ClassOf(err) != engine.ErrorCancelled {
		t.Errorf("cancellation error = %v, class = %s", err, engine.ClassOf(err))
	}
	if _, err := New("/bin/sh").Run(context.Background(), engine.RunRequest{OutputSchema: []byte(`{`)}, nil); engine.ClassOf(err) != engine.ErrorPolicyDenied {
		t.Errorf("invalid schema class = %s", engine.ClassOf(err))
	}
	if err := New("/bin/sh").Cancel(context.Background(), "run-1"); err == nil {
		t.Error("Cancel returned nil")
	}
}

func TestAdapterValidatesStructuredOutputAndRemovesSchemaFile(t *testing.T) {
	argsPath := filepath.Join(t.TempDir(), "args")
	schemaPathCapture := filepath.Join(t.TempDir(), "schema-path")
	bin := fakeScript(t, `
printf '%s\n' "$@" > "$CAPTURE_ARGS"
previous=""
for argument in "$@"; do
  if [ "$previous" = "--output-schema" ]; then
    printf '%s' "$argument" > "$CAPTURE_SCHEMA_PATH"
    test -s "$argument" || exit 12
  fi
  previous="$argument"
done
printf '%s\n' '{"type":"thread.started","thread_id":"thread-json"}'
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"status\":\"completed\",\"count\":2}"}}'
printf '%s\n' '{"type":"turn.completed"}'
`)
	schema := json.RawMessage(`{
		"$schema":"https://json-schema.org/draft/2020-12/schema",
		"type":"object",
		"properties":{"status":{"const":"completed"},"count":{"type":"integer"}},
		"required":["status","count"],
		"additionalProperties":false
	}`)
	result, err := New(bin).Run(context.Background(), engine.RunRequest{
		WorkDir:      t.TempDir(),
		Prompt:       "return JSON",
		OutputSchema: schema,
		Environment: map[string]string{
			"CAPTURE_ARGS":        argsPath,
			"CAPTURE_SCHEMA_PATH": schemaPathCapture,
		},
	}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(result.OutputJSON) != `{"status":"completed","count":2}` {
		t.Errorf("OutputJSON = %s", result.OutputJSON)
	}
	args, _ := os.ReadFile(argsPath)
	if !strings.Contains(string(args), "--output-schema\n") {
		t.Errorf("args = %q", args)
	}
	schemaPathBytes, _ := os.ReadFile(schemaPathCapture)
	schemaPath := string(schemaPathBytes)
	if schemaPath == "" {
		t.Fatal("schema path was not captured")
	}
	if _, err := os.Stat(schemaPath); !os.IsNotExist(err) {
		t.Errorf("temporary schema still exists: %s (err=%v)", schemaPath, err)
	}
}

func TestAdapterRejectsStructuredMismatchWithoutTerminalEvent(t *testing.T) {
	bin := fakeScript(t, `
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"count\":\"two\"}"}}'
printf '%s\n' '{"type":"turn.completed"}'
`)
	var events []engine.Event
	result, err := New(bin).Run(context.Background(), engine.RunRequest{
		WorkDir:      t.TempDir(),
		OutputSchema: json.RawMessage(`{"type":"object","properties":{"count":{"type":"integer"}},"required":["count"]}`),
	}, func(event engine.Event) error {
		events = append(events, event)
		return nil
	})
	if result != nil || engine.ClassOf(err) != engine.ErrorInvalidOutput {
		t.Fatalf("result = %#v, error = %v", result, err)
	}
	for _, event := range events {
		if event.Type == engine.EventCompleted || event.Type == engine.EventFailed {
			t.Errorf("invalid output emitted terminal event: %#v", event)
		}
	}
}

func TestAdapterRemovesSchemaFileOnFailurePaths(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	cases := []struct {
		name    string
		binary  func(*testing.T) string
		context func() context.Context
	}{
		{
			name:   "cancelled before start",
			binary: func(*testing.T) string { return "/bin/sh" },
			context: func() context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx
			},
		},
		{
			name: "nonzero exit",
			binary: func(t *testing.T) string {
				return fakeScript(t, "exit 7\n")
			},
			context: context.Background,
		},
		{
			name: "invalid output",
			binary: func(t *testing.T) string {
				return fakeScript(t, "printf '%s\\n' '{\"type\":\"turn.completed\"}'\n")
			},
			context: context.Background,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tempDir := t.TempDir()
			provider := &Adapter{binary: tc.binary(t), schemaTempDir: tempDir}
			_, _ = provider.Run(tc.context(), engine.RunRequest{
				WorkDir:      t.TempDir(),
				OutputSchema: schema,
			}, nil)
			entries, err := os.ReadDir(tempDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Errorf("temporary schema files remain: %v", entries)
			}
		})
	}
}

func TestAdapterInvalidSchemaDoesNotLaunchProvider(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "launched")
	bin := fakeScript(t, "touch "+shellQuote(marker)+"\n")
	result, err := New(bin).Run(context.Background(), engine.RunRequest{
		WorkDir:      t.TempDir(),
		OutputSchema: json.RawMessage(`{`),
	}, nil)
	if result != nil || engine.ClassOf(err) != engine.ErrorPolicyDenied {
		t.Fatalf("result = %#v, error = %v", result, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("provider launched for invalid schema (err=%v)", err)
	}
}

func TestAdapterClassifiesProcessDiagnostics(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
		resume bool
		want   engine.ErrorClass
	}{
		{"authentication", "Not logged in; API key required", false, engine.ErrorAuthentication},
		{"rate limit", "request failed: 429 rate limit", false, engine.ErrorRateLimit},
		{"resume", "session thread-1 was not found", true, engine.ErrorSessionResumeFailed},
		{"generic", "unexpected failure", false, engine.ErrorProcessExit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bin := fakeScript(t, fmt.Sprintf("printf '%%s\\n' %s >&2\nexit 7\n", shellQuote(tc.stderr)))
			request := engine.RunRequest{WorkDir: t.TempDir(), Prompt: "test"}
			var err error
			if tc.resume {
				request.ResumeSessionID = "thread-1"
				_, err = New(bin).Resume(context.Background(), request, nil)
			} else {
				_, err = New(bin).Run(context.Background(), request, nil)
			}
			if engine.ClassOf(err) != tc.want {
				t.Errorf("error = %v, class = %s, want %s", err, engine.ClassOf(err), tc.want)
			}
			if !strings.Contains(err.Error(), tc.stderr) {
				t.Errorf("error lacks stderr: %q", err)
			}
		})
	}
}

func TestAdapterRejectsInvalidSuccessfulStreams(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stdout string
		want   string
	}{
		{"missing terminal", `{"type":"turn.started"}`, "terminal"},
		{"empty success", `{"type":"turn.completed"}`, "empty successful"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := fakeScript(t, fmt.Sprintf("printf '%%s\\n' %s\n", shellQuote(tc.stdout)))
			result, err := New(bin).Run(context.Background(), engine.RunRequest{WorkDir: t.TempDir()}, nil)
			if result != nil || engine.ClassOf(err) != engine.ErrorInvalidOutput ||
				!strings.Contains(err.Error(), tc.want) {
				t.Errorf("result = %#v, error = %v", result, err)
			}
		})
	}
}

func TestLimitedStderrIsBoundedAndSanitized(t *testing.T) {
	capture := &limitedBuffer{limit: 16}
	_, _ = capture.Write([]byte("bad\x00line\n" + strings.Repeat("x", 40)))
	if capture.buf.Len() != 16 || !capture.truncated {
		t.Fatalf("capture bytes = %d, truncated = %v", capture.buf.Len(), capture.truncated)
	}
	got := sanitizeStderr(capture.String(), capture.truncated)
	if strings.ContainsAny(got, "\x00\n\r\t") || !strings.HasSuffix(got, "[truncated]") {
		t.Errorf("sanitized stderr = %q", got)
	}
}

func assertContains(t *testing.T, args []string, want string) {
	t.Helper()
	for _, arg := range args {
		if arg == want {
			return
		}
	}
	t.Errorf("args %v missing %q", args, want)
}

func contains(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func assertSequence(t *testing.T, args []string, sequence ...string) {
	t.Helper()
	for index := 0; index+len(sequence) <= len(args); index++ {
		if reflect.DeepEqual(args[index:index+len(sequence)], sequence) {
			return
		}
	}
	t.Errorf("args %v missing sequence %v", args, sequence)
}

func fakeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-codex")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
