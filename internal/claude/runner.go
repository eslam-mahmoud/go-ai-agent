package claude

import (
	"context"
	"encoding/json"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/engine"
	claudeengine "github.com/eslam-mahmoud/go-ai-agent/internal/engine/claude"
)

// Result is the legacy outcome consumed by the v1 orchestrator.
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
	WorkDir         string
	SessionID       string // for new session (--session-id)
	ResumeID        string // for resume (--resume)
	MaxTurns        int
	Timeout         time.Duration
	Prompt          string
	SkipPermissions bool // --dangerously-skip-permissions
}

type Runner interface {
	Run(ctx context.Context, opts RunOptions) (*Result, error)
}

type engineRunner struct {
	provider engine.Engine
}

func New(claudeBin string) Runner {
	return &engineRunner{provider: claudeengine.New(claudeBin)}
}

func (r *engineRunner) Run(ctx context.Context, opts RunOptions) (*Result, error) {
	request := engine.RunRequest{
		WorkDir:         opts.WorkDir,
		Prompt:          opts.Prompt,
		SessionID:       opts.SessionID,
		ResumeSessionID: opts.ResumeID,
		Timeout:         opts.Timeout,
		MaxTurns:        opts.MaxTurns,
		Policy: engine.Policy{
			SkipPermissions: opts.SkipPermissions,
		},
	}

	numTurns := 0
	captureTerminalMetadata := func(event engine.Event) error {
		if event.Type != engine.EventCompleted && event.Type != engine.EventFailed {
			return nil
		}
		var terminal struct {
			NumTurns int `json:"num_turns"`
		}
		if json.Unmarshal(event.Data, &terminal) == nil {
			numTurns = terminal.NumTurns
		}
		return nil
	}

	var (
		normalized *engine.Result
		err        error
	)
	if opts.ResumeID != "" {
		normalized, err = r.provider.Resume(ctx, request, captureTerminalMetadata)
	} else {
		normalized, err = r.provider.Run(ctx, request, captureTerminalMetadata)
	}
	if err != nil {
		return nil, err
	}

	needsInput, question := detectClarification(normalized.OutputText)
	return &Result{
		SessionID:  normalized.SessionID,
		Output:     normalized.OutputText,
		IsError:    normalized.Status == engine.ResultFailed,
		NumTurns:   numTurns,
		NeedsInput: needsInput,
		Question:   question,
	}, nil
}
