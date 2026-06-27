package orchestrator

import (
	"context"
	"fmt"
	"strings"

	githubclient "github.com/eslam-mahmoud/go-ai-agent/internal/github"
)

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

func (l *Loop) replyHelp(ctx context.Context, chatID int64) {
	_ = l.telegram.Reply(ctx, chatID,
		"*Madar commands*\n\n"+
			"/issue \\[owner/repo\\] Title\n"+
			"  Create a GitHub issue labeled `ready`\\.\n"+
			"  Body goes on the next lines\\.\n\n"+
			"/status\n"+
			"  Show active and waiting tasks\\.\n\n"+
			"/help\n"+
			"  Show this message\\.")
}
