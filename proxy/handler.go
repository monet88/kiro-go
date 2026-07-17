package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"kiro-go/auth"
	"kiro-go/config"
	"kiro-go/logger"
	"kiro-go/pool"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const tokenRefreshSkewSeconds int64 = 120

// Handler HTTP 处理器
type Handler struct {
	pool *pool.AccountPool
	// 运行时统计 (使用原子操作)
	totalRequests   int64
	successRequests int64
	failedRequests  int64
	totalTokens     int64
	totalCredits    float64 // float64 需要用锁保护
	creditsMu       sync.RWMutex
	startTime       int64
	stopRefresh     chan struct{}
	stopStatsSaver  chan struct{}
	// snapshotSaverDone is closed by the Prompt Cache Snapshot saver goroutine
	// after it performs its final flush on shutdown, letting Shutdown block until
	// the snapshot is safely on disk before the process exits.
	snapshotSaverDone chan struct{}
	// shutdownOnce guards Shutdown so the stop channels are closed exactly once,
	// even if Shutdown is called from multiple signal handlers.
	shutdownOnce sync.Once
	// 模型缓存
	cachedModels    []ModelInfo
	modelsCacheMu   sync.RWMutex
	modelsCacheTime int64
	promptCache     *promptCacheTracker
	// tokenRefreshLocks 为每个账号维护一把独立的刷新锁(accountID → *sync.Mutex),
	// 通过 accountRefreshLock 惰性创建。相比单把全局锁,不同账号的 token 刷新
	// (含 OIDC 网络往返 + 写盘)不再互相阻塞;同账号仍互斥,配合 ensureValidToken
	// 内的 double-check 实现"同账号刷新去重"。
	tokenRefreshLocks sync.Map
	// kamImports 跟踪批量(kam)导入任务的进度,供前端轮询查询。
	kamImports *kamImportManager
	// acquireOverride is a test-only seam that replaces the pool acquire in
	// runWithAccount. nil in production. See acquireRouteAccount.
	acquireOverride func(ctx context.Context, model string, excluded map[string]bool, affinityKey string) (*config.Account, func(), error)
}

// accountRefreshLock 返回该账号专属的 token 刷新锁,惰性创建。
// sync.Map.LoadOrStore 保证并发下同一 accountID 只会对应同一把锁。
func (h *Handler) accountRefreshLock(accountID string) *sync.Mutex {
	if v, ok := h.tokenRefreshLocks.Load(accountID); ok {
		return v.(*sync.Mutex)
	}
	actual, _ := h.tokenRefreshLocks.LoadOrStore(accountID, &sync.Mutex{})
	return actual.(*sync.Mutex)
}

const (
	thinkingSourceUnknown thinkingStreamSource = iota
	thinkingSourceReasoningEvent
	thinkingSourceTagBlock
)

func NewHandler() *Handler {
	// 启动时应用代理配置
	applyProxyConfig(config.GetProxyURL())

	totalReq, successReq, failedReq, totalTokens, totalCredits := config.GetStats()
	h := &Handler{
		pool:              pool.GetPool(),
		totalRequests:     int64(totalReq),
		successRequests:   int64(successReq),
		failedRequests:    int64(failedReq),
		totalTokens:       int64(totalTokens),
		totalCredits:      totalCredits,
		startTime:         time.Now().Unix(),
		stopRefresh:       make(chan struct{}),
		stopStatsSaver:    make(chan struct{}),
		snapshotSaverDone: make(chan struct{}),
		promptCache:       newPromptCacheTracker(defaultPromptCacheTTL, 0, 0),
		kamImports:        newKamImportManager(),
	}
	// Load the Prompt Cache Snapshot so cross-account cache prefixes survive a
	// process restart, then start the periodic atomic flush loop. The saver also
	// performs one final flush when stopStatsSaver is closed by Shutdown, so a
	// graceful stop (SIGINT/SIGTERM) persists the latest cache state; between
	// stops, durability comes from the periodic flush.
	snapshotPath := promptCacheSnapshotPath()
	if err := h.promptCache.LoadSnapshot(snapshotPath); err != nil {
		logger.Warnf("failed to load prompt cache snapshot: %v", err)
	}
	go func() {
		// Signal completion so Shutdown can block until the final flush lands.
		defer close(h.snapshotSaverDone)
		h.promptCache.startSnapshotSaver(snapshotPath, h.stopStatsSaver)
	}()
	// 启动后台刷新
	go h.backgroundRefresh()
	// 启动后台统计保存 (每30秒保存一次)
	go h.backgroundStatsSaver()
	startMetricsBackgroundFlush(h.stopStatsSaver)
	// 清理过期的 stored responses（>30 天）
	go purgeExpiredResponses(responsesDefaultTTL)
	return h
}

// Shutdown stops the Handler's background goroutines and triggers their exit
// flush paths: the stats saver persists a final stats snapshot and the prompt
// cache saver writes a final Prompt Cache Snapshot to disk. It is safe to call
// more than once (idempotent) and safe to call concurrently. Callers should
// invoke it during graceful shutdown (e.g. on SIGINT/SIGTERM) so the latest
// cache state survives a restart without waiting for the next periodic flush.
func (h *Handler) Shutdown() {
	h.shutdownOnce.Do(func() {
		close(h.stopRefresh)
		close(h.stopStatsSaver)
		// Wait for the prompt cache saver to finish its final flush so a caller
		// that exits the process right after Shutdown does not race the disk
		// write. Other savers (stats/metrics) flush synchronously on the same
		// stop signal; only the snapshot saver runs disk IO worth waiting for.
		<-h.snapshotSaverDone
	})
}

// backgroundRefresh 后台定时刷新账户信息
func (h *Handler) backgroundRefresh() {
	ticker := time.NewTicker(30 * time.Minute) // 每 30 分钟刷新一次
	defer ticker.Stop()

	// 启动时延迟 10 秒后执行一次
	time.Sleep(10 * time.Second)
	h.refreshModelsCache()
	h.refreshAllAccounts()

	for {
		select {
		case <-ticker.C:
			h.refreshModelsCache()
			h.refreshAllAccounts()
		case <-h.stopRefresh:
			return
		}
	}
}

// backgroundRefreshConcurrency bounds how many accounts are refreshed in
// parallel during a periodic sweep. Refresh is network-bound (token exchange +
// getUsageLimits per account), so a serial sweep of a large pool (1000+
// accounts) could take longer than the refresh interval itself, starving newly
// imported accounts of their first activation. Bounded parallelism keeps a full
// sweep quick without hammering upstream hard enough to trigger IP-level rate
// limiting.
const backgroundRefreshConcurrency = 10

// refreshAllAccounts 刷新所有账户信息（受控并发）
func (h *Handler) refreshAllAccounts() {
	accounts := config.GetAccounts()

	var wg sync.WaitGroup
	sem := make(chan struct{}, backgroundRefreshConcurrency)
	for i := range accounts {
		account := &accounts[i]
		// Skip only accounts that can never be refreshed: disabled, or holding
		// no credential at all. An empty AccessToken alone is NOT a skip reason —
		// a bulk-imported account may arrive with only a RefreshToken and must be
		// activated by exchanging it here (the previous guard skipped these
		// forever, leaving them permanently unusable).
		if !account.Enabled {
			continue
		}
		if account.AccessToken == "" && account.RefreshToken == "" {
			continue
		}

		wg.Add(1)
		sem <- struct{}{}
		go func(account *config.Account) {
			defer wg.Done()
			defer func() { <-sem }()
			h.refreshOneAccount(account)
		}(account)
	}
	wg.Wait()
	h.pool.Reload()
}

// refreshOneAccount refreshes a single account's token (when due or missing) and
// then its usage/subscription info. Safe to run concurrently with other accounts:
// it only mutates its own *account (a distinct slice element) and the config/pool
// helpers it calls lock internally.
func (h *Handler) refreshOneAccount(account *config.Account) {
	// Refresh the token when it is due to expire OR entirely missing. The
	// missing case activates imported accounts that arrived with only a refresh
	// token.
	needsToken := !account.IsApiKeyCredential() &&
		(account.AccessToken == "" ||
			account.ExpiresAt > 0 && time.Now().Unix() > account.ExpiresAt-tokenRefreshSkewSeconds)
	if needsToken {
		newAccessToken, newRefreshToken, newExpiresAt, profileArn, err := auth.RefreshToken(account)
		if err != nil {
			logger.Warnf("[BackgroundRefresh] Token refresh failed for %s: %v", account.Email, err)
			h.handleAccountFailure(account, err)
			return
		}
		account.AccessToken = newAccessToken
		if newRefreshToken != "" {
			account.RefreshToken = newRefreshToken
		}
		account.ExpiresAt = newExpiresAt
		config.UpdateAccountToken(account.ID, newAccessToken, newRefreshToken, newExpiresAt)
		h.pool.UpdateToken(account.ID, newAccessToken, newRefreshToken, newExpiresAt)
		if profileArn != "" {
			account.ProfileArn = profileArn
			config.UpdateAccountProfileArn(account.ID, profileArn)
		}
	}

	// 刷新账户信息
	info, err := RefreshAccountInfo(account)
	if err != nil {
		logger.Warnf("[BackgroundRefresh] Failed to refresh %s: %v", account.Email, err)
		return
	}

	config.UpdateAccountInfo(account.ID, *info)
	h.refreshAccountOverageIfExceeded(account, info)
	logger.Infof("[BackgroundRefresh] Refreshed %s: %s %.1f/%.1f", account.Email, info.SubscriptionType, info.UsageCurrent, info.UsageLimit)
}

func (h *Handler) refreshAccountOverageIfExceeded(account *config.Account, info *config.AccountInfo) {
	if account == nil || info == nil {
		return
	}
	// Usage is back within (or never exceeded) the subscription quota. Overage
	// points are zero by definition in that case, so clear any stale value left
	// over from a previous billing period instead of letting it linger (the bug
	// where a reset quota still showed "206 / 10,000" overage points). No extra
	// upstream call is needed — within-quota implies zero overage. The cap/rate
	// billing config is preserved.
	if info.UsageLimit <= 0 || info.UsageCurrent <= info.UsageLimit {
		if clearErr := config.ClearAccountCurrentOverages(account.ID, time.Now().Unix()); clearErr != nil {
			logger.Warnf("[Overage] failed to clear stale overage points for %s: %v", account.Email, clearErr)
		}
		return
	}
	snap, err := FetchOverageStatus(account)
	if err != nil {
		logger.Warnf("[Overage] failed to refresh overage status after usage exceeded for %s: %v", account.Email, err)
		return
	}
	if overagePoints := info.UsageCurrent - info.UsageLimit; overagePoints > snap.CurrentOverages {
		snap.CurrentOverages = overagePoints
	}
	if snap.OverageCap <= 0 {
		snap.OverageCap = 10000
	}
	if persistErr := PersistOverageSnapshot(account.ID, snap); persistErr != nil {
		logger.Warnf("[Overage] failed to persist overage status after usage exceeded for %s: %v", account.Email, persistErr)
	}
}

// validateApiKey 验证 API Key（Bool 包装，旧签名仍被部分调用方使用）
func (h *Handler) validateApiKey(r *http.Request) bool {
	_, err := h.authenticate(r)
	return err == nil
}

// authenticateForClaude runs authenticate and writes a Claude-style error on failure.
// Returns the request with the matched API key injected into context, or nil if auth failed.
func (h *Handler) authenticateForClaude(w http.ResponseWriter, r *http.Request) *http.Request {
	entry, err := h.authenticate(r)
	if err != nil {
		ae, _ := err.(*authError)
		if ae == nil {
			ae = newAuthError(http.StatusUnauthorized, "authentication_error", err.Error())
		}
		h.sendClaudeError(w, ae.status, ae.code, ae.message)
		return nil
	}
	return withApiKeyContext(r, entry)
}

// authenticateForOpenAI runs authenticate and writes an OpenAI-style error on failure.
func (h *Handler) authenticateForOpenAI(w http.ResponseWriter, r *http.Request) *http.Request {
	entry, err := h.authenticate(r)
	if err != nil {
		ae, _ := err.(*authError)
		if ae == nil {
			ae = newAuthError(http.StatusUnauthorized, "authentication_error", err.Error())
		}
		h.sendOpenAIError(w, ae.status, ae.code, ae.message)
		return nil
	}
	return withApiKeyContext(r, entry)
}

// ServeHTTP 路由分发
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// Debug-level request trace for fine-grained visibility
	logger.Debugf("[HTTP] %s %s from %s", r.Method, path, r.RemoteAddr)

	// CORS - 完整的头部支持
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Api-Key, anthropic-version, anthropic-beta, x-api-key, x-stainless-os, x-stainless-lang, x-stainless-package-version, x-stainless-runtime, x-stainless-runtime-version, x-stainless-arch")
	w.Header().Set("Access-Control-Expose-Headers", "x-request-id, x-ratelimit-limit-requests, x-ratelimit-limit-tokens, x-ratelimit-remaining-requests, x-ratelimit-remaining-tokens, x-ratelimit-reset-requests, x-ratelimit-reset-tokens")

	if r.Method == "OPTIONS" {
		w.WriteHeader(204)
		return
	}

	// 路由
	switch {
	// API 端点（需要验证 API Key）
	case path == "/v1/messages" || path == "/messages" || path == "/anthropic/v1/messages":
		ar := h.authenticateForClaude(w, r)
		if ar == nil {
			return
		}
		h.handleClaudeMessages(w, limitRequestBody(w, ar))
	case path == "/v1/messages/count_tokens" || path == "/messages/count_tokens":
		ar := h.authenticateForClaude(w, r)
		if ar == nil {
			return
		}
		h.handleCountTokens(w, limitRequestBody(w, ar))
	case path == "/v1/chat/completions" || path == "/chat/completions":
		ar := h.authenticateForOpenAI(w, r)
		if ar == nil {
			return
		}
		h.handleOpenAIChat(w, limitRequestBody(w, ar))
	case path == "/v1/responses" || path == "/responses":
		ar := h.authenticateForOpenAI(w, r)
		if ar == nil {
			return
		}
		h.handleOpenAIResponses(w, limitRequestBody(w, ar))
	case path == "/v1/models" || path == "/models":
		h.handleModels(w, r)
	case path == "/api/event_logging/batch":
		// Claude Code 遥测端点 - 直接返回 200 OK
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Write([]byte(`{"status":"ok"}`))

	// 管理端点
	case path == "/admin" || path == "/admin/":
		h.serveAdminPage(w, r)
	case strings.HasPrefix(path, "/admin/api/"):
		h.handleAdminAPI(w, r)
	case strings.HasPrefix(path, "/admin/"):
		h.serveStaticFile(w, r)

	// 健康检查
	case path == "/health" || path == "/":
		h.handleHealth(w, r)

	// 统计端点（需要 API Key 鉴权）
	case path == "/v1/stats":
		if !h.validateApiKey(r) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(401)
			json.NewEncoder(w).Encode(map[string]string{"error": "Invalid or missing API key"})
			return
		}
		h.handleStats(w, r)

	default:
		http.Error(w, "Not Found", 404)
	}
}

// limitRequestBody wraps the request body with http.MaxBytesReader so an
// oversized (or malicious) inbound request cannot balloon memory: the public
// inference handlers buffer the whole body with io.ReadAll, so without a cap a
// single huge POST would be fully read into RAM (and, multiplied by concurrent
// requests, exhaust it). Reading past the cap makes the subsequent ReadAll fail
// with *http.MaxBytesError, which the handlers translate into HTTP 413.
// The cap is configurable via MaxRequestBodyMB (see config.GetMaxRequestBodyBytes).
func limitRequestBody(w http.ResponseWriter, r *http.Request) *http.Request {
	if r == nil || r.Body == nil {
		return r
	}
	r.Body = http.MaxBytesReader(w, r.Body, config.GetMaxRequestBodyBytes())
	return r
}

// maxBytesExceeded reports whether err stems from the request body exceeding
// the limit installed by limitRequestBody, so handlers can return 413 instead
// of a generic 400.
func maxBytesExceeded(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}

// handleHealth 健康检查（不暴露统计数据）
func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "ok",
		"version": config.Version,
		"uptime":  time.Now().Unix() - h.startTime,
	})
}

// handleStats 统计数据（需要 API Key 鉴权）
func (h *Handler) handleStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":          "ok",
		"version":         config.Version,
		"accounts":        h.pool.Count(),
		"available":       h.pool.AvailableCount(),
		"totalRequests":   atomic.LoadInt64(&h.totalRequests),
		"successRequests": atomic.LoadInt64(&h.successRequests),
		"failedRequests":  atomic.LoadInt64(&h.failedRequests),
		"totalTokens":     atomic.LoadInt64(&h.totalTokens),
		"totalCredits":    h.getCredits(),
		"uptime":          time.Now().Unix() - h.startTime,
	})
}

// backgroundStatsSaver 后台定时保存统计数据
func (h *Handler) backgroundStatsSaver() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			h.saveStats()
		case <-h.stopStatsSaver:
			h.saveStats() // 退出前保存一次
			return
		}
	}
}

// saveStats 保存统计到配置文件
func (h *Handler) saveStats() {
	config.UpdateStats(
		int(atomic.LoadInt64(&h.totalRequests)),
		int(atomic.LoadInt64(&h.successRequests)),
		int(atomic.LoadInt64(&h.failedRequests)),
		int(atomic.LoadInt64(&h.totalTokens)),
		h.getCredits(),
	)
}

// getCredits 线程安全获取 credits
func (h *Handler) getCredits() float64 {
	h.creditsMu.RLock()
	defer h.creditsMu.RUnlock()
	return h.totalCredits
}

// addCredits 线程安全增加 credits
func (h *Handler) addCredits(credits float64) {
	h.creditsMu.Lock()
	h.totalCredits += credits
	h.creditsMu.Unlock()
}

// 统计记录 (使用原子操作)
func (h *Handler) recordSuccess(inputTokens, outputTokens int, credits float64) {
	atomic.AddInt64(&h.totalRequests, 1)
	atomic.AddInt64(&h.successRequests, 1)
	atomic.AddInt64(&h.totalTokens, int64(inputTokens+outputTokens))
	h.addCredits(credits)
}

// recordSuccessForApiKey is recordSuccess + per-API-key usage attribution.
// When apiKeyID is empty (legacy single-key path or unauthenticated path), only the
// global counters are updated. Persistence errors are logged but do not propagate.
func (h *Handler) recordSuccessForApiKey(apiKeyID string, inputTokens, outputTokens int, credits float64) {
	h.recordSuccess(inputTokens, outputTokens, credits)
	if apiKeyID == "" {
		return
	}
	if err := config.RecordApiKeyUsage(apiKeyID, int64(inputTokens+outputTokens), credits); err != nil {
		logger.Warnf("[ApiKey] failed to record usage for key %s: %v", apiKeyID, err)
	}
}

func (h *Handler) recordFailure() {
	atomic.AddInt64(&h.totalRequests, 1)
	atomic.AddInt64(&h.failedRequests, 1)
}

// ==================== 管理 API ====================

// ==================== 静态文件服务 ====================

func setAdminNoCacheHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
}

func (h *Handler) serveAdminPage(w http.ResponseWriter, r *http.Request) {
	setAdminNoCacheHeaders(w)
	http.ServeFile(w, r, "web/index.html")
}

func (h *Handler) serveStaticFile(w http.ResponseWriter, r *http.Request) {
	setAdminNoCacheHeaders(w)
	path := strings.TrimPrefix(r.URL.Path, "/admin/")
	http.ServeFile(w, r, "web/"+path)
}

// applyProxyConfig 将代理配置应用到所有出站 HTTP 客户端（Kiro API + auth 模块）
func applyProxyConfig(proxyURL string) {
	InitKiroHttpClient(proxyURL)
	auth.InitHttpClient(proxyURL)
}

func clampInt(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
