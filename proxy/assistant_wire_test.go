package proxy

import (
	"context"
	"encoding/json"
	"io"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func setupWireHandler(t *testing.T, accountID string) *Handler {
	t.Helper()
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:          accountID,
		Enabled:     true,
		AccessToken: "token-" + accountID,
		ProfileArn:  "arn:aws:codewhisperer:profile/" + accountID,
	}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("preferred endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("endpoint fallback: %v", err)
	}
	p := accountpool.GetPool()
	p.Reload()
	return &Handler{
		pool:        p,
		promptCache: newPromptCacheTracker(defaultPromptCacheTTL, 0, 0),
	}
}

func countUpstreamHits(t *testing.T) (*int32, *httptest.Server, func()) {
	t.Helper()
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Error(w, "should not be called", http.StatusInternalServerError)
	}))
	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{{URL: server.URL, Origin: "AI_EDITOR", Name: "test"}}
	oldClient := kiroHttpStore.Load()
	kiroHttpStore.Store(&http.Client{Timeout: time.Second, Transport: &http.Transport{}})
	cleanup := func() {
		server.Close()
		kiroEndpoints = oldEndpoints
		kiroHttpStore.Store(oldClient)
	}
	return &hits, server, cleanup
}

// Invalid external $ref must 400 before any upstream I/O on Claude.
func TestClaudeMessagesRejectsExternalSchemaRefWithoutUpstream(t *testing.T) {
	h := setupWireHandler(t, "claude-decl-tools")
	hits, _, cleanup := countUpstreamHits(t)
	defer cleanup()

	body := `{
		"model":"claude-sonnet-4.5",
		"max_tokens":16,
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{
			"name":"lookup",
			"description":"x",
			"input_schema":{"$ref":"https://example.com/schema.json"}
		}],
		"stream":false
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.handleClaudeMessages(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	if atomic.LoadInt32(hits) != 0 {
		t.Fatalf("expected no upstream I/O, hits=%d", atomic.LoadInt32(hits))
	}
	if !strings.Contains(rec.Body.String(), "invalid_request_error") {
		t.Fatalf("expected invalid_request_error, body=%s", rec.Body.String())
	}
}

func TestOpenAIChatRejectsExternalSchemaRefWithoutUpstream(t *testing.T) {
	h := setupWireHandler(t, "openai-decl-tools")
	hits, _, cleanup := countUpstreamHits(t)
	defer cleanup()

	body := `{
		"model":"claude-sonnet-4.5",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{
			"type":"function",
			"function":{
				"name":"lookup",
				"description":"x",
				"parameters":{"$ref":"https://example.com/schema.json"}
			}
		}],
		"stream":false
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.handleOpenAIChat(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	if atomic.LoadInt32(hits) != 0 {
		t.Fatalf("expected no upstream I/O, hits=%d", atomic.LoadInt32(hits))
	}
}

func TestResponsesRejectsExternalSchemaRefWithoutUpstream(t *testing.T) {
	h := setupWireHandler(t, "responses-decl-tools")
	hits, _, cleanup := countUpstreamHits(t)
	defer cleanup()

	body := `{
		"model":"claude-sonnet-4.5",
		"input":"hi",
		"store":false,
		"stream":false,
		"tools":[{
			"type":"function",
			"name":"lookup",
			"description":"x",
			"parameters":{"$ref":"https://example.com/schema.json"}
		}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.handleOpenAIResponses(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	if atomic.LoadInt32(hits) != 0 {
		t.Fatalf("expected no upstream I/O, hits=%d", atomic.LoadInt32(hits))
	}
}

// Invalid structured tool JSON after commit is Model Output Error: no Account
// penalty / no second account attempt.
func TestClaudeNonStreamModelOutputErrorNoFailover(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	for _, id := range []string{"a1", "a2"} {
		if err := config.AddAccount(config.Account{
			ID:          id,
			Enabled:     true,
			AccessToken: "token-" + id,
			ProfileArn:  "arn:aws:codewhisperer:profile/" + id,
		}); err != nil {
			t.Fatalf("add %s: %v", id, err)
		}
	}
	_ = config.UpdatePreferredEndpoint("kiro")
	_ = config.UpdateEndpointFallback(false)

	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
		// Structured tool with irreparable input for declared schema.
		_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "toolu_bad",
			"name":      "lookup",
			"input":     "{not-json",
			"stop":      true,
		}))
	}))
	defer server.Close()
	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{{URL: server.URL, Origin: "AI_EDITOR", Name: "test"}}
	defer func() { kiroEndpoints = oldEndpoints }()
	oldClient := kiroHttpStore.Load()
	kiroHttpStore.Store(&http.Client{Timeout: time.Second, Transport: &http.Transport{}})
	defer kiroHttpStore.Store(oldClient)

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p, promptCache: newPromptCacheTracker(defaultPromptCacheTTL, 0, 0)}

	// Build payload with declared tool schema via Claude translator path.
	reqBody := map[string]interface{}{
		"model":      "claude-sonnet-4.5",
		"max_tokens": 32,
		"messages":   []map[string]interface{}{{"role": "user", "content": "hi"}},
		"tools": []map[string]interface{}{{
			"name":         "lookup",
			"description":  "x",
			"input_schema": map[string]interface{}{"type": "object", "properties": map[string]interface{}{"q": map[string]interface{}{"type": "string"}}, "required": []string{"q"}},
		}},
		"stream": false,
	}
	raw, _ := json.Marshal(reqBody)
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(raw)))
	rec := httptest.NewRecorder()
	h.handleClaudeMessages(rec, httpReq)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 model output error, got %d body=%s", rec.Code, rec.Body.String())
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("expected exactly one upstream attempt (no failover), hits=%d", atomic.LoadInt32(&hits))
	}
	// Account should remain enabled (non-penalizing).
	accounts := config.GetAccounts()
	for _, a := range accounts {
		if !a.Enabled {
			t.Fatalf("model output error penalized account %s", a.ID)
		}
	}
	if strings.Contains(rec.Body.String(), "{not-json") {
		t.Fatalf("raw tool args leaked to client: %s", rec.Body.String())
	}
}

func TestCrossProtocolNormalizedToolCallEquivalence(t *testing.T) {
	// Same semantic toolUseEvent fixture through normalizer once; adapters only render.
	tools, err := buildDeclaredToolSet(map[string]string{"lookup": "lookup"}, map[string]interface{}{
		"lookup": map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{"q": map[string]interface{}{"type": "string"}},
			"required":   []interface{}{"q"},
		},
	})
	if err != nil {
		t.Fatalf("build tools: %v", err)
	}
	n := newAssistantNormalizer(tools)
	var calls []normalizedToolCall
	feed := func(ev kiroSemanticEvent) {
		for _, aev := range n.handle(ev) {
			if aev.kind == assistantKindToolCall {
				calls = append(calls, aev.tool)
			}
		}
	}
	start, _ := newToolStart("toolu_1", "lookup")
	feed(start)
	in, _ := newToolInput("toolu_1", "lookup", `{"q":"ok"}`, false)
	feed(in)
	stop, _ := newToolStop("toolu_1", "lookup")
	feed(stop)
	if len(calls) != 1 {
		t.Fatalf("expected 1 validated call, got %d", len(calls))
	}
	// Claude/OpenAI render helpers must preserve id/name/input.
	if calls[0].Name != "lookup" || calls[0].ID != "toolu_1" {
		t.Fatalf("unexpected call: %+v", calls[0])
	}
	if q, _ := calls[0].Input["q"].(string); q != "ok" {
		t.Fatalf("unexpected input: %+v", calls[0].Input)
	}
	// Bridge to KiroToolUse used by non-stream collectors.
	tu := kiroToolFromNormalized(calls[0])
	if tu.ToolUseID != calls[0].ID || tu.Name != calls[0].Name {
		t.Fatalf("bridge mismatch: %+v vs %+v", tu, calls[0])
	}
}

// Ensure stream path returns content without CallKiroAPI in production handlers.
func TestClaudeStreamUsesSemanticPathForToolUse(t *testing.T) {
	h := setupWireHandler(t, "claude-tool-stream")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"toolUseId": "toolu_ok",
			"name":      "lookup",
			"input":     `{"q":"x"}`,
			"stop":      true,
		}))
	}))
	defer server.Close()
	oldEndpoints := kiroEndpoints
	kiroEndpoints = []kiroEndpoint{{URL: server.URL, Origin: "AI_EDITOR", Name: "test"}}
	defer func() { kiroEndpoints = oldEndpoints }()
	oldClient := kiroHttpStore.Load()
	kiroHttpStore.Store(&http.Client{Timeout: time.Second, Transport: &http.Transport{}})
	defer kiroHttpStore.Store(oldClient)

	// Request through full handler with declared tool.
	body := `{
		"model":"claude-sonnet-4.5",
		"max_tokens":32,
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{
			"name":"lookup",
			"description":"x",
			"input_schema":{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}
		}],
		"stream":true
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.handleClaudeMessages(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	if !strings.Contains(out, "tool_use") {
		t.Fatalf("expected tool_use block, body=%s", out)
	}
	if !strings.Contains(out, "toolu_ok") || !strings.Contains(out, "lookup") {
		t.Fatalf("expected tool id/name in stream, body=%s", out)
	}
	if !strings.Contains(out, "message_stop") {
		t.Fatalf("expected message_stop, body=%s", out)
	}
	_ = context.Background()
	_ = io.Discard
}
