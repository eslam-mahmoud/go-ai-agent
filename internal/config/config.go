package config

import (
	"fmt"
	"os"
	"time"

	"github.com/joho/godotenv"
	"gopkg.in/yaml.v3"
)

type Config struct {
	PollInterval time.Duration
	Concurrency  ConcurrencyConfig
	Labels       LabelsConfig
	Repos        []string
	ContextDir   string
	Claude       ClaudeConfig
	CI           CIConfig
	GitHub       GitHubConfig
	Telegram     TelegramConfig
	DBPath       string
	WorkspaceDir string
}

type CIConfig struct {
	Enabled      bool
	MaxRetries   int
	PollInterval time.Duration
	WaitTimeout  time.Duration
}

type ConcurrencyConfig struct {
	Enabled    bool
	MaxParallel int
}

type LabelsConfig struct {
	Ready            string
	InProgress       string
	AwaitingFeedback string
	Done             string
}

type ClaudeConfig struct {
	OutputFormat          string
	MaxTurns              int
	RunTimeout            time.Duration
	AutoCompact           bool
	ContextResetThreshold float64
	SkipPermissions       bool
	MaxThreadChars        int // max chars of human thread passed to first-run prompt
}

type GitHubConfig struct {
	Token string
}

type TelegramConfig struct {
	BotToken   string
	AllowedIDs []string
}

type rawConfig struct {
	PollIntervalSeconds int `yaml:"poll_interval_seconds"`
	Concurrency         struct {
		Enabled    bool `yaml:"enabled"`
		MaxParallel int  `yaml:"max_parallel"`
	} `yaml:"concurrency"`
	Labels struct {
		Ready            string `yaml:"ready"`
		InProgress       string `yaml:"in_progress"`
		AwaitingFeedback string `yaml:"awaiting_feedback"`
		Done             string `yaml:"done"`
	} `yaml:"labels"`
	Repos      []string `yaml:"repos"`
	ContextDir string   `yaml:"context_dir"`
	Claude     struct {
		OutputFormat          string  `yaml:"output_format"`
		MaxTurns              int     `yaml:"max_turns"`
		RunTimeoutStr         string  `yaml:"run_timeout"`
		AutoCompact           bool    `yaml:"auto_compact"`
		ContextResetThreshold float64 `yaml:"context_reset_threshold"`
		SkipPermissions       bool    `yaml:"skip_permissions"`
		MaxThreadChars        int     `yaml:"max_thread_chars"`
	} `yaml:"claude"`
	CI struct {
		Enabled          bool   `yaml:"enabled"`
		MaxRetries       int    `yaml:"max_retries"`
		PollIntervalStr  string `yaml:"poll_interval"`
		WaitTimeoutStr   string `yaml:"wait_timeout"`
	} `yaml:"ci"`
	DBPath       string `yaml:"db_path"`
	WorkspaceDir string `yaml:"workspace_dir"`
}

func Load(configPath, envPath string) (*Config, error) {
	if envPath != "" {
		if err := godotenv.Load(envPath); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("load .env: %w", err)
		}
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var raw rawConfig
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	applyDefaults(&raw)

	runTimeout, err := time.ParseDuration(raw.Claude.RunTimeoutStr)
	if err != nil {
		return nil, fmt.Errorf("parse run_timeout %q: %w", raw.Claude.RunTimeoutStr, err)
	}
	ciPollInterval, err := time.ParseDuration(raw.CI.PollIntervalStr)
	if err != nil {
		return nil, fmt.Errorf("parse ci.poll_interval %q: %w", raw.CI.PollIntervalStr, err)
	}
	ciWaitTimeout, err := time.ParseDuration(raw.CI.WaitTimeoutStr)
	if err != nil {
		return nil, fmt.Errorf("parse ci.wait_timeout %q: %w", raw.CI.WaitTimeoutStr, err)
	}

	telegramIDs := splitCSV(os.Getenv("TELEGRAM_ALLOWED_IDS"))

	cfg := &Config{
		PollInterval: time.Duration(raw.PollIntervalSeconds) * time.Second,
		Concurrency: ConcurrencyConfig{
			Enabled:    raw.Concurrency.Enabled,
			MaxParallel: raw.Concurrency.MaxParallel,
		},
		Labels: LabelsConfig{
			Ready:            raw.Labels.Ready,
			InProgress:       raw.Labels.InProgress,
			AwaitingFeedback: raw.Labels.AwaitingFeedback,
			Done:             raw.Labels.Done,
		},
		Repos:      raw.Repos,
		ContextDir: raw.ContextDir,
		Claude: ClaudeConfig{
			OutputFormat:          raw.Claude.OutputFormat,
			MaxTurns:              raw.Claude.MaxTurns,
			RunTimeout:            runTimeout,
			AutoCompact:           raw.Claude.AutoCompact,
			ContextResetThreshold: raw.Claude.ContextResetThreshold,
			SkipPermissions:       raw.Claude.SkipPermissions,
			MaxThreadChars:        raw.Claude.MaxThreadChars,
		},
		GitHub: GitHubConfig{
			Token: os.Getenv("GITHUB_TOKEN"),
		},
		Telegram: TelegramConfig{
			BotToken:   os.Getenv("TELEGRAM_BOT_TOKEN"),
			AllowedIDs: telegramIDs,
		},
		CI: CIConfig{
			Enabled:      raw.CI.Enabled,
			MaxRetries:   raw.CI.MaxRetries,
			PollInterval: ciPollInterval,
			WaitTimeout:  ciWaitTimeout,
		},
		DBPath:       raw.DBPath,
		WorkspaceDir: raw.WorkspaceDir,
	}

	return cfg, nil
}

func applyDefaults(raw *rawConfig) {
	if raw.PollIntervalSeconds == 0 {
		raw.PollIntervalSeconds = 45
	}
	if raw.Concurrency.MaxParallel == 0 {
		raw.Concurrency.MaxParallel = 1
	}
	if raw.Labels.Ready == "" {
		raw.Labels.Ready = "ready"
	}
	if raw.Labels.InProgress == "" {
		raw.Labels.InProgress = "in-progress"
	}
	if raw.Labels.AwaitingFeedback == "" {
		raw.Labels.AwaitingFeedback = "awaiting-feedback"
	}
	if raw.Labels.Done == "" {
		raw.Labels.Done = "done"
	}
	if raw.ContextDir == "" {
		raw.ContextDir = ".claude-context"
	}
	if raw.Claude.OutputFormat == "" {
		raw.Claude.OutputFormat = "stream-json"
	}
	if raw.Claude.MaxTurns == 0 {
		raw.Claude.MaxTurns = 40
	}
	if raw.Claude.RunTimeoutStr == "" {
		raw.Claude.RunTimeoutStr = "30m"
	}
	if raw.Claude.ContextResetThreshold == 0 {
		raw.Claude.ContextResetThreshold = 0.6
	}
	if raw.Claude.MaxThreadChars == 0 {
		raw.Claude.MaxThreadChars = 8000
	}
	if raw.CI.MaxRetries == 0 {
		raw.CI.MaxRetries = 3
	}
	if raw.CI.PollIntervalStr == "" {
		raw.CI.PollIntervalStr = "30s"
	}
	if raw.CI.WaitTimeoutStr == "" {
		raw.CI.WaitTimeoutStr = "20m"
	}
	if raw.DBPath == "" {
		raw.DBPath = "/opt/madar/madar.db"
	}
	if raw.WorkspaceDir == "" {
		raw.WorkspaceDir = "/opt/madar/workspaces"
	}
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	var result []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			tok := trimSpace(s[start:i])
			if tok != "" {
				result = append(result, tok)
			}
			start = i + 1
		}
	}
	return result
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}
