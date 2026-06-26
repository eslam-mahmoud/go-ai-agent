package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/eslam-mahmoud/go-ai-agent/internal/claude"
	"github.com/eslam-mahmoud/go-ai-agent/internal/config"
	githubclient "github.com/eslam-mahmoud/go-ai-agent/internal/github"
	"github.com/eslam-mahmoud/go-ai-agent/internal/orchestrator"
	"github.com/eslam-mahmoud/go-ai-agent/internal/store"
	"github.com/eslam-mahmoud/go-ai-agent/internal/telegram"
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
	flag.Parse()

	if *showVersion {
		fmt.Printf("madar %s (commit %s, built %s)\n", Version, Commit, BuildDate)
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

	s, err := store.Open(cfg.DBPath)
	if err != nil {
		log.Error("failed to open store", "err", err)
		os.Exit(1)
	}
	defer s.Close()

	ghClient := githubclient.New(cfg.GitHub.Token)
	runner := claude.New("")
	tg := telegram.New(cfg.Telegram.BotToken, cfg.Telegram.AllowedIDs)

	loop := orchestrator.New(cfg, ghClient, runner, tg, s, log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	log.Info("madar ready", "version", Version, "repos", cfg.Repos, "db", cfg.DBPath)
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
