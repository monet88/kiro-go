package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"kiro-go/config"
	accountpool "kiro-go/pool"
)

// lockedFlushRecorder is a concurrency-safe ResponseWriter+Flusher for stream
// keepalive tests. The keepalive goroutine and the main handler goroutine both
// write to w during an upstream stall; production serializes those writes with
// the helper mutex, while this recorder only prevents httptest-style races in
// the test body capture itself.
type lockedFlushRecorder struct {
	mu  sync.Mutex
	hdr http.Header
	buf strings.Builder
}

func newLockedFlushRecorder() *lockedFlushRecorder {
	return &lockedFlushRecorder{hdr: make(http.Header)}
}

func (r *lockedFlushRecorder) Header() http.Header {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hdr
}

func (r *lockedFlushRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.Write(p)
}

func (r *lockedFlushRecorder) WriteHeader(int) {}

func (r *lockedFlushRecorder) Flush() {}

func (r *lockedFlushRecorder) body() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

// newStalledEventStreamServer returns an upstream that emits one assistant
// frame, idles for stallFor (simulating a long thinking gap), then emits a
// second frame. That mid-stream silence is what must trigger Stream Keepalive.
func newStalledEventStreamServer(t *testing.T, stallFor time.Duration) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("upstream test server ResponseWriter is not a Flusher")
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "first ",
		}))
		flusher.Flush()
		time.Sleep(stallFor)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "second",
		}))
		flusher.Flush()
	}))
}

// newBusyEventStreamServer emits many frames with a short gap so the stream is
// never idle long enough for a keepalive tick to fire.
func newBusyEventStreamServer(t *testing.T, frames int, gap time.Duration) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("upstream test server ResponseWriter is not a Flusher")
			return
		}
		w.WriteHeader(http.StatusOK)
		for i := 0; i < frames; i++ {
			_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
				"content": fmt.Sprintf("chunk-%d ", i),
			}))
			flusher.Flush()
			time.Sleep(gap)
		}
	}))
}

func withFastKeepalive(t *testing.T, d time.Duration) func() {
	t.Helper()
	old := streamKeepaliveInterval
	streamKeepaliveInterval = d
	return func() { streamKeepaliveInterval = old }
}

func withNoTotalTimeoutStreamClient(t *testing.T) func() {
	t.Helper()
	old := kiroHttpStore.Load()
	kiroHttpStore.Store(&http.Client{Timeout: 0, Transport: &http.Transport{}})
	return func() { kiroHttpStore.Store(old) }
}

func setupKeepaliveTestAccount(t *testing.T, id string) *Handler {
	t.Helper()
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:          id,
		Enabled:     true,
		AccessToken: "token-" + id,
		ProfileArn:  "arn:aws:codewhisperer:profile/" + id,
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("set preferred endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable endpoint fallback: %v", err)
	}
	// Successful stream handlers kick UpdateStats which saves config from a
	// background goroutine. On Windows that keeps the TempDir open past the
	// test body and flaky-fails RemoveAll; give the write a moment to finish.
	t.Cleanup(func() {
		time.Sleep(150 * time.Millisecond)
	})
	p := accountpool.GetPool()
	p.Reload()
	return &Handler{
		pool:        p,
		promptCache: newPromptCacheTracker(defaultPromptCacheTTL, 0, 0),
	}
}

func keepaliveTestPayload(model string) *KiroPayload {
	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage = KiroUserInputMessage{
		Content: "hello",
		ModelID: model,
		Origin:  "AI_EDITOR",
	}
	return payload
}

func wireKeepaliveUpstream(t *testing.T, server *httptest.Server) func() {
	t.Helper()
	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{{URL: server.URL, Origin: "AI_EDITOR", Name: "test"}}
	restoreClient := withNoTotalTimeoutStreamClient(t)
	return func() {
		kiroEndpoints = oldEndpoints
		restoreClient()
	}
}

// TestClaudeStreamEmitsKeepaliveDuringUpstreamStall: when upstream stalls
// between two assistant frames, Claude Messages SSE must emit a comment
// keepalive and still deliver both real text frames with a clean close.
func TestClaudeStreamEmitsKeepaliveDuringUpstreamStall(t *testing.T) {
	h := setupKeepaliveTestAccount(t, "claude-keepalive")
	defer withFastKeepalive(t, 20*time.Millisecond)()

	server := newStalledEventStreamServer(t, 200*time.Millisecond)
	defer server.Close()
	defer wireKeepaliveUpstream(t, server)()

	model := "claude-opus-4-8"
	rec := newLockedFlushRecorder()
	h.handleClaudeStream(context.Background(), rec, keepaliveTestPayload(model), model, false, claudeThinkingResponseOptions{}, 1000, nil, "")

	body := rec.body()
	if !strings.Contains(body, ": keepalive") {
		t.Fatalf("expected a keepalive comment emitted during upstream stall, got body=%s", body)
	}
	if !strings.Contains(body, "first ") || !strings.Contains(body, "second") {
		t.Fatalf("expected both upstream text frames in output, got body=%s", body)
	}
	if !strings.Contains(body, "message_start") {
		t.Fatalf("expected message_start, got body=%s", body)
	}
	if !strings.Contains(body, "message_stop") {
		t.Fatalf("expected message_stop, got body=%s", body)
	}
}

// TestOpenAIStreamEmitsKeepaliveDuringUpstreamStall covers the OpenAI chat
// completions SSE path with the same stall pattern.
func TestOpenAIStreamEmitsKeepaliveDuringUpstreamStall(t *testing.T) {
	h := setupKeepaliveTestAccount(t, "openai-keepalive")
	defer withFastKeepalive(t, 20*time.Millisecond)()

	server := newStalledEventStreamServer(t, 200*time.Millisecond)
	defer server.Close()
	defer wireKeepaliveUpstream(t, server)()

	model := "claude-opus-4-8"
	rec := newLockedFlushRecorder()
	h.handleOpenAIStream(context.Background(), rec, keepaliveTestPayload(model), model, false, 1000, "")

	body := rec.body()
	if !strings.Contains(body, ": keepalive") {
		t.Fatalf("expected a keepalive comment emitted during upstream stall, got body=%s", body)
	}
	if !strings.Contains(body, "first ") || !strings.Contains(body, "second") {
		t.Fatalf("expected both upstream text frames in output, got body=%s", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Fatalf("expected [DONE] terminator, got body=%s", body)
	}
}

// TestResponsesStreamEmitsKeepaliveDuringUpstreamStall covers Responses SSE
// via the same shared keepalive helper.
func TestResponsesStreamEmitsKeepaliveDuringUpstreamStall(t *testing.T) {
	h := setupKeepaliveTestAccount(t, "responses-keepalive")
	defer withFastKeepalive(t, 20*time.Millisecond)()

	server := newStalledEventStreamServer(t, 200*time.Millisecond)
	defer server.Close()
	defer wireKeepaliveUpstream(t, server)()

	model := "claude-opus-4-8"
	req := &ResponsesRequest{Model: model, Stream: true, Store: boolPtr(false)}
	rec := newLockedFlushRecorder()
	h.handleResponsesStream(context.Background(), rec, keepaliveTestPayload(model), model, false, 1000, "", "resp_keepalive", req, nil, false)

	body := rec.body()
	if !strings.Contains(body, ": keepalive") {
		t.Fatalf("expected a keepalive comment emitted during upstream stall, got body=%s", body)
	}
	if !strings.Contains(body, "first ") || !strings.Contains(body, "second") {
		t.Fatalf("expected both upstream text frames in output, got body=%s", body)
	}
	if !strings.Contains(body, "response.completed") && !strings.Contains(body, "[DONE]") {
		t.Fatalf("expected Responses completion framing, got body=%s", body)
	}
}

// TestClaudeStreamSkipsKeepaliveWhileDataFlows asserts keepalive comments are
// not injected when the stream is continuously active (AC: no noise while data
// is flowing normally).
func TestClaudeStreamSkipsKeepaliveWhileDataFlows(t *testing.T) {
	h := setupKeepaliveTestAccount(t, "claude-busy")
	// Interval longer than the per-frame gap so continuous writes keep resetting
	// last-write and suppress pings.
	defer withFastKeepalive(t, 50*time.Millisecond)()

	server := newBusyEventStreamServer(t, 12, 5*time.Millisecond)
	defer server.Close()
	defer wireKeepaliveUpstream(t, server)()

	model := "claude-opus-4-8"
	rec := newLockedFlushRecorder()
	h.handleClaudeStream(context.Background(), rec, keepaliveTestPayload(model), model, false, claudeThinkingResponseOptions{}, 1000, nil, "")

	body := rec.body()
	if strings.Contains(body, ": keepalive") {
		t.Fatalf("did not expect keepalive while data was flowing, got body=%s", body)
	}
	if !strings.Contains(body, "chunk-0 ") || !strings.Contains(body, "chunk-11 ") {
		t.Fatalf("expected busy stream text frames, got body=%s", body)
	}
}

// TestOpenAIStreamSkipsKeepaliveWhileDataFlows covers the same busy-stream
// no-noise AC for OpenAI chat completions.
func TestOpenAIStreamSkipsKeepaliveWhileDataFlows(t *testing.T) {
	h := setupKeepaliveTestAccount(t, "openai-busy")
	defer withFastKeepalive(t, 50*time.Millisecond)()

	server := newBusyEventStreamServer(t, 12, 5*time.Millisecond)
	defer server.Close()
	defer wireKeepaliveUpstream(t, server)()

	model := "claude-opus-4-8"
	rec := newLockedFlushRecorder()
	h.handleOpenAIStream(context.Background(), rec, keepaliveTestPayload(model), model, false, 1000, "")

	body := rec.body()
	if strings.Contains(body, ": keepalive") {
		t.Fatalf("did not expect keepalive while data was flowing, got body=%s", body)
	}
	if !strings.Contains(body, "chunk-0 ") || !strings.Contains(body, "chunk-11 ") {
		t.Fatalf("expected busy stream text frames, got body=%s", body)
	}
}

// TestResponsesStreamSkipsKeepaliveWhileDataFlows covers the same busy-stream
// no-noise AC for Responses SSE.
func TestResponsesStreamSkipsKeepaliveWhileDataFlows(t *testing.T) {
	h := setupKeepaliveTestAccount(t, "responses-busy")
	defer withFastKeepalive(t, 50*time.Millisecond)()

	server := newBusyEventStreamServer(t, 12, 5*time.Millisecond)
	defer server.Close()
	defer wireKeepaliveUpstream(t, server)()

	model := "claude-opus-4-8"
	req := &ResponsesRequest{Model: model, Stream: true, Store: boolPtr(false)}
	rec := newLockedFlushRecorder()
	h.handleResponsesStream(context.Background(), rec, keepaliveTestPayload(model), model, false, 1000, "", "resp_busy", req, nil, false)

	body := rec.body()
	if strings.Contains(body, ": keepalive") {
		t.Fatalf("did not expect keepalive while data was flowing, got body=%s", body)
	}
	if !strings.Contains(body, "chunk-0 ") || !strings.Contains(body, "chunk-11 ") {
		t.Fatalf("expected busy stream text frames, got body=%s", body)
	}
}

func boolPtr(v bool) *bool { return &v }
