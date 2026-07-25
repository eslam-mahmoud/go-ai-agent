package config

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"gopkg.in/yaml.v3"
)

type Config struct {
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

type rawConfig struct {
	Claude struct {
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

	if err := rejectRemovedKeys(data); err != nil {
		return nil, err
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

// removedKeys maps each key that v1 issue mode owned to what replaces it.
// A stale key is refused rather than ignored: an operator who still has
// `repos:` in their config believes work is queued against those repositories,
// and silence would let them go on believing it.
var removedKeys = map[string]string{
	"repos":                 "project.repo — one managed project per daemon",
	"labels":                "removed; task state lives in the database, not in issue labels",
	"concurrency":           "removed; delivery is sequential by design, one task at a time",
	"poll_interval_seconds": "project.interval",
	"context_dir":           "removed; modes read the project's own .madar/ documents",
}

// rejectRemovedKeys reports every stale v1 key at once, so migrating is one
// edit rather than a sequence of failed starts.
func rejectRemovedKeys(data []byte) error {
	var top map[string]yaml.Node
	if err := yaml.Unmarshal(data, &top); err != nil {
		// Malformed YAML is the decoder's error to report, with its line
		// numbers, rather than something to guess at here.
		return nil
	}
	found := make([]string, 0, len(removedKeys))
	for key := range top {
		if _, removed := removedKeys[key]; removed {
			found = append(found, key)
		}
	}
	if len(found) == 0 {
		return nil
	}
	sort.Strings(found)
	var message strings.Builder
	message.WriteString("config uses keys removed with v1 issue mode:")
	for _, key := range found {
		fmt.Fprintf(&message, "\n  %s → %s", key, removedKeys[key])
	}
	message.WriteString("\n\nMadar now runs one managed project. See the README section \"Configuration Reference\".")
	return errors.New(message.String())
}

func applyDefaults(raw *rawConfig) {
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
