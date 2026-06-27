package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"gopkg.in/yaml.v3"
)

// RepoConfig describes a watched repository. The YAML form accepts either a
// plain string ("owner/repo") or an object with optional per-repo overrides.
type RepoConfig struct {
	Name        string
	AutoMerge   *bool  // nil → inherit from CI.AutoMerge
	MergeMethod string // "" → inherit from CI.MergeMethod; valid: merge|squash|rebase
}

type Config struct {
	PollInterval time.Duration
	Concurrency  ConcurrencyConfig
	Labels       LabelsConfig
	Repos        []RepoConfig
	ContextDir   string
	Claude       ClaudeConfig
	CI           CIConfig
	Cleanup      CleanupConfig
	GitHub       GitHubConfig
	Telegram     TelegramConfig
	DBPath       string
	WorkspaceDir string
	ConfigPath   string // path of the loaded config file; set by Load
}

// RepoNames returns the name field of every configured repo.
func (cfg *Config) RepoNames() []string {
	names := make([]string, len(cfg.Repos))
	for i, r := range cfg.Repos {
		names[i] = r.Name
	}
	return names
}

// EffectiveAutoMerge returns the auto-merge setting for fullRepo,
// falling back to the global CI.AutoMerge when no per-repo override is set.
func (cfg *Config) EffectiveAutoMerge(fullRepo string) bool {
	for _, r := range cfg.Repos {
		if r.Name == fullRepo && r.AutoMerge != nil {
			return *r.AutoMerge
		}
	}
	return cfg.CI.AutoMerge
}

// EffectiveMergeMethod returns the merge method for fullRepo,
// falling back to CI.MergeMethod and then "merge".
func (cfg *Config) EffectiveMergeMethod(fullRepo string) string {
	for _, r := range cfg.Repos {
		if r.Name == fullRepo && r.MergeMethod != "" {
			return r.MergeMethod
		}
	}
	if cfg.CI.MergeMethod != "" {
		return cfg.CI.MergeMethod
	}
	return "merge"
}

type CIConfig struct {
	Enabled      bool
	MaxRetries   int
	PollInterval time.Duration
	WaitTimeout  time.Duration
	AutoMerge    bool   // merge PR automatically when CI passes (default false)
	MergeMethod  string // merge | squash | rebase (default "merge")
}

type CleanupConfig struct {
	Interval          time.Duration // how often to run pruning (default 24h)
	AuditLogRetention time.Duration // delete audit entries older than this (default 30d)
	TaskRetention     time.Duration // delete done tasks older than this (default 90d)
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
	Bin                   string // path to the claude CLI binary
	OutputFormat          string
	MaxTurns              int
	RunTimeout            time.Duration
	AutoCompact           bool
	ContextResetThreshold float64
	SkipPermissions       bool
	MaxThreadChars        int // max chars of human thread passed to first-run prompt
	MaxIssueBodyChars     int // max chars of issue body passed to first-run prompt
}

type GitHubConfig struct {
	Token string
}

type TelegramConfig struct {
	BotToken   string
	AllowedIDs []string
}

// rawRepoConfig supports both plain-string and object YAML forms:
//
//	repos:
//	  - owner/repo               # plain string
//	  - name: owner/repo2        # object with overrides
//	    auto_merge: true
//	    merge_method: squash
type rawRepoConfig struct {
	Name        string `yaml:"name"`
	AutoMerge   *bool  `yaml:"auto_merge"`
	MergeMethod string `yaml:"merge_method"`
}

func (r *rawRepoConfig) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		r.Name = value.Value
		return nil
	}
	type alias rawRepoConfig
	var a alias
	if err := value.Decode(&a); err != nil {
		return err
	}
	*r = rawRepoConfig(a)
	return nil
}

type rawConfig struct {
	PollIntervalSeconds int `yaml:"poll_interval_seconds"`
	Concurrency         struct {
		Enabled     bool `yaml:"enabled"`
		MaxParallel int  `yaml:"max_parallel"`
	} `yaml:"concurrency"`
	Labels struct {
		Ready            string `yaml:"ready"`
		InProgress       string `yaml:"in_progress"`
		AwaitingFeedback string `yaml:"awaiting_feedback"`
		Done             string `yaml:"done"`
	} `yaml:"labels"`
	Repos      []rawRepoConfig `yaml:"repos"`
	ContextDir string          `yaml:"context_dir"`
	Claude     struct {
		Bin                   string  `yaml:"bin"`
		OutputFormat          string  `yaml:"output_format"`
		MaxTurns              int     `yaml:"max_turns"`
		RunTimeoutStr         string  `yaml:"run_timeout"`
		AutoCompact           bool    `yaml:"auto_compact"`
		ContextResetThreshold float64 `yaml:"context_reset_threshold"`
		SkipPermissions       bool    `yaml:"skip_permissions"`
		MaxThreadChars        int     `yaml:"max_thread_chars"`
		MaxIssueBodyChars     int     `yaml:"max_issue_body_chars"`
	} `yaml:"claude"`
	CI struct {
		Enabled         bool   `yaml:"enabled"`
		MaxRetries      int    `yaml:"max_retries"`
		PollIntervalStr string `yaml:"poll_interval"`
		WaitTimeoutStr  string `yaml:"wait_timeout"`
		AutoMerge       bool   `yaml:"auto_merge"`
		MergeMethod     string `yaml:"merge_method"`
	} `yaml:"ci"`
	Cleanup struct {
		IntervalStr          string `yaml:"interval"`
		AuditLogRetentionStr string `yaml:"audit_log_retention"`
		TaskRetentionStr     string `yaml:"task_retention"`
	} `yaml:"cleanup"`
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
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("parse config (unknown or misspelled key?): %w", err)
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
	cleanupInterval, err := time.ParseDuration(raw.Cleanup.IntervalStr)
	if err != nil {
		return nil, fmt.Errorf("parse cleanup.interval %q: %w", raw.Cleanup.IntervalStr, err)
	}
	auditRetention, err := time.ParseDuration(raw.Cleanup.AuditLogRetentionStr)
	if err != nil {
		return nil, fmt.Errorf("parse cleanup.audit_log_retention %q: %w", raw.Cleanup.AuditLogRetentionStr, err)
	}
	taskRetention, err := time.ParseDuration(raw.Cleanup.TaskRetentionStr)
	if err != nil {
		return nil, fmt.Errorf("parse cleanup.task_retention %q: %w", raw.Cleanup.TaskRetentionStr, err)
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
		Repos:      rawToRepoConfigs(raw.Repos),
		ContextDir: raw.ContextDir,
		Claude: ClaudeConfig{
			Bin:                   raw.Claude.Bin,
			OutputFormat:          raw.Claude.OutputFormat,
			MaxTurns:              raw.Claude.MaxTurns,
			RunTimeout:            runTimeout,
			AutoCompact:           raw.Claude.AutoCompact,
			ContextResetThreshold: raw.Claude.ContextResetThreshold,
			SkipPermissions:       raw.Claude.SkipPermissions,
			MaxThreadChars:        raw.Claude.MaxThreadChars,
			MaxIssueBodyChars:     raw.Claude.MaxIssueBodyChars,
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
			AutoMerge:    raw.CI.AutoMerge,
			MergeMethod:  raw.CI.MergeMethod,
		},
		Cleanup: CleanupConfig{
			Interval:          cleanupInterval,
			AuditLogRetention: auditRetention,
			TaskRetention:     taskRetention,
		},
		DBPath:       raw.DBPath,
		WorkspaceDir: raw.WorkspaceDir,
		ConfigPath:   configPath,
	}

	return cfg, nil
}

func rawToRepoConfigs(raw []rawRepoConfig) []RepoConfig {
	out := make([]RepoConfig, len(raw))
	for i, r := range raw {
		out[i] = RepoConfig{Name: r.Name, AutoMerge: r.AutoMerge, MergeMethod: r.MergeMethod}
	}
	return out
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
	if raw.Claude.MaxIssueBodyChars == 0 {
		raw.Claude.MaxIssueBodyChars = 4000
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
	if raw.Cleanup.IntervalStr == "" {
		raw.Cleanup.IntervalStr = "24h"
	}
	if raw.Cleanup.AuditLogRetentionStr == "" {
		raw.Cleanup.AuditLogRetentionStr = "720h" // 30 days
	}
	if raw.Cleanup.TaskRetentionStr == "" {
		raw.Cleanup.TaskRetentionStr = "2160h" // 90 days
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
