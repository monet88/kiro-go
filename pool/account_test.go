package pool

import (
	"context"
	"errors"
	"kiro-go/config"
	"path/filepath"
	"testing"
	"time"
)

func TestOverLimitAccountsAreSkippedByDefault(t *testing.T) {
	p := &AccountPool{}
	normal := config.Account{ID: "normal"}
	overLimit := config.Account{ID: "over", UsageCurrent: 10, UsageLimit: 10}

	p.accounts = []config.Account{normal, overLimit}

	for i := 0; i < 5; i++ {
		acc := p.GetNext()
		if acc == nil {
			t.Fatalf("expected an account")
		}
		if acc.ID == "over" {
			t.Fatalf("expected over-limit account to be skipped when upstream OverageStatus is empty")
		}
	}
}

func TestOverLimitAccountsCanBeSelectedWhenUpstreamOverageEnabled(t *testing.T) {
	p := &AccountPool{}
	overLimit := config.Account{
		ID:            "over",
		UsageCurrent:  10,
		UsageLimit:    10,
		OverageStatus: "ENABLED",
	}

	p.accounts = []config.Account{overLimit}

	acc := p.GetNext()
	if acc == nil {
		t.Fatalf("expected upstream-enabled overage account to be selectable")
	}
	if acc.ID != "over" {
		t.Fatalf("expected overage account, got %q", acc.ID)
	}
}

func TestOverLimitAccountsRemainSkippedWhenUpstreamOverageDisabled(t *testing.T) {
	p := &AccountPool{}
	overLimit := config.Account{
		ID:            "over",
		UsageCurrent:  10,
		UsageLimit:    10,
		OverageStatus: "DISABLED",
	}

	p.accounts = []config.Account{overLimit}

	if acc := p.GetNext(); acc != nil {
		t.Fatalf("expected nil when upstream OverageStatus=DISABLED, got %q", acc.ID)
	}
}

func TestGetNextKeepsFiveMinuteTokenAvailable(t *testing.T) {
	p := &AccountPool{}
	account := config.Account{
		ID:          "acct-1",
		AccessToken: "access-token",
		ExpiresAt:   time.Now().Unix() + 300,
	}

	p.accounts = []config.Account{account}

	got := p.GetNext()
	if got == nil {
		t.Fatalf("expected five-minute token to be available")
	}
	if got.ID != account.ID {
		t.Fatalf("expected account %q, got %q", account.ID, got.ID)
	}
}

// ---------------------------------------------------------------------------
// IsAuthFailure
// ---------------------------------------------------------------------------

func TestIsAuthFailureRecognizes401And403(t *testing.T) {
	positives := []string{
		"HTTP 401 from server",
		"received 403 Forbidden",
		"bad credentials",
		"invalid_grant",
		"invalid_token",
		"token expired",
		"token has expired",
		"unauthorized",
	}
	for _, msg := range positives {
		if !IsAuthFailure(errors.New(msg)) {
			t.Errorf("IsAuthFailure(%q) = false, want true", msg)
		}
	}
}

func TestIsAuthFailureIgnoresFalsePositives(t *testing.T) {
	// hasStatusToken only excludes digit boundaries; e.g. "4011" contains "401"
	// but the trailing '1' is a digit so it does NOT match.
	negatives := []string{
		"status code 4011 found", // digit immediately after 401 → not a standalone token
		"error 14013 exceeded",   // digit before and after 401
		"some random error",
		"status 200 OK",
	}
	for _, msg := range negatives {
		if IsAuthFailure(errors.New(msg)) {
			t.Errorf("IsAuthFailure(%q) = true, want false", msg)
		}
	}
}

func TestIsAuthFailureNilError(t *testing.T) {
	if IsAuthFailure(nil) {
		t.Fatal("IsAuthFailure(nil) = true, want false")
	}
}

// ---------------------------------------------------------------------------
// IsSuspensionError
// ---------------------------------------------------------------------------

func TestIsSuspensionErrorDetectsKnownMessages(t *testing.T) {
	positives := []string{
		"account temporarily_suspended",
		"account temporarily suspended",
		"no available kiro profile",
		"No Available Kiro Profile", // case-insensitive
	}
	for _, msg := range positives {
		if !IsSuspensionError(errors.New(msg)) {
			t.Errorf("IsSuspensionError(%q) = false, want true", msg)
		}
	}
}

func TestIsSuspensionErrorIgnoresUnrelatedErrors(t *testing.T) {
	negatives := []string{
		"some other error",
		"unauthorized",
		"429 too many requests",
	}
	for _, msg := range negatives {
		if IsSuspensionError(errors.New(msg)) {
			t.Errorf("IsSuspensionError(%q) = true, want false", msg)
		}
	}
}

func TestIsSuspensionErrorNilError(t *testing.T) {
	if IsSuspensionError(nil) {
		t.Fatal("IsSuspensionError(nil) = true, want false")
	}
}

// ---------------------------------------------------------------------------
// GetNextForModelExcluding
// ---------------------------------------------------------------------------

func newTestPool(accounts ...config.Account) *AccountPool {
	p := &AccountPool{
		cooldowns:   make(map[string]time.Time),
		errorCounts: make(map[string]int),
		modelLists:  make(map[string]map[string]bool),
	}
	p.accounts = accounts
	return p
}

func TestGetNextForModelExcludingSkipsExcludedAccounts(t *testing.T) {
	p := newTestPool(
		config.Account{ID: "a"},
		config.Account{ID: "b"},
	)
	excluded := map[string]bool{"a": true}
	for i := 0; i < 5; i++ {
		acc := p.GetNextForModelExcluding("model", excluded)
		if acc == nil {
			t.Fatal("expected account b, got nil")
		}
		if acc.ID == "a" {
			t.Fatalf("excluded account a was returned on iteration %d", i)
		}
	}
}

func TestGetNextForModelExcludingReturnsNilWhenAllExcluded(t *testing.T) {
	p := newTestPool(config.Account{ID: "only"})
	acc := p.GetNextForModelExcluding("model", map[string]bool{"only": true})
	if acc != nil {
		t.Fatalf("expected nil when only account is excluded, got %q", acc.ID)
	}
}

func TestGetNextForModelExcludingReturnsNilOnEmptyPool(t *testing.T) {
	p := newTestPool()
	acc := p.GetNextForModelExcluding("model", map[string]bool{})
	if acc != nil {
		t.Fatalf("expected nil for empty pool, got %q", acc.ID)
	}
}

func TestAcquireForModelHonorsPerAccountConcurrencyAndOverflow(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.UpdateRoutingConcurrencyConfig(config.RoutingConcurrencyConfig{
		Enabled:                 true,
		GlobalQueueSize:         0,
		GlobalQueueTimeoutMs:    50,
		PerAccountMaxConcurrent: 1,
		StickyAccount:           true,
		OverflowToOtherAccounts: true,
	}); err != nil {
		t.Fatalf("UpdateRoutingConcurrencyConfig: %v", err)
	}
	p := newTestPool(config.Account{ID: "a"}, config.Account{ID: "b"})

	first, releaseFirst, err := p.AcquireForModel(context.Background(), "", nil, "key")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	second, releaseSecond, err := p.AcquireForModel(context.Background(), "", nil, "key")
	if err != nil {
		releaseFirst()
		t.Fatalf("second acquire: %v", err)
	}
	defer releaseFirst()
	defer releaseSecond()
	if first.ID == second.ID {
		t.Fatalf("expected overflow to another account, got %q twice", first.ID)
	}
}

func TestAcquireForModelQueueTimeout(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.UpdateRoutingConcurrencyConfig(config.RoutingConcurrencyConfig{
		Enabled:                 true,
		GlobalMaxConcurrent:     1,
		GlobalQueueSize:         1,
		GlobalQueueTimeoutMs:    10,
		PerAccountMaxConcurrent: 1,
		StickyAccount:           true,
		OverflowToOtherAccounts: true,
	}); err != nil {
		t.Fatalf("UpdateRoutingConcurrencyConfig: %v", err)
	}
	p := newTestPool(config.Account{ID: "a"})
	_, release, err := p.AcquireForModel(context.Background(), "", nil, "key")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer release()
	_, _, err = p.AcquireForModel(context.Background(), "", nil, "other-key")
	if !errors.Is(err, ErrRoutingQueueTimeout) {
		t.Fatalf("expected queue timeout, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// DisableAccount
// ---------------------------------------------------------------------------

func TestDisableAccountSetsCooldown(t *testing.T) {
	// Initialize a temporary config so SetAccountBanStatus can persist safely.
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	p := newTestPool()
	p.DisableAccount("test-id", "test reason")

	p.mu.RLock()
	cooldown, ok := p.cooldowns["test-id"]
	p.mu.RUnlock()

	if !ok {
		t.Fatal("expected cooldown to be set after DisableAccount")
	}
	// Safety-net cooldown must be at least 23 hours from now.
	minExpected := time.Now().Add(23 * time.Hour)
	if cooldown.Before(minExpected) {
		t.Fatalf("expected cooldown >= 23h in future, got %v", cooldown)
	}
}

func TestGetNextExcludingSkipsExcludedAccount(t *testing.T) {
	p := &AccountPool{
		accounts: []config.Account{
			{ID: "a", Enabled: true},
			{ID: "b", Enabled: true},
		},
		cooldowns:    make(map[string]time.Time),
		errorCounts:  make(map[string]int),
		modelLists:   make(map[string]map[string]bool),
		currentIndex: ^uint64(0),
	}

	acc := p.GetNextExcluding(map[string]bool{"a": true})
	if acc == nil || acc.ID != "b" {
		t.Fatalf("expected account b, got %#v", acc)
	}
}

func TestGetNextForModelExcludingSkipsExcludedAccount(t *testing.T) {
	p := &AccountPool{
		accounts: []config.Account{
			{ID: "a", Enabled: true},
			{ID: "b", Enabled: true},
		},
		cooldowns:    make(map[string]time.Time),
		errorCounts:  make(map[string]int),
		modelLists:   make(map[string]map[string]bool),
		currentIndex: ^uint64(0),
	}
	p.SetModelList("a", []string{"claude-sonnet-4.5"})
	p.SetModelList("b", []string{"claude-sonnet-4.5"})

	acc := p.GetNextForModelExcluding("claude-sonnet-4.5", map[string]bool{"a": true})
	if acc == nil || acc.ID != "b" {
		t.Fatalf("expected account b, got %#v", acc)
	}
}

// ---------------------------------------------------------------------------
// Reload over-usage filtering
// ---------------------------------------------------------------------------

func TestReloadKeepsOverQuotaAccountWhenAllowOverUsage(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:           "over",
		Enabled:      true,
		UsageCurrent: 10,
		UsageLimit:   10,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if err := config.UpdateAllowOverUsage(true); err != nil {
		t.Fatalf("UpdateAllowOverUsage: %v", err)
	}

	p := newTestPool()
	p.Reload()

	if got := p.GetNext(); got == nil || got.ID != "over" {
		t.Fatalf("expected over-quota account to remain routable when allowOverUsage=true, got %#v", got)
	}
}

func TestReloadDropsOverQuotaAccountWhenAllowOverUsageDisabled(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:           "over",
		Enabled:      true,
		UsageCurrent: 10,
		UsageLimit:   10,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	p := newTestPool()
	p.Reload()

	if got := p.GetNext(); got != nil {
		t.Fatalf("expected over-quota account to be dropped, got %q", got.ID)
	}
}

// ---------------------------------------------------------------------------
// Local failover routing extensions
// ---------------------------------------------------------------------------

func initPoolTestConfig(t *testing.T) {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgPath); err != nil {
		t.Fatalf("init config: %v", err)
	}
}

func TestGetNextAllowsExpiredAccountWithRefreshToken(t *testing.T) {
	p := &AccountPool{}
	account := config.Account{
		ID:           "refreshable",
		AccessToken:  "expired-access-token",
		RefreshToken: "refresh-token",
		ExpiresAt:    time.Now().Add(-time.Minute).Unix(),
	}

	p.accounts = []config.Account{account}

	got := p.GetNext()
	if got == nil {
		t.Fatalf("expected expired account with refresh token to be routable for refresh")
	}
	if got.ID != account.ID {
		t.Fatalf("expected account %q, got %q", account.ID, got.ID)
	}
}

func TestGetNextSkipsExpiredAccountWithoutRefreshToken(t *testing.T) {
	p := &AccountPool{}
	p.accounts = []config.Account{
		{ID: "expired", AccessToken: "expired-access-token", ExpiresAt: time.Now().Add(-time.Minute).Unix()},
	}

	if got := p.GetNext(); got != nil {
		t.Fatalf("expected expired account without refresh token to be skipped, got %#v", got)
	}
}

func TestGetNextAllowsExpiredApiKeyAccount(t *testing.T) {
	p := &AccountPool{}
	p.accounts = []config.Account{{
		ID:           "api-key",
		AccessToken:  "ksk_static",
		KiroApiKey:   "ksk_static",
		AuthMethod:   "api_key",
		RefreshToken: "stale-refresh-token",
		ExpiresAt:    time.Now().Add(-time.Minute).Unix(),
	}}

	got := p.GetNext()
	if got == nil {
		t.Fatal("expected expired API-key Account to remain routable")
	}
	if got.ID != "api-key" {
		t.Fatalf("expected API-key Account, got %q", got.ID)
	}
}

func TestGetNextSkipsApiKeyAccountWithoutSecret(t *testing.T) {
	p := &AccountPool{}
	p.accounts = []config.Account{{ID: "api-key", AuthMethod: "api_key"}}

	if got := p.GetNext(); got != nil {
		t.Fatalf("expected API-key Account without a secret to be skipped, got %#v", got)
	}
}

func TestApiKeyAccountIgnoresLegacyQuotaCache(t *testing.T) {
	account := config.Account{
		ID:           "api-key",
		AccessToken:  "ksk_static",
		KiroApiKey:   "ksk_static",
		AuthMethod:   "api_key",
		UsageCurrent: 100,
		UsageLimit:   100,
	}
	if isQuotaBlocked(account, false) {
		t.Fatal("API-key Account must not be blocked by unsupported legacy quota metadata")
	}
}

func TestUpdateTokenDoesNotClobberApiKeyAccount(t *testing.T) {
	p := &AccountPool{}
	p.accounts = []config.Account{{
		ID:          "api-key",
		AccessToken: "ksk_static",
		KiroApiKey:  "ksk_static",
		AuthMethod:  "api_key",
	}}

	p.UpdateToken("api-key", "oauth-access", "oauth-refresh", time.Now().Add(time.Hour).Unix())

	got := p.accounts[0]
	if got.AccessToken != "ksk_static" || got.KiroApiKey != "ksk_static" {
		t.Fatalf("API-key Account bearer was clobbered: %+v", got)
	}
	if got.RefreshToken != "" || got.ExpiresAt != 0 {
		t.Fatalf("OAuth token metadata was written to API-key Account: %+v", got)
	}
}

func TestAvailableCountMatchesRealRoutingConstraints(t *testing.T) {
	initPoolTestConfig(t)
	if err := config.UpdateAllowOverUsage(false); err != nil {
		t.Fatalf("disable global over-usage: %v", err)
	}

	now := time.Now()
	p := &AccountPool{
		accounts: []config.Account{
			{ID: "ok", AccessToken: "token", ExpiresAt: now.Add(10 * time.Minute).Unix()},
			{ID: "cooldown", AccessToken: "token", ExpiresAt: now.Add(10 * time.Minute).Unix()},
			{ID: "expired", AccessToken: "token", ExpiresAt: now.Add(30 * time.Second).Unix()},
			{ID: "refreshable", AccessToken: "token", RefreshToken: "refresh-token", ExpiresAt: now.Add(30 * time.Second).Unix()},
			{ID: "over", AccessToken: "token", ExpiresAt: now.Add(10 * time.Minute).Unix(), UsageCurrent: 10, UsageLimit: 10},
		},
		cooldowns: map[string]time.Time{"cooldown": now.Add(time.Minute)},
	}

	if got := p.AvailableCount(); got != 2 {
		t.Fatalf("expected 2 available or refreshable accounts, got %d", got)
	}
}

func TestAvailableCountSkipsExpiredAccountWithoutRefreshToken(t *testing.T) {
	initPoolTestConfig(t)
	if err := config.UpdateAllowOverUsage(false); err != nil {
		t.Fatalf("disable global over-usage: %v", err)
	}

	now := time.Now()
	p := &AccountPool{
		accounts: []config.Account{
			{ID: "expired", AccessToken: "token", ExpiresAt: now.Add(-time.Minute).Unix()},
		},
	}

	if got := p.AvailableCount(); got != 0 {
		t.Fatalf("expected expired account without refresh token to be unavailable, got %d", got)
	}
}

func TestHealthSnapshotsKeepRefreshableExpiredAccountsRoutable(t *testing.T) {
	initPoolTestConfig(t)
	if err := config.UpdateAllowOverUsage(false); err != nil {
		t.Fatalf("disable global over-usage: %v", err)
	}

	now := time.Now()
	p := &AccountPool{
		accounts: []config.Account{
			{ID: "refreshable", AccessToken: "token", RefreshToken: "refresh-token", ExpiresAt: now.Add(-time.Minute).Unix()},
			{ID: "expired", AccessToken: "token", ExpiresAt: now.Add(-time.Minute).Unix()},
		},
		errorCounts: make(map[string]int),
		requestLog:  make(map[string][]requestEvent),
		lastErrorAt: make(map[string]time.Time),
	}

	snapshots := p.GetHealthSnapshots()
	if !snapshots["refreshable"].CanRoute {
		t.Fatalf("expected expired account with refresh token to remain routable for refresh")
	}
	if snapshots["expired"].CanRoute {
		t.Fatalf("expected expired account without refresh token to be non-routable")
	}
}

// ---------------------------------------------------------------------------
// Conversation affinity (sticky routing)
// ---------------------------------------------------------------------------

func setRoutingConfig(t *testing.T, rc config.RoutingConcurrencyConfig) {
	t.Helper()
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.UpdateRoutingConcurrencyConfig(rc); err != nil {
		t.Fatalf("UpdateRoutingConcurrencyConfig: %v", err)
	}
}

// With concurrency limiting disabled, the same affinity key must still pin to
// one account across turns (prompt-cache reuse), instead of round-robin.
func TestAcquireStickyDisabledConcurrencyPinsSameAccount(t *testing.T) {
	setRoutingConfig(t, config.RoutingConcurrencyConfig{
		Enabled:       false,
		StickyAccount: true,
	})
	p := newTestPool(config.Account{ID: "a"}, config.Account{ID: "b"}, config.Account{ID: "c"})

	first, rel, err := p.AcquireForModel(context.Background(), "", nil, "conv-1")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	rel()
	for i := 0; i < 10; i++ {
		acc, rel, err := p.AcquireForModel(context.Background(), "", nil, "conv-1")
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		rel()
		if acc.ID != first.ID {
			t.Fatalf("turn %d routed to %q, expected sticky %q", i, acc.ID, first.ID)
		}
	}
}

// Empty affinity key must fall back to round-robin (no pinning), so single-shot
// requests still spread across accounts.
func TestAcquireEmptyAffinityRoundRobins(t *testing.T) {
	setRoutingConfig(t, config.RoutingConcurrencyConfig{
		Enabled:       false,
		StickyAccount: true,
	})
	p := newTestPool(config.Account{ID: "a"}, config.Account{ID: "b"})
	seen := map[string]bool{}
	for i := 0; i < 6; i++ {
		acc, rel, err := p.AcquireForModel(context.Background(), "", nil, "")
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		rel()
		seen[acc.ID] = true
	}
	if len(seen) < 2 {
		t.Fatalf("empty affinity should round-robin across accounts, only saw %v", seen)
	}
}

// Different conversations should be distributable to different accounts.
func TestAcquireDifferentAffinityCanUseDifferentAccounts(t *testing.T) {
	setRoutingConfig(t, config.RoutingConcurrencyConfig{
		Enabled:       false,
		StickyAccount: true,
	})
	p := newTestPool(config.Account{ID: "a"}, config.Account{ID: "b"})
	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		key := "conv-" + string(rune('A'+i))
		acc, rel, err := p.AcquireForModel(context.Background(), "", nil, key)
		if err != nil {
			t.Fatalf("acquire %s: %v", key, err)
		}
		rel()
		seen[acc.ID] = true
	}
	if len(seen) < 2 {
		t.Fatalf("distinct conversations should spread across accounts, only saw %v", seen)
	}
}

// When concurrency limiting is on and the sticky account is busy, the request
// overflows to another account, but the pin must NOT move: once the original
// account frees up the conversation returns to it (cache-warm).
func TestAcquireStickyOverflowDoesNotMovePin(t *testing.T) {
	setRoutingConfig(t, config.RoutingConcurrencyConfig{
		Enabled:                 true,
		GlobalQueueSize:         0,
		GlobalQueueTimeoutMs:    50,
		PerAccountMaxConcurrent: 1,
		StickyAccount:           true,
		OverflowToOtherAccounts: true,
	})
	p := newTestPool(config.Account{ID: "a"}, config.Account{ID: "b"})

	// Establish the pin for conv-1.
	first, relFirst, err := p.AcquireForModel(context.Background(), "", nil, "conv-1")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	// Second concurrent turn of conv-1: sticky account is busy → overflow.
	second, relSecond, err := p.AcquireForModel(context.Background(), "", nil, "conv-1")
	if err != nil {
		relFirst()
		t.Fatalf("overflow acquire: %v", err)
	}
	if second.ID == first.ID {
		relFirst()
		relSecond()
		t.Fatalf("expected overflow to a different account")
	}
	relFirst()
	relSecond()

	// Next turn of conv-1 (nothing busy) must return to the original pinned account.
	third, relThird, err := p.AcquireForModel(context.Background(), "", nil, "conv-1")
	if err != nil {
		t.Fatalf("third acquire: %v", err)
	}
	defer relThird()
	if third.ID != first.ID {
		t.Fatalf("pin moved after overflow: got %q, expected %q", third.ID, first.ID)
	}
}

func TestStickyEntryExpires(t *testing.T) {
	p := newTestPool(config.Account{ID: "a"})
	p.ensureRuntimeMapsLocked()
	now := time.Now()
	p.setStickyLocked("conv-1", "a", now)
	if got := p.lookupStickyLocked("conv-1", now.Add(stickyTTL-time.Second)); got != "a" {
		t.Fatalf("expected live pin, got %q", got)
	}
	if got := p.lookupStickyLocked("conv-1", now.Add(stickyTTL+time.Second)); got != "" {
		t.Fatalf("expected expired pin to be empty, got %q", got)
	}
	if _, ok := p.routeStickyByKey["conv-1"]; ok {
		t.Fatal("expired entry should have been deleted on lookup")
	}
}

func TestStickyEvictionRespectsCapacity(t *testing.T) {
	p := newTestPool(config.Account{ID: "a"})
	p.ensureRuntimeMapsLocked()
	base := time.Now()
	// Fill to capacity with staggered expiries (key i expires soonest for small i).
	for i := 0; i < stickyMaxEntries; i++ {
		p.routeStickyByKey[string(rune(i))+"-k"] = stickyEntry{
			accountID: "a",
			expiresAt: base.Add(time.Duration(i) * time.Millisecond),
		}
	}
	// Inserting a new key beyond capacity must evict, keeping size bounded.
	p.setStickyLocked("brand-new", "a", base.Add(time.Hour))
	if len(p.routeStickyByKey) > stickyMaxEntries {
		t.Fatalf("sticky map exceeded capacity: %d > %d", len(p.routeStickyByKey), stickyMaxEntries)
	}
	if _, ok := p.routeStickyByKey["brand-new"]; !ok {
		t.Fatal("newly inserted key should be present after eviction")
	}
}

// Sticky outcome counters: first turn = miss, subsequent same-key turns = hit;
// empty affinity key is not counted.
func TestStickyOutcomeCounters(t *testing.T) {
	setRoutingConfig(t, config.RoutingConcurrencyConfig{
		Enabled:       false,
		StickyAccount: true,
	})
	p := newTestPool(config.Account{ID: "a"}, config.Account{ID: "b"})

	// First turn of a new conversation → miss.
	_, rel, err := p.AcquireForModel(context.Background(), "", nil, "conv-x")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	rel()
	// Next two turns → hits.
	for i := 0; i < 2; i++ {
		_, rel, err := p.AcquireForModel(context.Background(), "", nil, "conv-x")
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		rel()
	}
	// Empty key must not be counted.
	_, rel, err = p.AcquireForModel(context.Background(), "", nil, "")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	rel()

	stats := p.RoutingStats()
	if got := stats["stickyMissTotal"].(uint64); got != 1 {
		t.Fatalf("stickyMissTotal = %d, want 1", got)
	}
	if got := stats["stickyHitTotal"].(uint64); got != 2 {
		t.Fatalf("stickyHitTotal = %d, want 2", got)
	}
	if got := stats["stickyDivertTotal"].(uint64); got != 0 {
		t.Fatalf("stickyDivertTotal = %d, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// BalanceMode routing (health / managed / aggressive)
// ---------------------------------------------------------------------------

// TestBalanceModeAggressivePrefersMostUtilizedWithHeadroom verifies aggressive
// mode concentrates load on the fullest account that still has headroom.
func TestBalanceModeAggressivePrefersMostUtilizedWithHeadroom(t *testing.T) {
	initPoolTestConfig(t)
	if err := config.UpdateBalanceMode("aggressive"); err != nil {
		t.Fatalf("set balance mode: %v", err)
	}
	p := newTestPool(
		config.Account{ID: "low", UsageCurrent: 200, UsageLimit: 1000},  // frac 0.2
		config.Account{ID: "mid", UsageCurrent: 600, UsageLimit: 1000},  // frac 0.6
		config.Account{ID: "high", UsageCurrent: 900, UsageLimit: 1000}, // frac 0.9
	)
	if acc := p.GetNextForModel("model"); acc == nil || acc.ID != "high" {
		t.Fatalf("aggressive should pick most-utilized account with headroom (high), got %#v", acc)
	}
}

func TestBalanceModeAggressiveIgnoresApiKeyUsageMetadata(t *testing.T) {
	initPoolTestConfig(t)
	if err := config.UpdateBalanceMode("aggressive"); err != nil {
		t.Fatalf("set balance mode: %v", err)
	}
	apiKey := config.Account{ID: "api-key", KiroApiKey: "ksk_static", AccessToken: "ksk_static", AuthMethod: "api_key", UsageCurrent: 900, UsageLimit: 1000, UsagePercent: 0.9}
	if got := effectiveUsageFraction(apiKey, false); got != 0 {
		t.Fatalf("API-key usage fraction = %v, want 0", got)
	}
}

// TestBalanceModeAggressiveUsesTotalBudgetWithOverage proves fullness is
// measured against the total budget (subscription + overage cap) — not the
// subscription quota — when overage is in effect. Account "overSub" has blown
// past its subscription limit but sits at only ~10% of its total budget, so it
// must rank *below* "withinSub" which is at 25% of its subscription quota.
func TestBalanceModeAggressiveUsesTotalBudgetWithOverage(t *testing.T) {
	initPoolTestConfig(t)
	if err := config.UpdateBalanceMode("aggressive"); err != nil {
		t.Fatalf("set balance mode: %v", err)
	}
	if err := config.UpdateAllowOverUsage(true); err != nil {
		t.Fatalf("allow over usage: %v", err)
	}
	p := newTestPool(
		// 1137/1000 subscription (113%) but 1137/(1000+10000)=10.3% of total budget.
		config.Account{ID: "overSub", UsageCurrent: 1137, UsageLimit: 1000, OverageStatus: "ENABLED", OverageCap: 10000},
		// 500/2000 subscription = 25%, no overage.
		config.Account{ID: "withinSub", UsageCurrent: 500, UsageLimit: 2000},
	)
	if acc := p.GetNextForModel("model"); acc == nil || acc.ID != "withinSub" {
		t.Fatalf("aggressive should rank by total-budget fraction (withinSub 25%% > overSub 10%%), got %#v", acc)
	}
}

// TestBalanceModeAggressiveSkipsFullAccount verifies a genuinely full account
// (fraction >= 1) ranks below any account that still has headroom.
func TestBalanceModeAggressiveSkipsFullAccount(t *testing.T) {
	initPoolTestConfig(t)
	if err := config.UpdateBalanceMode("aggressive"); err != nil {
		t.Fatalf("set balance mode: %v", err)
	}
	p := newTestPool(
		// Full: at subscription limit with overage enabled (routable, but full).
		config.Account{ID: "full", UsageCurrent: 1000, UsageLimit: 1000, OverageStatus: "ENABLED"},
		config.Account{ID: "headroom", UsageCurrent: 500, UsageLimit: 1000},
	)
	if acc := p.GetNextForModel("model"); acc == nil || acc.ID != "headroom" {
		t.Fatalf("aggressive should prefer account with headroom over a full one, got %#v", acc)
	}
}

// TestBalanceModeHealthPrefersHealthiest verifies health mode spreads load to
// the healthiest/emptiest account (lower usage → higher health score).
func TestBalanceModeHealthPrefersHealthiest(t *testing.T) {
	initPoolTestConfig(t)
	if err := config.UpdateBalanceMode("health"); err != nil {
		t.Fatalf("set balance mode: %v", err)
	}
	p := newTestPool(
		config.Account{ID: "healthy", UsageCurrent: 100, UsageLimit: 1000, UsagePercent: 0.1},
		config.Account{ID: "loaded", UsageCurrent: 900, UsageLimit: 1000, UsagePercent: 0.9},
	)
	if acc := p.GetNextForModel("model"); acc == nil || acc.ID != "healthy" {
		t.Fatalf("health should pick the healthiest (lowest usage) account, got %#v", acc)
	}
}

func TestBalanceModeHealthIgnoresApiKeyUsageMetadata(t *testing.T) {
	p := newTestPool()
	apiKey := config.Account{ID: "api-key", KiroApiKey: "ksk_static", AccessToken: "ksk_static", AuthMethod: "api_key", UsageCurrent: 1000, UsageLimit: 1000, UsagePercent: 1}
	clean := apiKey
	clean.UsageCurrent = 0
	clean.UsageLimit = 0
	clean.UsagePercent = 0
	now := time.Now()
	if got, want := p.computeHealthScoreLocked(&apiKey, 0, 0, 0, now), p.computeHealthScoreLocked(&clean, 0, 0, 0, now); got != want {
		t.Fatalf("API-key health score changed by stale usage metadata: got %d want %d", got, want)
	}
}

// TestBalanceModeManagedRoundRobin verifies managed mode keeps plain weighted
// round-robin: successive picks rotate through every account.
func TestBalanceModeManagedRoundRobin(t *testing.T) {
	initPoolTestConfig(t)
	if err := config.UpdateBalanceMode("managed"); err != nil {
		t.Fatalf("set balance mode: %v", err)
	}
	p := newTestPool(
		config.Account{ID: "a"},
		config.Account{ID: "b"},
		config.Account{ID: "c"},
	)
	seen := make(map[string]bool)
	for i := 0; i < 3; i++ {
		acc := p.GetNextForModel("model")
		if acc == nil {
			t.Fatalf("managed round-robin returned nil on pick %d", i)
		}
		seen[acc.ID] = true
	}
	for _, id := range []string{"a", "b", "c"} {
		if !seen[id] {
			t.Fatalf("managed round-robin did not cover account %q over 3 picks (seen=%v)", id, seen)
		}
	}
}

// --- Sliding-window routing metrics (RoutingStatsWindow) ---

// TestRoutingStatsWindowCountsOnlyInWindow verifies samples outside the trailing
// window are excluded while in-window samples are aggregated by kind.
func TestRoutingStatsWindowCountsOnlyInWindow(t *testing.T) {
	p := newTestPool()
	now := time.Now()
	p.routeSamples = []routeSample{
		{at: now.Add(-90 * time.Second), kind: routeSampleProcessed, accountID: "a"}, // out of 60s window
		{at: now.Add(-30 * time.Second), kind: routeSampleProcessed, accountID: "a"},
		{at: now.Add(-10 * time.Second), kind: routeSampleProcessed, accountID: "b"},
		{at: now.Add(-5 * time.Second), kind: routeSampleEnqueued, waiting: 2},
		{at: now.Add(-3 * time.Second), kind: routeSampleRejected},
		{at: now.Add(-1 * time.Second), kind: routeSampleTimeout},
	}
	stats := p.RoutingStatsWindow(time.Minute, now)
	if got := stats["processedTotal"].(uint64); got != 2 {
		t.Fatalf("processedTotal = %d, want 2 (the -90s sample is out of window)", got)
	}
	if got := stats["enqueuedTotal"].(uint64); got != 1 {
		t.Fatalf("enqueuedTotal = %d, want 1", got)
	}
	if got := stats["rejectedTotal"].(uint64); got != 1 {
		t.Fatalf("rejectedTotal = %d, want 1", got)
	}
	if got := stats["timeoutTotal"].(uint64); got != 1 {
		t.Fatalf("timeoutTotal = %d, want 1", got)
	}
	per := stats["perAccountActive"].(map[string]int)
	if per["a"] != 1 || per["b"] != 1 {
		t.Fatalf("perAccountActive = %v, want a:1 b:1 (a's -90s sample excluded)", per)
	}
}

// TestRoutingStatsWindow5MinIncludesMore verifies the larger window admits
// samples the 1-minute window excludes.
func TestRoutingStatsWindow5MinIncludesMore(t *testing.T) {
	p := newTestPool()
	now := time.Now()
	p.routeSamples = []routeSample{
		{at: now.Add(-90 * time.Second), kind: routeSampleProcessed, accountID: "a"},
		{at: now.Add(-30 * time.Second), kind: routeSampleProcessed, accountID: "a"},
	}
	if got := p.RoutingStatsWindow(time.Minute, now)["processedTotal"].(uint64); got != 1 {
		t.Fatalf("1m processedTotal = %d, want 1", got)
	}
	if got := p.RoutingStatsWindow(5*time.Minute, now)["processedTotal"].(uint64); got != 2 {
		t.Fatalf("5m processedTotal = %d, want 2", got)
	}
}

// TestRoutingStatsWindowActivePeak verifies active/waiting report the in-window
// peak from sample snapshots.
func TestRoutingStatsWindowActivePeak(t *testing.T) {
	p := newTestPool()
	now := time.Now()
	p.routeSamples = []routeSample{
		{at: now.Add(-40 * time.Second), kind: routeSampleProcessed, accountID: "a", active: 2},
		{at: now.Add(-20 * time.Second), kind: routeSampleProcessed, accountID: "b", active: 5}, // peak
		{at: now.Add(-10 * time.Second), kind: routeSampleProcessed, accountID: "c", active: 3},
		{at: now.Add(-15 * time.Second), kind: routeSampleEnqueued, waiting: 4},
	}
	stats := p.RoutingStatsWindow(time.Minute, now)
	if got := stats["active"].(int); got != 5 {
		t.Fatalf("active peak = %d, want 5", got)
	}
	if got := stats["waiting"].(int); got != 4 {
		t.Fatalf("waiting peak = %d, want 4", got)
	}
}

// TestRoutingStatsWindowPeakFallsBackToCurrent verifies that when no rising-edge
// sample exists in the window (e.g. a long request spanning the whole window),
// the peak falls back to the current instantaneous gauge rather than showing 0.
func TestRoutingStatsWindowPeakFallsBackToCurrent(t *testing.T) {
	p := newTestPool()
	now := time.Now()
	// No in-window samples at all, but a request is currently active.
	p.routeGlobalActive = 3
	p.routeWaiting = 1
	stats := p.RoutingStatsWindow(time.Minute, now)
	if got := stats["active"].(int); got != 3 {
		t.Fatalf("active = %d, want 3 (fallback to current gauge)", got)
	}
	if got := stats["waiting"].(int); got != 1 {
		t.Fatalf("waiting = %d, want 1 (fallback to current gauge)", got)
	}
}

// TestRoutingStatsWindowStickyClassification verifies sticky hit/miss/divert are
// counted from processed-sample flags.
func TestRoutingStatsWindowStickyClassification(t *testing.T) {
	p := newTestPool()
	now := time.Now()
	p.routeSamples = []routeSample{
		{at: now.Add(-30 * time.Second), kind: routeSampleProcessed, accountID: "a", stickyHit: true},
		{at: now.Add(-20 * time.Second), kind: routeSampleProcessed, accountID: "b", stickyMiss: true},
		{at: now.Add(-10 * time.Second), kind: routeSampleProcessed, accountID: "c", stickyDivert: true},
		{at: now.Add(-5 * time.Second), kind: routeSampleProcessed, accountID: "a", stickyHit: true},
	}
	stats := p.RoutingStatsWindow(time.Minute, now)
	if got := stats["stickyHitTotal"].(uint64); got != 2 {
		t.Fatalf("stickyHitTotal = %d, want 2", got)
	}
	if got := stats["stickyMissTotal"].(uint64); got != 1 {
		t.Fatalf("stickyMissTotal = %d, want 1", got)
	}
	if got := stats["stickyDivertTotal"].(uint64); got != 1 {
		t.Fatalf("stickyDivertTotal = %d, want 1", got)
	}
}

// TestPruneRouteSamplesDropsOld verifies the rolling log trims entries older than
// routeSampleWindow on append.
func TestPruneRouteSamplesDropsOld(t *testing.T) {
	p := newTestPool()
	now := time.Now()
	p.routeSamples = []routeSample{
		{at: now.Add(-routeSampleWindow - time.Minute), kind: routeSampleProcessed, accountID: "old"},
		{at: now.Add(-routeSampleWindow - time.Second), kind: routeSampleProcessed, accountID: "old2"},
	}
	p.appendRouteSampleLocked(routeSample{at: now, kind: routeSampleProcessed, accountID: "new"})
	if len(p.routeSamples) != 1 {
		t.Fatalf("after prune len = %d, want 1 (only the new sample)", len(p.routeSamples))
	}
	if p.routeSamples[0].accountID != "new" {
		t.Fatalf("remaining sample = %q, want \"new\"", p.routeSamples[0].accountID)
	}
}
