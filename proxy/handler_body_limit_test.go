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
	"strings"
	"testing"
)

// newBodyLimitHandler seeds a config with maxRequestBodyMB=1 and returns a
// Handler wired to the (empty) account pool. No API keys are configured, so
// authentication passes and requests reach the body-reading stage.
func newBodyLimitHandler(t *testing.T) *Handler {
	t.Helper()
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")
	seed := map[string]interface{}{
		"password":         "p",
		"port":             8080,
		"host":             "0.0.0.0",
		"requireApiKey":    false,
		"maxRequestBodyMB": 1,
		"accounts":         []map[string]interface{}{},
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
	if got := config.GetMaxRequestBodyBytes(); got != 1<<20 {
		t.Fatalf("expected 1 MiB cap, got %d", got)
	}
	p := accountpool.GetPool()
	p.Reload()
	return &Handler{pool: p, promptCache: newPromptCacheTracker(defaultPromptCacheTTL, 0, 0)}
}

// TestPublicEndpointsRejectOversizedBodyWith413 verifies that a request body
// exceeding the configured cap is rejected with HTTP 413 on each public
// inference endpoint, rather than being read wholesale into memory.
func TestPublicEndpointsRejectOversizedBodyWith413(t *testing.T) {
	h := newBodyLimitHandler(t)

	// 2 MiB of valid JSON-ish payload: comfortably over the 1 MiB cap.
	oversized := `{"model":"claude-sonnet-4.5","messages":[{"role":"user","content":"` +
		strings.Repeat("A", 2<<20) + `"}]}`

	cases := []struct {
		name string
		path string
	}{
		{"claude_messages", "/v1/messages"},
		{"claude_count_tokens", "/v1/messages/count_tokens"},
		{"openai_chat", "/v1/chat/completions"},
		{"openai_responses", "/v1/responses"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.path, bytes.NewReader([]byte(oversized)))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("%s: expected 413, got %d body=%s", tc.path, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestPublicEndpointAcceptsBodyWithinLimit confirms the cap does not reject a
// normal small body: it must pass the body-reading stage (a 413 must NOT be
// returned). Downstream failures (no accounts) are fine — we only assert the
// request is not rejected for size.
func TestPublicEndpointAcceptsBodyWithinLimit(t *testing.T) {
	h := newBodyLimitHandler(t)

	small := `{"model":"claude-sonnet-4.5","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(small)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("small body should not be rejected as too large, got 413 body=%s", rec.Body.String())
	}
}
