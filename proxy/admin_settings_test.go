package proxy

import (
	"encoding/json"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// newSettingsTestHandler wires a Handler backed by a fresh temp config so the
// admin settings endpoints can be exercised without touching real state.
func newSettingsTestHandler(t *testing.T) *Handler {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	p := accountpool.GetPool()
	p.Reload()
	return &Handler{
		pool:        p,
		promptCache: newPromptCacheTracker(defaultPromptCacheTTL, 0, 0),
	}
}

// TestApiGetSettingsReturnsPromptCacheDefaults verifies the settings GET surface
// exposes the three ticket-#5 knobs with their built-in defaults on a fresh config.
func TestApiGetSettingsReturnsPromptCacheDefaults(t *testing.T) {
	h := newSettingsTestHandler(t)

	rec := httptest.NewRecorder()
	h.apiGetSettings(rec, httptest.NewRequest(http.MethodGet, "/api/settings", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if v, ok := got["maxPayloadBytes"].(float64); !ok || int(v) != config.DefaultMaxPayloadBytes {
		t.Fatalf("expected maxPayloadBytes=%d, got %#v", config.DefaultMaxPayloadBytes, got["maxPayloadBytes"])
	}
	if v, ok := got["promptCacheMaxEntries"].(float64); !ok || int(v) != config.DefaultPromptCacheMaxEntries {
		t.Fatalf("expected promptCacheMaxEntries=%d, got %#v", config.DefaultPromptCacheMaxEntries, got["promptCacheMaxEntries"])
	}
	if v, ok := got["promptCacheMaxRatio"].(float64); !ok || v != config.DefaultPromptCacheMaxRatio {
		t.Fatalf("expected promptCacheMaxRatio=%v, got %#v", config.DefaultPromptCacheMaxRatio, got["promptCacheMaxRatio"])
	}
}

// TestApiUpdateSettingsPersistsPromptCacheKnobs verifies POST persists the three
// knobs and a subsequent GET reflects the updated values.
func TestApiUpdateSettingsPersistsPromptCacheKnobs(t *testing.T) {
	h := newSettingsTestHandler(t)

	body := `{"maxPayloadBytes":3000000,"promptCacheMaxEntries":4096,"promptCacheMaxRatio":0.5}`
	rec := httptest.NewRecorder()
	h.apiUpdateSettings(rec, httptest.NewRequest(http.MethodPost, "/api/settings", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("update expected 200, got %d (%s)", rec.Code, rec.Body.String())
	}

	if got := config.GetMaxPayloadBytes(); got != 3000000 {
		t.Fatalf("expected persisted maxPayloadBytes=3000000, got %d", got)
	}
	if got := config.GetPromptCacheMaxEntries(); got != 4096 {
		t.Fatalf("expected persisted promptCacheMaxEntries=4096, got %d", got)
	}
	if got := config.GetPromptCacheMaxRatio(); got != 0.5 {
		t.Fatalf("expected persisted promptCacheMaxRatio=0.5, got %v", got)
	}

	// Subsequent GET reflects the updates.
	rec2 := httptest.NewRecorder()
	h.apiGetSettings(rec2, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	var got map[string]interface{}
	if err := json.Unmarshal(rec2.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if v, _ := got["maxPayloadBytes"].(float64); int(v) != 3000000 {
		t.Fatalf("expected GET maxPayloadBytes=3000000, got %#v", got["maxPayloadBytes"])
	}
	if v, _ := got["promptCacheMaxEntries"].(float64); int(v) != 4096 {
		t.Fatalf("expected GET promptCacheMaxEntries=4096, got %#v", got["promptCacheMaxEntries"])
	}
	if v, _ := got["promptCacheMaxRatio"].(float64); v != 0.5 {
		t.Fatalf("expected GET promptCacheMaxRatio=0.5, got %#v", got["promptCacheMaxRatio"])
	}
}
