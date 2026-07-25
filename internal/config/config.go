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
	Reconcile    ReconcileConfig
	Project      ProjectConfig
	Policy       PolicyConfig
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

// ProjectConfig selects and configures v2 project mode. It is opt-in: an
// existing installation keeps running v1 issue mode until it says otherwise.
type ProjectConfig struct {
	Enabled        bool
	Repo           string
	AutoInitialize bool
	Interval       time.Duration
	Budgets        BudgetConfig
}

// PolicyConfig is the deployment's safety policy. An absent block constrains
// nothing, so upgrading without one behaves exactly as before.
type PolicyConfig struct {
	CommandDefault  string
	CommandAllow    []string
	CommandDeny     []string
	WritablePaths   []string
	DeniedPaths     []string
	RequireApproval []string
}

// BudgetConfig bounds what one task may consume. Every zero means unlimited,
// which is the historical behaviour.
type BudgetConfig struct {
	MaxTaskDuration    time.Duration
	MaxReviewFixCycles int
	MaxCIFixCycles     int
	MaxModeRetries     int
}

// ReconcileConfig controls GitHub reconciliation. A zero interval disables
// periodic passes; startup reconciliation is controlled separately.
type ReconcileConfig struct {
	Interval  time.Duration // how often to reconcile (default 15m, 0 disables)
	OnStartup bool          // reconcile once before picking up work (default true)
}

type ConcurrencyConfig struct {
	Enabled     bool
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
	Model                 string // optional model pinned to each new legacy execution
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
	// CommandMaxAge refuses commands older than this, so a replayed update
	// backlog cannot take effect long after it was written. Zero disables it.
	CommandMaxAge time.Duration
	// RateWindow and the two limits bound how fast one sender may issue
	// commands. Control commands get the tighter limit.
	RateWindow          time.Duration
	MaxCommandsPerLimit int
	MaxControlPerLimit  int
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
		Model                 string  `yaml:"model"`
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
	Reconcile struct {
		IntervalStr string `yaml:"interval"`
		OnStartup   *bool  `yaml:"on_startup"`
	} `yaml:"reconcile"`
	Project struct {
		Enabled        bool   `yaml:"enabled"`
		Repo           string `yaml:"repo"`
		AutoInitialize bool   `yaml:"auto_initialize"`
		IntervalStr    string `yaml:"interval"`
		Budgets        struct {
			MaxTaskDurationStr string `yaml:"max_task_duration"`
			MaxReviewFixCycles int    `yaml:"max_review_fix_cycles"`
			MaxCIFixCycles     int    `yaml:"max_ci_fix_cycles"`
			MaxModeRetries     int    `yaml:"max_mode_retries"`
		} `yaml:"budgets"`
	} `yaml:"project"`
	Policy struct {
		Commands struct {
			Default string   `yaml:"default"`
			Allow   []string `yaml:"allow"`
			Deny    []string `yaml:"deny"`
		} `yaml:"commands"`
		Paths struct {
			Writable []string `yaml:"writable"`
			Deny     []string `yaml:"deny"`
		} `yaml:"paths"`
		RequireApproval []string `yaml:"require_approval"`
	} `yaml:"policy"`
	Telegram struct {
		CommandMaxAgeStr     string `yaml:"command_max_age"`
		RateWindowStr        string `yaml:"rate_window"`
		MaxCommandsPerWindow int    `yaml:"max_commands_per_window"`
		MaxControlPerWindow  int    `yaml:"max_control_per_window"`
	} `yaml:"telegram"`
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
	projectInterval, err := time.ParseDuration(raw.Project.IntervalStr)
	if err != nil {
		return nil, fmt.Errorf("parse project.interval %q: %w", raw.Project.IntervalStr, err)
	}
	maxTaskDuration, err := time.ParseDuration(raw.Project.Budgets.MaxTaskDurationStr)
	if err != nil {
		return nil, fmt.Errorf(
			"parse project.budgets.max_task_duration %q: %w",
			raw.Project.Budgets.MaxTaskDurationStr, err,
		)
	}
	for name, value := range map[string]int{
		"max_review_fix_cycles": raw.Project.Budgets.MaxReviewFixCycles,
		"max_ci_fix_cycles":     raw.Project.Budgets.MaxCIFixCycles,
		"max_mode_retries":      raw.Project.Budgets.MaxModeRetries,
	} {
		if value < 0 {
			return nil, fmt.Errorf("project.budgets.%s cannot be negative", name)
		}
	}
	if raw.Project.Enabled && strings.TrimSpace(raw.Project.Repo) == "" {
		return nil, fmt.Errorf("project.repo is required when project mode is enabled")
	}
	commandMaxAge, err := time.ParseDuration(raw.Telegram.CommandMaxAgeStr)
	if err != nil {
		return nil, fmt.Errorf(
			"parse telegram.command_max_age %q: %w",
			raw.Telegram.CommandMaxAgeStr, err,
		)
	}
	rateWindow, err := time.ParseDuration(raw.Telegram.RateWindowStr)
	if err != nil {
		return nil, fmt.Errorf(
			"parse telegram.rate_window %q: %w", raw.Telegram.RateWindowStr, err,
		)
	}
	reconcileInterval, err := time.ParseDuration(raw.Reconcile.IntervalStr)
	if err != nil {
		return nil, fmt.Errorf("parse reconcile.interval %q: %w", raw.Reconcile.IntervalStr, err)
	}
	if reconcileInterval < 0 {
		return nil, fmt.Errorf("reconcile.interval cannot be negative")
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
			Enabled:     raw.Concurrency.Enabled,
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
			Model:                 raw.Claude.Model,
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
			BotToken:            os.Getenv("TELEGRAM_BOT_TOKEN"),
			AllowedIDs:          telegramIDs,
			CommandMaxAge:       commandMaxAge,
			RateWindow:          rateWindow,
			MaxCommandsPerLimit: raw.Telegram.MaxCommandsPerWindow,
			MaxControlPerLimit:  raw.Telegram.MaxControlPerWindow,
		},
		CI: CIConfig{
			Enabled:      raw.CI.Enabled,
			MaxRetries:   raw.CI.MaxRetries,
			PollInterval: ciPollInterval,
			WaitTimeout:  ciWaitTimeout,
			AutoMerge:    raw.CI.AutoMerge,
			MergeMethod:  raw.CI.MergeMethod,
		},
		Project: ProjectConfig{
			Enabled:        raw.Project.Enabled,
			Repo:           strings.TrimSpace(raw.Project.Repo),
			AutoInitialize: raw.Project.AutoInitialize,
			Interval:       projectInterval,
			Budgets: BudgetConfig{
				MaxTaskDuration:    maxTaskDuration,
				MaxReviewFixCycles: raw.Project.Budgets.MaxReviewFixCycles,
				MaxCIFixCycles:     raw.Project.Budgets.MaxCIFixCycles,
				MaxModeRetries:     raw.Project.Budgets.MaxModeRetries,
			},
		},
		Policy: PolicyConfig{
			CommandDefault:  strings.TrimSpace(raw.Policy.Commands.Default),
			CommandAllow:    raw.Policy.Commands.Allow,
			CommandDeny:     raw.Policy.Commands.Deny,
			WritablePaths:   raw.Policy.Paths.Writable,
			DeniedPaths:     raw.Policy.Paths.Deny,
			RequireApproval: raw.Policy.RequireApproval,
		},
		Reconcile: ReconcileConfig{
			Interval:  reconcileInterval,
			OnStartup: *raw.Reconcile.OnStartup,
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
	if raw.Reconcile.IntervalStr == "" {
		raw.Reconcile.IntervalStr = "15m"
	}
	if raw.Project.IntervalStr == "" {
		raw.Project.IntervalStr = "30s"
	}
	if raw.Project.Budgets.MaxTaskDurationStr == "" {
		// Zero means unlimited, preserving behaviour for anyone who upgrades
		// without adding a budgets block.
		raw.Project.Budgets.MaxTaskDurationStr = "0s"
	}
	if raw.Telegram.CommandMaxAgeStr == "" {
		raw.Telegram.CommandMaxAgeStr = "10m"
	}
	if raw.Telegram.RateWindowStr == "" {
		raw.Telegram.RateWindowStr = "1m"
	}
	if raw.Telegram.MaxCommandsPerWindow == 0 {
		raw.Telegram.MaxCommandsPerWindow = 20
	}
	if raw.Telegram.MaxControlPerWindow == 0 {
		raw.Telegram.MaxControlPerWindow = 5
	}
	if raw.Reconcile.OnStartup == nil {
		// A restart should repair drift before building on it.
		enabled := true
		raw.Reconcile.OnStartup = &enabled
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
