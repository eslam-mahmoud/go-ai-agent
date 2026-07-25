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
	"sort"
	"syscall"
	"time"

	"github.com/eslam-mahmoud/go-ai-agent/internal/app"
	"github.com/eslam-mahmoud/go-ai-agent/internal/config"
	"github.com/eslam-mahmoud/go-ai-agent/internal/domain"
	"github.com/eslam-mahmoud/go-ai-agent/internal/engine"
	claudeengine "github.com/eslam-mahmoud/go-ai-agent/internal/engine/claude"
	githubclient "github.com/eslam-mahmoud/go-ai-agent/internal/github"
	"github.com/eslam-mahmoud/go-ai-agent/internal/project"
	"github.com/eslam-mahmoud/go-ai-agent/internal/projectcli"
	"github.com/eslam-mahmoud/go-ai-agent/internal/projectloop"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
	"github.com/eslam-mahmoud/go-ai-agent/internal/telegram"
	"github.com/eslam-mahmoud/go-ai-agent/internal/updater"
	"github.com/eslam-mahmoud/go-ai-agent/internal/workspace"
)

// commandPollInterval is how often Telegram is polled for owner commands.
// It is fixed rather than configurable: it governs chat responsiveness, not
// delivery, and the delivery cadence already has its own setting.
const commandPollInterval = 5 * time.Second

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

	configPath := flag.String("config", "",
		"path to config.yaml (default: discovered — see -help)")
	envPath := flag.String("env", "",
		"path to the .env file (default: beside the config that was found)")
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

	discovery := config.DiscoverConfig(*configPath)
	if discovery.ConfigPath == "" {
		log.Error("failed to load config", "err", discovery.NotFoundError())
		os.Exit(1)
	}
	// The .env sits beside the config that was actually found. Defaulting it
	// to the working directory is how a correct -config still ends up
	// reporting a missing token: the config loads, the credentials do not,
	// and the error names the wrong problem.
	cfg, err := config.Load(discovery.ConfigPath, config.ResolveEnv(*envPath, discovery.ConfigPath))
	if err != nil {
		log.Error("failed to load config", "err", err, "path", discovery.ConfigPath)
		os.Exit(1)
	}

	// Status reads the local database and nothing else, so it runs before the
	// checks for credentials and tools it will never use.
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

	if cfg.GitHub.Token == "" {
		log.Error("GITHUB_TOKEN is required",
			"env", config.ResolveEnv(*envPath, discovery.ConfigPath))
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

	// The daemon delivers exactly one project, so it cannot start without
	// knowing which. The CLI subcommands take --repo explicitly and do not
	// need this, which is why the check lives here rather than in Load.
	if cfg.Project.Repo == "" {
		log.Error("project.repo is required to run the daemon",
			"hint", "set project.repo in config.yaml, then run `madar project create --repo <owner/name>`")
		os.Exit(1)
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
	// Registering the provider validates it before anything tries to run a
	// mode through it.
	if _, err := engine.NewRegistry(provider); err != nil {
		log.Error("failed to initialize engine registry", "err", err)
		os.Exit(1)
	}
	tg := telegram.New(cfg.Telegram.BotToken, cfg.Telegram.AllowedIDs)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	log.Info("madar ready",
		"version", Version, "repo", cfg.Project.Repo, "db", cfg.DBPath)
	log.Debug("effective config",
		"interval", cfg.Project.Interval,
		"auto_initialize", cfg.Project.AutoInitialize,
		"ci_enabled", cfg.CI.Enabled,
		"claude_bin", claudeBin,
		"skip_permissions", cfg.Claude.SkipPermissions)

	// The checkout has to exist before any mode runs in it.
	checkout, err := workspace.New(
		cfg.WorkspaceDir, cfg.Project.Repo, cfg.GitHub.Token, log,
	)
	if err != nil {
		log.Error("invalid workspace configuration", "err", err)
		os.Exit(1)
	}
	if err := checkout.Ensure(ctx); err != nil {
		log.Error("workspace setup failed", "err", err)
		os.Exit(1)
	}
	checkout.Refresh(ctx)

	// Recovery comes first: reconciling against state a crash left
	// half-written would repair the wrong thing.
	if err := projectloop.Recover(cfg, s, log); err != nil {
		log.Error("startup recovery failed", "err", err)
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

	// One Telegram poller, owned here. Telegram hands each update to whoever
	// asks first, so a second one would steal messages from this one.
	if router, err := projectloop.BuildCommands(cfg, s); err != nil {
		log.Error("command surface failed to start", "err", err)
		os.Exit(1)
	} else if router == nil {
		log.Warn("owner commands disabled: telegram.allowed_ids is empty")
	} else if commands, err := projectloop.NewCommandLoop(tg, router, log); err != nil {
		log.Error("command loop failed to start", "err", err)
		os.Exit(1)
	} else {
		log.Info("owner commands enabled")
		go func() {
			if err := commands.Run(ctx, commandPollInterval); err != nil &&
				!errors.Is(err, context.Canceled) {
				log.Error("command loop exited", "err", err)
			}
		}()
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

	// Delivery starts only after recovery and reconciliation, so it never
	// builds on state a restart has not yet repaired.
	delivery, err := projectloop.Build(dependencies)
	if err != nil {
		log.Error("project mode failed to start", "err", err)
		os.Exit(1)
	}
	log.Info("delivery loop starting",
		"repo", cfg.Project.Repo, "interval", cfg.Project.Interval)
	if err := delivery.Run(ctx, cfg.Project.Interval); err != nil &&
		!errors.Is(err, context.Canceled) {
		log.Error("delivery loop exited", "err", err)
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

// printStatus reports the managed project rather than a task queue: with v1
// issue mode gone there is one project, one backlog, and one task in the lane.
func printStatus(s *store.Store, cfg *config.Config) {
	version, _ := s.SchemaVersion()
	fmt.Printf("madar status\n")
	fmt.Printf("  schema version : %d\n", version)
	fmt.Printf("  db             : %s\n", cfg.DBPath)
	fmt.Printf("  repo           : %s\n", cfg.Project.Repo)

	record, err := s.GetProjectByRepo(cfg.Project.Repo)
	if err != nil {
		fmt.Printf("  project        : unreadable (%v)\n", err)
		return
	}
	if record == nil {
		fmt.Printf("  project        : none — run `madar project create`\n")
		return
	}
	fmt.Printf("  project        : %s (%s, health %s)\n",
		record.Name, record.State, record.Health)

	tasks, err := s.ListProjectTasks(record.ID)
	if err != nil {
		fmt.Printf("  backlog        : unreadable (%v)\n", err)
		return
	}
	counts := map[domain.TaskStatus]int{}
	for _, task := range tasks {
		counts[task.Status]++
	}
	fmt.Printf("  backlog        : %d task(s)\n", len(tasks))
	// Sorted so two runs of -status can be diffed against each other.
	statuses := make([]string, 0, len(counts))
	for status := range counts {
		statuses = append(statuses, string(status))
	}
	sort.Strings(statuses)
	for _, status := range statuses {
		fmt.Printf("    %-16s %d\n", status, counts[domain.TaskStatus(status)])
	}

	if record.CurrentTaskID == nil {
		fmt.Printf("  current task   : none\n")
		return
	}
	current, err := s.GetProjectTaskByID(*record.CurrentTaskID)
	if err != nil || current == nil {
		fmt.Printf("  current task   : #%d (unreadable)\n", *record.CurrentTaskID)
		return
	}
	fmt.Printf("  current task   : #%d %s (%s)\n",
		current.ID, current.Title, current.Status)
	if current.IssueNumber > 0 {
		fmt.Printf("    issue        : #%d\n", current.IssueNumber)
	}
	if current.BranchName != "" {
		fmt.Printf("    branch       : %s\n", current.BranchName)
	}
	if current.PRNumber > 0 {
		fmt.Printf("    pull request : #%d\n", current.PRNumber)
	}
	execution, err := s.GetLatestTaskExecution(current.ID)
	if err == nil && execution != nil {
		fmt.Printf("    last run     : %s %s (engine %s, model %s)\n",
			execution.Mode, execution.Status,
			displayEngine(execution.Engine), displayModel(execution.Model))
	}
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
