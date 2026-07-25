package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// Update represents a single inbound Telegram update.
type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message"`
}

// Message is the subset of Telegram message fields Madar uses.
type Message struct {
	MessageID int64  `json:"message_id"`
	Text      string `json:"text"`
	Chat      struct {
		ID int64 `json:"id"`
	} `json:"chat"`
	From struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
	} `json:"from"`
}

type Gateway interface {
	NotifyClarification(ctx context.Context, issueURL, question string) error
	NotifyCompletion(ctx context.Context, issueURL, summary string) error
	NotifyError(ctx context.Context, issueURL string, err error) error
	// GetUpdates polls for new inbound messages starting from offset.
	// Returns nil slice (not error) when the gateway is not configured.
	GetUpdates(ctx context.Context, offset int64) ([]Update, error)
	// Reply sends a message to a specific chat by its numeric ID.
	Reply(ctx context.Context, chatID int64, text string) error
	// Send broadcasts already-rendered text to every configured chat. It is
	// the delivery half of the v2 notification router.
	Send(ctx context.Context, text string) error
	// SendStatus posts a message and returns its identity so it can be edited
	// in place later. It targets the first configured chat only, since one
	// live status message per project is the point.
	SendStatus(ctx context.Context, text string) (chatID, messageID int64, err error)
	// EditStatus replaces the text of a previously sent status message.
	EditStatus(ctx context.Context, chatID, messageID int64, text string) error
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

const telegramMaxLen = 4096

func (g *gateway) send(ctx context.Context, chatID, text string) error {
	if len(text) > telegramMaxLen {
		text = text[:telegramMaxLen-15] + "\n…[truncated]"
	}
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

// Send broadcasts pre-rendered text. Callers own the wording; the gateway
// owns delivery.
func (g *gateway) Send(ctx context.Context, text string) error {
	return g.broadcast(ctx, text)
}

type editMessageRequest struct {
	ChatID    int64  `json:"chat_id"`
	MessageID int64  `json:"message_id"`
	Text      string `json:"text"`
	ParseMode string `json:"parse_mode"`
}

// SendStatus posts to the first configured chat and returns the identity of
// the message, so later updates edit it instead of adding to the timeline.
func (g *gateway) SendStatus(
	ctx context.Context,
	text string,
) (int64, int64, error) {
	if g.botToken == "" || len(g.allowedIDs) == 0 {
		return 0, 0, nil
	}
	chatID, err := strconv.ParseInt(strings.TrimSpace(g.allowedIDs[0]), 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("parse telegram chat ID: %w", err)
	}
	payload, err := json.Marshal(sendMessageRequest{
		ChatID:    strings.TrimSpace(g.allowedIDs[0]),
		Text:      truncateForTelegram(text),
		ParseMode: "Markdown",
	})
	if err != nil {
		return 0, 0, err
	}
	var response struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageID int64 `json:"message_id"`
		} `json:"result"`
	}
	if err := g.post(ctx, "sendMessage", payload, &response); err != nil {
		return 0, 0, err
	}
	if !response.OK || response.Result.MessageID == 0 {
		return 0, 0, fmt.Errorf("telegram did not return a message ID")
	}
	return chatID, response.Result.MessageID, nil
}

func (g *gateway) EditStatus(
	ctx context.Context,
	chatID, messageID int64,
	text string,
) error {
	if g.botToken == "" || chatID == 0 || messageID == 0 {
		return nil
	}
	payload, err := json.Marshal(editMessageRequest{
		ChatID:    chatID,
		MessageID: messageID,
		Text:      truncateForTelegram(text),
		ParseMode: "Markdown",
	})
	if err != nil {
		return err
	}
	return g.post(ctx, "editMessageText", payload, nil)
}

// post performs one Telegram API call, decoding into out when supplied.
func (g *gateway) post(
	ctx context.Context,
	method string,
	payload []byte,
	out any,
) error {
	url := fmt.Sprintf("%s/bot%s/%s", g.apiBase, g.botToken, method)
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
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func truncateForTelegram(text string) string {
	if len(text) > telegramMaxLen {
		return text[:telegramMaxLen-15] + "\n…[truncated]"
	}
	return text
}

func (g *gateway) GetUpdates(ctx context.Context, offset int64) ([]Update, error) {
	if g.botToken == "" {
		return nil, nil
	}
	url := fmt.Sprintf("%s/bot%s/getUpdates?offset=%d&limit=100", g.apiBase, g.botToken, offset)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		OK     bool     `json:"ok"`
		Result []Update `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if !result.OK {
		return nil, fmt.Errorf("telegram getUpdates returned ok=false")
	}
	return result.Result, nil
}

func (g *gateway) Reply(ctx context.Context, chatID int64, text string) error {
	return g.send(ctx, fmt.Sprintf("%d", chatID), text)
}

// escapeMarkdown escapes all Telegram MarkdownV1 special characters.
func escapeMarkdown(s string) string {
	s = strings.ReplaceAll(s, "_", "\\_")
	s = strings.ReplaceAll(s, "*", "\\*")
	s = strings.ReplaceAll(s, "`", "\\`")
	s = strings.ReplaceAll(s, "[", "\\[")
	return s
}
