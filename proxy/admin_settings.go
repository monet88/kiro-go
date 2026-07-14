package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

func (h *Handler) apiGetStatus(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"accounts":                h.pool.Count(),
		"available":               h.pool.AvailableCount(),
		"totalRequests":           h.totalRequests,
		"successRequests":         h.successRequests,
		"failedRequests":          h.failedRequests,
		"totalTokens":             h.totalTokens,
		"totalCredits":            h.totalCredits,
		"uptime":                  time.Now().Unix() - h.startTime,
		"routingConcurrencyStats": h.pool.RoutingStats(),
	})
}

func (h *Handler) apiGetSettings(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"apiKey":                  config.GetApiKey(),
		"requireApiKey":           config.IsApiKeyRequired(),
		"port":                    config.GetPort(),
		"host":                    config.GetHost(),
		"allowOverUsage":          config.GetAllowOverUsage(),
		"balanceMode":             config.GetBalanceMode(),
		"routingConcurrency":      config.GetRoutingConcurrencyConfig(),
		"routingConcurrencyStats": h.pool.RoutingStats(),
		"maxPayloadBytes":         config.GetMaxPayloadBytes(),
		"promptCacheMaxEntries":   config.GetPromptCacheMaxEntries(),
		"promptCacheMaxRatio":     config.GetPromptCacheMaxRatio(),
	})
}

func (h *Handler) apiGetPromptFilter(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(config.GetPromptFilterConfig())
}

func (h *Handler) apiUpdatePromptFilter(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FilterClaudeCode      *bool                      `json:"filterClaudeCode,omitempty"`
		FilterEnvNoise        *bool                      `json:"filterEnvNoise,omitempty"`
		FilterStripBoundaries *bool                      `json:"filterStripBoundaries,omitempty"`
		Rules                 *[]config.PromptFilterRule `json:"rules,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// Read current config to fill in any fields not provided in the request.
	current := config.GetPromptFilterConfig()
	fcc := current.FilterClaudeCode
	fen := current.FilterEnvNoise
	fsb := current.FilterStripBoundaries
	rules := current.Rules
	if req.FilterClaudeCode != nil {
		fcc = *req.FilterClaudeCode
	}
	if req.FilterEnvNoise != nil {
		fen = *req.FilterEnvNoise
	}
	if req.FilterStripBoundaries != nil {
		fsb = *req.FilterStripBoundaries
	}
	if req.Rules != nil {
		rules = *req.Rules
	}
	if err := config.UpdatePromptFilterConfig(fcc, fen, fsb, rules); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) apiUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ApiKey                   *string                          `json:"apiKey,omitempty"`
		RequireApiKey            *bool                            `json:"requireApiKey,omitempty"`
		Password                 string                           `json:"password,omitempty"`
		AllowOverUsage           *bool                            `json:"allowOverUsage,omitempty"`
		BalanceMode              *string                          `json:"balanceMode,omitempty"`
		RoutingConcurrency       *config.RoutingConcurrencyConfig `json:"routingConcurrency,omitempty"`
		ServerReadTimeoutSeconds *int                             `json:"serverReadTimeoutSeconds,omitempty"`
		ServerIdleTimeoutSeconds *int                             `json:"serverIdleTimeoutSeconds,omitempty"`
		MaxPayloadBytes          *int                             `json:"maxPayloadBytes,omitempty"`
		PromptCacheMaxEntries    *int                             `json:"promptCacheMaxEntries,omitempty"`
		PromptCacheMaxRatio      *float64                         `json:"promptCacheMaxRatio,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if err := config.UpdateSettingsPatch(req.ApiKey, req.RequireApiKey, req.Password); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// 更新超额使用设置
	if req.AllowOverUsage != nil {
		if err := config.UpdateAllowOverUsage(*req.AllowOverUsage); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		// Rebuild the pool so over-quota accounts are re-included or dropped immediately.
		h.pool.Reload()
	}
	if req.BalanceMode != nil {
		if err := config.UpdateBalanceMode(*req.BalanceMode); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}
	if req.RoutingConcurrency != nil {
		if err := config.UpdateRoutingConcurrencyConfig(*req.RoutingConcurrency); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}

	// Server timeouts — saved immediately but require a container restart to
	// take effect (the HTTP server reads them only at startup).
	if req.ServerReadTimeoutSeconds != nil {
		if err := config.UpdateServerReadTimeout(*req.ServerReadTimeoutSeconds); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}
	if req.ServerIdleTimeoutSeconds != nil {
		if err := config.UpdateServerIdleTimeout(*req.ServerIdleTimeoutSeconds); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}

	// Prompt Cache knobs (ticket #5). MaxPayloadBytes takes effect on the next
	// request (the truncation pass reads it per call); the cache LRU bound and
	// ratio are read by the tracker at construction, so changes to those apply
	// fully only after a restart — the persisted value is authoritative.
	if req.MaxPayloadBytes != nil {
		if err := config.UpdateMaxPayloadBytes(*req.MaxPayloadBytes); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}
	if req.PromptCacheMaxEntries != nil {
		if err := config.UpdatePromptCacheMaxEntries(*req.PromptCacheMaxEntries); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}
	if req.PromptCacheMaxRatio != nil {
		if err := config.UpdatePromptCacheMaxRatio(*req.PromptCacheMaxRatio); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) apiGetStats(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"totalRequests":   atomic.LoadInt64(&h.totalRequests),
		"successRequests": atomic.LoadInt64(&h.successRequests),
		"failedRequests":  atomic.LoadInt64(&h.failedRequests),
		"totalTokens":     atomic.LoadInt64(&h.totalTokens),
		"totalCredits":    h.getCredits(),
		"uptime":          time.Now().Unix() - h.startTime,
	})
}

func (h *Handler) apiResetStats(w http.ResponseWriter, r *http.Request) {
	atomic.StoreInt64(&h.totalRequests, 0)
	atomic.StoreInt64(&h.successRequests, 0)
	atomic.StoreInt64(&h.failedRequests, 0)
	atomic.StoreInt64(&h.totalTokens, 0)
	h.creditsMu.Lock()
	h.totalCredits = 0
	h.creditsMu.Unlock()
	config.UpdateStats(0, 0, 0, 0, 0)
	_ = resetMetricsStore()
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) apiMetricsSummary(w http.ResponseWriter, r *http.Request) {
	rangeDur := parseMetricsRange(r.URL.Query().Get("range"))
	json.NewEncoder(w).Encode(summarizeMetrics(rangeDur))
}

func (h *Handler) apiMetricsTimeseries(w http.ResponseWriter, r *http.Request) {
	rangeDur := parseMetricsRange(r.URL.Query().Get("range"))
	bucketDur := parseMetricsBucket(r.URL.Query().Get("bucket"), rangeDur)
	metric := r.URL.Query().Get("metric")
	json.NewEncoder(w).Encode(buildMetricsTimeseries(rangeDur, bucketDur, metric))
}

func (h *Handler) apiMetricsTop(w http.ResponseWriter, r *http.Request) {
	rangeDur := parseMetricsRange(r.URL.Query().Get("range"))
	groupBy := r.URL.Query().Get("groupBy")
	if groupBy == "" {
		groupBy = "model"
	}
	metric := r.URL.Query().Get("metric")
	if metric == "" {
		metric = "tokens"
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	json.NewEncoder(w).Encode(map[string]interface{}{
		"rangeSeconds": int64(rangeDur.Seconds()),
		"groupBy":      groupBy,
		"metric":       metric,
		"items":        buildMetricsTop(rangeDur, groupBy, metric, limit),
	})
}

func (h *Handler) apiMetricsReset(w http.ResponseWriter, r *http.Request) {
	if err := resetMetricsStore(); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiMetricsLive serves the real-time concurrency dashboard payload:
// concurrency overview (current vs configured limits + cumulative counters),
// per-account concurrency distribution (with email + limit), and the most
// recent request records for the live request stream.
func (h *Handler) apiMetricsLive(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > metricsLiveCapacity {
		limit = 50
	}

	// window selects the sliding-window span for all live metrics. Only 60s and
	// 300s are offered by the UI; anything else falls back to 60s.
	windowSec, _ := strconv.Atoi(r.URL.Query().Get("window"))
	if windowSec != 300 {
		windowSec = 60
	}
	window := time.Duration(windowSec) * time.Second
	now := time.Now()

	rc := config.GetRoutingConcurrencyConfig()
	stats := h.pool.RoutingStatsWindow(window, now)

	// Per-account distribution: windowed request count per account, enriched
	// with email + per-account concurrency limit.
	perAccount := make([]map[string]interface{}, 0)
	if raw, ok := stats["perAccountActive"].(map[string]int); ok {
		for id, active := range raw {
			entry := map[string]interface{}{
				"accountId": id,
				"active":    active,
				"limit":     rc.PerAccountMaxConcurrent,
			}
			if acc := h.pool.GetByID(id); acc != nil {
				entry["email"] = acc.Email
			}
			perAccount = append(perAccount, entry)
		}
	}

	concurrency := map[string]interface{}{
		"enabled":        rc.Enabled,
		"active":         stats["active"],
		"maxConcurrent":  rc.GlobalMaxConcurrent,
		"waiting":        stats["waiting"],
		"queueSize":      rc.GlobalQueueSize,
		"enqueuedTotal":  stats["enqueuedTotal"],
		"processedTotal": stats["processedTotal"],
		"rejectedTotal":  stats["rejectedTotal"],
		"timeoutTotal":   stats["timeoutTotal"],
		"requestTotal":   stats["requestTotal"],
		"rpm":            stats["requestsLastMinute"],
		"window":         windowSec,
	}

	// Sticky (conversation affinity) outcomes. hitRate is over affinity-keyed
	// requests only (hit+miss+divert); a "miss" is the unavoidable first turn of
	// a new conversation, so it is not a failure.
	stickyHit := toUint64(stats["stickyHitTotal"])
	stickyMiss := toUint64(stats["stickyMissTotal"])
	stickyDivert := toUint64(stats["stickyDivertTotal"])
	stickyTotal := stickyHit + stickyMiss + stickyDivert
	hitRate := 0.0
	if stickyTotal > 0 {
		hitRate = float64(int64(float64(stickyHit)/float64(stickyTotal)*1000+0.5)) / 10 // one decimal %
	}
	sticky := map[string]interface{}{
		"enabled":     rc.StickyAccount,
		"hitTotal":    stickyHit,
		"missTotal":   stickyMiss,
		"divertTotal": stickyDivert,
		"total":       stickyTotal,
		"hitRate":     hitRate,
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"concurrency": concurrency,
		"sticky":      sticky,
		"perAccount":  perAccount,
		"recent":      recentLiveRequestsSince(limit, int64(windowSec), now.Unix()),
	})
}

// toUint64 coerces the interface{} values from RoutingStats (which holds uint64)
// to uint64, defaulting to 0.
func toUint64(v interface{}) uint64 {
	if n, ok := v.(uint64); ok {
		return n
	}
	return 0
}

// apiGenerateMachineId 生成新的机器码
func (h *Handler) apiGenerateMachineId(w http.ResponseWriter, r *http.Request) {
	machineId := config.GenerateMachineId()
	json.NewEncoder(w).Encode(map[string]string{"machineId": machineId})
}

// apiGetThinkingConfig 获取 thinking 配置
func (h *Handler) apiGetThinkingConfig(w http.ResponseWriter, r *http.Request) {
	cfg := config.GetThinkingConfig()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"suffix":       cfg.Suffix,
		"openaiFormat": cfg.OpenAIFormat,
		"claudeFormat": cfg.ClaudeFormat,
	})
}

// apiUpdateThinkingConfig 更新 thinking 配置
func (h *Handler) apiUpdateThinkingConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Suffix       string `json:"suffix"`
		OpenAIFormat string `json:"openaiFormat"`
		ClaudeFormat string `json:"claudeFormat"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// 验证格式
	validFormats := map[string]bool{"reasoning_content": true, "thinking": true, "think": true}
	if req.OpenAIFormat != "" && !validFormats[req.OpenAIFormat] {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid openaiFormat, must be: reasoning_content, thinking, or think"})
		return
	}
	if req.ClaudeFormat != "" && !validFormats[req.ClaudeFormat] {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid claudeFormat, must be: reasoning_content, thinking, or think"})
		return
	}

	if err := config.UpdateThinkingConfig(req.Suffix, req.OpenAIFormat, req.ClaudeFormat); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGetEndpointConfig 获取端点配置
func (h *Handler) apiGetEndpointConfig(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"preferredEndpoint": config.GetPreferredEndpoint(),
		"endpointFallback":  config.GetEndpointFallback(),
	})
}

// apiUpdateEndpointConfig 更新端点配置
func (h *Handler) apiUpdateEndpointConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PreferredEndpoint string `json:"preferredEndpoint"`
		EndpointFallback  *bool  `json:"endpointFallback"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	valid := map[string]bool{"auto": true, "kiro": true, "codewhisperer": true, "amazonq": true}
	if !valid[req.PreferredEndpoint] {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid endpoint, must be: auto, kiro, codewhisperer, or amazonq"})
		return
	}

	if err := config.UpdatePreferredEndpoint(req.PreferredEndpoint); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	if req.EndpointFallback != nil {
		config.UpdateEndpointFallback(*req.EndpointFallback)
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGetProxy 获取当前代理配置
func (h *Handler) apiGetProxy(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{
		"proxyURL": config.GetProxyURL(),
	})
}

// apiUpdateProxy 更新代理配置并立即生效
func (h *Handler) apiUpdateProxy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProxyURL string `json:"proxyURL"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// 验证代理 URL 格式（非空时）
	if req.ProxyURL != "" {
		if !strings.HasPrefix(req.ProxyURL, "http://") &&
			!strings.HasPrefix(req.ProxyURL, "https://") &&
			!strings.HasPrefix(req.ProxyURL, "socks5://") &&
			!strings.HasPrefix(req.ProxyURL, "socks5h://") {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "proxyURL must start with http://, https://, socks5://, or socks5h://"})
			return
		}
	}

	if err := config.UpdateProxySettings(req.ProxyURL); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// 立即应用新的代理配置
	applyProxyConfig(req.ProxyURL)

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGetVersion 获取版本信息
func (h *Handler) apiGetVersion(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{
		"version": config.Version,
	})
}

// apiExportAccounts 导出账号凭证
func (h *Handler) apiExportAccounts(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"` // 为空则导出全部
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// 如果 body 为空或解析失败，导出全部
		req.IDs = nil
	}

	accounts := config.GetAccounts()

	// 如果指定了 ID，只导出指定的
	if len(req.IDs) > 0 {
		idSet := make(map[string]bool)
		for _, id := range req.IDs {
			idSet[id] = true
		}
		var filtered []config.Account
		for _, a := range accounts {
			if idSet[a.ID] {
				filtered = append(filtered, a)
			}
		}
		accounts = filtered
	}

	// 构建兼容 Kiro Account Manager 的导出格式
	type ExportCredentials struct {
		AccessToken  string `json:"accessToken"`
		CsrfToken    string `json:"csrfToken"`
		RefreshToken string `json:"refreshToken"`
		ClientID     string `json:"clientId,omitempty"`
		ClientSecret string `json:"clientSecret,omitempty"`
		Region       string `json:"region,omitempty"`
		ExpiresAt    int64  `json:"expiresAt"`
		AuthMethod   string `json:"authMethod,omitempty"`
		Provider     string `json:"provider,omitempty"`
	}

	type ExportSubscription struct {
		Type  string `json:"type"`
		Title string `json:"title,omitempty"`
	}

	type ExportUsage struct {
		Current     float64 `json:"current"`
		Limit       float64 `json:"limit"`
		PercentUsed float64 `json:"percentUsed"`
		LastUpdated int64   `json:"lastUpdated"`
	}

	type ExportAccount struct {
		ID           string             `json:"id"`
		Email        string             `json:"email"`
		Nickname     string             `json:"nickname,omitempty"`
		Idp          string             `json:"idp"`
		UserId       string             `json:"userId,omitempty"`
		MachineId    string             `json:"machineId,omitempty"`
		Credentials  ExportCredentials  `json:"credentials"`
		Subscription ExportSubscription `json:"subscription"`
		Usage        ExportUsage        `json:"usage"`
		Tags         []string           `json:"tags"`
		Status       string             `json:"status"`
		CreatedAt    int64              `json:"createdAt"`
		LastUsedAt   int64              `json:"lastUsedAt"`
	}

	type ExportData struct {
		Version    string          `json:"version"`
		ExportedAt int64           `json:"exportedAt"`
		Accounts   []ExportAccount `json:"accounts"`
		Groups     []interface{}   `json:"groups"`
		Tags       []interface{}   `json:"tags"`
	}

	exportAccounts := make([]ExportAccount, 0, len(accounts))
	for _, a := range accounts {
		// 映射 provider 到 idp
		idp := a.Provider
		if idp == "" {
			if a.AuthMethod == "social" {
				idp = "Google"
			} else {
				idp = "BuilderId"
			}
		}

		// 映射 authMethod
		authMethod := a.AuthMethod
		if authMethod == "idc" {
			authMethod = "IdC"
		}

		// 映射订阅类型
		subType := "Free"
		rawType := strings.ToUpper(a.SubscriptionType)
		if strings.Contains(rawType, "PRO_PLUS") || strings.Contains(rawType, "PROPLUS") {
			subType = "Pro_Plus"
		} else if strings.Contains(rawType, "PRO") {
			subType = "Pro"
		} else if strings.Contains(rawType, "POWER") {
			subType = "Pro_Plus"
		}

		exportAccounts = append(exportAccounts, ExportAccount{
			ID:        a.ID,
			Email:     a.Email,
			Nickname:  a.Nickname,
			Idp:       idp,
			UserId:    a.UserId,
			MachineId: a.MachineId,
			Credentials: ExportCredentials{
				AccessToken:  a.AccessToken,
				CsrfToken:    "",
				RefreshToken: a.RefreshToken,
				ClientID:     a.ClientID,
				ClientSecret: a.ClientSecret,
				Region:       a.Region,
				ExpiresAt:    a.ExpiresAt * 1000, // 转为毫秒时间戳
				AuthMethod:   authMethod,
				Provider:     a.Provider,
			},
			Subscription: ExportSubscription{
				Type:  subType,
				Title: a.SubscriptionTitle,
			},
			Usage: ExportUsage{
				Current:     a.UsageCurrent,
				Limit:       a.UsageLimit,
				PercentUsed: a.UsagePercent,
				LastUpdated: time.Now().UnixMilli(),
			},
			Tags:       []string{},
			Status:     "active",
			CreatedAt:  time.Now().UnixMilli(),
			LastUsedAt: time.Now().UnixMilli(),
		})
	}

	data := ExportData{
		Version:    config.Version,
		ExportedAt: time.Now().UnixMilli(),
		Accounts:   exportAccounts,
		Groups:     []interface{}{},
		Tags:       []interface{}{},
	}

	json.NewEncoder(w).Encode(data)
}
