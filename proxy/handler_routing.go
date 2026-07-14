package proxy

import (
	"context"
	"errors"
	"kiro-go/auth"
	"kiro-go/config"
	"kiro-go/logger"
	"kiro-go/pool"
	"net/http"
	"time"
)

// clientFacingClaudeErrorType maps an HTTP status code to the Anthropic error
// "type" clients expect. When all retry attempts are exhausted (e.g. the pool is
// drained by a 429 quarantine wave) the proxy should surface a rate_limit_error
// rather than masking it as a generic api_error/500, so clients back off instead
// of treating it as a server bug.
func clientFacingClaudeErrorType(status int) string {
	switch status {
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusUnauthorized, http.StatusForbidden:
		return "authentication_error"
	case http.StatusPaymentRequired:
		return "billing_error"
	case http.StatusServiceUnavailable:
		return "overloaded_error"
	default:
		return "api_error"
	}
}

// clientFacingOpenAIErrorType is the OpenAI/Responses equivalent of
// clientFacingClaudeErrorType.
func clientFacingOpenAIErrorType(status int) string {
	switch status {
	case http.StatusTooManyRequests:
		return "rate_limit_exceeded"
	case http.StatusUnauthorized, http.StatusForbidden:
		return "authentication_error"
	case http.StatusPaymentRequired:
		return "insufficient_quota"
	default:
		return "server_error"
	}
}

// logRetryExhausted records the otherwise-silent failure where every account
// retry attempt failed. Without this, a pool drained by a 429 quarantine wave
// produced a bare 500 with no log line, making the cause impossible to diagnose.
func logRetryExhausted(protocol, model string, statusCode int, errType string, lastErr error) {
	logger.Warnf("[%s] all %d account retry attempts exhausted (model=%s status=%d type=%s): %v",
		protocol, getAccountRetryAttempts(), model, statusCode, errType, lastErr)
}

func (h *Handler) acquireRouteAccount(ctx context.Context, model string, excluded map[string]bool, apiKeyID string) (*config.Account, func(), error) {
	return h.pool.AcquireForModel(ctx, model, excluded, apiKeyID)
}

func isRoutingLimitError(err error) bool {
	return errors.Is(err, pool.ErrRoutingQueueFull) || errors.Is(err, pool.ErrRoutingQueueTimeout)
}

func routingErrorMessage(err error) string {
	if errors.Is(err, pool.ErrRoutingQueueFull) {
		return "Routing queue full"
	}
	if errors.Is(err, pool.ErrRoutingQueueTimeout) {
		return "Routing queue timeout"
	}
	if err != nil {
		return err.Error()
	}
	return "Routing unavailable"
}

// ensureValidToken 确保 token 有效
func (h *Handler) ensureValidToken(account *config.Account) error {
	if account.ExpiresAt == 0 || time.Now().Unix() < account.ExpiresAt-tokenRefreshSkewSeconds {
		return nil
	}

	mu := h.accountRefreshLock(account.ID)
	mu.Lock()
	defer mu.Unlock()

	// Another concurrent request may have refreshed this account while we waited.
	if latest := h.pool.GetByID(account.ID); latest != nil {
		account.AccessToken = latest.AccessToken
		account.RefreshToken = latest.RefreshToken
		account.ExpiresAt = latest.ExpiresAt
		account.ProfileArn = latest.ProfileArn
		if account.ExpiresAt == 0 || time.Now().Unix() < account.ExpiresAt-tokenRefreshSkewSeconds {
			return nil
		}
	}

	accessToken, refreshToken, expiresAt, profileArn, err := auth.RefreshToken(account)
	if err != nil {
		return err
	}

	// Persist under pool/config first; request-local copy is updated after so
	// concurrent acquires observe tokens via GetByID rather than shared fields.
	h.pool.UpdateToken(account.ID, accessToken, refreshToken, expiresAt)
	config.UpdateAccountToken(account.ID, accessToken, refreshToken, expiresAt)
	if profileArn != "" {
		h.pool.UpdateProfileArn(account.ID, profileArn)
		config.UpdateAccountProfileArn(account.ID, profileArn)
	}

	account.AccessToken = accessToken
	if refreshToken != "" {
		account.RefreshToken = refreshToken
	}
	account.ExpiresAt = expiresAt
	if profileArn != "" {
		account.ProfileArn = profileArn
	}

	return nil
}
