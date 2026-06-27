package orchestrator

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/eslam-mahmoud/go-ai-agent/internal/config"
	githubclient "github.com/eslam-mahmoud/go-ai-agent/internal/github"
)

// appendRepoToConfig inserts "  - repoName" into the repos: block of configPath.
func appendRepoToConfig(configPath, repoName string) error {
	if configPath == "" {
		return fmt.Errorf("config path not set")
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")

	// Find the last "  - " entry inside the repos: block.
	inRepos := false
	lastRepoLine := -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "repos:" {
			inRepos = true
			continue
		}
		if inRepos {
			if strings.HasPrefix(trimmed, "- ") {
				lastRepoLine = i
			} else if trimmed != "" && !strings.HasPrefix(trimmed, "#") &&
				!strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
				break // left the repos block
			}
		}
	}
	if lastRepoLine == -1 {
		return fmt.Errorf("could not locate repos: block in %s", configPath)
	}

	newLine := "  - " + repoName
	updated := make([]string, 0, len(lines)+1)
	updated = append(updated, lines[:lastRepoLine+1]...)
	updated = append(updated, newLine)
	updated = append(updated, lines[lastRepoLine+1:]...)

	return os.WriteFile(configPath, []byte(strings.Join(updated, "\n")), 0644)
}

// checkTelegramCommands polls for inbound Telegram messages and handles /issue commands.
// Called on every tick so new messages are picked up within one poll interval.
func (l *Loop) checkTelegramCommands(ctx context.Context) {
	updates, err := l.telegram.GetUpdates(ctx, l.telegramOffset)
	if err != nil {
		l.log.Debug("telegram getUpdates failed", "err", err)
		return
	}
	for _, u := range updates {
		l.telegramOffset = u.UpdateID + 1
		if u.Message == nil || u.Message.Text == "" {
			continue
		}
		if !l.isAllowedChatID(u.Message.Chat.ID) {
			l.log.Debug("telegram message from unknown chat, ignoring", "chat_id", u.Message.Chat.ID)
			continue
		}
		l.handleTelegramMessage(ctx, u.Message.Chat.ID, u.Message.Text)
	}
}

// isAllowedChatID returns true if chatID is in the configured allowedIDs list.
func (l *Loop) isAllowedChatID(chatID int64) bool {
	needle := fmt.Sprintf("%d", chatID)
	for _, id := range l.cfg.Telegram.AllowedIDs {
		if strings.TrimSpace(id) == needle {
			return true
		}
	}
	return false
}

// handleTelegramMessage routes an inbound message. Currently supports /issue.
func (l *Loop) handleTelegramMessage(ctx context.Context, chatID int64, text string) {
	text = strings.TrimSpace(text)
	switch {
	case strings.HasPrefix(text, "/issue"):
		l.handleIssueCommand(ctx, chatID, text)
	case strings.HasPrefix(text, "/addrepo"):
		l.handleAddRepoCommand(ctx, chatID, text)
	case strings.HasPrefix(text, "/repos"):
		l.handleReposCommand(ctx, chatID)
	case strings.HasPrefix(text, "/status"):
		l.handleStatusCommand(ctx, chatID)
	case strings.HasPrefix(text, "/help") || text == "/start":
		l.replyHelp(ctx, chatID)
	default:
		_ = l.telegram.Reply(ctx, chatID,
			"Unknown command. Send /help to see available commands.")
	}
}

// handleIssueCommand parses and executes:
//
//	/issue [owner/repo] Title
//	Optional body (all lines after the first)
//
// If owner/repo is omitted, the first configured repo is used.
func (l *Loop) handleIssueCommand(ctx context.Context, chatID int64, text string) {
	// Strip the /issue prefix and any @bot_name suffix that Telegram adds in groups.
	rest := strings.TrimSpace(strings.SplitN(text, "\n", 2)[0])
	rest = strings.TrimPrefix(rest, "/issue")
	if idx := strings.Index(rest, " "); idx == -1 {
		// /issue with no arguments
		_ = l.telegram.Reply(ctx, chatID,
			"Usage: /issue [owner/repo] Title\nOptional body on the next lines.\n\n"+
				"Example:\n/issue eslam-mahmoud/eslam.me Fix login bug\nDetails here…")
		return
	}
	rest = strings.TrimSpace(rest[strings.Index(rest, " "):])

	// Separate body (lines after first) from the header line.
	var body string
	if nl := strings.Index(text, "\n"); nl != -1 {
		body = strings.TrimSpace(text[nl+1:])
	}

	// Detect optional owner/repo token.
	fullRepo := ""
	titlePart := rest
	if first := strings.Fields(rest)[0]; strings.Contains(first, "/") {
		fullRepo = first
		titlePart = strings.TrimSpace(rest[len(first):])
	}

	// Fall back to first configured repo when none specified.
	if fullRepo == "" {
		if len(l.cfg.Repos) == 0 {
			_ = l.telegram.Reply(ctx, chatID, "❌ No repos configured in config.yaml.")
			return
		}
		if len(l.cfg.Repos) > 1 {
			var names []string
			for _, r := range l.cfg.Repos {
				names = append(names, "• "+r.Name)
			}
			_ = l.telegram.Reply(ctx, chatID,
				"Multiple repos configured — specify one:\n/issue owner/repo Title\n\n"+
					strings.Join(names, "\n"))
			return
		}
		fullRepo = l.cfg.Repos[0].Name
	}

	title := strings.TrimSpace(titlePart)
	if title == "" {
		_ = l.telegram.Reply(ctx, chatID, "❌ Issue title cannot be empty.")
		return
	}

	owner, repo, err := githubclient.SplitRepo(fullRepo)
	if err != nil {
		_ = l.telegram.Reply(ctx, chatID, fmt.Sprintf("❌ Invalid repo format: %q. Expected owner/repo.", fullRepo))
		return
	}

	issue, err := l.gh.CreateIssue(ctx, owner, repo, title, body, []string{l.cfg.Labels.Ready})
	if err != nil {
		l.log.Error("telegram /issue: create issue failed", "err", err)
		_ = l.telegram.Reply(ctx, chatID, fmt.Sprintf("❌ Failed to create issue: %v", err))
		return
	}

	l.log.Info("issue created via Telegram", "repo", fullRepo, "issue", issue.Number, "title", title)
	_ = l.telegram.Reply(ctx, chatID,
		fmt.Sprintf("✅ Issue #%d created on *%s*\n%s\n\nLabeled `ready` — Madar will pick it up shortly.",
			issue.Number, fullRepo, issue.HTMLURL))
}

// handleStatusCommand replies with a brief summary of active tasks.
func (l *Loop) handleStatusCommand(ctx context.Context, chatID int64) {
	active, _ := l.store.CountActive()
	inProgress, _ := l.store.ListByState("in-progress")
	waiting, _ := l.store.ListByState("awaiting-feedback")

	var sb strings.Builder
	fmt.Fprintf(&sb, "📊 *Madar status*\n\nActive tasks: %d\n", active)
	if len(inProgress) > 0 {
		sb.WriteString("\n*In progress:*\n")
		for _, t := range inProgress {
			fmt.Fprintf(&sb, "• #%d %s\n", t.IssueNumber, t.Repo)
		}
	}
	if len(waiting) > 0 {
		sb.WriteString("\n*Awaiting feedback:*\n")
		for _, t := range waiting {
			fmt.Fprintf(&sb, "• #%d %s\n", t.IssueNumber, t.Repo)
		}
	}
	if len(inProgress) == 0 && len(waiting) == 0 {
		sb.WriteString("\nIdle — no active tasks.")
	}
	_ = l.telegram.Reply(ctx, chatID, sb.String())
}

// handleAddRepoCommand adds a repo to the watch list immediately and persists
// it to config.yaml so it survives restarts.
func (l *Loop) handleAddRepoCommand(ctx context.Context, chatID int64, text string) {
	parts := strings.Fields(text)
	if len(parts) < 2 {
		_ = l.telegram.Reply(ctx, chatID,
			"Usage: /addrepo owner/repo\n\nExample:\n/addrepo eslam-mahmoud/my-project")
		return
	}
	fullRepo := strings.TrimSpace(parts[1])
	if _, _, err := githubclient.SplitRepo(fullRepo); err != nil {
		_ = l.telegram.Reply(ctx, chatID,
			fmt.Sprintf("❌ Invalid format: %q — expected owner/repo", fullRepo))
		return
	}

	// Check for duplicates.
	for _, r := range l.cfg.Repos {
		if r.Name == fullRepo {
			_ = l.telegram.Reply(ctx, chatID,
				fmt.Sprintf("Already watching *%s*", fullRepo))
			return
		}
	}

	// Add to in-memory config immediately (takes effect on next tick).
	l.cfg.Repos = append(l.cfg.Repos, config.RepoConfig{Name: fullRepo})

	// Persist to config.yaml so the repo survives restarts.
	if err := appendRepoToConfig(l.cfg.ConfigPath, fullRepo); err != nil {
		l.log.Warn("could not persist repo to config.yaml", "repo", fullRepo, "err", err)
		_ = l.telegram.Reply(ctx, chatID,
			fmt.Sprintf("⚠️ Added *%s* for this session but could not write to config.yaml: %v\n\nAdd it manually to persist across restarts.", fullRepo, err))
		return
	}

	// Clone the workspace now so Madar can start picking up issues immediately.
	singleCfg := *l.cfg
	singleCfg.Repos = []config.RepoConfig{{Name: fullRepo}}
	if err := EnsureWorkspaces(ctx, &singleCfg, l.log); err != nil {
		l.log.Warn("workspace clone failed for new repo", "repo", fullRepo, "err", err)
		_ = l.telegram.Reply(ctx, chatID,
			fmt.Sprintf("⚠️ Added *%s* but workspace clone failed: %v\nMadar will retry on the next poll.", fullRepo, err))
		return
	}

	l.log.Info("repo added via Telegram", "repo", fullRepo)
	_ = l.telegram.Reply(ctx, chatID,
		fmt.Sprintf("✅ Now watching *%s*\n\nOpen an issue and label it `ready` to queue a task.", fullRepo))
}

// handleReposCommand lists currently watched repos.
func (l *Loop) handleReposCommand(ctx context.Context, chatID int64) {
	if len(l.cfg.Repos) == 0 {
		_ = l.telegram.Reply(ctx, chatID, "No repos configured. Use /addrepo owner/repo to add one.")
		return
	}
	var sb strings.Builder
	sb.WriteString("*Watched repos:*\n")
	for _, r := range l.cfg.Repos {
		sb.WriteString("• " + r.Name)
		if r.AutoMerge != nil && *r.AutoMerge {
			sb.WriteString(" _(auto-merge)_")
		}
		sb.WriteString("\n")
	}
	_ = l.telegram.Reply(ctx, chatID, sb.String())
}

func (l *Loop) replyHelp(ctx context.Context, chatID int64) {
	_ = l.telegram.Reply(ctx, chatID,
		"*Madar commands*\n\n"+
			"/issue \\[owner/repo\\] Title\n"+
			"  Create a GitHub issue labeled `ready`\\.  Body on next lines\\.\n\n"+
			"/addrepo owner/repo\n"+
			"  Add a repo to the watch list\\.\n\n"+
			"/repos\n"+
			"  List currently watched repos\\.\n\n"+
			"/status\n"+
			"  Show active and waiting tasks\\.\n\n"+
			"/help\n"+
			"  Show this message\\.")
}
