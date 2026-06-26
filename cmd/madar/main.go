package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/eslam-mahmoud/go-ai-agent/internal/claude"
	"github.com/eslam-mahmoud/go-ai-agent/internal/config"
	githubclient "github.com/eslam-mahmoud/go-ai-agent/internal/github"
	"github.com/eslam-mahmoud/go-ai-agent/internal/orchestrator"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
	"github.com/eslam-mahmoud/go-ai-agent/internal/telegram"
	"github.com/eslam-mahmoud/go-ai-agent/internal/updater"
)

// Build-time variables injected via -ldflags.
var (
	Version   = "dev"
	BuildDate = "unknown"
	Commit    = "unknown"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config.yaml")
	envPath := flag.String("env", ".env", "path to .env file")
	logLevel := flag.String("log-level", "info", "log level: debug|info|warn|error")
	showVersion := flag.Bool("version", false, "print version and exit")
	showStatus := flag.Bool("status", false, "print agent status from the database and exit")
	doUpdate := flag.Bool("update", false, "check for and apply the latest Madar release, then exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("madar %s (commit %s, built %s)\n", Version, Commit, BuildDate)
		os.Exit(0)
	}

	if *doUpdate {
		runUpdate(Version)
		os.Exit(0)
	}

	log := newLogger(*logLevel)

	cfg, err := config.Load(*configPath, *envPath)
	if err != nil {
		log.Error("failed to load config", "err", err)
		os.Exit(1)
	}

	if cfg.GitHub.Token == "" {
		log.Error("GITHUB_TOKEN is required")
		os.Exit(1)
	}

	// Validate that the claude binary is on PATH (or at the configured path).
	claudeBin := cfg.Claude.Bin
	if claudeBin == "" {
		claudeBin = "claude"
	}
	if _, err := exec.LookPath(claudeBin); err != nil {
		log.Error("claude binary not found — install Claude Code or set claude.bin in config",
			"bin", claudeBin, "err", err)
		os.Exit(1)
	}

	s, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Error("failed to open store", "err", err)
		os.Exit(1)
	}
	defer s.Close()

	if *showStatus {
		printStatus(s, cfg)
		os.Exit(0)
	}

	ghClient := githubclient.New(cfg.GitHub.Token)
	runner := claude.New(cfg.Claude.Bin)
	tg := telegram.New(cfg.Telegram.BotToken, cfg.Telegram.AllowedIDs)

	loop := orchestrator.New(cfg, ghClient, runner, tg, s, log)
	loop.SetCurrentVersion(Version)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	log.Info("madar ready", "version", Version, "repos", cfg.Repos, "db", cfg.DBPath)
	log.Debug("effective config",
		"poll_interval", cfg.PollInterval,
		"max_parallel", cfg.Concurrency.MaxParallel,
		"ci_enabled", cfg.CI.Enabled,
		"claude_bin", claudeBin,
		"skip_permissions", cfg.Claude.SkipPermissions)

	// Ensure required labels exist on every configured repo. This catches
	// misconfigured label names early and bootstraps fresh repos.
	requiredLabels := map[string]string{
		cfg.Labels.Ready:            "0075ca",
		cfg.Labels.InProgress:       "e4e669",
		cfg.Labels.AwaitingFeedback: "d93f0b",
		cfg.Labels.Done:             "0e8a16",
	}
	for _, fullRepo := range cfg.Repos {
		owner, repo, err := githubclient.SplitRepo(fullRepo)
		if err != nil {
			log.Warn("invalid repo, skipping label check", "repo", fullRepo)
			continue
		}
		if err := ghClient.EnsureLabels(ctx, owner, repo, requiredLabels); err != nil {
			log.Warn("label setup failed", "repo", fullRepo, "err", err)
		} else {
			log.Debug("labels verified", "repo", fullRepo)
		}
	}

	if err := orchestrator.EnsureWorkspaces(ctx, cfg, log); err != nil {
		log.Error("workspace setup failed", "err", err)
		os.Exit(1)
	}
	if err := loop.Run(ctx); err != nil && err != context.Canceled {
		log.Error("loop exited", "err", err)
		os.Exit(1)
	}
	log.Info("madar stopped")
}

func runUpdate(currentVersion string) {
	ctx := context.Background()
	fmt.Printf("Current version: %s\n", currentVersion)
	fmt.Print("Checking for updates... ")

	rel, err := updater.Check(ctx, currentVersion)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nUpdate check failed: %v\n", err)
		os.Exit(1)
	}
	if rel == nil {
		fmt.Printf("already up to date.\n")
		return
	}

	fmt.Printf("found %s\n", rel.Version)
	fmt.Printf("Downloading %s... ", rel.AssetURL)
	if err := updater.Apply(ctx, rel); err != nil {
		fmt.Fprintf(os.Stderr, "\nUpdate failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("done.\nUpdated to %s. Restart madar to use the new version.\n", rel.Version)
}

func printStatus(s *store.Store, cfg *config.Config) {
	v, _ := s.SchemaVersion()
	active, _ := s.CountActive()
	inProgress, _ := s.ListByState(store.StateInProgress)
	waiting, _ := s.ListByState(store.StateAwaitingFeedback)
	ciWaiting, _ := s.ListByCIState(store.CIStateWaiting)

	fmt.Printf("madar status\n")
	fmt.Printf("  schema version : %d\n", v)
	fmt.Printf("  db             : %s\n", cfg.DBPath)
	fmt.Printf("  repos          : %v\n", cfg.Repos)
	fmt.Printf("  active (claude): %d\n", active)
	fmt.Printf("  in-progress    : %d\n", len(inProgress))
	for _, t := range inProgress {
		fmt.Printf("    #%d %s (session %s)\n", t.IssueNumber, t.Repo, t.SessionID)
	}
	fmt.Printf("  awaiting-feedback: %d\n", len(waiting))
	for _, t := range waiting {
		fmt.Printf("    #%d %s\n", t.IssueNumber, t.Repo)
	}
	fmt.Printf("  ci-watching    : %d\n", len(ciWaiting))
	for _, t := range ciWaiting {
		fmt.Printf("    #%d %s (pr=%d)\n", t.IssueNumber, t.Repo, t.PRNumber)
	}
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl}))
}
