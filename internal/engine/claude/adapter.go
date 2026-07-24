package claude

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
	defaultBinary  = "claude"
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

func (a *Adapter) Name() string {
	return "claude"
}

func (a *Adapter) Capabilities(context.Context) (engine.CapabilitySet, error) {
	return engine.CapabilitySet{
		Resume:           true,
		StructuredOutput: false,
		Streaming:        true,
		Usage:            true,
		Cancellation:     false,
		OutputSchema:     false,
	}, nil
}

func (a *Adapter) Run(
	ctx context.Context,
	request engine.RunRequest,
	emit func(engine.Event) error,
) (*engine.Result, error) {
	return a.run(ctx, request, false, emit)
}

func (a *Adapter) Resume(
	ctx context.Context,
	request engine.RunRequest,
	emit func(engine.Event) error,
) (*engine.Result, error) {
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
		a.Name(),
		"cancel",
		errors.New("out-of-band cancellation is not supported; cancel the run context"),
	)
}

func (a *Adapter) run(
	ctx context.Context,
	request engine.RunRequest,
	resume bool,
	emit func(engine.Event) error,
) (*engine.Result, error) {
	if len(request.OutputSchema) > 0 {
		return nil, engine.NewExecutionError(
			engine.ErrorPolicyDenied,
			a.Name(),
			"validate-request",
			errors.New("Claude adapter does not support output schemas"),
		)
	}
	if err := validateWorkDir(request.WorkDir); err != nil {
		return nil, engine.NewExecutionError(
			engine.ErrorWorkspaceInvalid,
			a.Name(),
			"validate-workspace",
			err,
		)
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

	args := buildArgs(request, resume)
	cmd := exec.CommandContext(cmdCtx, a.binary, args...)
	cmd.Dir = request.WorkDir
	cmd.Env = mergeEnvironment(os.Environ(), request.Environment)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, engine.NewExecutionError(
			engine.ErrorUnknown,
			a.Name(),
			"open-stdout",
			fmt.Errorf("stdout pipe: %w", err),
		)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, engine.NewExecutionError(
			engine.ErrorUnknown,
			a.Name(),
			"open-stderr",
			fmt.Errorf("stderr pipe: %w", err),
		)
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
			fmt.Errorf("start claude: %w", err),
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
			withStderr(
				fmt.Sprintf("claude timed out after %s", request.Timeout),
				context.DeadlineExceeded,
				stderrText,
			),
		)
	}
	if waitErr != nil && errors.Is(cmdCtx.Err(), context.Canceled) {
		return nil, contextExecutionError("run", context.Canceled, stderrText)
	}
	if waitErr != nil {
		return nil, engine.NewExecutionError(
			engine.ErrorProcessExit,
			a.Name(),
			"run",
			withStderr("claude process failed", waitErr, stderrText),
		)
	}
	if parseErr != nil {
		var emissionErr *eventEmissionError
		if errors.As(parseErr, &emissionErr) {
			return nil, engine.NewExecutionError(
				engine.ErrorUnknown,
				a.Name(),
				"emit-event",
				emissionErr,
			)
		}
		return nil, engine.NewExecutionError(
			engine.ErrorInvalidOutput,
			a.Name(),
			"parse-output",
			withStderr("parse claude stream", parseErr, stderrText),
		)
	}
	if stderrErr != nil {
		return nil, engine.NewExecutionError(
			engine.ErrorUnknown,
			a.Name(),
			"read-stderr",
			fmt.Errorf("read claude stderr: %w", stderrErr),
		)
	}

	result := parsed.result
	result.ExitCode = 0
	result.StartedAt = startedAt
	result.CompletedAt = completedAt
	if err := parsed.emitTerminal(emit); err != nil {
		return nil, engine.NewExecutionError(
			engine.ErrorUnknown,
			a.Name(),
			"emit-event",
			err,
		)
	}
	return result, nil
}

func buildArgs(request engine.RunRequest, resume bool) []string {
	args := []string{
		"-p", request.Prompt,
		"--output-format", "stream-json",
		"--verbose",
	}
	if resume {
		args = append(args, "--resume", request.ResumeSessionID)
	} else if request.SessionID != "" {
		args = append(args, "--session-id", request.SessionID)
	}
	if request.Model != "" {
		args = append(args, "--model", request.Model)
	}
	if request.MaxTurns > 0 {
		args = append(args, "--max-turns", fmt.Sprintf("%d", request.MaxTurns))
	}
	if request.Policy.SkipPermissions {
		args = append(args, "--dangerously-skip-permissions")
	}
	return args
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

type streamEvent struct {
	Type      string            `json:"type"`
	Subtype   string            `json:"subtype"`
	SessionID string            `json:"session_id"`
	Message   *assistantMessage `json:"message"`
	IsError   bool              `json:"is_error"`
	Result    string            `json:"result"`
	NumTurns  int               `json:"num_turns"`
	Usage     *usage            `json:"usage"`
}

type assistantMessage struct {
	Content []contentBlock `json:"content"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type usage struct {
	InputTokens       int64 `json:"input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
	CachedInputTokens int64 `json:"cache_read_input_tokens"`
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

func (e *eventEmissionError) Unwrap() error {
	return e.err
}

func (p *parsedStream) emit(
	emit func(engine.Event) error,
	eventType engine.EventType,
	sessionID string,
	message string,
	data json.RawMessage,
) error {
	if emit == nil {
		return nil
	}
	p.sequence++
	if err := emit(engine.Event{
		Sequence:  p.sequence,
		Type:      eventType,
		Timestamp: time.Now().UTC(),
		SessionID: sessionID,
		Message:   message,
		Data:      data,
	}); err != nil {
		return &eventEmissionError{eventType: eventType, err: err}
	}
	return nil
}

func (p *parsedStream) emitTerminal(emit func(engine.Event) error) error {
	if p.result.Usage != (engine.Usage{}) {
		if err := p.emit(emit, engine.EventUsage, p.result.SessionID, "", p.terminalRaw); err != nil {
			return err
		}
	}
	terminalType := engine.EventCompleted
	if p.result.Status == engine.ResultFailed {
		terminalType = engine.EventFailed
	}
	return p.emit(emit, terminalType, p.result.SessionID, p.result.OutputText, p.terminalRaw)
}

func parseStream(r io.Reader, emit func(engine.Event) error) (*parsedStream, error) {
	parsed := &parsedStream{result: &engine.Result{}}
	result := parsed.result
	var assistantTexts []string
	sawTerminalResult := false

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		raw := json.RawMessage(append([]byte(nil), line...))
		var event streamEvent
		if err := json.Unmarshal(raw, &event); err != nil {
			continue
		}

		switch event.Type {
		case "system":
			if event.Subtype == "init" && event.SessionID != "" {
				result.SessionID = event.SessionID
				if err := parsed.emit(emit, engine.EventSessionStarted, event.SessionID, "", raw); err != nil {
					return nil, err
				}
			}
		case "assistant":
			if event.Message != nil {
				for _, block := range event.Message.Content {
					if block.Type != "text" || block.Text == "" {
						continue
					}
					assistantTexts = append(assistantTexts, block.Text)
					if err := parsed.emit(emit, engine.EventProgress, result.SessionID, block.Text, raw); err != nil {
						return nil, err
					}
				}
			}
		case "result":
			sawTerminalResult = true
			parsed.terminalRaw = raw
			if event.SessionID != "" {
				result.SessionID = event.SessionID
			}
			result.OutputText = event.Result
			if event.IsError {
				result.Status = engine.ResultFailed
			} else {
				result.Status = engine.ResultCompleted
			}
			if event.Usage != nil {
				result.Usage = engine.Usage{
					InputTokens:       event.Usage.InputTokens,
					OutputTokens:      event.Usage.OutputTokens,
					CachedInputTokens: event.Usage.CachedInputTokens,
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		_, _ = io.Copy(io.Discard, r)
		return nil, err
	}
	if !sawTerminalResult {
		return nil, errors.New("claude stream ended without terminal result event")
	}
	if result.OutputText == "" && len(assistantTexts) > 0 {
		result.OutputText = strings.Join(assistantTexts, "\n")
	}
	if result.Status == engine.ResultCompleted && strings.TrimSpace(result.OutputText) == "" {
		return nil, errors.New("claude returned an empty successful result")
	}
	return parsed, nil
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

func contextExecutionError(operation string, cause error, stderr string) error {
	class := engine.ErrorCancelled
	message := "claude execution cancelled"
	if errors.Is(cause, context.DeadlineExceeded) {
		class = engine.ErrorTimeout
		message = "claude execution timed out"
	}
	return engine.NewExecutionError(
		class,
		"claude",
		operation,
		withStderr(message, cause, stderr),
	)
}

type limitedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	originalLen := len(p)
	remaining := b.limit - b.buf.Len()
	if remaining <= 0 {
		b.truncated = b.truncated || originalLen > 0
		return originalLen, nil
	}
	if len(p) > remaining {
		p = p[:remaining]
		b.truncated = true
	}
	_, _ = b.buf.Write(p)
	return originalLen, nil
}

func (b *limitedBuffer) String() string {
	return b.buf.String()
}

func sanitizeStderr(stderr string, truncated bool) string {
	stderr = strings.ToValidUTF8(stderr, "\uFFFD")
	stderr = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
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
