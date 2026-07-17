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
	// acquireOverride is a test seam: when set it replaces the pool acquire so
	// the Account Routing loop can be driven deterministically (routing-limit,
	// empty-pool, cancellation) without saturating a real pool. It is nil in
	// production, where the pool is the sole source of Accounts.
	if h.acquireOverride != nil {
		return h.acquireOverride(ctx, model, excluded, apiKeyID)
	}
	return h.pool.AcquireForModel(ctx, model, excluded, apiKeyID)
}

func isRoutingLimitError(err error) bool {
	return errors.Is(err, pool.ErrRoutingQueueFull) || errors.Is(err, pool.ErrRoutingQueueTimeout)
}

// ==================== Account Routing ====================
//
// Account Routing is the deep module that owns one outbound request's Account
// lifecycle: acquire a routing slot, ensure the token is valid, run the caller's
// work against the Account, release the slot, classify any failure, back off,
// and fail over to another Account. It knows nothing about HTTP, SSE, or protocol
// shape — callers keep all rendering, metrics, and client-facing error
// translation. The typed upstream error (*KiroAPIError: status + endpoint + body)
// is carried through unflattened so operators see faithful diagnostics.

// attemptStatus is the disposition a caller's attempt callback returns, telling
// the Account Routing module how to proceed.
type attemptStatus int

const (
	// attemptOK: the work completed successfully. The module stops with no error.
	attemptOK attemptStatus = iota
	// attemptRetryable: the Account failed in an Account-attributable way that
	// warrants failover. The module classifies + penalises the Account, backs off
	// if the error warrants it, and tries the next Account.
	attemptRetryable
	// attemptTerminal: the caller cannot fail over (e.g. it has already committed
	// model output to the client). The module stops immediately. Whether the
	// Account is penalised is governed by attemptResult.penalizeAccount, so a
	// caller-local failure (e.g. a result-parse error) does not wrongly punish an
	// Account that answered correctly.
	attemptTerminal
)

// attemptResult is what a caller's attempt callback returns.
type attemptResult struct {
	status          attemptStatus
	err             error
	penalizeAccount bool // consulted only when status == attemptTerminal
}

// attemptSuccess reports that the work completed; the module stops with success.
func attemptSuccess() attemptResult { return attemptResult{status: attemptOK} }

// attemptRetry reports an Account-attributable failure warranting failover.
func attemptRetry(err error) attemptResult {
	return attemptResult{status: attemptRetryable, err: err}
}

// attemptStop reports a caller-terminal condition (no failover). penalizeAccount
// decides whether the Account is classified/penalised for err.
func attemptStop(err error, penalizeAccount bool) attemptResult {
	return attemptResult{status: attemptTerminal, err: err, penalizeAccount: penalizeAccount}
}

// routeStopReason is the closed set of reasons the Account Routing loop stopped.
// Callers render keyed on this rather than re-deriving it from error shapes.
type routeStopReason int

const (
	// routeStopSuccess: an attempt succeeded.
	routeStopSuccess routeStopReason = iota
	// routeStopExhausted: usable Accounts ran out (retry budget spent, or the pool
	// drained after at least one real attempt). lastErr holds the last upstream
	// error — render its status.
	routeStopExhausted
	// routeStopUnavailable: no Account could be acquired and none was ever tried
	// (empty pool / ErrRoutingUnavailable). Render 503.
	routeStopUnavailable
	// routeStopRoutingLimit: the routing queue was full or timed out. Render 429.
	routeStopRoutingLimit
	// routeStopCanceled: the request context was cancelled (client gone). The
	// Account is not penalised and no failover happens.
	routeStopCanceled
	// routeStopCallerTerminal: the caller returned attemptStop (e.g. output was
	// already committed). lastErr holds the failure; the caller renders the close.
	routeStopCallerTerminal
)

// routeOutcome is what runWithAccount returns for the caller to render.
type routeOutcome struct {
	// lastAccount is the last Account handed to the callback (nil if none was
	// ever acquired).
	lastAccount *config.Account
	// lastErr is the last upstream error, kept as its concrete type
	// (*KiroAPIError when it came from Kiro) so status/endpoint/body survive.
	lastErr error
	// acquireErr is the error from the final acquire attempt, if the loop ended
	// on an acquire failure.
	acquireErr error
	// stopReason tells the caller how to render.
	stopReason routeStopReason
}

// runWithAccount owns the full Account lifecycle for one outbound request. It
// repeatedly acquires an Account (honouring conversation affinity), ensures its
// token is valid, runs attempt against it, releases the slot, and — on an
// Account-attributable failure — classifies the Account and fails over to the
// next one, backing off first when the error warrants it. The enforced order per
// attempt is: acquire -> ensure token -> work -> release -> classify -> backoff.
//
// The excluded map is fully internal. release always runs exactly once per
// acquired Account, including when attempt panics — in which case the panic is
// re-raised after release so it is never swallowed into a route error. Context
// cancellation stops the loop immediately without failover or Account penalty.
func (h *Handler) runWithAccount(
	ctx context.Context,
	model string,
	affinityKey string,
	attempt func(account *config.Account) attemptResult,
) routeOutcome {
	if ctx == nil {
		ctx = context.Background()
	}

	excluded := make(map[string]bool)
	var outcome routeOutcome
	attempts := getAccountRetryAttempts()

	for i := 0; i < attempts; i++ {
		if ctx.Err() != nil {
			outcome.acquireErr = ctx.Err()
			outcome.stopReason = routeStopCanceled
			return outcome
		}

		account, release, acquireErr := h.acquireRouteAccount(ctx, model, excluded, affinityKey)
		if acquireErr != nil {
			outcome.acquireErr = acquireErr
			switch {
			case isRoutingLimitError(acquireErr):
				outcome.stopReason = routeStopRoutingLimit
			case errors.Is(acquireErr, context.Canceled) ||
				errors.Is(acquireErr, context.DeadlineExceeded) || ctx.Err() != nil:
				outcome.stopReason = routeStopCanceled
			case outcome.lastErr != nil:
				// Pool drained after at least one real attempt: surface the last
				// upstream error instead of a bare "no accounts".
				outcome.stopReason = routeStopExhausted
			default:
				outcome.stopReason = routeStopUnavailable
			}
			return outcome
		}

		result := h.runAccountAttempt(account, release, attempt)
		outcome.lastAccount = account

		switch result.status {
		case attemptOK:
			outcome.stopReason = routeStopSuccess
			return outcome

		case attemptTerminal:
			outcome.lastErr = result.err
			// Only penalise when the caller says the failure is the Account's
			// fault and the context is still live (a cancelled request is not the
			// Account's fault).
			if result.penalizeAccount && ctx.Err() == nil {
				h.handleAccountError(account, excluded, result.err)
			}
			if ctx.Err() != nil {
				outcome.stopReason = routeStopCanceled
			} else {
				outcome.stopReason = routeStopCallerTerminal
			}
			return outcome

		case attemptRetryable:
			outcome.lastErr = result.err
			// Context cancellation is not the Account's fault: stop without
			// penalising or failing over.
			if ctx.Err() != nil {
				outcome.stopReason = routeStopCanceled
				return outcome
			}
			h.handleAccountError(account, excluded, result.err)
			// Back off only when another attempt will actually follow, and only
			// when the error warrants it. The timer is cancellable by context.
			if i < attempts-1 && shouldBackoffBeforeRetry(result.err) {
				if !sleepWithContext(ctx, retryBackoffAfterRateLimit()) {
					outcome.stopReason = routeStopCanceled
					return outcome
				}
			}
		}
	}

	// Retry budget spent.
	if outcome.lastErr != nil {
		outcome.stopReason = routeStopExhausted
	} else {
		outcome.stopReason = routeStopUnavailable
	}
	return outcome
}

// runAccountAttempt ensures the token is valid and runs attempt against one
// acquired Account, guaranteeing the routing slot is released exactly once —
// even if ensureValidToken or the callback panics, in which case the panic is
// re-raised after release rather than swallowed. A token-refresh failure is
// reported as an Account-attributable retryable failure.
func (h *Handler) runAccountAttempt(
	account *config.Account,
	release func(),
	attempt func(account *config.Account) attemptResult,
) attemptResult {
	released := false
	releaseOnce := func() {
		if !released {
			released = true
			release()
		}
	}
	defer func() {
		if r := recover(); r != nil {
			releaseOnce()
			panic(r)
		}
	}()

	if err := h.ensureValidToken(account); err != nil {
		releaseOnce()
		return attemptRetry(err)
	}

	result := attempt(account)
	releaseOnce()
	return result
}

// sleepWithContext waits for d or until ctx is cancelled. It returns true if the
// full duration elapsed, false if the context was cancelled first. A
// non-positive duration returns true immediately.
func sleepWithContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
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
	// API-key Accounts (ADR-0002) carry a static Kiro API Key as bearer and have
	// no OAuth refresh material. Their token never expires from our side, so skip
	// the refresh path entirely — attempting auth.RefreshToken would fail for lack
	// of a refresh token and could wrongly disable an otherwise-usable account.
	if account.IsApiKeyCredential() {
		return nil
	}
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
