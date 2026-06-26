package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoad_defaults(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("repos: [owner/repo]\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.PollInterval != 45*time.Second {
		t.Errorf("PollInterval = %v, want 45s", cfg.PollInterval)
	}
	if cfg.Labels.Ready != "ready" {
		t.Errorf("Labels.Ready = %q, want ready", cfg.Labels.Ready)
	}
	if cfg.Labels.InProgress != "in-progress" {
		t.Errorf("Labels.InProgress = %q, want in-progress", cfg.Labels.InProgress)
	}
	if cfg.Labels.AwaitingFeedback != "awaiting-feedback" {
		t.Errorf("Labels.AwaitingFeedback = %q, want awaiting-feedback", cfg.Labels.AwaitingFeedback)
	}
	if cfg.Labels.Done != "done" {
		t.Errorf("Labels.Done = %q, want done", cfg.Labels.Done)
	}
	if cfg.Claude.MaxTurns != 40 {
		t.Errorf("Claude.MaxTurns = %d, want 40", cfg.Claude.MaxTurns)
	}
	if cfg.Claude.RunTimeout != 30*time.Minute {
		t.Errorf("Claude.RunTimeout = %v, want 30m", cfg.Claude.RunTimeout)
	}
	if cfg.Claude.ContextResetThreshold != 0.6 {
		t.Errorf("Claude.ContextResetThreshold = %v, want 0.6", cfg.Claude.ContextResetThreshold)
	}
	if len(cfg.Repos) != 1 || cfg.Repos[0] != "owner/repo" {
		t.Errorf("Repos = %v, want [owner/repo]", cfg.Repos)
	}
}

func TestLoad_overrides(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	yaml := `
poll_interval_seconds: 60
concurrency:
  enabled: true
  max_parallel: 3
labels:
  ready: backlog
  in_progress: wip
  awaiting_feedback: blocked
  done: closed
repos:
  - acme/alpha
  - acme/beta
claude:
  max_turns: 20
  run_timeout: 10m
  context_reset_threshold: 0.8
`
	if err := os.WriteFile(cfgPath, []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.PollInterval != 60*time.Second {
		t.Errorf("PollInterval = %v, want 60s", cfg.PollInterval)
	}
	if !cfg.Concurrency.Enabled {
		t.Error("Concurrency.Enabled should be true")
	}
	if cfg.Concurrency.MaxParallel != 3 {
		t.Errorf("MaxParallel = %d, want 3", cfg.Concurrency.MaxParallel)
	}
	if cfg.Labels.Ready != "backlog" {
		t.Errorf("Labels.Ready = %q, want backlog", cfg.Labels.Ready)
	}
	if cfg.Claude.MaxTurns != 20 {
		t.Errorf("Claude.MaxTurns = %d, want 20", cfg.Claude.MaxTurns)
	}
	if cfg.Claude.RunTimeout != 10*time.Minute {
		t.Errorf("RunTimeout = %v, want 10m", cfg.Claude.RunTimeout)
	}
	if len(cfg.Repos) != 2 {
		t.Errorf("Repos len = %d, want 2", len(cfg.Repos))
	}
}

func TestLoad_envVars(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	envPath := filepath.Join(dir, ".env")

	if err := os.WriteFile(cfgPath, []byte("repos: []\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envPath, []byte("GITHUB_TOKEN=ghp_test123\nTELEGRAM_BOT_TOKEN=bot:abc\nTELEGRAM_ALLOWED_IDS=111,222\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath, envPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.GitHub.Token != "ghp_test123" {
		t.Errorf("GitHub.Token = %q, want ghp_test123", cfg.GitHub.Token)
	}
	if cfg.Telegram.BotToken != "bot:abc" {
		t.Errorf("Telegram.BotToken = %q", cfg.Telegram.BotToken)
	}
	if len(cfg.Telegram.AllowedIDs) != 2 {
		t.Errorf("AllowedIDs len = %d, want 2", len(cfg.Telegram.AllowedIDs))
	}
}

func TestLoad_ciDefaults(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("repos: []\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.CI.Enabled {
		t.Error("CI.Enabled should default to false")
	}
	if cfg.CI.MaxRetries != 3 {
		t.Errorf("CI.MaxRetries = %d, want 3", cfg.CI.MaxRetries)
	}
	if cfg.CI.PollInterval != 30*time.Second {
		t.Errorf("CI.PollInterval = %v, want 30s", cfg.CI.PollInterval)
	}
	if cfg.CI.WaitTimeout != 20*time.Minute {
		t.Errorf("CI.WaitTimeout = %v, want 20m", cfg.CI.WaitTimeout)
	}
}

func TestLoad_ciOverrides(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	yaml := `
repos: []
ci:
  enabled: true
  max_retries: 5
  poll_interval: 1m
  wait_timeout: 10m
`
	if err := os.WriteFile(cfgPath, []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if !cfg.CI.Enabled {
		t.Error("CI.Enabled should be true")
	}
	if cfg.CI.MaxRetries != 5 {
		t.Errorf("CI.MaxRetries = %d, want 5", cfg.CI.MaxRetries)
	}
	if cfg.CI.PollInterval != time.Minute {
		t.Errorf("CI.PollInterval = %v, want 1m", cfg.CI.PollInterval)
	}
	if cfg.CI.WaitTimeout != 10*time.Minute {
		t.Errorf("CI.WaitTimeout = %v, want 10m", cfg.CI.WaitTimeout)
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
