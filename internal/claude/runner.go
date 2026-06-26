package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// Result is the outcome of a claude invocation.
type Result struct {
	SessionID string
	Output    string
	IsError   bool
	NumTurns  int
	// NeedsInput is true when Claude asked a clarifying question.
	NeedsInput bool
	// Question holds the clarifying question text if NeedsInput is true.
	Question string
}

// TokenUsage tracks cumulative token usage across a session for context management.
type TokenUsage struct {
	InputTokens  int
	OutputTokens int
}

type RunOptions struct {
	WorkDir              string
	SessionID            string // for new session (--session-id)
	ResumeID             string // for resume (--resume)
	MaxTurns             int
	Timeout              time.Duration
	Prompt               string
	SkipPermissions      bool   // --dangerously-skip-permissions
}

type Runner interface {
	Run(ctx context.Context, opts RunOptions) (*Result, error)
}

type cliRunner struct {
	claudeBin string
}

func New(claudeBin string) Runner {
	if claudeBin == "" {
		claudeBin = "claude"
	}
	return &cliRunner{claudeBin: claudeBin}
}

// streamEvent represents one line of stream-json output from claude.
type streamEvent struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`

	// system/init
	SessionID string `json:"session_id"`

	// assistant message
	Message *assistantMessage `json:"message"`

	// result
	IsError  bool   `json:"is_error"`
	Result   string `json:"result"`
	NumTurns int    `json:"num_turns"`
	Usage    *usage `json:"usage"`
}

type assistantMessage struct {
	Content []contentBlock `json:"content"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

func (r *cliRunner) Run(ctx context.Context, opts RunOptions) (*Result, error) {
	args := buildArgs(opts)

	cmdCtx := ctx
	var cancel context.CancelFunc
	if opts.Timeout > 0 {
		cmdCtx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(cmdCtx, r.claudeBin, args...)
	cmd.Dir = opts.WorkDir

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start claude: %w", err)
	}

	result, parseErr := parseStream(stdout)
	waitErr := cmd.Wait()

	if parseErr != nil {
		return nil, fmt.Errorf("parse stream: %w", parseErr)
	}
	if waitErr != nil && cmdCtx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("claude timed out after %s", opts.Timeout)
	}
	// Non-zero exit is expected for error cases; result.IsError will be set.

	return result, nil
}

func buildArgs(opts RunOptions) []string {
	args := []string{
		"-p", opts.Prompt,
		"--output-format", "stream-json",
		"--verbose",
	}
	if opts.ResumeID != "" {
		args = append(args, "--resume", opts.ResumeID)
	} else if opts.SessionID != "" {
		args = append(args, "--session-id", opts.SessionID)
	}
	if opts.MaxTurns > 0 {
		args = append(args, "--max-turns", fmt.Sprintf("%d", opts.MaxTurns))
	}
	if opts.SkipPermissions {
		args = append(args, "--dangerously-skip-permissions")
	}
	return args
}

func parseStream(r io.Reader) (*Result, error) {
	result := &Result{}
	var assistantTexts []string

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var event streamEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			continue // skip unparseable lines
		}

		switch event.Type {
		case "system":
			if event.Subtype == "init" && event.SessionID != "" {
				result.SessionID = event.SessionID
			}
		case "assistant":
			if event.Message != nil {
				for _, block := range event.Message.Content {
					if block.Type == "text" && block.Text != "" {
						assistantTexts = append(assistantTexts, block.Text)
					}
				}
			}
		case "result":
			result.IsError = event.IsError
			result.NumTurns = event.NumTurns
			if event.SessionID != "" {
				result.SessionID = event.SessionID
			}
			result.Output = event.Result
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	// If output is empty, build it from assistant texts
	if result.Output == "" && len(assistantTexts) > 0 {
		result.Output = strings.Join(assistantTexts, "\n")
	}

	// Detect clarification request
	result.NeedsInput, result.Question = detectClarification(result.Output)

	return result, nil
}

// detectClarification checks whether the output contains a clarification request.
// Claude is prompted to start with "NEEDS_CLARIFICATION:" when it requires human input.
const clarificationPrefix = "NEEDS_CLARIFICATION:"

func detectClarification(output string) (bool, string) {
	trimmed := strings.TrimSpace(output)
	// Check explicit prefix first
	if strings.HasPrefix(trimmed, clarificationPrefix) {
		question := strings.TrimSpace(trimmed[len(clarificationPrefix):])
		return true, question
	}
	return false, ""
}

// BuildFirstRunPrompt creates the prompt for the first invocation of a task.
// issueNumber derives the required branch name; maxBodyChars truncates the
// issue body if it exceeds the limit (0 = no limit).
func BuildFirstRunPrompt(issueTitle, issueBody, threadComments string, issueNumber, maxBodyChars int) string {
	branch := fmt.Sprintf("madar/issue-%d", issueNumber)
	var sb strings.Builder
	sb.WriteString("You are working on the following GitHub Issue task. Complete the task fully and autonomously.\n\n")
	sb.WriteString("IMPORTANT RULES:\n")
	sb.WriteString(fmt.Sprintf("1. Create a branch named exactly `%s` for all your changes.\n", branch))
	sb.WriteString("2. Commit your changes to that branch and push it.\n")
	sb.WriteString("3. Open a pull request from that branch and include 'PR: #<number>' (e.g. 'PR: #42') on its own line in your final response so the CI watcher can track it.\n")
	sb.WriteString("4. If you need clarification before proceeding, respond with exactly:\n")
	sb.WriteString("   NEEDS_CLARIFICATION: <your question here>\n\n")
	sb.WriteString("Otherwise, complete the task and summarize what you did.\n\n")
	sb.WriteString("---\n")
	sb.WriteString("Title: ")
	sb.WriteString(issueTitle)
	sb.WriteString("\n\n")
	if issueBody != "" {
		body := issueBody
		if maxBodyChars > 0 && len(body) > maxBodyChars {
			body = body[:maxBodyChars] + "\n[truncated — see issue for full description]"
		}
		sb.WriteString("Description:\n")
		sb.WriteString(body)
		sb.WriteString("\n\n")
	}
	if threadComments != "" {
		sb.WriteString("Issue thread:\n")
		sb.WriteString(threadComments)
		sb.WriteString("\n")
	}
	return sb.String()
}

// BuildResumePrompt creates the prompt for resuming after a human replied.
func BuildResumePrompt(humanReply string) string {
	return fmt.Sprintf("Maintainer answered: %s\n\nContinue with the task.", humanReply)
}

// BuildCIFixPrompt creates the prompt sent to Claude when CI fails.
// branch and prNumber tell Claude exactly where to push so it doesn't
// open a new branch or PR instead of amending the existing one.
func BuildCIFixPrompt(failureOutput, branch string, prNumber, retryN, maxRetries int) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(
		"CI failed on attempt %d of %d.\n\n", retryN, maxRetries,
	))
	sb.WriteString(fmt.Sprintf(
		"IMPORTANT: Push your fix to the existing branch `%s` (PR #%d is already open). "+
			"Do NOT create a new branch or open a new PR.\n\n",
		branch, prNumber,
	))
	sb.WriteString("If you cannot fix the issue, respond with:\n")
	sb.WriteString("NEEDS_CLARIFICATION: <description of the problem>\n\n")
	sb.WriteString("--- CI Failure Output ---\n")
	sb.WriteString(failureOutput)
	sb.WriteString("\n------------------------\n\n")
	sb.WriteString("Diagnose the root cause, fix it, and push to the branch above.")
	return sb.String()
}
