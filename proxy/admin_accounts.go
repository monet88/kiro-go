package proxy

import (
	"encoding/json"
	"kiro-go/auth"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"strings"
	"time"
)

func isAccountOverageEffective(account config.Account) bool {
	return strings.EqualFold(account.OverageStatus, "ENABLED") || (account.UsageLimit > 0 && account.UsageCurrent > account.UsageLimit)
}

func accountForAdminResponse(account config.Account) config.Account {
	config.NormalizeApiKeyCredential(&account)
	return account
}

func (h *Handler) apiGet429Probes(w http.ResponseWriter, r *http.Request) {
	logs := getKiro429ProbeLogs()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ttlSeconds": int64(kiro429ProbeTTL / time.Second),
		"count":      len(logs),
		"items":      logs,
	})
}

func (h *Handler) apiClear429Probes(w http.ResponseWriter, r *http.Request) {
	count := clearKiro429ProbeLogs()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"cleared": count,
	})
}

func (h *Handler) apiGetUpstreamErrorProbes(w http.ResponseWriter, r *http.Request) {
	logs := getUpstreamErrorProbeLogs()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ttlSeconds": int64(upstreamErrorProbeTTL / time.Second),
		"count":      len(logs),
		"items":      logs,
	})
}

func (h *Handler) apiClearUpstreamErrorProbes(w http.ResponseWriter, r *http.Request) {
	count := clearUpstreamErrorProbeLogs()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"cleared": count,
	})
}

func (h *Handler) apiGetAccounts(w http.ResponseWriter, r *http.Request) {
	accounts := config.GetAccounts()
	poolAccounts := h.pool.GetAllAccounts()
	healthSnapshots := h.pool.GetHealthSnapshots()

	// 合并运行时统计
	statsMap := make(map[string]config.Account)
	for _, a := range poolAccounts {
		statsMap[a.ID] = a
	}

	// 隐藏敏感信息
	now := time.Now().Unix()
	result := make([]map[string]interface{}, len(accounts))
	for i, a := range accounts {
		// 获取运行时统计：优先使用内存池中的实时值；
		// 若账号因 429 冷却/超额被移出池子（reloadLocked 会跳过被挂起的账号），
		// 则回退到 config 中持久化的统计值，避免请求数等指标在冷却期间显示为 0。
		stats := a
		if poolStat, ok := statsMap[a.ID]; ok {
			stats = poolStat
		}
		health := healthSnapshots[a.ID]
		coolingUntil := health.CoolingUntil
		recent429Count, probe429Rate := getKiro429ProbeRate(a.ID)
		recent429Rate := health.Rate429
		if probe429Rate > recent429Rate {
			recent429Rate = probe429Rate
		}
		if a.BanReason == config.AutoQuarantineSuspicious429Reason() && a.BanTime > 0 {
			until := a.BanTime + int64(time.Hour/time.Second)
			if until > now {
				coolingUntil = until
			}
		}
		isApiKeyAccount := a.IsApiKeyCredential()
		displayAccount := accountForAdminResponse(a)

		result[i] = map[string]interface{}{
			"id":                a.ID,
			"email":             a.Email,
			"userId":            a.UserId,
			"nickname":          a.Nickname,
			"authMethod":        a.AuthMethod,
			"isApiKeyAccount":   isApiKeyAccount,
			"provider":          a.Provider,
			"region":            a.Region,
			"enabled":           a.Enabled,
			"banStatus":         a.BanStatus,
			"banReason":         a.BanReason,
			"banTime":           a.BanTime,
			"expiresAt":         displayAccount.ExpiresAt,
			"hasToken":          a.AccessToken != "",
			"hasRefreshToken":   displayAccount.RefreshToken != "",
			"machineId":         a.MachineId,
			"weight":            a.Weight,
			"overageStatus":     displayAccount.OverageStatus,
			"overageEffective":  isAccountOverageEffective(displayAccount),
			"overageCapability": displayAccount.OverageCapability,
			"overageCap":        displayAccount.OverageCap,
			"overageRate":       displayAccount.OverageRate,
			"currentOverages":   displayAccount.CurrentOverages,
			"overageCheckedAt":  displayAccount.OverageCheckedAt,
			"proxyURL":          a.ProxyURL,
			"subscriptionType":  a.SubscriptionType,
			"subscriptionTitle": a.SubscriptionTitle,
			"daysRemaining":     a.DaysRemaining,
			"usageCurrent":      displayAccount.UsageCurrent,
			"usageLimit":        displayAccount.UsageLimit,
			"usagePercent":      displayAccount.UsagePercent,
			"nextResetDate":     displayAccount.NextResetDate,
			"lastRefresh":       displayAccount.LastRefresh,
			"trialUsageCurrent": displayAccount.TrialUsageCurrent,
			"trialUsageLimit":   displayAccount.TrialUsageLimit,
			"trialUsagePercent": displayAccount.TrialUsagePercent,
			"trialStatus":       displayAccount.TrialStatus,
			"trialExpiresAt":    displayAccount.TrialExpiresAt,
			"requestCount":      stats.RequestCount,
			"errorCount":        stats.ErrorCount,
			"totalTokens":       stats.TotalTokens,
			"totalCredits":      stats.TotalCredits,
			"lastUsed":          stats.LastUsed,
			"healthScore":       health.HealthScore,
			"recent429Rate":     recent429Rate,
			"recent429Count":    recent429Count + health.QuotaErrors,
			"modeBucket":        health.ModeBucket,
			"canRoute":          health.CanRoute,
			"coolingUntil":      coolingUntil,
			"lastErrorAt":       health.LastErrorAt,
		}
	}
	json.NewEncoder(w).Encode(result)
}

func (h *Handler) apiAddAccount(w http.ResponseWriter, r *http.Request) {
	var account config.Account
	if err := json.NewDecoder(r.Body).Decode(&account); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if account.ID == "" {
		account.ID = auth.GenerateAccountID()
	}
	if account.Region == "" {
		account.Region = "us-east-1"
	}
	// Match other add-account auth paths so request tracking headers are present
	// immediately (API-key Accounts skip OAuth login which would mint a machine id).
	if account.MachineId == "" {
		account.MachineId = config.GenerateMachineId()
	}
	// Enforce the API-key Account invariants on the local copy too (config.AddAccount
	// normalizes what it persists, but the copy below drives the model-fetch guard and
	// response): AuthMethod→api_key and AccessToken mirrored from KiroApiKey (ADR-0002).
	config.NormalizeApiKeyCredential(&account)
	if err := config.ValidateApiKeyCredential(account); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	// Static ksk_ keys are regional on management/runtime.kiro.dev. Probe the
	// given region first, then common regions, so a wrong default (us-east-1)
	// still lands on the serving region (often eu-central-1). Soft on failure:
	// persist with the operator-supplied region so offline/dev keys still add.
	if account.IsApiKeyCredential() {
		if info, region, err := probeApiKeyServingRegion(&account); err == nil {
			applyApiKeyProbeResult(&account, info, region)
			logger.Infof("[Admin] API-key Account validated in region %s email=%s", region, account.Email)
		} else {
			logger.Warnf("[Admin] API-key Account region probe failed (saving with region=%s): %v", account.Region, err)
		}
	}

	if err := config.AddAccount(account); err != nil {
		logger.Warnf("[Admin] add account failed: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	// 新账号若已启用且有 token，立即拉取并缓存模型列表
	if account.Enabled && account.AccessToken != "" {
		go func(acc config.Account) {
			if err := h.fetchAndCacheAccountModels(&acc); err != nil {
				logger.Warnf("[ModelsCache] Auto-refresh failed for new account %s: %v", acc.Email, err)
			}
		}(account)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "id": account.ID})
}

func (h *Handler) apiDeleteAccount(w http.ResponseWriter, r *http.Request, id string) {
	if err := config.DeleteAccount(id); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) apiUpdateAccount(w http.ResponseWriter, r *http.Request, id string) {
	var updates map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// 获取现有账号
	accounts := config.GetAccounts()
	var existing *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			existing = &accounts[i]
			break
		}
	}
	if existing == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	// 只更新传入的字段
	oldEnabled := existing.Enabled
	if v, ok := updates["enabled"].(bool); ok {
		existing.Enabled = v
		// Keep BanStatus in sync with an explicit operator toggle. A manual
		// disable must be marked DISABLED (not left as SUSPENDED), otherwise the
		// auto-429 restore sweep re-enables the account once its quarantine
		// window elapses — silently undoing the operator's action.
		if v {
			existing.BanStatus = "ACTIVE"
			existing.BanReason = ""
			existing.BanTime = 0
		} else {
			existing.BanStatus = "DISABLED"
			existing.BanReason = config.OperatorDisabledReason()
			existing.BanTime = time.Now().Unix()
		}
	}
	if v, ok := updates["nickname"].(string); ok {
		existing.Nickname = v
	}
	if v, ok := updates["machineId"].(string); ok {
		existing.MachineId = v
	}
	if v, ok := updates["weight"].(float64); ok {
		existing.Weight = int(v)
	}
	if v, ok := updates["proxyURL"].(string); ok {
		existing.ProxyURL = v
	}

	if err := config.UpdateAccount(id, *existing); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	// 账号从禁用→启用时，自动拉取并缓存模型列表
	if !oldEnabled && existing.Enabled && existing.AccessToken != "" {
		go func(acc config.Account) {
			if err := h.fetchAndCacheAccountModels(&acc); err != nil {
				logger.Warnf("[ModelsCache] Auto-refresh failed for re-enabled account %s: %v", acc.Email, err)
			}
		}(*existing)
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGetAccountOverage 拉取并返回单个账号的上游 Overages 状态。
// 同步把结果写回 config.json 缓存，确保 UI 与持久化一致。
func (h *Handler) apiGetAccountOverage(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}
	if account.IsApiKeyCredential() {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Overages are not supported for an API-key Account"})
		return
	}

	snap, err := FetchOverageStatus(account)
	if err != nil {
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if persistErr := PersistOverageSnapshot(id, snap); persistErr != nil {
		logger.Warnf("[Overage] persist GET overage failed for %s: %v", account.Email, persistErr)
	}
	h.pool.Reload()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":           true,
		"overageStatus":     snap.Status,
		"overageCapability": snap.Capability,
		"subscriptionTitle": snap.SubscriptionTitle,
		"overageCap":        snap.OverageCap,
		"overageRate":       snap.OverageRate,
		"currentOverages":   snap.CurrentOverages,
		"overageCheckedAt":  snap.CheckedAt,
	})
}

// apiSetAccountOverage 翻转单个账号的上游 Overages 开关，并刷新缓存。
// Body: {"enabled": true|false}
func (h *Handler) apiSetAccountOverage(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}
	if account.IsApiKeyCredential() {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Overages cannot be toggled for an API-key Account"})
		return
	}

	snap, err := SetOverageStatus(account, body.Enabled)
	if err != nil {
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if persistErr := PersistOverageSnapshot(id, snap); persistErr != nil {
		logger.Warnf("[Overage] persist SET overage failed for %s: %v", account.Email, persistErr)
	}
	h.pool.Reload()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":           true,
		"overageStatus":     snap.Status,
		"overageCapability": snap.Capability,
		"subscriptionTitle": snap.SubscriptionTitle,
		"overageCap":        snap.OverageCap,
		"overageRate":       snap.OverageRate,
		"currentOverages":   snap.CurrentOverages,
		"overageCheckedAt":  snap.CheckedAt,
	})
}

func (h *Handler) refreshAccountTokenIfNeeded(account *config.Account) error {
	if account == nil || account.IsApiKeyCredential() || account.RefreshToken == "" {
		return nil
	}
	newAccessToken, newRefreshToken, newExpiresAt, profileArn, err := auth.RefreshToken(account)
	if err != nil {
		return err
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
	return nil
}

// apiBatchAccounts 批量操作账号（启用/禁用/刷新）
func (h *Handler) apiBatchAccounts(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs    []string `json:"ids"`
		Action string   `json:"action"` // "enable", "disable", "refresh"
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if len(req.IDs) == 0 {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "No account IDs provided"})
		return
	}

	switch req.Action {
	case "enable", "disable":
		enabled := req.Action == "enable"
		accounts := config.GetAccounts()
		idSet := make(map[string]bool)
		for _, id := range req.IDs {
			idSet[id] = true
		}
		var toRefreshModels []config.Account
		successCount := 0
		failCount := 0
		matchedCount := 0
		for _, a := range accounts {
			if idSet[a.ID] {
				matchedCount++
				shouldRefreshModels := enabled && !a.Enabled && a.AccessToken != ""
				a.Enabled = enabled
				if enabled && a.BanStatus != "" && a.BanStatus != "ACTIVE" {
					a.BanStatus = "ACTIVE"
					a.BanReason = ""
					a.BanTime = 0
				} else if !enabled {
					// Mark a manual disable as DISABLED so the auto-429 restore
					// sweep can't silently re-enable it later.
					a.BanStatus = "DISABLED"
					a.BanReason = config.OperatorDisabledReason()
					a.BanTime = time.Now().Unix()
				}
				if err := config.UpdateAccount(a.ID, a); err != nil {
					failCount++
					continue
				}
				successCount++
				if shouldRefreshModels {
					toRefreshModels = append(toRefreshModels, a)
				}
			}
		}
		failCount += len(idSet) - matchedCount
		h.pool.Reload()
		// 为本次新启用的账号异步拉取模型缓存
		for _, acc := range toRefreshModels {
			go func(a config.Account) {
				a.Enabled = true
				if err := h.fetchAndCacheAccountModels(&a); err != nil {
					logger.Warnf("[ModelsCache] Auto-refresh failed for batch-enabled account %s: %v", a.Email, err)
				}
			}(acc)
		}
		if failCount > 0 {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"count":   successCount,
				"failed":  failCount,
			})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "count": successCount})

	case "refresh":
		successCount := 0
		failCount := 0
		for _, id := range req.IDs {
			accounts := config.GetAccounts()
			var account *config.Account
			for i := range accounts {
				if accounts[i].ID == id {
					account = &accounts[i]
					break
				}
			}
			if account == nil {
				failCount++
				continue
			}
			// Refresh OAuth tokens only. API-key Accounts may carry stale
			// RefreshToken data from an imported record, but never use it.
			_ = h.refreshAccountTokenIfNeeded(account)
			// 刷新账户信息
			info, err := RefreshAccountInfo(account)
			if err != nil {
				failCount++
				continue
			}
			config.UpdateAccountInfo(id, *info)
			h.refreshAccountOverageIfExceeded(account, info)
			successCount++
		}
		h.pool.Reload()
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":   true,
			"refreshed": successCount,
			"failed":    failCount,
		})

	default:
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid action: " + req.Action})
	}
}

// apiTestAccount tests a specific account by sending a real model request through its proxy.
func (h *Handler) apiTestAccount(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	if err := h.ensureValidToken(account); err != nil {
		h.handleAccountTestFailure(account, err)
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": "Token refresh failed: " + err.Error()})
		return
	}

	// Parse test model from request body (optional)
	var req struct {
		Model string `json:"model"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	req.Model = strings.TrimSpace(req.Model)

	// Build a minimal chat payload
	thinkingCfg := config.GetThinkingConfig()
	actualModel, thinking := ParseModelAndThinking(req.Model, thinkingCfg.Suffix)

	openaiReq := &OpenAIRequest{
		Model:     actualModel,
		Messages:  []OpenAIMessage{{Role: "user", Content: "say ok"}},
		MaxTokens: 5,
		Stream:    false,
	}
	kiroPayload := OpenAIToKiro(openaiReq, thinking)

	var content string
	callback := &KiroStreamCallback{
		OnText:         func(text string, isThinking bool) { content += text },
		OnToolUse:      func(tu KiroToolUse) {},
		OnComplete:     func(inTok, outTok int) {},
		OnError:        func(err error) {},
		OnCredits:      func(c float64) {},
		OnContextUsage: func(pct float64) {},
	}

	err := CallKiroAPI(r.Context(), account, kiroPayload, callback)
	if err != nil {
		h.handleAccountTestFailure(account, err)
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.RestoreAccount(id)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"reply":   content,
		"model":   req.Model,
		"enabled": true,
	})
}

// apiRefreshAccount 刷新账户信息（使用量、订阅等）
func (h *Handler) apiRefreshAccount(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}

	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	// 检查 token 是否快过期，先刷新
	if account.ExpiresAt > 0 && time.Now().Unix() > account.ExpiresAt-tokenRefreshSkewSeconds {
		if err := h.refreshAccountTokenIfNeeded(account); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": "Token refresh failed: " + err.Error()})
			return
		}
	}

	// 获取账户信息
	info, err := RefreshAccountInfo(account)
	if err != nil {
		// 检查是否为封禁相关错误
		errMsg := err.Error()
		if strings.Contains(errMsg, "TEMPORARILY_SUSPENDED") || strings.Contains(errMsg, "Account suspended") {
			// 封禁状态已在 RefreshAccountInfo 中处理，静默返回成功
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true,
				"message": "Account status updated",
			})
			return
		}

		// 如果是 403/401，说明 token 无效，尝试刷新后重试
		if strings.Contains(errMsg, "403") || strings.Contains(errMsg, "401") || strings.Contains(errMsg, "invalid") || strings.Contains(errMsg, "expired") {
			if refreshErr := h.refreshAccountTokenIfNeeded(account); refreshErr == nil {
				// 重试
				info, err = RefreshAccountInfo(account)
				if err != nil {
					// 重试后仍然失败，检查是否为封禁状态
					if strings.Contains(err.Error(), "TEMPORARILY_SUSPENDED") || strings.Contains(err.Error(), "Account suspended") {
						json.NewEncoder(w).Encode(map[string]interface{}{
							"success": true,
							"message": "Account status updated",
						})
						return
					}
				}
			}
		}

		// 其他错误才显示错误信息
		if err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}

	// 保存到配置
	if err := config.UpdateAccountInfo(id, *info); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	h.refreshAccountOverageIfExceeded(account, info)
	h.pool.Reload()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"info":    info,
	})
}

// apiGetAccountFull 获取单个账号的完整信息（包含敏感字段）
func (h *Handler) apiGetAccountFull(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	poolAccounts := h.pool.GetAllAccounts()
	healthSnapshots := h.pool.GetHealthSnapshots()

	// 查找指定账号
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}

	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	// 获取运行时统计：优先内存池实时值，否则回退到 config 持久值
	// （429 冷却/超额账号会被移出池子，此时内存池查不到）。
	stats := *account
	for _, a := range poolAccounts {
		if a.ID == id {
			stats = a
			break
		}
	}

	health := healthSnapshots[id]
	coolingUntil := health.CoolingUntil
	recent429Count, probe429Rate := getKiro429ProbeRate(account.ID)
	recent429Rate := health.Rate429
	if probe429Rate > recent429Rate {
		recent429Rate = probe429Rate
	}
	if account.BanReason == config.AutoQuarantineSuspicious429Reason() && account.BanTime > 0 {
		until := account.BanTime + int64(time.Hour/time.Second)
		if until > time.Now().Unix() {
			coolingUntil = until
		}
	}

	// API-key Accounts hold a static Kiro API Key (ksk_…) mirrored into
	// AccessToken. Never return the raw secret even from the "full" detail
	// endpoint (ADR-0002): mask it and surface the masked value under kiroApiKey
	// so the UI can show the credential kind without leaking it.
	displayAccount := accountForAdminResponse(*account)
	accessTokenField := displayAccount.AccessToken
	refreshTokenField := displayAccount.RefreshToken
	isApiKey := account.IsApiKeyCredential()
	if isApiKey {
		accessTokenField = config.MaskKiroApiKey(account.AccessToken)
		refreshTokenField = ""
	}

	// 返回完整账号信息（包含敏感字段）
	result := map[string]interface{}{
		"id":                account.ID,
		"email":             account.Email,
		"userId":            account.UserId,
		"nickname":          account.Nickname,
		"accessToken":       accessTokenField,
		"refreshToken":      refreshTokenField,
		"clientId":          displayAccount.ClientID,
		"clientSecret":      displayAccount.ClientSecret,
		"authMethod":        account.AuthMethod,
		"isApiKeyAccount":   isApiKey,
		"kiroApiKey":        config.MaskKiroApiKey(account.KiroApiKey),
		"provider":          account.Provider,
		"region":            account.Region,
		"expiresAt":         displayAccount.ExpiresAt,
		"machineId":         account.MachineId,
		"weight":            account.Weight,
		"overageStatus":     displayAccount.OverageStatus,
		"overageEffective":  isAccountOverageEffective(displayAccount),
		"overageCapability": displayAccount.OverageCapability,
		"overageCap":        displayAccount.OverageCap,
		"overageRate":       displayAccount.OverageRate,
		"currentOverages":   displayAccount.CurrentOverages,
		"overageCheckedAt":  displayAccount.OverageCheckedAt,
		"proxyURL":          account.ProxyURL,
		"enabled":           account.Enabled,
		"banStatus":         account.BanStatus,
		"banReason":         account.BanReason,
		"banTime":           account.BanTime,
		"subscriptionType":  account.SubscriptionType,
		"subscriptionTitle": account.SubscriptionTitle,
		"daysRemaining":     account.DaysRemaining,
		"usageCurrent":      displayAccount.UsageCurrent,
		"usageLimit":        displayAccount.UsageLimit,
		"usagePercent":      displayAccount.UsagePercent,
		"nextResetDate":     displayAccount.NextResetDate,
		"lastRefresh":       displayAccount.LastRefresh,
		"trialUsageCurrent": displayAccount.TrialUsageCurrent,
		"trialUsageLimit":   displayAccount.TrialUsageLimit,
		"trialUsagePercent": displayAccount.TrialUsagePercent,
		"trialStatus":       displayAccount.TrialStatus,
		"trialExpiresAt":    displayAccount.TrialExpiresAt,
		"requestCount":      stats.RequestCount,
		"errorCount":        stats.ErrorCount,
		"totalTokens":       stats.TotalTokens,
		"totalCredits":      stats.TotalCredits,
		"lastUsed":          stats.LastUsed,
		"healthScore":       health.HealthScore,
		"recent429Rate":     recent429Rate,
		"recent429Count":    recent429Count + health.QuotaErrors,
		"modeBucket":        health.ModeBucket,
		"canRoute":          health.CanRoute,
		"coolingUntil":      coolingUntil,
		"lastErrorAt":       health.LastErrorAt,
	}

	json.NewEncoder(w).Encode(result)
}
