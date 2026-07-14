// Package pool 账号池管理
// 实现轮询负载均衡、错误冷却、Token 刷新
package pool

import (
	"context"
	"errors"
	"kiro-go/config"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const tokenRefreshSkewSeconds int64 = 120
const routingEventWindow = time.Hour

var (
	ErrRoutingQueueFull    = errors.New("routing queue full")
	ErrRoutingQueueTimeout = errors.New("routing queue timeout")
	ErrRoutingUnavailable  = errors.New("no available accounts")
)

type requestEvent struct {
	at      time.Time
	is429   bool
	isError bool
}

// routeSampleWindow bounds how long routing-decision samples are retained. It
// must cover the largest selectable live window (5min) plus headroom so
// sliding-window aggregation never runs short of data.
const routeSampleWindow = 6 * time.Minute

type routeSampleKind uint8

const (
	routeSampleProcessed routeSampleKind = iota // successfully acquired a route slot
	routeSampleEnqueued                          // had to wait in the queue at least once
	routeSampleRejected                          // rejected because the queue was full
	routeSampleTimeout                           // timed out while waiting in the queue
)

// routeSample is a single routing-decision event with the gauge snapshot taken
// at the moment it occurred. Sliding-window aggregation derives windowed
// counters, per-account request counts and active/waiting peaks from these.
type routeSample struct {
	at           time.Time
	kind         routeSampleKind
	accountID    string // selected account (routeSampleProcessed only)
	stickyHit    bool
	stickyMiss   bool
	stickyDivert bool
	active       int // global active snapshot after increment (routeSampleProcessed)
	waiting      int // global waiting snapshot after increment (routeSampleEnqueued)
}

type AccountHealthSnapshot struct {
	ID           string
	Requests     int     `json:"requests"`
	QuotaErrors  int     `json:"quotaErrors"`
	ErrorCount   int     `json:"errorCount"`
	Rate429      float64 `json:"rate429"`
	StablePool   bool    `json:"stablePool"`
	HealthScore  int     `json:"healthScore"`
	LastErrorAt  int64   `json:"lastErrorAt,omitempty"`
	CoolingUntil int64   `json:"coolingUntil,omitempty"`
	CanRoute     bool    `json:"canRoute"`
	ModeBucket   string  `json:"modeBucket,omitempty"`
}

// stickyEntry maps a conversation affinity key to the account it was pinned to,
// with an expiry so stale conversations don't keep an account pinned forever and
// the sticky map cannot grow without bound.
type stickyEntry struct {
	accountID string
	expiresAt time.Time
}

const (
	// stickyTTL is how long a conversation stays pinned to an account after its
	// last request. Aligned with the maximum prompt-cache window (1h) so the pin
	// outlives the cache it is meant to reuse. Each hit refreshes the expiry.
	stickyTTL = time.Hour
	// stickyMaxEntries caps the sticky map size to bound memory under many
	// distinct conversations; when exceeded the soonest-to-expire entries are
	// evicted first.
	stickyMaxEntries = 10000
)

// AccountPool 账号池
type AccountPool struct {
	mu                      sync.RWMutex
	accounts                []config.Account
	totalAccounts           int
	currentIndex            uint64
	cooldowns               map[string]time.Time       // 账号冷却时间
	errorCounts             map[string]int             // 连续错误计数
	modelLists              map[string]map[string]bool // accountID → set of modelIDs (from ListAvailableModels)
	requestLog              map[string][]requestEvent  // accountID → rolling request events (last hour)
	lastErrorAt             map[string]time.Time       // accountID → last error timestamp
	routeActiveByAccount    map[string]int
	routeLastStartByAccount map[string]time.Time
	routeStickyByKey        map[string]stickyEntry
	routeGlobalActive       int
	routeWaiting            int
	routeNotify             chan struct{}
	lastAutoRestoreRefresh  time.Time
	autoRestoreRefresh      bool

	// Cumulative routing counters (lifetime since process start).
	routeEnqueuedTotal  uint64 // requests that had to wait in the queue at least once
	routeProcessedTotal uint64 // requests that successfully acquired a route slot
	routeRejectedTotal  uint64 // requests rejected because the queue was full
	routeTimeoutTotal   uint64 // requests that timed out while waiting in the queue

	// Sticky (conversation-affinity) outcome counters. Only requests carrying a
	// non-empty affinity key are counted, so hit+miss+divert == affinity-keyed
	// successful acquisitions.
	routeStickyHitTotal    uint64 // had a pin and routed to it (prompt cache reused)
	routeStickyMissTotal   uint64 // no pin yet: new conversation established one
	routeStickyDivertTotal uint64 // had a pin but routed elsewhere (busy/unhealthy)

	// routeRequestTotal is incremented on every successful route acquisition,
	// regardless of sticky status. Used by the live panel to compute RPM.
	routeRequestTotal uint64

	// routeSamples is a rolling log of routing-decision events (last
	// routeSampleWindow) used for sliding-window live metrics. Guarded by mu.
	routeSamples []routeSample
}

var (
	pool     *AccountPool
	poolOnce sync.Once
)

// GetPool 获取全局账号池单例
func GetPool() *AccountPool {
	poolOnce.Do(func() {
		pool = &AccountPool{
			cooldowns:               make(map[string]time.Time),
			errorCounts:             make(map[string]int),
			modelLists:              make(map[string]map[string]bool),
			requestLog:              make(map[string][]requestEvent),
			lastErrorAt:             make(map[string]time.Time),
			routeActiveByAccount:    make(map[string]int),
			routeLastStartByAccount: make(map[string]time.Time),
			routeStickyByKey:        make(map[string]stickyEntry),
			routeNotify:             make(chan struct{}),
			autoRestoreRefresh:      true,
		}
		pool.Reload()
	})
	return pool
}

// Reload 从配置重新加载账号
// 构建加权列表：weight<=1 出现 1 次，weight>=2 出现 weight 次。
// 额度耗尽的账号是否参与调度由上游 OverageStatus 决定（DISABLED → 跳过）。
func (p *AccountPool) Reload() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reloadLocked()
}

func (p *AccountPool) reloadLocked() {
	enabled := config.GetEnabledAccounts()
	allowOverUsage := config.GetAllowOverUsage()
	var weighted []config.Account
	for _, a := range enabled {
		if isQuotaBlocked(a, allowOverUsage) {
			continue
		}
		w := effectiveWeight(a.Weight)
		for j := 0; j < w; j++ {
			weighted = append(weighted, a)
		}
	}
	p.accounts = weighted
	p.totalAccounts = len(enabled)
	p.ensureRuntimeMapsLocked()
	p.notifyRouteWaitersLocked()
}

func (p *AccountPool) refreshAutoRestoredAccounts() {
	if !p.autoRestoreRefresh {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.lastAutoRestoreRefresh.IsZero() && time.Since(p.lastAutoRestoreRefresh) < time.Minute {
		return
	}
	p.lastAutoRestoreRefresh = time.Now()
	p.reloadLocked()
}

// GetNext 获取下一个可用账号
func (p *AccountPool) GetNext() *config.Account {
	return p.GetNextExcluding(nil)
}

// GetNextExcluding 获取下一个可用账号（加权轮询），并跳过指定账号。
func (p *AccountPool) GetNextExcluding(excluded map[string]bool) *config.Account {
	p.refreshAutoRestoredAccounts()
	p.mu.RLock()
	defer p.mu.RUnlock()
	return copyAccount(p.getNextLockedExcept("", excluded))
}

// GetNextExcept 获取下一个可用账号，并排除已尝试的账号。
func (p *AccountPool) GetNextExcept(exclude map[string]bool) *config.Account {
	p.refreshAutoRestoredAccounts()
	p.mu.RLock()
	defer p.mu.RUnlock()
	return copyAccount(p.getNextLockedExcept("", exclude))
}

// SetModelList 缓存账号支持的模型集合（由 handler 在刷新后调用）
func (p *AccountPool) SetModelList(accountID string, modelIDs []string) {
	set := make(map[string]bool, len(modelIDs))
	for _, id := range modelIDs {
		set[strings.ToLower(strings.TrimSpace(id))] = true
	}
	p.mu.Lock()
	p.modelLists[accountID] = set
	p.mu.Unlock()
}

// GetModelList 返回该账号缓存的模型 ID 列表（供 admin API 使用）。
// 若尚无缓存则返回空切片。
func (p *AccountPool) GetModelList(accountID string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	set, ok := p.modelLists[accountID]
	if !ok || len(set) == 0 {
		return []string{}
	}
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	return ids
}

// accountHasModel 检查账号是否支持指定模型。
// 若该账号尚无模型列表（冷启动），视为支持所有模型。
func (p *AccountPool) accountHasModel(accountID, model string) bool {
	list, ok := p.modelLists[accountID]
	if !ok || len(list) == 0 {
		return true // 冷启动：列表未就绪，乐观放行
	}
	return list[strings.ToLower(strings.TrimSpace(model))]
}

// GetNextForModel 获取下一个支持指定模型的可用账号。
// model 应为去掉 thinking 后缀的实际模型名。
// 若无账号有该模型列表数据，行为与 GetNext 相同（乐观路由）。
func (p *AccountPool) GetNextForModel(model string) *config.Account {
	return p.GetNextForModelExcluding(model, nil)
}

// GetNextForModelExcluding 获取下一个支持指定模型的可用账号，并跳过指定账号。
func (p *AccountPool) GetNextForModelExcluding(model string, excluded map[string]bool) *config.Account {
	p.refreshAutoRestoredAccounts()
	p.mu.RLock()
	defer p.mu.RUnlock()
	return copyAccount(p.getNextLockedExcept(model, excluded))
}

// GetNextForModelExcept 获取下一个支持指定模型的可用账号，并排除已尝试账号。
func (p *AccountPool) GetNextForModelExcept(model string, exclude map[string]bool) *config.Account {
	p.refreshAutoRestoredAccounts()
	p.mu.RLock()
	defer p.mu.RUnlock()
	return copyAccount(p.getNextLockedExcept(model, exclude))
}

func needsTokenRefresh(acc config.Account, now time.Time) bool {
	return acc.ExpiresAt > 0 && now.Unix() > acc.ExpiresAt-tokenRefreshSkewSeconds
}

func hasRefreshToken(acc config.Account) bool {
	return strings.TrimSpace(acc.RefreshToken) != ""
}

func canRouteByToken(acc config.Account, now time.Time) bool {
	return !needsTokenRefresh(acc, now) || hasRefreshToken(acc)
}

// copyAccount returns a value copy of the account so callers receive a snapshot
// detached from the pool's backing slice. config.Account is a pure data struct
// (no mutex/sync fields), so a shallow copy is safe and prevents callers from
// racing with in-pool mutations (token refresh, stats updates, reloads).
func copyAccount(acc *config.Account) *config.Account {
	if acc == nil {
		return nil
	}
	cp := *acc
	return &cp
}

// effectiveUsageFraction reports how "full" an account is on a 0..1+ scale.
// When overage is in effect (global AllowOverUsage on, or the account's upstream
// OverageStatus=ENABLED) the fraction is measured against the *total* budget
// (subscription limit + overage cap) rather than the subscription limit alone.
// This prevents an account that exceeded its subscription quota but still has
// plenty of overage headroom (e.g. 3118/1000 subscription == 312%, yet only
// 28% of a 1000+10000 total budget) from being treated as full.
func effectiveUsageFraction(acc config.Account, allowOverUsage bool) float64 {
	if acc.UsageLimit <= 0 {
		return 0 // unknown / unlimited → treat as empty
	}
	budget := acc.UsageLimit
	if (allowOverUsage || strings.EqualFold(acc.OverageStatus, "ENABLED")) && acc.OverageCap > 0 {
		budget = acc.UsageLimit + acc.OverageCap
	}
	return acc.UsageCurrent / budget
}

// subscriptionTierRank returns a bonus that biases candidateOrderLocked
// toward free-tier accounts so they are consumed before paid subscriptions.
// The bonus is large enough (2.0) to dominate the 0..1 usage-fraction range.
//   FREE      → +2.0  (consume first)
//   (unknown) → +1.0  (neutral — no subscription info yet)
//   PRO       →  0.0
//   PRO_PLUS  → -1.0
//   POWER     → -2.0  (consume last)
func subscriptionTierRank(subscriptionType string) float64 {
	switch strings.ToUpper(strings.TrimSpace(subscriptionType)) {
	case "FREE":
		return 2.0
	case "PRO":
		return 0.0
	case "PRO_PLUS":
		return -1.0
	case "POWER":
		return -2.0
	default:
		return 1.0 // unknown → between FREE and PRO
	}
}

// modeRankLocked returns a preference score for acc under the given balance
// mode; higher means the account should be picked sooner. Only "aggressive" and
// "health" use ranking — "managed" keeps plain weighted round-robin and never
// calls this. Caller must hold the pool lock (reads runtime maps).
func (p *AccountPool) modeRankLocked(acc *config.Account, mode string, allowOverUsage bool, now time.Time) float64 {
	switch mode {
	case "aggressive":
		// Concentrate load: prefer the account that is most utilized but still
		// has headroom, so one account fills up before spilling to the next.
		// Subscription-tier bonus biases the ranking so free-tier accounts
		// are consumed first, preserving paid quota for when free is exhausted.
		frac := effectiveUsageFraction(*acc, allowOverUsage)
		rank := frac
		if frac >= 1.0 {
			rank = -frac // genuinely full: rank below every account with headroom
		}
		return rank + subscriptionTierRank(acc.SubscriptionType)
	case "health":
		// Spread load to the healthiest/emptiest account.
		reqs, qe, rate := p.getRecentStatsLocked(acc.ID, now)
		return float64(p.computeHealthScoreLocked(acc, reqs, qe, rate, now))
	default:
		return 0
	}
}

// candidateOrderLocked returns indices into p.accounts in the order they should
// be tried for the given balance mode, de-duplicated by account ID (the backing
// slice repeats an account `weight` times). Caller must hold the pool lock.
//
//   - managed: weighted round-robin. currentIndex advances by one so successive
//     calls rotate the starting point, preserving the existing fair-rotation
//     behavior plus weight bias (heavier accounts occupy more slots).
//   - health / aggressive: stable sort by modeRankLocked descending.
func (p *AccountPool) candidateOrderLocked(mode string, allowOverUsage bool, now time.Time) []int {
	n := len(p.accounts)
	if n == 0 {
		return nil
	}

	// Base order: weighted round-robin from the next rotating start point,
	// de-duplicated by account ID (the backing slice repeats an account
	// `weight` times). This rotation is the tie-breaker that keeps load
	// spreading across equally-ranked accounts on successive calls — without
	// it, a stable sort would always pick the same account when ranks tie
	// (e.g. a cold pool where every account has zero usage / equal health).
	start := int(atomic.AddUint64(&p.currentIndex, 1) % uint64(n))
	order := make([]int, 0, n)
	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		idx := (start + i) % n
		id := p.accounts[idx].ID
		if seen[id] {
			continue
		}
		seen[id] = true
		order = append(order, idx)
	}

	if mode != "health" && mode != "aggressive" {
		return order // managed: plain weighted round-robin
	}

	// health / aggressive: stable-sort the round-robin base by rank descending.
	// Stability preserves the rotating order among equally-ranked accounts
	// (load spreads), while higher-ranked accounts are tried first.
	sort.SliceStable(order, func(a, b int) bool {
		return p.modeRankLocked(&p.accounts[order[a]], mode, allowOverUsage, now) >
			p.modeRankLocked(&p.accounts[order[b]], mode, allowOverUsage, now)
	})
	return order
}

func (p *AccountPool) getNextLockedExcept(model string, exclude map[string]bool) *config.Account {
	if len(p.accounts) == 0 {
		return nil
	}

	allowOverUsage := config.GetAllowOverUsage()
	now := time.Now()
	mode := config.GetBalanceMode()

	// Try accounts in mode-preferred order (managed = weighted round-robin).
	for _, idx := range p.candidateOrderLocked(mode, allowOverUsage, now) {
		acc := &p.accounts[idx]
		if !p.canRouteAccountLocked(acc, model, exclude, allowOverUsage, now, true) {
			continue
		}
		return acc
	}

	// fallback：找冷却时间最短且仍满足 token/model/额度约束的账号
	var best *config.Account
	var earliest time.Time
	seenFallback := make(map[string]bool)
	for i := range p.accounts {
		acc := &p.accounts[i]
		if seenFallback[acc.ID] {
			continue
		}
		seenFallback[acc.ID] = true
		if exclude != nil && exclude[acc.ID] {
			continue
		}
		if model != "" && !p.accountHasModel(acc.ID, model) {
			continue
		}
		if !canRouteByToken(*acc, now) {
			continue
		}
		if isQuotaBlocked(*acc, allowOverUsage) {
			continue
		}
		if cooldown, ok := p.cooldowns[acc.ID]; ok && now.Before(cooldown) {
			if best == nil || cooldown.Before(earliest) {
				best = acc
				earliest = cooldown
			}
		} else {
			return acc
		}
	}
	return best
}

func (p *AccountPool) canRouteAccountLocked(acc *config.Account, model string, exclude map[string]bool, allowOverUsage bool, now time.Time, skipCooling bool) bool {
	if acc == nil {
		return false
	}
	if exclude != nil && exclude[acc.ID] {
		return false
	}
	if model != "" && !p.accountHasModel(acc.ID, model) {
		return false
	}
	if skipCooling {
		if cooldown, ok := p.cooldowns[acc.ID]; ok && now.Before(cooldown) {
			return false
		}
	}
	if !canRouteByToken(*acc, now) {
		return false
	}
	if isQuotaBlocked(*acc, allowOverUsage) {
		return false
	}
	return true
}

type routingTryResult struct {
	account *config.Account
	wait    time.Duration
	notify  <-chan struct{}
	busy    bool
}

func (p *AccountPool) AcquireForModel(ctx context.Context, model string, excluded map[string]bool, affinityKey string) (*config.Account, func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	rc := config.GetRoutingConcurrencyConfig()
	if !rc.Enabled {
		// Concurrency limiting is off, but conversation affinity still applies so
		// multi-turn conversations reuse the same account's prompt cache. Falls
		// back to weighted round-robin when sticky is disabled or the key is empty.
		var acc *config.Account
		if rc.StickyAccount && strings.TrimSpace(affinityKey) != "" {
			p.refreshAutoRestoredAccounts()
			allowOverUsage := config.GetAllowOverUsage()
			now := time.Now()
			p.mu.Lock()
			p.ensureRuntimeMapsLocked()
			acc = copyAccount(p.getStickyOrNextLocked(model, excluded, affinityKey, allowOverUsage, now))
			p.mu.Unlock()
		} else {
			acc = p.GetNextForModelExcluding(model, excluded)
		}
		if acc == nil {
			return nil, nil, ErrRoutingUnavailable
		}
		atomic.AddUint64(&p.routeProcessedTotal, 1)
		// Concurrency limiting is off, so there is no active/waiting gauge to
		// snapshot (both 0); still record the processed event so windowed RPM,
		// processed count and per-account distribution are populated when the
		// bypass path is taken.
		p.recordRouteSample(routeSample{kind: routeSampleProcessed, accountID: acc.ID})
		return acc, func() {}, nil
	}

	queueTimeout := time.Duration(rc.GlobalQueueTimeoutMs) * time.Millisecond
	if queueTimeout <= 0 {
		queueTimeout = 30 * time.Second
	}
	deadline := time.NewTimer(queueTimeout)
	defer deadline.Stop()
	queued := false
	defer func() {
		if queued {
			p.mu.Lock()
			if p.routeWaiting > 0 {
				p.routeWaiting--
			}
			p.mu.Unlock()
		}
	}()

	for {
		res, err := p.tryAcquireForModel(model, excluded, affinityKey, rc)
		if err != nil {
			return nil, nil, err
		}
		if res.account != nil {
			atomic.AddUint64(&p.routeProcessedTotal, 1)
			return res.account, p.releaseRouteFunc(res.account.ID), nil
		}
		if !res.busy {
			return nil, nil, ErrRoutingUnavailable
		}
		if !queued {
			if rc.GlobalQueueSize <= 0 {
				atomic.AddUint64(&p.routeRejectedTotal, 1)
				p.recordRouteSample(routeSample{kind: routeSampleRejected})
				return nil, nil, ErrRoutingQueueFull
			}
			p.mu.Lock()
			if p.routeWaiting >= rc.GlobalQueueSize {
				p.mu.Unlock()
				atomic.AddUint64(&p.routeRejectedTotal, 1)
				p.recordRouteSample(routeSample{kind: routeSampleRejected})
				return nil, nil, ErrRoutingQueueFull
			}
			p.routeWaiting++
			waitingSnapshot := p.routeWaiting
			queued = true
			p.mu.Unlock()
			atomic.AddUint64(&p.routeEnqueuedTotal, 1)
			p.recordRouteSample(routeSample{kind: routeSampleEnqueued, waiting: waitingSnapshot})
		}

		var intervalC <-chan time.Time
		var timer *time.Timer
		if res.wait > 0 {
			timer = time.NewTimer(res.wait)
			intervalC = timer.C
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return nil, nil, ctx.Err()
		case <-deadline.C:
			if timer != nil {
				timer.Stop()
			}
			atomic.AddUint64(&p.routeTimeoutTotal, 1)
			p.recordRouteSample(routeSample{kind: routeSampleTimeout})
			return nil, nil, ErrRoutingQueueTimeout
		case <-res.notify:
			if timer != nil {
				timer.Stop()
			}
		case <-intervalC:
		}
	}
}

// lookupStickyLocked returns the account ID pinned to affinityKey if it exists
// and has not expired. Expired entries are removed. Caller must hold p.mu.
func (p *AccountPool) lookupStickyLocked(affinityKey string, now time.Time) string {
	if strings.TrimSpace(affinityKey) == "" {
		return ""
	}
	entry, ok := p.routeStickyByKey[affinityKey]
	if !ok {
		return ""
	}
	if !entry.expiresAt.After(now) {
		delete(p.routeStickyByKey, affinityKey)
		return ""
	}
	return entry.accountID
}

// setStickyLocked pins affinityKey to accountID with a refreshed TTL, pruning
// expired entries and evicting the soonest-to-expire entry when over capacity.
// Caller must hold p.mu.
func (p *AccountPool) setStickyLocked(affinityKey, accountID string, now time.Time) {
	if strings.TrimSpace(affinityKey) == "" || accountID == "" {
		return
	}
	// Refreshing an existing key never grows the map, so only prune/evict when
	// inserting a genuinely new key.
	if _, exists := p.routeStickyByKey[affinityKey]; !exists {
		p.pruneStickyLocked(now)
		if len(p.routeStickyByKey) >= stickyMaxEntries {
			p.evictSoonestStickyLocked()
		}
	}
	p.routeStickyByKey[affinityKey] = stickyEntry{accountID: accountID, expiresAt: now.Add(stickyTTL)}
}

// pruneStickyLocked drops all expired sticky entries. Caller must hold p.mu.
func (p *AccountPool) pruneStickyLocked(now time.Time) {
	for key, entry := range p.routeStickyByKey {
		if !entry.expiresAt.After(now) {
			delete(p.routeStickyByKey, key)
		}
	}
}

// evictSoonestStickyLocked removes the entry with the earliest expiry to keep
// the map within stickyMaxEntries. Caller must hold p.mu.
func (p *AccountPool) evictSoonestStickyLocked() {
	var soonestKey string
	var soonest time.Time
	first := true
	for key, entry := range p.routeStickyByKey {
		if first || entry.expiresAt.Before(soonest) {
			soonestKey = key
			soonest = entry.expiresAt
			first = false
		}
	}
	if soonestKey != "" {
		delete(p.routeStickyByKey, soonestKey)
	}
}

// getStickyOrNextLocked selects an account honoring conversation affinity even
// when concurrency limiting is disabled: it prefers the account previously
// pinned to affinityKey (if routable), otherwise falls back to weighted
// round-robin and pins the chosen account. Caller must hold p.mu.
func (p *AccountPool) getStickyOrNextLocked(model string, excluded map[string]bool, affinityKey string, allowOverUsage bool, now time.Time) *config.Account {
	hadPin := false
	if stickyID := p.lookupStickyLocked(affinityKey, now); stickyID != "" {
		hadPin = true
		for i := range p.accounts {
			if p.accounts[i].ID != stickyID {
				continue
			}
			if p.canRouteAccountLocked(&p.accounts[i], model, excluded, allowOverUsage, now, true) {
				p.setStickyLocked(affinityKey, stickyID, now) // refresh TTL
				atomic.AddUint64(&p.routeStickyHitTotal, 1)
				atomic.AddUint64(&p.routeRequestTotal, 1)
				return &p.accounts[i]
			}
			break
		}
	}
	acc := p.getNextLockedExcept(model, excluded)
	if acc != nil {
		p.setStickyLocked(affinityKey, acc.ID, now)
		if hadPin {
			atomic.AddUint64(&p.routeStickyDivertTotal, 1)
		} else {
			atomic.AddUint64(&p.routeStickyMissTotal, 1)
		}
		atomic.AddUint64(&p.routeRequestTotal, 1)
	}
	return acc
}

func (p *AccountPool) tryAcquireForModel(model string, excluded map[string]bool, affinityKey string, rc config.RoutingConcurrencyConfig) (routingTryResult, error) {
	p.refreshAutoRestoredAccounts()
	allowOverUsage := config.GetAllowOverUsage()
	now := time.Now()

	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureRuntimeMapsLocked()
	if len(p.accounts) == 0 {
		return routingTryResult{}, ErrRoutingUnavailable
	}
	if rc.GlobalMaxConcurrent > 0 && p.routeGlobalActive >= rc.GlobalMaxConcurrent {
		return routingTryResult{busy: true, notify: p.routeNotify}, nil
	}

	var earliestWait time.Duration
	busySeen := false

	// tryAccount attempts to reserve a slot on acc. isStickyTarget marks the
	// account currently pinned to affinityKey: only that account (or the first
	// pin for a new conversation) updates the sticky map. Overflow picks to other
	// accounts must NOT overwrite the pin, so the conversation returns to its
	// cache-warm account once it frees up.
	tryAccount := func(acc *config.Account, isStickyTarget bool) (*config.Account, bool) {
		if acc == nil {
			return nil, false
		}
		// --------------- model compatibility ---------------
		if model != "" && !p.accountHasModel(acc.ID, model) {
			return nil, false // truly incompatible — never wait
		}

		// --------------- transient unavailability ---------------
		if excluded != nil && excluded[acc.ID] {
			return nil, false // handler-level exclusion; pool cooldown handles pacing
		}
		if cooldown, ok := p.cooldowns[acc.ID]; ok && now.Before(cooldown) {
			busySeen = true
			if wait := cooldown.Sub(now); earliestWait <= 0 || wait < earliestWait {
				earliestWait = wait
			}
			return nil, true
		}
		if !canRouteByToken(*acc, now) {
			return nil, false // token expired — wait for refresh
		}
		if isQuotaBlocked(*acc, allowOverUsage) {
			return nil, false // quota exhausted — wait for reset
		}

		// --------------- capacity limits ---------------
		if p.routeActiveByAccount[acc.ID] >= rc.PerAccountMaxConcurrent {
			busySeen = true
			return nil, true
		}
		if rc.PerAccountMinIntervalMs > 0 {
			minInterval := time.Duration(rc.PerAccountMinIntervalMs) * time.Millisecond
			if lastStart, ok := p.routeLastStartByAccount[acc.ID]; ok {
				if wait := minInterval - now.Sub(lastStart); wait > 0 {
					busySeen = true
					if earliestWait <= 0 || wait < earliestWait {
						earliestWait = wait
					}
					return nil, true
				}
			}
		}

		// --------------- acquire ---------------
		p.routeGlobalActive++
		p.routeActiveByAccount[acc.ID]++
		p.routeLastStartByAccount[acc.ID] = now
		if rc.StickyAccount && isStickyTarget {
			p.setStickyLocked(affinityKey, acc.ID, now)
		}
		return acc, true
	}

	stickyID := ""
	if rc.StickyAccount {
		stickyID = p.lookupStickyLocked(affinityKey, now)
	}
	if stickyID != "" {
		for i := range p.accounts {
			if p.accounts[i].ID != stickyID {
				continue
			}
			if acc, considered := tryAccount(&p.accounts[i], true); acc != nil || considered {
				if acc != nil || !rc.OverflowToOtherAccounts {
					if acc != nil {
						atomic.AddUint64(&p.routeStickyHitTotal, 1)
						atomic.AddUint64(&p.routeRequestTotal, 1)
						p.appendRouteSampleLocked(routeSample{
							kind:      routeSampleProcessed,
							accountID: acc.ID,
							stickyHit: true,
							active:    p.routeGlobalActive,
						})
					}
					return routingTryResult{account: copyAccount(acc), busy: acc == nil, wait: earliestWait, notify: p.routeNotify}, nil
				}
				break
			}
			if !rc.OverflowToOtherAccounts {
				return routingTryResult{}, ErrRoutingUnavailable
			}
			break
		}
	}

	// New conversation (no existing pin) may establish one; overflow from an
	// existing pin must not, so it stays bound to the cache-warm account.
	canPinHere := rc.StickyAccount && stickyID == ""
	mode := config.GetBalanceMode()
	// candidateOrderLocked is already de-duplicated by account ID, so the sticky
	// target (already tried above) is the only index we still need to skip.
	for _, idx := range p.candidateOrderLocked(mode, allowOverUsage, now) {
		acc := &p.accounts[idx]
		if stickyID != "" && acc.ID == stickyID {
			continue
		}
		if selected, _ := tryAccount(acc, canPinHere); selected != nil {
			stickyMiss := false
			stickyDivert := false
			if strings.TrimSpace(affinityKey) != "" {
				if stickyID != "" {
					// Had a pin but it was busy/unhealthy → routed elsewhere this turn.
					atomic.AddUint64(&p.routeStickyDivertTotal, 1)
					stickyDivert = true
				} else if rc.StickyAccount {
					// New conversation established its pin here.
					atomic.AddUint64(&p.routeStickyMissTotal, 1)
					stickyMiss = true
				}
			}
			atomic.AddUint64(&p.routeRequestTotal, 1)
			p.appendRouteSampleLocked(routeSample{
				kind:         routeSampleProcessed,
				accountID:    selected.ID,
				stickyMiss:   stickyMiss,
				stickyDivert: stickyDivert,
				active:       p.routeGlobalActive,
			})
			return routingTryResult{account: copyAccount(selected)}, nil
		}
	}
	// All candidates exhausted. Only transient cooldowns (which expire on their
	// own) set busySeen above, so the queue waits just long enough for them to
	// recover. Accounts blocked by handler-level exclusion, quota exhaustion or
	// token expiry do NOT trigger waiting: those states do not self-heal within
	// the queue window, so failing fast with ErrRoutingUnavailable is correct
	// and avoids holding the caller for the full 30s queue timeout.
	return routingTryResult{busy: busySeen, wait: earliestWait, notify: p.routeNotify}, nil
}

func (p *AccountPool) releaseRouteFunc(accountID string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.ensureRuntimeMapsLocked()
			if p.routeActiveByAccount[accountID] > 0 {
				p.routeActiveByAccount[accountID]--
				if p.routeActiveByAccount[accountID] == 0 {
					delete(p.routeActiveByAccount, accountID)
				}
			}
			if p.routeGlobalActive > 0 {
				p.routeGlobalActive--
			}
			p.notifyRouteWaitersLocked()
		})
	}
}

func (p *AccountPool) notifyRouteWaitersLocked() {
	old := p.routeNotify
	p.routeNotify = make(chan struct{})
	close(old)
}

func (p *AccountPool) RoutingStats() map[string]interface{} {
	p.mu.RLock()
	defer p.mu.RUnlock()
	perAccount := make(map[string]int, len(p.routeActiveByAccount))
	for id, n := range p.routeActiveByAccount {
		if n > 0 {
			perAccount[id] = n
		}
	}
	return map[string]interface{}{
		"active":            p.routeGlobalActive,
		"waiting":           p.routeWaiting,
		"perAccountActive":  perAccount,
		"enqueuedTotal":     atomic.LoadUint64(&p.routeEnqueuedTotal),
		"processedTotal":    atomic.LoadUint64(&p.routeProcessedTotal),
		"rejectedTotal":     atomic.LoadUint64(&p.routeRejectedTotal),
		"timeoutTotal":      atomic.LoadUint64(&p.routeTimeoutTotal),
		"stickyHitTotal":    atomic.LoadUint64(&p.routeStickyHitTotal),
		"stickyMissTotal":   atomic.LoadUint64(&p.routeStickyMissTotal),
		"stickyDivertTotal": atomic.LoadUint64(&p.routeStickyDivertTotal),
		"requestTotal":      atomic.LoadUint64(&p.routeRequestTotal),
		// True sliding-window RPM: count of routing events in the trailing 60s,
		// computed server-side from per-request timestamps so it is stable
		// regardless of the client's polling interval.
		"requestsLastMinute": p.requestsInWindowLocked(time.Minute, time.Now()),
	}
}

func (p *AccountPool) getRecentStatsLocked(id string, now time.Time) (requests int, quotaErrors int, rate429 float64) {
	events := p.requestLog[id]
	if len(events) == 0 {
		return 0, 0, 0
	}
	cutoff := now.Add(-routingEventWindow)
	for _, event := range events {
		if event.at.Before(cutoff) {
			continue
		}
		requests++
		if event.is429 {
			quotaErrors++
		}
	}
	if requests > 0 {
		rate429 = float64(quotaErrors) / float64(requests)
	}
	return
}

// requestsInWindowLocked counts routing events across all accounts within the
// trailing window ending at now. This is a true sliding-window measure: every
// request carries its own timestamp, so the count does not depend on the
// caller's polling cadence and does not flicker between polls.
func (p *AccountPool) requestsInWindowLocked(window time.Duration, now time.Time) int {
	cutoff := now.Add(-window)
	count := 0
	for _, events := range p.requestLog {
		for i := len(events) - 1; i >= 0; i-- {
			if events[i].at.Before(cutoff) {
				break // events are append-ordered, so older ones precede
			}
			count++
		}
	}
	return count
}

func (p *AccountPool) pruneRequestLogLocked(id string, now time.Time) {
	events := p.requestLog[id]
	if len(events) == 0 {
		return
	}
	cutoff := now.Add(-routingEventWindow)
	idx := 0
	for idx < len(events) && events[idx].at.Before(cutoff) {
		idx++
	}
	if idx == 0 {
		return
	}
	if idx >= len(events) {
		delete(p.requestLog, id)
		return
	}
	p.requestLog[id] = append([]requestEvent(nil), events[idx:]...)
}

func (p *AccountPool) ensureRuntimeMapsLocked() {
	if p.cooldowns == nil {
		p.cooldowns = make(map[string]time.Time)
	}
	if p.errorCounts == nil {
		p.errorCounts = make(map[string]int)
	}
	if p.modelLists == nil {
		p.modelLists = make(map[string]map[string]bool)
	}
	if p.requestLog == nil {
		p.requestLog = make(map[string][]requestEvent)
	}
	if p.lastErrorAt == nil {
		p.lastErrorAt = make(map[string]time.Time)
	}
	if p.routeActiveByAccount == nil {
		p.routeActiveByAccount = make(map[string]int)
	}
	if p.routeLastStartByAccount == nil {
		p.routeLastStartByAccount = make(map[string]time.Time)
	}
	if p.routeStickyByKey == nil {
		p.routeStickyByKey = make(map[string]stickyEntry)
	}
	if p.routeNotify == nil {
		p.routeNotify = make(chan struct{})
	}
}

func (p *AccountPool) appendRequestEventLocked(id string, is429 bool, isError bool) {
	p.ensureRuntimeMapsLocked()
	now := time.Now()
	p.pruneRequestLogLocked(id, now)
	p.requestLog[id] = append(p.requestLog[id], requestEvent{at: now, is429: is429, isError: isError})
	if isError {
		p.lastErrorAt[id] = now
	}
}

// recordRouteSample appends a routing-decision event, pruning entries older than
// routeSampleWindow. Takes its own short-lived lock so callers (which run with
// no lock held at the recording sites) stay simple; the slice is guarded by mu.
func (p *AccountPool) recordRouteSample(s routeSample) {
	if s.at.IsZero() {
		s.at = time.Now()
	}
	p.mu.Lock()
	p.appendRouteSampleLocked(s)
	p.mu.Unlock()
}

// appendRouteSampleLocked appends a routing-decision event. Caller must hold mu.
// Used directly from the acquire path, which already holds the lock when it
// knows the sticky outcome and the post-increment gauge snapshot.
func (p *AccountPool) appendRouteSampleLocked(s routeSample) {
	if s.at.IsZero() {
		s.at = time.Now()
	}
	p.pruneRouteSamplesLocked(s.at)
	p.routeSamples = append(p.routeSamples, s)
}

// pruneRouteSamplesLocked drops samples older than routeSampleWindow relative to
// now. Samples are append-ordered by time, so a single leading-trim suffices.
func (p *AccountPool) pruneRouteSamplesLocked(now time.Time) {
	if len(p.routeSamples) == 0 {
		return
	}
	cutoff := now.Add(-routeSampleWindow)
	idx := 0
	for idx < len(p.routeSamples) && p.routeSamples[idx].at.Before(cutoff) {
		idx++
	}
	if idx == 0 {
		return
	}
	if idx >= len(p.routeSamples) {
		p.routeSamples = p.routeSamples[:0]
		return
	}
	p.routeSamples = append([]routeSample(nil), p.routeSamples[idx:]...)
}

// RoutingStatsWindow returns sliding-window routing metrics over the trailing
// `window`, ending now. Counters (processed/enqueued/rejected/timeout, sticky
// outcomes) count samples in the window. active/waiting are reported as the
// window peak, falling back to the current instantaneous gauge so a long request
// spanning the whole window (no rising-edge sample inside it) never shows 0
// while traffic is live. perAccount is the per-account processed count in window.
func (p *AccountPool) RoutingStatsWindow(window time.Duration, now time.Time) map[string]interface{} {
	p.mu.RLock()
	defer p.mu.RUnlock()

	cutoff := now.Add(-window)
	var processed, enqueued, rejected, timeout uint64
	var stickyHit, stickyMiss, stickyDivert uint64
	var activePeak, waitingPeak int
	perAccount := make(map[string]int)

	for i := len(p.routeSamples) - 1; i >= 0; i-- {
		s := p.routeSamples[i]
		if s.at.Before(cutoff) {
			break // append-ordered: everything earlier is also out of window
		}
		switch s.kind {
		case routeSampleProcessed:
			processed++
			if s.accountID != "" {
				perAccount[s.accountID]++
			}
			if s.active > activePeak {
				activePeak = s.active
			}
			if s.stickyHit {
				stickyHit++
			}
			if s.stickyMiss {
				stickyMiss++
			}
			if s.stickyDivert {
				stickyDivert++
			}
		case routeSampleEnqueued:
			enqueued++
			if s.waiting > waitingPeak {
				waitingPeak = s.waiting
			}
		case routeSampleRejected:
			rejected++
		case routeSampleTimeout:
			timeout++
		}
	}

	// Peak falls back to the current instantaneous gauge (see doc comment).
	if p.routeGlobalActive > activePeak {
		activePeak = p.routeGlobalActive
	}
	if p.routeWaiting > waitingPeak {
		waitingPeak = p.routeWaiting
	}

	return map[string]interface{}{
		"active":             activePeak,
		"waiting":            waitingPeak,
		"perAccountActive":   perAccount,
		"enqueuedTotal":      enqueued,
		"processedTotal":     processed,
		"rejectedTotal":      rejected,
		"timeoutTotal":       timeout,
		"stickyHitTotal":     stickyHit,
		"stickyMissTotal":    stickyMiss,
		"stickyDivertTotal":  stickyDivert,
		"requestTotal":       processed,
		"requestsLastMinute": p.requestsInWindowLocked(window, now),
	}
}

func (p *AccountPool) computeHealthScoreLocked(acc *config.Account, requests int, quotaErrors int, rate429 float64, now time.Time) int {
	score := 55
	score += subscriptionRank(*acc) * 6
	score += minInt(20, int((1.0-acc.UsagePercent)*20))
	score += minInt(8, effectiveWeight(acc.Weight)-1)
	score -= p.errorCounts[acc.ID] * 12
	score -= int(rate429 * 45)
	if isOverUsageLimit(*acc) {
		score -= 12
	}
	if lastErr, ok := p.lastErrorAt[acc.ID]; ok {
		if now.Sub(lastErr) < 10*time.Minute {
			score -= 10
		}
	}
	if requests == 0 {
		score += 4
	}
	if score < 0 {
		return 0
	}
	if score > 100 {
		return 100
	}
	return score
}

func subscriptionRank(acc config.Account) int {
	tier := strings.ToUpper(strings.TrimSpace(acc.SubscriptionType + " " + acc.SubscriptionTitle))
	switch {
	case strings.Contains(tier, "PRO_PLUS"), strings.Contains(tier, "PROPLUS"), strings.Contains(tier, "PRO+"):
		return 4
	case strings.Contains(tier, "POWER"):
		return 3
	case strings.Contains(tier, "PRO"):
		return 2
	default:
		return 1
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// GetByID 根据 ID 获取账号
func (p *AccountPool) GetByID(id string) *config.Account {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for i := range p.accounts {
		if p.accounts[i].ID == id {
			return copyAccount(&p.accounts[i])
		}
	}
	return nil
}

// RecordSuccess 记录请求成功，清除冷却
func (p *AccountPool) RecordSuccess(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureRuntimeMapsLocked()
	delete(p.cooldowns, id)
	p.errorCounts[id] = 0
	p.appendRequestEventLocked(id, false, false)
}

// RestoreAccount marks a previously disabled/cooling account usable again after a
// successful manual test.
func (p *AccountPool) RestoreAccount(id string) {
	_ = config.SetAccountEnabled(id, true)
	p.mu.Lock()
	p.ensureRuntimeMapsLocked()
	delete(p.cooldowns, id)
	p.errorCounts[id] = 0
	delete(p.lastErrorAt, id)
	p.requestLog[id] = nil
	p.mu.Unlock()
	p.Reload()
}

// RecordTransient429 records a retryable upstream 429 without disabling the
// account. These events are used for recent-429 visibility and health scoring.
// When cooldown > 0 it also applies a short cooling window in the same critical
// section so the pool-level queue can pace retries: tryAccount treats a cooling
// account as busy and makes the AcquireForModel queue wait for it to recover,
// which keeps a single model-compatible account from being hammered with 429s.
func (p *AccountPool) RecordTransient429(id string, cooldown time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureRuntimeMapsLocked()
	p.appendRequestEventLocked(id, true, true)
	if cooldown > 0 {
		p.cooldowns[id] = time.Now().Add(cooldown)
	}
}

// RecordError 记录请求错误，设置冷却
func (p *AccountPool) RecordError(id string, isQuotaError bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ensureRuntimeMapsLocked()

	p.errorCounts[id]++
	p.appendRequestEventLocked(id, isQuotaError, true)

	if isQuotaError {
		// 配额错误，冷却 1 小时
		p.cooldowns[id] = time.Now().Add(time.Hour)
	} else if p.errorCounts[id] >= 3 {
		// 连续 3 次错误，冷却 1 分钟
		p.cooldowns[id] = time.Now().Add(time.Minute)
	}
}

// IsAuthFailure reports whether an error indicates the refresh token / credentials
// have been revoked or invalidated upstream (401, 403 with auth markers, etc.).
// These accounts cannot be recovered automatically and must be re-authenticated.
func IsAuthFailure(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	lower := strings.ToLower(msg)

	// Match HTTP status codes only when they appear as standalone tokens to avoid
	// false positives from arbitrary digits in the error body (e.g. request IDs).
	if hasStatusToken(msg, "401") || hasStatusToken(msg, "403") {
		return true
	}
	if strings.Contains(lower, "bad credentials") ||
		strings.Contains(lower, "invalid_grant") ||
		strings.Contains(lower, "invalid grant") ||
		strings.Contains(lower, "invalid_token") ||
		strings.Contains(lower, "invalid token") ||
		strings.Contains(lower, "token expired") ||
		strings.Contains(lower, "token has expired") ||
		strings.Contains(lower, "unauthorized") {
		return true
	}
	return false
}

// hasStatusToken returns true when status appears in s with non-digit boundaries
// on both sides, so "401" matches "HTTP 401 from ..." but not "request_401abc".
func hasStatusToken(s, status string) bool {
	for {
		idx := strings.Index(s, status)
		if idx < 0 {
			return false
		}
		leftOK := idx == 0 || !isDigit(s[idx-1])
		rightIdx := idx + len(status)
		rightOK := rightIdx >= len(s) || !isDigit(s[rightIdx])
		if leftOK && rightOK {
			return true
		}
		s = s[idx+len(status):]
	}
}

func isDigit(b byte) bool {
	return b >= '0' && b <= '9'
}

// IsSuspensionError reports whether the error indicates the account has been
// temporarily suspended by upstream or has no available Kiro profile.
// Unlike auth failures (revoked credentials), these may be transient, but
// the account should be disabled until an operator re-enables it.
func IsSuspensionError(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "temporarily_suspended") ||
		strings.Contains(lower, "temporarily suspended") ||
		strings.Contains(lower, "no available kiro profile")
}

// DisableAccount marks an account as disabled (auth revoked / unrecoverable),
// removes it from the in-memory pool so subsequent requests skip it, and
// persists the change via config.SetAccountBanStatus.
func (p *AccountPool) DisableAccount(id, reason string) {
	if err := config.SetAccountBanStatus(id, "DISABLED", reason); err != nil {
		// best effort — even if persistence fails, drop it from memory
		_ = err
	}
	p.mu.Lock()
	// Long cooldown as a safety net in case Reload races
	p.cooldowns[id] = time.Now().Add(24 * time.Hour)
	p.mu.Unlock()
	p.Reload()
}

// QuarantineAccount429 disables an account for the standard 1h suspicious-429
// window while keeping it eligible for automatic restoration afterwards.
func (p *AccountPool) QuarantineAccount429(id string) {
	_ = config.SuspendAccountTemporarily(id, config.AutoQuarantineSuspicious429Reason())
	p.mu.Lock()
	p.ensureRuntimeMapsLocked()
	p.errorCounts[id]++
	p.appendRequestEventLocked(id, true, true)
	p.cooldowns[id] = time.Now().Add(time.Hour)
	p.mu.Unlock()
	p.Reload()
}

// MarkOverLimit marks an account as over usage limit (after a 402 / OVERAGE response).
// With the upstream OverageStatus model, the live status is refreshed via
// FetchOverageStatus from the request handler; here we just cooldown briefly so
// the next attempt picks a different account, then reload.
func (p *AccountPool) MarkOverLimit(id string) {
	p.mu.Lock()
	p.cooldowns[id] = time.Now().Add(time.Hour)
	p.mu.Unlock()
	p.Reload()
}

// UpdateToken 更新账号 Token
func (p *AccountPool) UpdateToken(id, accessToken, refreshToken string, expiresAt int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.accounts {
		if p.accounts[i].ID == id {
			p.accounts[i].AccessToken = accessToken
			if refreshToken != "" {
				p.accounts[i].RefreshToken = refreshToken
			}
			p.accounts[i].ExpiresAt = expiresAt
		}
	}
}

// UpdateProfileArn updates the in-memory profile ARN for an account under the
// pool lock. Routing hands out account copies; callers must not write shared
// pool entries directly.
func (p *AccountPool) UpdateProfileArn(id, profileArn string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := range p.accounts {
		if p.accounts[i].ID == id {
			p.accounts[i].ProfileArn = profileArn
			return
		}
	}
}

// Count 返回账号总数
func (p *AccountPool) Count() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.totalAccounts > 0 {
		return p.totalAccounts
	}

	seen := make(map[string]bool)
	for _, acc := range p.accounts {
		seen[acc.ID] = true
	}
	return len(seen)
}

// AvailableCount 返回可用账号数
func (p *AccountPool) AvailableCount() int {
	p.refreshAutoRestoredAccounts()
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	allowOverUsage := config.GetAllowOverUsage()
	count := 0
	seen := make(map[string]bool)
	for _, acc := range p.accounts {
		if seen[acc.ID] {
			continue
		}
		seen[acc.ID] = true
		if cooldown, ok := p.cooldowns[acc.ID]; ok && now.Before(cooldown) {
			continue
		}
		if !canRouteByToken(acc, now) {
			continue
		}
		if isQuotaBlocked(acc, allowOverUsage) {
			continue
		}
		count++
	}
	return count
}

// UpdateStats 更新账号统计
func (p *AccountPool) UpdateStats(id string, tokens int, credits float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var updated bool
	var requestCount, errorCount, totalTokens int
	var totalCredits float64
	var lastUsed int64
	for i := range p.accounts {
		if p.accounts[i].ID == id {
			if !updated {
				p.accounts[i].RequestCount++
				p.accounts[i].TotalTokens += tokens
				p.accounts[i].TotalCredits += credits
				p.accounts[i].LastUsed = time.Now().Unix()

				requestCount = p.accounts[i].RequestCount
				errorCount = p.accounts[i].ErrorCount
				totalTokens = p.accounts[i].TotalTokens
				totalCredits = p.accounts[i].TotalCredits
				lastUsed = p.accounts[i].LastUsed
				updated = true
				continue
			}
			p.accounts[i].RequestCount = requestCount
			p.accounts[i].ErrorCount = errorCount
			p.accounts[i].TotalTokens = totalTokens
			p.accounts[i].TotalCredits = totalCredits
			p.accounts[i].LastUsed = lastUsed
		}
	}
	if updated {
		go config.UpdateAccountStats(id, requestCount, errorCount, totalTokens, totalCredits, lastUsed)
	}
}

// GetAllAccounts 获取所有账号副本
func (p *AccountPool) GetAllAccounts() []config.Account {
	p.refreshAutoRestoredAccounts()
	p.mu.RLock()
	defer p.mu.RUnlock()
	result := make([]config.Account, len(p.accounts))
	copy(result, p.accounts)
	return result
}

func (p *AccountPool) GetHealthSnapshots() map[string]AccountHealthSnapshot {
	p.refreshAutoRestoredAccounts()
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	allowOverUsage := config.GetAllowOverUsage()
	seen := make(map[string]bool)
	result := make(map[string]AccountHealthSnapshot)
	for i := range p.accounts {
		acc := &p.accounts[i]
		if seen[acc.ID] {
			continue
		}
		seen[acc.ID] = true
		requests, quotaErrors, rate429 := p.getRecentStatsLocked(acc.ID, now)
		canRoute := true
		if !canRouteByToken(*acc, now) {
			canRoute = false
		}
		if isQuotaBlocked(*acc, allowOverUsage) {
			canRoute = false
		}
		if cooldown, ok := p.cooldowns[acc.ID]; ok && now.Before(cooldown) {
			canRoute = false
		}
		snapshot := AccountHealthSnapshot{
			ID:          acc.ID,
			Requests:    requests,
			QuotaErrors: quotaErrors,
			ErrorCount:  p.errorCounts[acc.ID],
			Rate429:     rate429,
			StablePool:  requests < 3 || rate429 < 0.2,
			HealthScore: p.computeHealthScoreLocked(acc, requests, quotaErrors, rate429, now),
			CanRoute:    canRoute,
		}
		if lastErr, ok := p.lastErrorAt[acc.ID]; ok {
			snapshot.LastErrorAt = lastErr.Unix()
		}
		if cooldown, ok := p.cooldowns[acc.ID]; ok {
			snapshot.CoolingUntil = cooldown.Unix()
		}
		if snapshot.StablePool {
			snapshot.ModeBucket = "stable"
		} else {
			snapshot.ModeBucket = "probe"
		}
		result[acc.ID] = snapshot
	}
	return result
}

func isOverUsageLimit(acc config.Account) bool {
	return acc.UsageLimit > 0 && acc.UsageCurrent >= acc.UsageLimit
}

// isQuotaBlocked reports whether an over-quota account should be skipped.
// Upstream OverageStatus=ENABLED and global allowOverUsage both keep it routable.
func isQuotaBlocked(acc config.Account, allowOverUsage bool) bool {
	return isOverUsageLimit(acc) && !isUpstreamOverageEnabled(acc) && !allowOverUsage
}

// isUpstreamOverageEnabled reports whether the upstream Overages switch is ON for this account.
// "ENABLED" → true; anything else (DISABLED, UNKNOWN, empty) → false.
func isUpstreamOverageEnabled(acc config.Account) bool {
	return strings.EqualFold(acc.OverageStatus, "ENABLED")
}

func effectiveWeight(weight int) int {
	if weight < 1 {
		return 1
	}
	return weight
}
