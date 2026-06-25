package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type Gateway interface {
	NotifyClarification(ctx context.Context, issueURL, question string) error
	NotifyCompletion(ctx context.Context, issueURL, summary string) error
	NotifyError(ctx context.Context, issueURL string, err error) error
}

type gateway struct {
	botToken   string
	allowedIDs []string
	apiBase    string // overridable for tests
	httpClient *http.Client
}

func New(botToken string, allowedIDs []string) Gateway {
	return &gateway{
		botToken:   botToken,
		allowedIDs: allowedIDs,
		apiBase:    "https://api.telegram.org",
		httpClient: &http.Client{},
	}
}

// NewWithBase creates a gateway with a custom API base URL (for testing).
func NewWithBase(botToken string, allowedIDs []string, apiBase string, client *http.Client) Gateway {
	return &gateway{
		botToken:   botToken,
		allowedIDs: allowedIDs,
		apiBase:    apiBase,
		httpClient: client,
	}
}

func (g *gateway) NotifyClarification(ctx context.Context, issueURL, question string) error {
	text := fmt.Sprintf(
		"🤔 *Madar needs your input*\n\n"+
			"Question: %s\n\n"+
			"Reply on the issue: %s",
		escapeMarkdown(question),
		issueURL,
	)
	return g.broadcast(ctx, text)
}

func (g *gateway) NotifyCompletion(ctx context.Context, issueURL, summary string) error {
	text := fmt.Sprintf(
		"✅ *Task completed*\n\n"+
			"%s\n\n"+
			"Issue: %s",
		escapeMarkdown(summary),
		issueURL,
	)
	return g.broadcast(ctx, text)
}

func (g *gateway) NotifyError(ctx context.Context, issueURL string, err error) error {
	text := fmt.Sprintf(
		"❌ *Task error*\n\n"+
			"Error: %s\n\n"+
			"Issue: %s",
		escapeMarkdown(err.Error()),
		issueURL,
	)
	return g.broadcast(ctx, text)
}

func (g *gateway) broadcast(ctx context.Context, text string) error {
	if g.botToken == "" || len(g.allowedIDs) == 0 {
		return nil // not configured — silently skip
	}
	var errs []string
	for _, chatID := range g.allowedIDs {
		if err := g.send(ctx, chatID, text); err != nil {
			errs = append(errs, fmt.Sprintf("chat %s: %v", chatID, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("telegram send errors: %s", strings.Join(errs, "; "))
	}
	return nil
}

type sendMessageRequest struct {
	ChatID    string `json:"chat_id"`
	Text      string `json:"text"`
	ParseMode string `json:"parse_mode"`
}

func (g *gateway) send(ctx context.Context, chatID, text string) error {
	payload, err := json.Marshal(sendMessageRequest{
		ChatID:    chatID,
		Text:      text,
		ParseMode: "Markdown",
	})
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/bot%s/sendMessage", g.apiBase, g.botToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram API returned %d", resp.StatusCode)
	}
	return nil
}

// escapeMarkdown escapes Telegram MarkdownV1 special characters.
func escapeMarkdown(s string) string {
	// For MarkdownV1, only * _ ` [ need escaping
	s = strings.ReplaceAll(s, "_", "\\_")
	s = strings.ReplaceAll(s, "[", "\\[")
	return s
}
