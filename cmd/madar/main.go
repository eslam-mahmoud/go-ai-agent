package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/app"
	"github.com/eslam-mahmoud/go-ai-agent/internal/config"
	"github.com/eslam-mahmoud/go-ai-agent/internal/engine"
	claudeengine "github.com/eslam-mahmoud/go-ai-agent/internal/engine/claude"
	githubclient "github.com/eslam-mahmoud/go-ai-agent/internal/github"
	"github.com/eslam-mahmoud/go-ai-agent/internal/orchestrator"
	"github.com/eslam-mahmoud/go-ai-agent/internal/project"
	"github.com/eslam-mahmoud/go-ai-agent/internal/projectcli"
	"github.com/eslam-mahmoud/go-ai-agent/internal/projectloop"
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
	if len(os.Args) > 1 && os.Args[1] == "migrate-project" {
		if err := projectcli.RunMigration(os.Args[2:], os.Stdout, os.Stderr); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			fmt.Fprintf(os.Stderr, "madar migrate-project: %v\n", err)
			os.Exit(2)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "project" {
		if err := projectcli.Run(os.Args[2:], os.Stdout, os.Stderr); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return
			}
			fmt.Fprintf(os.Stderr, "madar project: %v\n", err)
			os.Exit(2)
		}
		return
	}

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

	if *showStatus {
		s, err := store.OpenReadOnly(cfg.DBPath)
		if err != nil {
			log.Error("failed to open store for status", "err", err)
			os.Exit(1)
		}
		printStatus(s, cfg)
		_ = s.Close()
		return
	}

	instanceLock, err := app.AcquireInstanceLock(cfg.DBPath)
	if err != nil {
		if errors.Is(err, app.ErrAlreadyLocked) {
			log.Error("another Madar daemon is already using this database",
				"db", cfg.DBPath, "err", err)
		} else {
			log.Error("failed to acquire single-instance lock",
				"db", cfg.DBPath, "err", err)
		}
		os.Exit(1)
	}
	defer func() {
		if err := instanceLock.Release(); err != nil {
			log.Error("failed to release instance lock", "path", instanceLock.Path(), "err", err)
		}
	}()
	log.Info("single-instance lock acquired",
		"path", instanceLock.Path(), "pid", os.Getpid())

	s, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Error("failed to open store", "err", err)
		os.Exit(1)
	}
	defer s.Close()

	ghClient := githubclient.New(cfg.GitHub.Token)
	// The safety policy is deployment-wide, so it is applied at the engine
	// rather than by each mode: one place to get right instead of seven.
	toolRules, err := projectloop.BuildToolRules(cfg)
	if err != nil {
		log.Error("invalid safety policy", "err", err)
		os.Exit(1)
	}
	if !toolRules.Empty() {
		log.Info("safety policy loaded",
			"allow", len(toolRules.Allow),
			"ask", len(toolRules.Ask),
			"deny", len(toolRules.Deny))
	}
	provider := claudeengine.NewWithToolRules(cfg.Claude.Bin, toolRules)
	engineRegistry, err := engine.NewRegistry(provider)
	if err != nil {
		log.Error("failed to initialize engine registry", "err", err)
		os.Exit(1)
	}
	tg := telegram.New(cfg.Telegram.BotToken, cfg.Telegram.AllowedIDs)

	loop, err := orchestrator.New(
		cfg,
		ghClient,
		engineRegistry,
		"claude",
		cfg.Claude.Model,
		tg,
		s,
		log,
	)
	if err != nil {
		log.Error("failed to initialize orchestrator", "err", err)
		os.Exit(1)
	}
	loop.SetCurrentVersion(Version)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	log.Info("madar ready", "version", Version, "repos", cfg.RepoNames(), "db", cfg.DBPath)
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
	for _, repoCfg := range cfg.Repos {
		fullRepo := repoCfg.Name
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

	// Recovery comes first: reconciling against state a crash left
	// half-written would repair the wrong thing.
	if err := projectloop.Recover(cfg, s, log); err != nil {
		log.Error("v2 startup recovery failed", "err", err)
		os.Exit(1)
	}

	// Reconcile before picking up work, so a restart repairs drift rather
	// than building on it, then keep reconciling in the background.
	if scheduler, err := project.NewReconcileScheduler(
		mustReconciler(s, ghClient, log),
		s,
		cfg.Reconcile.Interval,
		log,
	); err != nil {
		log.Warn("reconciliation disabled", "err", err)
	} else {
		if cfg.Reconcile.OnStartup {
			if _, err := scheduler.ReconcileOnce(ctx); err != nil && ctx.Err() == nil {
				// GitHub being unavailable must not stop delivery.
				log.Warn("startup reconciliation failed", "err", err)
			}
		}
		if cfg.Reconcile.Interval > 0 {
			go func() {
				ticker := time.NewTicker(cfg.Reconcile.Interval)
				defer ticker.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-ticker.C:
						if _, err := scheduler.ReconcileOnce(ctx); err != nil &&
							ctx.Err() == nil {
							log.Warn("periodic reconciliation failed", "err", err)
						}
					}
				}
			}()
		}
	}

	// The owner command surface shares the v1 Telegram poller, so project
	// commands work the moment project mode is on.
	if router, err := projectloop.BuildCommands(cfg, s); err != nil {
		log.Error("v2 command surface failed to start", "err", err)
		os.Exit(1)
	} else if router != nil {
		loop.SetProjectCommands(router)
		log.Info("v2 command surface enabled")
	} else if cfg.Project.Enabled {
		log.Warn("v2 commands disabled: telegram.allowed_ids is empty")
	}

	// Optional stages are passed only when their credentials exist, so the
	// loop degrades to what it can actually do instead of failing on every
	// call to a service that was never configured.
	dependencies := projectloop.Dependencies{
		Config: cfg,
		Store:  s,
		Engine: provider,
		Log:    log,
	}
	if cfg.GitHub.Token != "" {
		dependencies.GitHub = ghClient
	}
	if cfg.Telegram.BotToken != "" {
		dependencies.Status = tg
	}

	// Delivery starts only after recovery and reconciliation, so v2 never
	// builds on state a restart has not yet repaired.
	if delivery, err := projectloop.Build(dependencies); err != nil {
		log.Error("v2 project mode failed to start", "err", err)
		os.Exit(1)
	} else if delivery != nil {
		log.Info("v2 project mode enabled",
			"repo", cfg.Project.Repo, "interval", cfg.Project.Interval)
		go func() {
			if err := delivery.Run(ctx, cfg.Project.Interval); err != nil &&
				!errors.Is(err, context.Canceled) {
				log.Error("v2 delivery loop exited", "err", err)
			}
		}()
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
	interrupted, _ := s.ListByState(store.StateInterrupted)
	recovering, _ := s.ListByState(store.StateRecovering)
	waiting, _ := s.ListByState(store.StateAwaitingFeedback)
	ciWaiting, _ := s.ListByCIState(store.CIStateWaiting)

	fmt.Printf("madar status\n")
	fmt.Printf("  schema version : %d\n", v)
	fmt.Printf("  db             : %s\n", cfg.DBPath)
	fmt.Printf("  repos          : %v\n", cfg.RepoNames())
	fmt.Printf("  active runs    : %d\n", active)
	fmt.Printf("  in-progress    : %d\n", len(inProgress))
	for _, t := range inProgress {
		printTaskExecution(t)
	}
	fmt.Printf("  interrupted   : %d\n", len(interrupted))
	for _, t := range interrupted {
		printTaskExecution(t)
	}
	fmt.Printf("  recovering    : %d\n", len(recovering))
	for _, t := range recovering {
		printTaskExecution(t)
	}
	fmt.Printf("  awaiting-feedback: %d\n", len(waiting))
	for _, t := range waiting {
		printTaskExecution(t)
	}
	fmt.Printf("  ci-watching    : %d\n", len(ciWaiting))
	for _, t := range ciWaiting {
		fmt.Printf(
			"    #%d %s (engine %s, model %s, session %s, pr %d)\n",
			t.IssueNumber,
			t.Repo,
			displayEngine(t.Engine),
			displayModel(t.Model),
			t.SessionID,
			t.PRNumber,
		)
	}
}

func printTaskExecution(t *store.Task) {
	fmt.Printf(
		"    #%d %s (engine %s, model %s, session %s)\n",
		t.IssueNumber,
		t.Repo,
		displayEngine(t.Engine),
		displayModel(t.Model),
		t.SessionID,
	)
}

func displayEngine(name string) string {
	if name == "" {
		return "legacy-unbound"
	}
	return name
}

func displayModel(name string) string {
	if name == "" {
		return "provider-default"
	}
	return name
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

// mustReconciler builds the reconciler used by the daemon. A construction
// failure is reported by the caller rather than crashing startup.
func mustReconciler(
	st *store.Store,
	client githubclient.Client,
	log *slog.Logger,
) *project.Reconciler {
	reconciler, err := project.NewReconciler(st, client)
	if err != nil {
		log.Warn("reconciler unavailable", "err", err)
		return nil
	}
	return reconciler
}
