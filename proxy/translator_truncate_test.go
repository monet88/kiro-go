package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"strings"
	"testing"
)

// TestClaudeToKiroTruncatesOversizedHistory builds a conversation whose history
// far exceeds the upstream input limit and verifies the converted payload is
// trimmed below the configured MaxPayloadBytes cap, that a truncation
// placeholder is inserted, and that the current message is preserved.
func TestClaudeToKiroTruncatesOversizedHistory(t *testing.T) {
	// ~2KB chunk repeated across many turns to blow past the byte limit.
	big := strings.Repeat("lorem ipsum dolor sit amet ", 80) // ~2.1KB

	msgs := []ClaudeMessage{
		{Role: "user", Content: "start the long task"},
	}
	for i := 0; i < 800; i++ {
		msgs = append(msgs,
			ClaudeMessage{Role: "assistant", Content: "step result: " + big},
			ClaudeMessage{Role: "user", Content: "next: " + big},
		)
	}
	msgs = append(msgs, ClaudeMessage{Role: "user", Content: "FINAL: summarize everything above"})

	req := &ClaudeRequest{
		Model:    "claude-opus-4.8",
		System:   "You are a helpful assistant.",
		Messages: msgs,
	}

	payload := ClaudeToKiro(req, false)

	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	limit := config.GetMaxPayloadBytes()
	if len(raw) > limit {
		t.Fatalf("payload size %d exceeds limit %d after truncation", len(raw), limit)
	}

	// The current message must be preserved.
	cur := payload.ConversationState.CurrentMessage.UserInputMessage
	if !strings.Contains(cur.Content, "FINAL: summarize everything above") {
		t.Fatalf("current message lost after truncation, got %q", cur.Content[:min(80, len(cur.Content))])
	}

	// A truncation placeholder must be present in history.
	foundPlaceholder := false
	for _, h := range payload.ConversationState.History {
		if h.UserInputMessage != nil && strings.Contains(h.UserInputMessage.Content, "truncated to fit") {
			foundPlaceholder = true
			break
		}
	}
	if !foundPlaceholder {
		t.Fatalf("expected a truncation placeholder in history")
	}

	// System priming should still be at the front.
	if len(payload.ConversationState.History) < 2 {
		t.Fatalf("expected priming retained, history too short")
	}
	primingUser := payload.ConversationState.History[0].UserInputMessage
	if primingUser == nil || !strings.Contains(primingUser.Content, "helpful assistant") {
		t.Fatalf("expected system priming retained at front")
	}
}

// TestClaudeToKiroSmallPayloadNotTruncated ensures normal-sized conversations
// are left untouched (no placeholder inserted).
func TestClaudeToKiroSmallPayloadNotTruncated(t *testing.T) {
	req := &ClaudeRequest{
		Model:  "claude-opus-4.8",
		System: "You are helpful.",
		Messages: []ClaudeMessage{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "hi"},
			{Role: "user", Content: "how are you?"},
		},
	}
	payload := ClaudeToKiro(req, false)
	for _, h := range payload.ConversationState.History {
		if h.UserInputMessage != nil && strings.Contains(h.UserInputMessage.Content, "truncated to fit") {
			t.Fatalf("small payload should not be truncated")
		}
	}
}

// TestTruncateCurrentMessageUTF8Safe verifies the last-resort current-message
// truncation backs off to a rune boundary so a multi-byte character is never
// split, which would otherwise produce invalid UTF-8 and break JSON marshalling
// or upstream processing.
func TestTruncateCurrentMessageUTF8Safe(t *testing.T) {
	// 3-byte CJK runes so most byte budgets land mid-rune without a backoff.
	content := strings.Repeat("上下文内容", 2000) // 10000 runes, 30000 bytes
	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: content,
		ModelID: "claude-sonnet-4.5",
		Origin:  "AI_EDITOR",
	}

	// Cap well below the content size to force truncation of the current message.
	truncateCurrentMessage(payload, 4096)

	got := payload.ConversationState.CurrentMessage.UserInputMessage.Content
	if len(got) >= len(content) {
		t.Fatalf("expected current message to be truncated, got %d >= %d bytes", len(got), len(content))
	}
	// Must remain valid UTF-8 (no rune split at the cut point).
	if string([]rune(got)) != got {
		t.Fatalf("truncation split a multi-byte rune: result is not valid UTF-8")
	}
	// The whole payload must marshal cleanly (invalid UTF-8 would corrupt JSON).
	if _, err := json.Marshal(payload); err != nil {
		t.Fatalf("payload with truncated content failed to marshal: %v", err)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
