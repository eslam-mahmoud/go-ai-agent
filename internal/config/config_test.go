package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// write creates a config file and returns its path.
func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad_defaults(t *testing.T) {
	cfg, err := Load(write(t, "project:\n  repo: owner/repo\n"), "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Project.Repo != "owner/repo" {
		t.Errorf("Project.Repo = %q", cfg.Project.Repo)
	}
	if cfg.Project.Interval != 30*time.Second {
		t.Errorf("Project.Interval = %v, want 30s", cfg.Project.Interval)
	}
	if cfg.Claude.MaxTurns != 40 {
		t.Errorf("Claude.MaxTurns = %d, want 40", cfg.Claude.MaxTurns)
	}
	if cfg.Claude.Model != "" {
		t.Errorf("Claude.Model = %q, want provider default", cfg.Claude.Model)
	}
	if cfg.Claude.RunTimeout != 30*time.Minute {
		t.Errorf("Claude.RunTimeout = %v, want 30m", cfg.Claude.RunTimeout)
	}
	// Every budget defaults to unlimited, which is what upgrading without a
	// budgets block has to mean.
	if cfg.Project.Budgets != (BudgetConfig{}) {
		t.Errorf("Budgets = %#v, want all zero", cfg.Project.Budgets)
	}
}

func TestLoad_overrides(t *testing.T) {
	cfg, err := Load(write(t, `
project:
  repo: acme/alpha
  auto_initialize: true
  interval: 2m
  budgets:
    max_task_duration: 45m
    max_review_fix_cycles: 3
claude:
  model: sonnet
  max_turns: 20
  run_timeout: 10m
`), "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Project.Repo != "acme/alpha" || !cfg.Project.AutoInitialize {
		t.Errorf("Project = %#v", cfg.Project)
	}
	if cfg.Project.Interval != 2*time.Minute {
		t.Errorf("Project.Interval = %v, want 2m", cfg.Project.Interval)
	}
	if cfg.Project.Budgets.MaxTaskDuration != 45*time.Minute {
		t.Errorf("MaxTaskDuration = %v, want 45m", cfg.Project.Budgets.MaxTaskDuration)
	}
	if cfg.Project.Budgets.MaxReviewFixCycles != 3 {
		t.Errorf("MaxReviewFixCycles = %d, want 3", cfg.Project.Budgets.MaxReviewFixCycles)
	}
	if cfg.Claude.Model != "sonnet" || cfg.Claude.MaxTurns != 20 {
		t.Errorf("Claude = %#v", cfg.Claude)
	}
}

// A config still carrying v1 keys must be refused with the replacement named.
// Silently ignoring `repos:` would leave an operator believing work is queued
// against repositories the daemon no longer looks at.
func TestLoad_removedV1KeysAreRefusedWithTheirReplacement(t *testing.T) {
	tests := []struct {
		key  string
		body string
		want string
	}{
		{"repos", "repos: [owner/repo]\n", "project.repo"},
		{"labels", "labels:\n  ready: backlog\n", "task state lives in the database"},
		{"concurrency", "concurrency:\n  max_parallel: 3\n", "sequential by design"},
		{"poll_interval_seconds", "poll_interval_seconds: 60\n", "project.interval"},
		{"context_dir", "context_dir: .claude-context\n", "project's own .madar/"},
	}
	for _, test := range tests {
		t.Run(test.key, func(t *testing.T) {
			_, err := Load(write(t, test.body), "")
			if err == nil {
				t.Fatalf("%s must be refused, not ignored", test.key)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %q does not name the replacement %q", err, test.want)
			}
		})
	}
}

// Migrating should be one edit, not a sequence of failed starts.
func TestLoad_reportsEveryRemovedKeyAtOnce(t *testing.T) {
	_, err := Load(write(t, "repos: [a/b]\nconcurrency:\n  max_parallel: 2\n"), "")
	if err == nil {
		t.Fatal("expected the stale config to be refused")
	}
	for _, key := range []string{"repos", "concurrency"} {
		if !strings.Contains(err.Error(), key) {
			t.Fatalf("error %q does not mention %q", err, key)
		}
	}
}

func TestLoad_envVars(t *testing.T) {
	path := write(t, "project:\n  repo: owner/repo\n")
	envPath := filepath.Join(filepath.Dir(path), ".env")
	if err := os.WriteFile(envPath, []byte(
		"GITHUB_TOKEN=ghp_test123\nTELEGRAM_BOT_TOKEN=bot:abc\nTELEGRAM_ALLOWED_IDS=111,222\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path, envPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.GitHub.Token != "ghp_test123" {
		t.Errorf("GitHub.Token = %q", cfg.GitHub.Token)
	}
	if cfg.Telegram.BotToken != "bot:abc" {
		t.Errorf("Telegram.BotToken = %q", cfg.Telegram.BotToken)
	}
	if len(cfg.Telegram.AllowedIDs) != 2 {
		t.Errorf("AllowedIDs len = %d, want 2", len(cfg.Telegram.AllowedIDs))
	}
}

func TestLoad_ciDefaults(t *testing.T) {
	cfg, err := Load(write(t, "project:\n  repo: owner/repo\n"), "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CI.Enabled {
		t.Error("CI.Enabled should default to false")
	}
	if cfg.CI.PollInterval != 30*time.Second {
		t.Errorf("CI.PollInterval = %v, want 30s", cfg.CI.PollInterval)
	}
}

func TestLoad_ciOverrides(t *testing.T) {
	cfg, err := Load(write(t, `
project:
  repo: owner/repo
ci:
  enabled: true
  max_retries: 5
  poll_interval: 1m
  wait_timeout: 10m
`), "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.CI.Enabled || cfg.CI.MaxRetries != 5 {
		t.Errorf("CI = %#v", cfg.CI)
	}
	if cfg.CI.PollInterval != time.Minute || cfg.CI.WaitTimeout != 10*time.Minute {
		t.Errorf("CI = %#v", cfg.CI)
	}
}

func TestLoad_unknownKeyReturnsError(t *testing.T) {
	// Typo: "max_retires" instead of "max_retries".
	_, err := Load(write(t, "ci:\n  max_retires: 5\n"), "")
	if err == nil {
		t.Error("expected an error for the misspelled key")
	}
}

func TestLoad_missingFile(t *testing.T) {
	_, err := Load("/no/such/file.yaml", "")
	if err == nil {
		t.Error("expected error for missing config file")
	}
}

func TestSplitCSV(t *testing.T) {
	cases := []struct {
		input string
		want  []string
	}{
		{"", nil},
		{"a", []string{"a"}},
		{"a,b,c", []string{"a", "b", "c"}},
		{"a, b , c", []string{"a", "b", "c"}},
	}
	for _, tc := range cases {
		got := splitCSV(tc.input)
		if len(got) != len(tc.want) {
			t.Errorf("splitCSV(%q) = %v, want %v", tc.input, got, tc.want)
			continue
		}
		for i := range tc.want {
			if got[i] != tc.want[i] {
				t.Errorf("splitCSV(%q)[%d] = %q, want %q", tc.input, i, got[i], tc.want[i])
			}
		}
	}
}
