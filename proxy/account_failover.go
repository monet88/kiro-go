package proxy

import (
	"kiro-go/auth"
	"kiro-go/config"
	"kiro-go/logger"
	"math/rand"
	"strings"
	"time"
)

// getAccountRetryAttempts returns the max number of accounts the proxy tries
// before giving up. Reads from config (hot-reloadable via admin API) with a
// fallback of 4.
func getAccountRetryAttempts() int {
	rc := config.GetRoutingConcurrencyConfig()
	if rc.AccountRetryAttempts > 0 {
		return rc.AccountRetryAttempts
	}
	return 4
}

// getTransient429Cooldown returns the cooldown duration applied after a
// transient (retryable) upstream 429. Reads from config with a default of 5s.
func getTransient429Cooldown() time.Duration {
	rc := config.GetRoutingConcurrencyConfig()
	ms := rc.Transient429CooldownMs
	if ms <= 0 {
		ms = 5000
	}
	return time.Duration(ms) * time.Millisecond
}

// retryBackoffAfterRateLimit returns a short randomized sleep duration to insert
// between retries after the previous attempt hit a rate-limit (429) error. The
// jitter spreads concurrent retries so they don't re-synchronize on the upstream.
func retryBackoffAfterRateLimit() time.Duration {
	// 1 s base + 0–1 s jitter
	return time.Duration(1000+rand.Intn(1000)) * time.Millisecond
}

// shouldBackoffBeforeRetry reports whether err indicates a rate-limit condition
// that warrants a short delay before the next account retry, reducing the chance
// that the next account immediately triggers the same IP-level throttle.
func shouldBackoffBeforeRetry(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "429") ||
		strings.Contains(msg, "rate_limit") ||
		strings.Contains(msg, "too many requests") ||
		strings.Contains(msg, "suspicious activity") ||
		strings.Contains(msg, "temporary limits")
}

func isSuspicious429ErrorMessage(msg string) bool {
	return strings.Contains(strings.ToLower(msg), "429") && classifyKiro429Body(msg) == "suspicious_temporary_limits"
}

func isTransient429ErrorMessage(msg string) bool {
	lower := strings.ToLower(msg)
	if !strings.Contains(lower, "429") && !strings.Contains(lower, "too many requests") {
		return false
	}
	return classifyKiro429Body(msg) != "suspicious_temporary_limits"
}

func isQuotaErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "quota")
}

func isOverageErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "402") && strings.Contains(msg, "overage")
}

func isSuspensionErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "temporarily_suspended") ||
		strings.Contains(msg, "temporarily is suspended") ||
		strings.Contains(msg, "account suspended")
}

func isProfileUnavailableErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "no available kiro profile")
}

func isAuthErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "http 401") ||
		strings.Contains(msg, "http 403") ||
		strings.Contains(msg, "refresh failed: 401") ||
		strings.Contains(msg, "refresh failed: 403") ||
		strings.Contains(msg, "bad credentials") ||
		strings.Contains(msg, "unauthorized") ||
		strings.Contains(msg, "forbidden") ||
		strings.Contains(msg, "authentication failed") ||
		strings.Contains(msg, "token invalid") ||
		strings.Contains(msg, "token expired") ||
		strings.Contains(msg, "invalid_grant") ||
		strings.Contains(msg, "access token expired") ||
		strings.Contains(msg, "refresh token expired")
}

func (h *Handler) disableAccount(account *config.Account, banStatus, banReason string) {
	if account == nil {
		return
	}

	updatedAccount := *account
	if !updatedAccount.Enabled && updatedAccount.BanStatus == banStatus && updatedAccount.BanReason == banReason {
		return
	}

	updatedAccount.Enabled = false
	updatedAccount.BanStatus = banStatus
	updatedAccount.BanReason = banReason
	updatedAccount.BanTime = time.Now().Unix()

	if err := config.UpdateAccount(account.ID, updatedAccount); err != nil {
		logger.Warnf("[AccountFailover] Failed to disable %s: %v", account.Email, err)
		return
	}

	logger.Warnf("[AccountFailover] Disabled %s: %s", account.Email, banReason)
	h.pool.Reload()
}

func (h *Handler) disableAccountOverage(account *config.Account) {
	if account == nil {
		return
	}

	snap, fetchErr := FetchOverageStatus(account)
	if fetchErr != nil {
		logger.Warnf("[AccountFailover] Failed to refresh overage status for %s: %v", account.Email, fetchErr)
		return
	}
	if persistErr := PersistOverageSnapshot(account.ID, snap); persistErr != nil {
		logger.Warnf("[AccountFailover] Failed to persist overage snapshot for %s: %v", account.Email, persistErr)
		return
	}

	logger.Warnf("[AccountFailover] Refreshed overage status for %s after upstream overage limit error: %s", account.Email, snap.Status)
	h.pool.Reload()
}

func (h *Handler) handleAccountFailure(account *config.Account, err error) {
	if account == nil || err == nil {
		return
	}

	errMsg := err.Error()
	switch {
	case isOverageErrorMessage(errMsg):
		h.disableAccountOverage(account)
		h.pool.RecordError(account.ID, false)
	case isSuspicious429ErrorMessage(errMsg):
		// Treat suspicious 429 the same as transient — short cooldown, no disable.
		h.pool.RecordTransient429(account.ID, getTransient429Cooldown())
		logger.Warnf("[AccountFailover] Suspicious 429 for %s, keeping account enabled (quarantine disabled)", account.Email)
	case isTransient429ErrorMessage(errMsg):
		// Apply a configurable cooldown so the pool queue paces retries against
		// the upstream rate-limit window instead of hammering the same account.
		h.pool.RecordTransient429(account.ID, getTransient429Cooldown())
		logger.Warnf("[AccountFailover] Transient 429 for %s, keeping account enabled for retry", account.Email)
	case isQuotaErrorMessage(errMsg):
		// Short cooldown instead of quarantine — account stays enabled.
		h.pool.RecordTransient429(account.ID, getTransient429Cooldown())
		logger.Warnf("[AccountFailover] Quota 429 for %s, keeping account enabled (quarantine disabled)", account.Email)
	case isSuspensionErrorMessage(errMsg):
		h.disableAccount(account, "BANNED", "AWS temporarily suspended - unusual user activity detected")
	case isProfileUnavailableErrorMessage(errMsg):
		// Profile ARN may be transiently unresolvable (upstream blip, stale token).
		// Treat as a soft failure: short cooldown so the next request rotates account,
		// but never auto-disable — operators can still investigate via warn logs.
		h.pool.RecordError(account.ID, false)
	case isAuthErrorMessage(errMsg):
		// Upstream 403 bodies often say "token invalid" for profile/region/route
		// mismatches, not only for truly dead credentials. Attempt one OIDC/social
		// refresh before disabling: if refresh works the account stays online and
		// background refresh continues; real invalid_grant still disables.
		if h.tryRefreshAccountAfterAuthError(account) {
			logger.Warnf("[AccountFailover] Auth-looking error for %s recovered by token refresh; keeping account enabled", account.Email)
			h.pool.RecordError(account.ID, false)
			return
		}
		h.disableAccount(account, "DISABLED", "Authentication failed - token invalid or expired")
	default:
		h.pool.RecordError(account.ID, false)
	}
}

// tryRefreshAccountAfterAuthError forces a credential refresh after an upstream
// auth-looking failure. Returns true when a new access token was obtained so the
// caller can keep the account enabled. Failed refresh (including invalid_grant)
// returns false and leaves disable/ban decisions to the caller.
func (h *Handler) tryRefreshAccountAfterAuthError(account *config.Account) bool {
	if account == nil || strings.TrimSpace(account.RefreshToken) == "" {
		return false
	}

	mu := h.accountRefreshLock(account.ID)
	mu.Lock()
	defer mu.Unlock()

	accessToken, refreshToken, expiresAt, profileArn, err := auth.RefreshToken(account)
	if err != nil {
		logger.Warnf("[AccountFailover] Token refresh after auth error failed for %s: %v", account.Email, err)
		return false
	}
	if strings.TrimSpace(accessToken) == "" {
		return false
	}

	h.pool.UpdateToken(account.ID, accessToken, refreshToken, expiresAt)
	account.AccessToken = accessToken
	if refreshToken != "" {
		account.RefreshToken = refreshToken
	}
	account.ExpiresAt = expiresAt
	if err := config.UpdateAccountToken(account.ID, accessToken, refreshToken, expiresAt); err != nil {
		logger.Warnf("[AccountFailover] Failed to persist refreshed token for %s: %v", account.Email, err)
	}
	if profileArn != "" {
		account.ProfileArn = profileArn
		if err := config.UpdateAccountProfileArn(account.ID, profileArn); err != nil {
			logger.Warnf("[AccountFailover] Failed to persist profile ARN for %s: %v", account.Email, err)
		}
	}
	return true
}

func (h *Handler) handleAccountTestFailure(account *config.Account, err error) {
	if account == nil || err == nil {
		return
	}

	errMsg := err.Error()
	switch {
	case isSuspicious429ErrorMessage(errMsg):
		// Manual test path: no quarantine, just log.
		h.pool.RecordTransient429(account.ID, 0)
		logger.Warnf("[AccountFailover] Manual test hit suspicious 429 for %s, keeping account enabled (quarantine disabled)", account.Email)
	case isTransient429ErrorMessage(errMsg):
		// Manual test path: record the 429 for visibility but do NOT cool the
		// account — a one-off probe should not pause live routing.
		h.pool.RecordTransient429(account.ID, 0)
		logger.Warnf("[AccountFailover] Manual test hit transient 429 for %s, keeping account enabled", account.Email)
	case isQuotaErrorMessage(errMsg):
		// No quarantine, just log.
		h.pool.RecordTransient429(account.ID, 0)
		logger.Warnf("[AccountFailover] Manual test hit quota 429 for %s, keeping account enabled (quarantine disabled)", account.Email)
	case isOverageErrorMessage(errMsg):
		h.disableAccountOverage(account)
		h.disableAccount(account, "DISABLED", "Manual test failed: "+errMsg)
	case isSuspensionErrorMessage(errMsg):
		h.disableAccount(account, "BANNED", "AWS temporarily suspended - unusual user activity detected")
	case isProfileUnavailableErrorMessage(errMsg):
		h.disableAccount(account, "SUSPENDED", "No available Kiro profile")
	case isAuthErrorMessage(errMsg):
		h.disableAccount(account, "DISABLED", "Authentication failed - token invalid or expired")
	default:
		h.disableAccount(account, "DISABLED", "Manual test failed: "+errMsg)
	}
}

// handleAccountError is the single-point wrapper for recording an account
// failure in a handler retry loop. It calls handleAccountFailure to apply
// cooldowns / disable / quarantine, and manages the excluded map so that
// transient 429 errors do NOT permanently exclude the account — the pool-level
// cooldown handles pacing instead.
func (h *Handler) handleAccountError(account *config.Account, excluded map[string]bool, err error) {
	excluded[account.ID] = true
	h.handleAccountFailure(account, err)
	// All 429 variants (transient, suspicious, quota) now use short cooldown
	// instead of quarantine. Clear the handler-level exclusion so the pool
	// queue can wait and retry the same account once cooldown expires.
	errMsg := err.Error()
	if isTransient429ErrorMessage(errMsg) || isSuspicious429ErrorMessage(errMsg) || isQuotaErrorMessage(errMsg) {
		delete(excluded, account.ID)
	}
}
