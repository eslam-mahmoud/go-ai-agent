package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGateway_notifyWhenUnconfigured(t *testing.T) {
	gw := New("", nil)
	ctx := context.Background()
	// Should not error when bot token is empty
	if err := gw.NotifyClarification(ctx, "https://github.com/x/y/issues/1", "question?"); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestGateway_notifyWhenNoAllowedIDs(t *testing.T) {
	gw := New("token123", nil)
	ctx := context.Background()
	if err := gw.NotifyCompletion(ctx, "https://github.com/x/y/issues/1", "done"); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestGateway_sendsClarification(t *testing.T) {
	var received []sendMessageRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req sendMessageRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		received = append(received, req)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	gw := NewWithBase("mytoken", []string{"111", "222"}, srv.URL, srv.Client())
	ctx := context.Background()
	err := gw.NotifyClarification(ctx, "https://github.com/x/y/issues/1#comment-1", "Which DB?")
	if err != nil {
		t.Fatalf("NotifyClarification: %v", err)
	}

	if len(received) != 2 {
		t.Fatalf("expected 2 messages sent, got %d", len(received))
	}
	if received[0].ChatID != "111" || received[1].ChatID != "222" {
		t.Errorf("unexpected chat IDs: %v, %v", received[0].ChatID, received[1].ChatID)
	}
	if !strings.Contains(received[0].Text, "Which DB") {
		t.Errorf("message text should contain question, got: %q", received[0].Text)
	}
	if !strings.Contains(received[0].Text, "https://github.com") {
		t.Errorf("message text should contain issue URL")
	}
}

func TestGateway_sendsCompletion(t *testing.T) {
	var received []sendMessageRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req sendMessageRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		received = append(received, req)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	gw := NewWithBase("tok", []string{"999"}, srv.URL, srv.Client())
	ctx := context.Background()
	if err := gw.NotifyCompletion(ctx, "https://github.com/x/y/issues/5", "All done!"); err != nil {
		t.Fatalf("NotifyCompletion: %v", err)
	}
	if len(received) != 1 {
		t.Fatalf("expected 1 message, got %d", len(received))
	}
	if !strings.Contains(received[0].Text, "All done") {
		t.Errorf("completion text missing summary: %q", received[0].Text)
	}
}

func TestGateway_sendsError(t *testing.T) {
	var received []sendMessageRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req sendMessageRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		received = append(received, req)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	gw := NewWithBase("tok", []string{"100"}, srv.URL, srv.Client())
	ctx := context.Background()
	if err := gw.NotifyError(ctx, "https://github.com/x/y/issues/2", errTest("timed out")); err != nil {
		t.Fatalf("NotifyError: %v", err)
	}
	if len(received) != 1 {
		t.Fatalf("expected 1 message, got %d", len(received))
	}
	if !strings.Contains(received[0].Text, "timed out") {
		t.Errorf("error text missing message: %q", received[0].Text)
	}
}

func TestGateway_httpError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	gw := NewWithBase("tok", []string{"1"}, srv.URL, srv.Client())
	ctx := context.Background()
	err := gw.NotifyClarification(ctx, "url", "question?")
	if err == nil {
		t.Error("expected error for HTTP 500")
	}
}

func TestEscapeMarkdown(t *testing.T) {
	cases := []struct{ input, want string }{
		{"hello", "hello"},
		{"under_score", "under\\_score"},
		{"[link]", "\\[link]"},
		{"*bold*", "\\*bold\\*"},
		{"`code`", "\\`code\\`"},
		{"normal text", "normal text"},
		{"_*`[all", "\\_\\*\\`\\[all"},
	}
	for _, tc := range cases {
		got := escapeMarkdown(tc.input)
		if got != tc.want {
			t.Errorf("escapeMarkdown(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestGateway_truncatesLongMessage(t *testing.T) {
	var received []sendMessageRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req sendMessageRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		received = append(received, req)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	gw := NewWithBase("tok", []string{"1"}, srv.URL, srv.Client())
	// Build a message longer than 4096 bytes.
	longSummary := strings.Repeat("a", 5000)
	if err := gw.NotifyCompletion(context.Background(), "https://github.com/x/y/issues/1", longSummary); err != nil {
		t.Fatalf("NotifyCompletion: %v", err)
	}
	if len(received) != 1 {
		t.Fatalf("expected 1 message, got %d", len(received))
	}
	if len(received[0].Text) > telegramMaxLen {
		t.Errorf("message length %d exceeds Telegram limit %d", len(received[0].Text), telegramMaxLen)
	}
	if !strings.Contains(received[0].Text, "truncated") {
		t.Error("truncated message should contain truncation marker")
	}
}

type errTest string

func (e errTest) Error() string { return string(e) }
