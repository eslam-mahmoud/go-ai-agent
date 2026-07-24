package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/eslam-mahmoud/go-ai-agent/internal/engine"
)

const (
	defaultBinary  = "codex"
	maxStderrBytes = 8 * 1024
)

type Adapter struct {
	binary string
}

var _ engine.Engine = (*Adapter)(nil)

func New(binary string) *Adapter {
	if binary == "" {
		binary = defaultBinary
	}
	return &Adapter{binary: binary}
}

func (a *Adapter) Name() string { return "codex" }

func (a *Adapter) Capabilities(context.Context) (engine.CapabilitySet, error) {
	return engine.CapabilitySet{
		Resume:       true,
		Streaming:    true,
		Usage:        true,
		Cancellation: false,
	}, nil
}

func (a *Adapter) Run(ctx context.Context, request engine.RunRequest, emit func(engine.Event) error) (*engine.Result, error) {
	return a.run(ctx, request, false, emit)
}

func (a *Adapter) Resume(ctx context.Context, request engine.RunRequest, emit func(engine.Event) error) (*engine.Result, error) {
	if request.ResumeSessionID == "" {
		return nil, engine.NewExecutionError(
			engine.ErrorSessionMissing,
			a.Name(),
			"resume",
			errors.New("resume session ID is required"),
		)
	}
	return a.run(ctx, request, true, emit)
}

func (a *Adapter) Cancel(context.Context, string) error {
	return engine.NewExecutionError(
		engine.ErrorUnknown,
		"codex",
		"cancel",
		errors.New("out-of-band cancellation is not supported; cancel the run context"),
	)
}

func (a *Adapter) run(ctx context.Context, request engine.RunRequest, resume bool, emit func(engine.Event) error) (*engine.Result, error) {
	if len(request.OutputSchema) > 0 {
		return nil, engine.NewExecutionError(
			engine.ErrorPolicyDenied,
			a.Name(),
			"validate-request",
			errors.New("Codex schema support is deferred to structured-output validation"),
		)
	}
	if err := validateWorkDir(request.WorkDir); err != nil {
		return nil, engine.NewExecutionError(engine.ErrorWorkspaceInvalid, a.Name(), "validate-workspace", err)
	}

	cmdCtx := ctx
	var cancel context.CancelFunc
	if request.Timeout > 0 {
		cmdCtx, cancel = context.WithTimeout(ctx, request.Timeout)
		defer cancel()
	}
	if err := cmdCtx.Err(); err != nil {
		return nil, contextExecutionError("start", err, "")
	}

	cmd := exec.CommandContext(cmdCtx, a.binary, buildArgs(request, resume)...)
	cmd.Dir = request.WorkDir
	cmd.Env = mergeEnvironment(os.Environ(), request.Environment)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, engine.NewExecutionError(engine.ErrorUnknown, a.Name(), "open-stdout", fmt.Errorf("stdout pipe: %w", err))
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, engine.NewExecutionError(engine.ErrorUnknown, a.Name(), "open-stderr", fmt.Errorf("stderr pipe: %w", err))
	}

	startedAt := time.Now().UTC()
	if err := cmd.Start(); err != nil {
		if ctxErr := cmdCtx.Err(); ctxErr != nil {
			return nil, contextExecutionError("start", ctxErr, "")
		}
		return nil, engine.NewExecutionError(
			engine.ErrorProviderUnavailable,
			a.Name(),
			"start",
			fmt.Errorf("start codex: %w", err),
		)
	}

	stderrCapture := &limitedBuffer{limit: maxStderrBytes}
	stderrDone := make(chan error, 1)
	go func() {
		_, copyErr := io.Copy(stderrCapture, stderr)
		stderrDone <- copyErr
	}()

	parsed, parseErr := parseStream(stdout, emit)
	stderrErr := <-stderrDone
	waitErr := cmd.Wait()
	completedAt := time.Now().UTC()
	stderrText := sanitizeStderr(stderrCapture.String(), stderrCapture.truncated)

	if waitErr != nil && errors.Is(cmdCtx.Err(), context.DeadlineExceeded) {
		return nil, engine.NewExecutionError(
			engine.ErrorTimeout,
			a.Name(),
			"run",
			withStderr(fmt.Sprintf("codex timed out after %s", request.Timeout), context.DeadlineExceeded, stderrText),
		)
	}
	if waitErr != nil && errors.Is(cmdCtx.Err(), context.Canceled) {
		return nil, contextExecutionError("run", context.Canceled, stderrText)
	}
	if waitErr != nil {
		return nil, engine.NewExecutionError(
			classifyProcessExit(stderrText, resume),
			a.Name(),
			"run",
			withStderr("codex process failed", waitErr, stderrText),
		)
	}
	if parseErr != nil {
		var emissionErr *eventEmissionError
		if errors.As(parseErr, &emissionErr) {
			return nil, engine.NewExecutionError(engine.ErrorUnknown, a.Name(), "emit-event", emissionErr)
		}
		return nil, engine.NewExecutionError(
			engine.ErrorInvalidOutput,
			a.Name(),
			"parse-output",
			withStderr("parse codex stream", parseErr, stderrText),
		)
	}
	if stderrErr != nil {
		return nil, engine.NewExecutionError(engine.ErrorUnknown, a.Name(), "read-stderr", stderrErr)
	}

	result := parsed.result
	result.ExitCode = 0
	result.StartedAt = startedAt
	result.CompletedAt = completedAt
	if err := parsed.emitTerminal(emit); err != nil {
		return nil, engine.NewExecutionError(engine.ErrorUnknown, a.Name(), "emit-event", err)
	}
	return result, nil
}

func buildArgs(request engine.RunRequest, resume bool) []string {
	args := []string{"exec"}
	if resume {
		args = append(args, "resume")
	}
	args = append(args, "--json")
	if request.Model != "" {
		args = append(args, "--model", request.Model)
	}
	if request.Policy.SkipPermissions {
		args = append(args, "--dangerously-bypass-approvals-and-sandbox")
	} else {
		if request.Policy.ApprovalPolicy != "" {
			args = append(args, "-c", fmt.Sprintf("approval_policy=%q", request.Policy.ApprovalPolicy))
		}
		if request.Policy.Sandbox != "" {
			if resume {
				args = append(args, "-c", fmt.Sprintf("sandbox_mode=%q", request.Policy.Sandbox))
			} else {
				args = append(args, "--sandbox", request.Policy.Sandbox)
			}
		}
	}
	if resume {
		args = append(args, request.ResumeSessionID)
	}
	args = append(args, request.Prompt)
	return args
}

type rawEvent struct {
	Type     string          `json:"type"`
	ThreadID string          `json:"thread_id"`
	Message  string          `json:"message"`
	Error    json.RawMessage `json:"error"`
	Item     *rawItem        `json:"item"`
	Usage    *rawUsage       `json:"usage"`
}

type rawItem struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Status  string          `json:"status"`
	Text    string          `json:"text"`
	Command string          `json:"command"`
	Changes json.RawMessage `json:"changes"`
}

type rawUsage struct {
	InputTokens       int64 `json:"input_tokens"`
	CachedInputTokens int64 `json:"cached_input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
}

type parsedStream struct {
	result      *engine.Result
	terminalRaw json.RawMessage
	sequence    int64
}

type eventEmissionError struct {
	eventType engine.EventType
	err       error
}

func (e *eventEmissionError) Error() string {
	return fmt.Sprintf("emit %s event: %v", e.eventType, e.err)
}
func (e *eventEmissionError) Unwrap() error { return e.err }

func (p *parsedStream) emit(callback func(engine.Event) error, eventType engine.EventType, message string, data json.RawMessage) error {
	if callback == nil {
		return nil
	}
	p.sequence++
	if err := callback(engine.Event{
		Sequence:  p.sequence,
		Type:      eventType,
		Timestamp: time.Now().UTC(),
		SessionID: p.result.SessionID,
		Message:   message,
		Data:      data,
	}); err != nil {
		return &eventEmissionError{eventType: eventType, err: err}
	}
	return nil
}

func (p *parsedStream) emitTerminal(callback func(engine.Event) error) error {
	if p.result.Usage != (engine.Usage{}) {
		if err := p.emit(callback, engine.EventUsage, "", p.terminalRaw); err != nil {
			return err
		}
	}
	terminalType := engine.EventCompleted
	if p.result.Status == engine.ResultFailed {
		terminalType = engine.EventFailed
	}
	return p.emit(callback, terminalType, p.result.OutputText, p.terminalRaw)
}

func parseStream(reader io.Reader, emit func(engine.Event) error) (*parsedStream, error) {
	parsed := &parsedStream{result: &engine.Result{}}
	var agentMessages []string
	sawTerminal := false

	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		raw := json.RawMessage(append([]byte(nil), line...))
		var event rawEvent
		if err := json.Unmarshal(raw, &event); err != nil {
			continue
		}

		switch event.Type {
		case "thread.started":
			if event.ThreadID != "" {
				parsed.result.SessionID = event.ThreadID
				if err := parsed.emit(emit, engine.EventSessionStarted, "", raw); err != nil {
					return nil, drainStreamAfterError(reader, err)
				}
			}
		case "turn.started":
			if err := parsed.emit(emit, engine.EventStepStarted, "", raw); err != nil {
				return nil, drainStreamAfterError(reader, err)
			}
		case "item.started", "item.completed":
			if event.Item == nil {
				continue
			}
			eventType, message := normalizeItem(event.Type, event.Item)
			if event.Item.Type == "agent_message" && event.Type == "item.completed" && event.Item.Text != "" {
				agentMessages = append(agentMessages, event.Item.Text)
			}
			if eventType != "" {
				if err := parsed.emit(emit, eventType, message, raw); err != nil {
					return nil, drainStreamAfterError(reader, err)
				}
			}
		case "turn.completed":
			sawTerminal = true
			parsed.terminalRaw = raw
			parsed.result.Status = engine.ResultCompleted
			parsed.result.OutputText = ""
			if event.Usage != nil {
				parsed.result.Usage = engine.Usage{
					InputTokens:       event.Usage.InputTokens,
					CachedInputTokens: event.Usage.CachedInputTokens,
					OutputTokens:      event.Usage.OutputTokens,
				}
			}
		case "turn.failed", "error":
			sawTerminal = true
			parsed.terminalRaw = raw
			parsed.result.Status = engine.ResultFailed
			parsed.result.OutputText = eventErrorMessage(event)
		}
	}
	if err := scanner.Err(); err != nil {
		_, _ = io.Copy(io.Discard, reader)
		return nil, err
	}
	if !sawTerminal {
		return nil, errors.New("codex stream ended without terminal turn event")
	}
	if parsed.result.Status == engine.ResultCompleted {
		if len(agentMessages) > 0 {
			parsed.result.OutputText = agentMessages[len(agentMessages)-1]
		}
		if strings.TrimSpace(parsed.result.OutputText) == "" {
			return nil, errors.New("codex returned an empty successful result")
		}
	}
	return parsed, nil
}

func drainStreamAfterError(reader io.Reader, err error) error {
	_, _ = io.Copy(io.Discard, reader)
	return err
}

func normalizeItem(eventType string, item *rawItem) (engine.EventType, string) {
	switch item.Type {
	case "agent_message", "reasoning":
		if eventType == "item.completed" {
			return engine.EventProgress, item.Text
		}
	case "command_execution", "mcp_tool_call", "web_search":
		if eventType == "item.started" {
			return engine.EventToolStarted, item.Command
		}
		return engine.EventToolCompleted, item.Command
	case "file_change":
		if eventType == "item.completed" {
			return engine.EventFileChanged, ""
		}
	}
	return "", ""
}

func eventErrorMessage(event rawEvent) string {
	if event.Message != "" {
		return event.Message
	}
	if len(event.Error) > 0 {
		var stringValue string
		if json.Unmarshal(event.Error, &stringValue) == nil && stringValue != "" {
			return stringValue
		}
		var value struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(event.Error, &value) == nil && value.Message != "" {
			return value.Message
		}
		return string(event.Error)
	}
	return "Codex turn failed"
}

func classifyProcessExit(stderr string, resume bool) engine.ErrorClass {
	lower := strings.ToLower(stderr)
	switch {
	case strings.Contains(lower, "rate limit"), strings.Contains(lower, "429"):
		return engine.ErrorRateLimit
	case strings.Contains(lower, "not logged in"),
		strings.Contains(lower, "authentication"),
		strings.Contains(lower, "unauthorized"),
		strings.Contains(lower, "api key"):
		return engine.ErrorAuthentication
	case resume && (strings.Contains(lower, "session") || strings.Contains(lower, "thread")):
		return engine.ErrorSessionResumeFailed
	default:
		return engine.ErrorProcessExit
	}
}

func validateWorkDir(workDir string) error {
	if workDir == "" {
		return nil
	}
	info, err := os.Stat(workDir)
	if err != nil {
		return fmt.Errorf("validate work directory %q: %w", workDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("validate work directory %q: not a directory", workDir)
	}
	return nil
}

func mergeEnvironment(base []string, overrides map[string]string) []string {
	if len(overrides) == 0 {
		return base
	}
	result := append([]string(nil), base...)
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		result = append(result, key+"="+overrides[key])
	}
	return result
}

func contextExecutionError(operation string, cause error, stderr string) error {
	class := engine.ErrorCancelled
	message := "codex execution cancelled"
	if errors.Is(cause, context.DeadlineExceeded) {
		class = engine.ErrorTimeout
		message = "codex execution timed out"
	}
	return engine.NewExecutionError(class, "codex", operation, withStderr(message, cause, stderr))
}

type limitedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedBuffer) Write(data []byte) (int, error) {
	originalLen := len(data)
	remaining := b.limit - b.buf.Len()
	if remaining <= 0 {
		b.truncated = b.truncated || originalLen > 0
		return originalLen, nil
	}
	if len(data) > remaining {
		data = data[:remaining]
		b.truncated = true
	}
	_, _ = b.buf.Write(data)
	return originalLen, nil
}

func (b *limitedBuffer) String() string { return b.buf.String() }

func sanitizeStderr(stderr string, truncated bool) string {
	stderr = strings.ToValidUTF8(stderr, "\uFFFD")
	stderr = strings.Map(func(character rune) rune {
		if unicode.IsControl(character) {
			return ' '
		}
		return character
	}, stderr)
	stderr = strings.Join(strings.Fields(stderr), " ")
	if truncated {
		stderr = strings.TrimSpace(stderr + " [truncated]")
	}
	return stderr
}

func withStderr(message string, cause error, stderr string) error {
	if stderr == "" {
		return fmt.Errorf("%s: %w", message, cause)
	}
	return fmt.Errorf("%s: %w; stderr: %s", message, cause, stderr)
}
