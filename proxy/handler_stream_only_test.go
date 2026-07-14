package proxy

import (
	"bytes"
	"encoding/json"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// newStreamOnlyHandler seeds a config that requires API keys and holds two keys:
// one stream-only and one unrestricted. Authentication is enforced so the
// matched key's StreamOnly flag governs whether synchronous requests are
// rejected. The account pool is empty: stream requests are expected to fail
// downstream, but must first pass the stream-only gate.
func newStreamOnlyHandler(t *testing.T) *Handler {
	t.Helper()
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")
	seed := map[string]interface{}{
		"password":      "p",
		"port":          8080,
		"host":          "0.0.0.0",
		"requireApiKey": true,
		"apiKeys": []map[string]interface{}{
			{"id": "k-stream", "key": "sk-stream-only", "enabled": true, "streamOnly": true},
			{"id": "k-open", "key": "sk-unrestricted", "enabled": true, "streamOnly": false},
		},
		"accounts": []map[string]interface{}{},
	}
	raw, err := json.MarshalIndent(seed, "", "  ")
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	if err := os.WriteFile(cfgFile, raw, 0600); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	p := accountpool.GetPool()
	p.Reload()
	return &Handler{pool: p, promptCache: newPromptCacheTracker(defaultPromptCacheTTL, 0, 0)}
}

func streamOnlyRequest(path, key string, stream bool) *http.Request {
	payload := map[string]interface{}{
		"model":  "claude-sonnet-4.5",
		"stream": stream,
	}
	// The /v1/responses endpoint uses the OpenAI Responses schema (`input`)
	// rather than `messages`, and validates a non-empty input before reaching
	// the stream-only gate, so it needs the matching shape.
	if path == "/v1/responses" {
		payload["input"] = []map[string]interface{}{
			{"role": "user", "content": "hi"},
		}
	} else {
		payload["messages"] = []map[string]string{{"role": "user", "content": "hi"}}
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	return req
}

// TestStreamOnlyKeyRejectsSyncRequests verifies that a stream-only key gets a
// 403 on a non-streaming request across all three public inference endpoints.
func TestStreamOnlyKeyRejectsSyncRequests(t *testing.T) {
	h := newStreamOnlyHandler(t)

	for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, streamOnlyRequest(path, "sk-stream-only", false))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s: expected 403 for sync request on stream-only key, got %d body=%s", path, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestStreamOnlyKeyAllowsStreamRequests confirms the gate does not block a
// streaming request from the same stream-only key: the response must not be a
// 403 (it may fail later with no accounts available, which is acceptable here).
func TestStreamOnlyKeyAllowsStreamRequests(t *testing.T) {
	h := newStreamOnlyHandler(t)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, streamOnlyRequest("/v1/messages", "sk-stream-only", true))
	if rec.Code == http.StatusForbidden {
		t.Fatalf("stream request on stream-only key must not be forbidden, got 403 body=%s", rec.Body.String())
	}
}

// TestUnrestrictedKeyAllowsSyncRequests confirms a normal key (StreamOnly=false)
// is not affected: a sync request must not be rejected with 403.
func TestUnrestrictedKeyAllowsSyncRequests(t *testing.T) {
	h := newStreamOnlyHandler(t)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, streamOnlyRequest("/v1/messages", "sk-unrestricted", false))
	if rec.Code == http.StatusForbidden {
		t.Fatalf("sync request on unrestricted key must not be forbidden, got 403 body=%s", rec.Body.String())
	}
}
