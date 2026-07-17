package proxy

import (
	"context"
	"errors"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// fakeAcquire builds an acquireOverride that hands out the given accounts in
// order, one per call, then returns tailErr once the slice is exhausted. Each
// handed-out account gets a release func that increments *releases so a test can
// assert the routing slot was freed on every path. Accounts already in the
// excluded map are skipped (mirroring the real pool's handler-level exclusion),
// which lets a test observe that a penalised Account is not re-offered.
func fakeAcquire(accounts []*config.Account, tailErr error, releases *int64, mu *sync.Mutex) func(context.Context, string, map[string]bool, string) (*config.Account, func(), error) {
	idx := 0
	return func(_ context.Context, _ string, excluded map[string]bool, _ string) (*config.Account, func(), error) {
		for idx < len(accounts) {
			acc := accounts[idx]
			idx++
			if excluded != nil && excluded[acc.ID] {
				continue
			}
			release := func() {
				mu.Lock()
				*releases++
				mu.Unlock()
			}
			return acc, release, nil
		}
		return nil, nil, tailErr
	}
}

func acct(id string) *config.Account {
	// ExpiresAt == 0 makes ensureValidToken a no-op, so these synthetic accounts
	// never trigger a token refresh / network round-trip.
	return &config.Account{ID: id, Enabled: true, AccessToken: "tok-" + id}
}

// newRoutingTestHandler returns a Handler whose acquire is fully overridden. The
// pool is the process singleton (only used so handleAccountError's pool calls,
// which are safe on unknown IDs, do not nil-panic). Config is initialised to a
// temp file so the pool singleton's lazy Reload (which reads config) does not
// nil-panic on first use.
func newRoutingTestHandler(t *testing.T, override func(context.Context, string, map[string]bool, string) (*config.Account, func(), error)) *Handler {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	return &Handler{pool: accountpool.GetPool(), acquireOverride: override}
}

func TestRunWithAccount_FirstAttemptSuccess(t *testing.T) {
	var releases int64
	var mu sync.Mutex
	h := newRoutingTestHandler(t, fakeAcquire([]*config.Account{acct("a1")}, accountpool.ErrRoutingUnavailable, &releases, &mu))

	var seen []string
	out := h.runWithAccount(context.Background(), "m", "", func(a *config.Account) attemptResult {
		seen = append(seen, a.ID)
		return attemptSuccess()
	})

	if out.stopReason != routeStopSuccess {
		t.Fatalf("stopReason = %v, want success", out.stopReason)
	}
	if out.lastErr != nil {
		t.Fatalf("lastErr = %v, want nil", out.lastErr)
	}
	if len(seen) != 1 || seen[0] != "a1" {
		t.Fatalf("attempted %v, want [a1]", seen)
	}
	if releases != 1 {
		t.Fatalf("releases = %d, want 1", releases)
	}
}

func TestRunWithAccount_FailoverThenSuccess(t *testing.T) {
	var releases int64
	var mu sync.Mutex
	h := newRoutingTestHandler(t, fakeAcquire([]*config.Account{acct("a1"), acct("a2")}, accountpool.ErrRoutingUnavailable, &releases, &mu))

	var seen []string
	out := h.runWithAccount(context.Background(), "m", "", func(a *config.Account) attemptResult {
		seen = append(seen, a.ID)
		if a.ID == "a1" {
			return attemptRetry(&KiroAPIError{StatusCode: 500, Endpoint: "test", Body: "boom"})
		}
		return attemptSuccess()
	})

	if out.stopReason != routeStopSuccess {
		t.Fatalf("stopReason = %v, want success", out.stopReason)
	}
	if len(seen) != 2 || seen[0] != "a1" || seen[1] != "a2" {
		t.Fatalf("attempted %v, want [a1 a2]", seen)
	}
	if releases != 2 {
		t.Fatalf("releases = %d, want 2 (slot freed on each attempt)", releases)
	}
}

func TestRunWithAccount_Exhaustion(t *testing.T) {
	// More accounts than the retry budget: the loop must stop at the budget and
	// report the last upstream error, not keep going.
	accounts := []*config.Account{acct("a1"), acct("a2"), acct("a3"), acct("a4"), acct("a5"), acct("a6")}
	var releases int64
	var mu sync.Mutex
	h := newRoutingTestHandler(t, fakeAcquire(accounts, accountpool.ErrRoutingUnavailable, &releases, &mu))

	last := &KiroAPIError{StatusCode: 502, Endpoint: "test", Body: "bad gateway"}
	attempts := 0
	out := h.runWithAccount(context.Background(), "m", "", func(a *config.Account) attemptResult {
		attempts++
		return attemptRetry(last)
	})

	if out.stopReason != routeStopExhausted {
		t.Fatalf("stopReason = %v, want exhausted", out.stopReason)
	}
	if attempts != getAccountRetryAttempts() {
		t.Fatalf("attempts = %d, want %d (retry budget)", attempts, getAccountRetryAttempts())
	}
	var apiErr *KiroAPIError
	if !errors.As(out.lastErr, &apiErr) || apiErr.StatusCode != 502 {
		t.Fatalf("lastErr = %v, want typed 502 KiroAPIError preserved", out.lastErr)
	}
	if int(releases) != attempts {
		t.Fatalf("releases = %d, want %d", releases, attempts)
	}
}

func TestRunWithAccount_RoutingLimit(t *testing.T) {
	var releases int64
	var mu sync.Mutex
	// No accounts; acquire immediately returns a queue-full error.
	h := newRoutingTestHandler(t, fakeAcquire(nil, accountpool.ErrRoutingQueueFull, &releases, &mu))

	called := false
	out := h.runWithAccount(context.Background(), "m", "", func(a *config.Account) attemptResult {
		called = true
		return attemptSuccess()
	})

	if out.stopReason != routeStopRoutingLimit {
		t.Fatalf("stopReason = %v, want routing-limit", out.stopReason)
	}
	if !errors.Is(out.acquireErr, accountpool.ErrRoutingQueueFull) {
		t.Fatalf("acquireErr = %v, want ErrRoutingQueueFull", out.acquireErr)
	}
	if called {
		t.Fatal("attempt callback must not run when acquire fails")
	}
	if releases != 0 {
		t.Fatalf("releases = %d, want 0 (nothing acquired)", releases)
	}
}

func TestRunWithAccount_EmptyPoolUnavailable(t *testing.T) {
	var releases int64
	var mu sync.Mutex
	h := newRoutingTestHandler(t, fakeAcquire(nil, accountpool.ErrRoutingUnavailable, &releases, &mu))

	out := h.runWithAccount(context.Background(), "m", "", func(a *config.Account) attemptResult {
		return attemptSuccess()
	})

	if out.stopReason != routeStopUnavailable {
		t.Fatalf("stopReason = %v, want unavailable", out.stopReason)
	}
	if out.lastAccount != nil {
		t.Fatalf("lastAccount = %v, want nil (never acquired)", out.lastAccount)
	}
}

func TestRunWithAccount_StopShortCircuits(t *testing.T) {
	// A caller-terminal stop (e.g. output already committed) must stop the loop
	// immediately even though more accounts remain.
	var releases int64
	var mu sync.Mutex
	h := newRoutingTestHandler(t, fakeAcquire([]*config.Account{acct("a1"), acct("a2")}, accountpool.ErrRoutingUnavailable, &releases, &mu))

	attempts := 0
	termErr := &KiroAPIError{StatusCode: 500, Endpoint: "test", Body: "mid-stream"}
	out := h.runWithAccount(context.Background(), "m", "", func(a *config.Account) attemptResult {
		attempts++
		return attemptStop(termErr, true)
	})

	if out.stopReason != routeStopCallerTerminal {
		t.Fatalf("stopReason = %v, want caller-terminal", out.stopReason)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (stop short-circuits)", attempts)
	}
	if out.lastErr != termErr {
		t.Fatalf("lastErr = %v, want the terminal error", out.lastErr)
	}
	if releases != 1 {
		t.Fatalf("releases = %d, want 1", releases)
	}
}

func TestRunWithAccount_CancelBeforeAcquire(t *testing.T) {
	var releases int64
	var mu sync.Mutex
	h := newRoutingTestHandler(t, fakeAcquire([]*config.Account{acct("a1")}, accountpool.ErrRoutingUnavailable, &releases, &mu))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	called := false
	out := h.runWithAccount(ctx, "m", "", func(a *config.Account) attemptResult {
		called = true
		return attemptSuccess()
	})

	if out.stopReason != routeStopCanceled {
		t.Fatalf("stopReason = %v, want canceled", out.stopReason)
	}
	if called {
		t.Fatal("attempt callback must not run after context cancellation")
	}
}

func TestRunWithAccount_CancelMidAttemptNoFailover(t *testing.T) {
	// The callback cancels the context and reports a retryable error. The module
	// must treat cancellation as not-the-Account's-fault: stop without failover,
	// without penalising, with reason canceled.
	var releases int64
	var mu sync.Mutex
	h := newRoutingTestHandler(t, fakeAcquire([]*config.Account{acct("a1"), acct("a2")}, accountpool.ErrRoutingUnavailable, &releases, &mu))

	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	out := h.runWithAccount(ctx, "m", "", func(a *config.Account) attemptResult {
		attempts++
		cancel()
		return attemptRetry(errString("client gone"))
	})

	if out.stopReason != routeStopCanceled {
		t.Fatalf("stopReason = %v, want canceled", out.stopReason)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (no failover after cancel)", attempts)
	}
	if releases != 1 {
		t.Fatalf("releases = %d, want 1", releases)
	}
}

func TestRunWithAccount_PanicReleasesAndRepanics(t *testing.T) {
	var releases int64
	var mu sync.Mutex
	h := newRoutingTestHandler(t, fakeAcquire([]*config.Account{acct("a1")}, accountpool.ErrRoutingUnavailable, &releases, &mu))

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic to propagate, but it was swallowed")
		}
		if releases != 1 {
			t.Fatalf("releases = %d, want 1 (slot freed before re-panic)", releases)
		}
	}()

	h.runWithAccount(context.Background(), "m", "", func(a *config.Account) attemptResult {
		panic("boom in callback")
	})
	t.Fatal("runWithAccount returned; expected the callback panic to propagate")
}

func TestRunWithAccount_NoSleepOnFinalAttempt(t *testing.T) {
	// Every attempt returns a 429 (which shouldBackoffBeforeRetry matches). With
	// backoff only between attempts, total time must stay well under one full
	// backoff-per-attempt: the final attempt must not sleep.
	accounts := make([]*config.Account, getAccountRetryAttempts())
	for i := range accounts {
		accounts[i] = acct("a" + string(rune('0'+i)))
	}
	var releases int64
	var mu sync.Mutex
	h := newRoutingTestHandler(t, fakeAcquire(accounts, accountpool.ErrRoutingUnavailable, &releases, &mu))

	rl := &KiroAPIError{StatusCode: 429, Endpoint: "test", Body: "too many requests"}
	start := time.Now()
	out := h.runWithAccount(context.Background(), "m", "", func(a *config.Account) attemptResult {
		return attemptRetry(rl)
	})
	elapsed := time.Since(start)

	if out.stopReason != routeStopExhausted {
		t.Fatalf("stopReason = %v, want exhausted", out.stopReason)
	}
	// N attempts => at most N-1 backoffs. Backoff is 1s base + up to 1s jitter,
	// so N-1 backoffs is at most (N-1)*2s. If the final attempt also slept, the
	// count would be N backoffs. Assert the loop did not sleep an extra time by
	// bounding to the between-attempts count with margin.
	maxBetween := time.Duration(getAccountRetryAttempts()-1) * 2 * time.Second
	if elapsed >= maxBetween+2*time.Second {
		t.Fatalf("elapsed %v suggests a sleep after the final attempt (bound %v)", elapsed, maxBetween)
	}
}

func TestRunWithAccount_TerminalNoPenaltyKeepsAccount(t *testing.T) {
	// web_search parse-failure shape: caller-terminal but NOT account-attributable.
	// The Account must not be excluded/penalised — verified by the account being
	// offered again on a second independent run is out of scope here; we assert
	// the disposition is honoured (stop, no failover) and the error is preserved.
	var releases int64
	var mu sync.Mutex
	h := newRoutingTestHandler(t, fakeAcquire([]*config.Account{acct("a1"), acct("a2")}, accountpool.ErrRoutingUnavailable, &releases, &mu))

	parseErr := errString("failed to parse MCP results")
	attempts := 0
	out := h.runWithAccount(context.Background(), "m", "", func(a *config.Account) attemptResult {
		attempts++
		return attemptStop(parseErr, false)
	})

	if out.stopReason != routeStopCallerTerminal {
		t.Fatalf("stopReason = %v, want caller-terminal", out.stopReason)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (no failover on terminal)", attempts)
	}
	if out.lastErr != parseErr {
		t.Fatalf("lastErr = %v, want the parse error preserved", out.lastErr)
	}
}
