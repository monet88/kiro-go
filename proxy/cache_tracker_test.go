package proxy

import (
	"strings"
	"testing"
	"time"
)

func TestPromptCacheTrackerComputeAndUpdate(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour, 0, 0)
	longSystem := strings.Repeat("You are a helpful coding assistant with deep knowledge of Go, Rust, Python, and TypeScript. ", 80)
	req := &ClaudeRequest{
		Model: "claude-sonnet-4.5",
		System: []interface{}{
			map[string]interface{}{
				"type": "text",
				"text": longSystem,
				"cache_control": map[string]interface{}{
					"type": "ephemeral",
				},
			},
		},
		Messages: []ClaudeMessage{{Role: "user", Content: "hello world"}},
	}

	profile := tracker.BuildClaudeProfile(req, 120)
	if profile == nil {
		t.Fatalf("expected cache profile to be built")
	}

	first := tracker.Compute("acct-1", profile)
	if first.CacheCreationInputTokens <= 0 {
		t.Fatalf("expected first request to create cache tokens, got %+v", first)
	}
	if first.CacheReadInputTokens != 0 {
		t.Fatalf("expected first request to have zero cache reads, got %+v", first)
	}

	tracker.Update("acct-1", profile)
	second := tracker.Compute("acct-1", profile)
	if second.CacheReadInputTokens <= 0 {
		t.Fatalf("expected repeated request to read cache tokens, got %+v", second)
	}
	if second.CacheCreationInputTokens != 0 {
		t.Fatalf("expected repeated request to avoid cache creation, got %+v", second)
	}
}

func TestBuildClaudeUsageMapIncludesCacheFields(t *testing.T) {
	usage := promptCacheUsage{
		CacheCreationInputTokens:   30,
		CacheReadInputTokens:       20,
		CacheCreation5mInputTokens: 10,
		CacheCreation1hInputTokens: 20,
	}

	m := buildClaudeUsageMap(100, 50, usage, true)

	if got := m["input_tokens"]; got != 50 {
		t.Fatalf("expected billed input tokens 50, got %#v", got)
	}
	if got := m["cache_creation_input_tokens"]; got != 30 {
		t.Fatalf("expected cache creation tokens 30, got %#v", got)
	}
	if got := m["cache_read_input_tokens"]; got != 20 {
		t.Fatalf("expected cache read tokens 20, got %#v", got)
	}
	creation, ok := m["cache_creation"].(map[string]int)
	if !ok {
		t.Fatalf("expected typed cache creation map, got %#v", m["cache_creation"])
	}
	if creation["ephemeral_5m_input_tokens"] != 10 || creation["ephemeral_1h_input_tokens"] != 20 {
		t.Fatalf("unexpected ttl breakdown: %#v", creation)
	}
}

// TestPromptCacheStableAcrossBillingHeaderDrift verifies that Claude Code's
// per-request "x-anthropic-billing-header: cc_version=...; cch=...;" system
// block (whose content drifts on every request) does not break cache hits.
// The tracker should ignore that metadata when fingerprinting cached prefixes.
func TestPromptCacheStableAcrossBillingHeaderDrift(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour, 0, 0)
	mainSystem := strings.Repeat("You are a helpful coding assistant with deep knowledge of Go, Rust, Python, and TypeScript. ", 80)

	build := func(billingHdr string) *ClaudeRequest {
		return &ClaudeRequest{
			Model: "claude-sonnet-4.5",
			System: []interface{}{
				map[string]interface{}{
					"type": "text",
					"text": billingHdr,
				},
				map[string]interface{}{
					"type": "text",
					"text": mainSystem,
					"cache_control": map[string]interface{}{
						"type": "ephemeral",
					},
				},
			},
			Messages: []ClaudeMessage{{Role: "user", Content: "hello world"}},
		}
	}

	req1 := build("x-anthropic-billing-header: cc_version=2.1.87.1; cch=aaaa;")
	profile1 := tracker.BuildClaudeProfile(req1, 2048)
	if profile1 == nil {
		t.Fatalf("profile1 should be built")
	}
	first := tracker.Compute("acct-1", profile1)
	if first.CacheReadInputTokens != 0 {
		t.Fatalf("expected no cache read on first request, got %+v", first)
	}
	tracker.Update("acct-1", profile1)

	req2 := build("x-anthropic-billing-header: cc_version=2.1.87.42; cch=bbbb; padding=xxyyzz;")
	profile2 := tracker.BuildClaudeProfile(req2, 2048)
	if profile2 == nil {
		t.Fatalf("profile2 should be built")
	}
	second := tracker.Compute("acct-1", profile2)
	if second.CacheReadInputTokens == 0 {
		t.Fatalf("expected cache read after billing header drift, got %+v", second)
	}
}

func TestPromptCacheStableWhenBillingHeaderAppearsOrDisappears(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour, 0, 0)
	mainSystem := strings.Repeat("You are a helpful coding assistant with deep knowledge of Go, Rust, Python, and TypeScript. ", 80)

	build := func(includeBilling bool) *ClaudeRequest {
		system := []interface{}{}
		if includeBilling {
			system = append(system, map[string]interface{}{
				"type": "text",
				"text": "x-anthropic-billing-header: cc_version=2.1.87.1; cch=aaaa;",
			})
		}
		system = append(system, map[string]interface{}{
			"type": "text",
			"text": mainSystem,
			"cache_control": map[string]interface{}{
				"type": "ephemeral",
			},
		})
		return &ClaudeRequest{
			Model:    "claude-sonnet-4.5",
			System:   system,
			Messages: []ClaudeMessage{{Role: "user", Content: "hello world"}},
		}
	}

	withBilling := tracker.BuildClaudeProfile(build(true), 2048)
	if withBilling == nil {
		t.Fatalf("profile with billing header should be built")
	}
	tracker.Update("acct-1", withBilling)

	withoutBilling := tracker.BuildClaudeProfile(build(false), 2048)
	if withoutBilling == nil {
		t.Fatalf("profile without billing header should be built")
	}
	result := tracker.Compute("acct-1", withoutBilling)
	if result.CacheReadInputTokens == 0 {
		t.Fatalf("expected cache read when billing header disappears, got %+v", result)
	}
}

func TestCanonicalCacheValueIgnoresPositionKeys(t *testing.T) {
	first := canonicalizeCacheValue(stripCachePositionKeys(map[string]interface{}{
		"kind":         "system",
		"system_index": 0,
		"block": map[string]interface{}{
			"type": "text",
			"text": "stable",
		},
	}))
	second := canonicalizeCacheValue(stripCachePositionKeys(map[string]interface{}{
		"kind":         "system",
		"system_index": 1,
		"block": map[string]interface{}{
			"type": "text",
			"text": "stable",
		},
	}))
	if first != second {
		t.Fatalf("expected position keys to be ignored, got %q vs %q", first, second)
	}
}

func TestCanonicalCacheValuePreservesSemanticPositionKeys(t *testing.T) {
	first := canonicalizeCacheValue(map[string]interface{}{
		"kind": "system",
		"block": map[string]interface{}{
			"type":        "text",
			"text":        "stable",
			"block_index": 1,
		},
	})
	second := canonicalizeCacheValue(map[string]interface{}{
		"kind": "system",
		"block": map[string]interface{}{
			"type":        "text",
			"text":        "stable",
			"block_index": 2,
		},
	})
	if first == second {
		t.Fatalf("expected semantic block_index fields to remain fingerprinted")
	}
}

// TestPromptCacheImplicitBreakpointAtMessageEnd verifies that once any
// explicit cache_control breakpoint has been seen, subsequent message-end
// boundaries act as implicit breakpoints. This allows multi-turn conversations
// to hit earlier stored prefix fingerprints even when the newest messages
// lack explicit cache_control.
func TestPromptCacheImplicitBreakpointAtMessageEnd(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour, 0, 0)
	systemText := strings.Repeat("You are a helpful coding assistant with deep knowledge of Go, Rust, Python, and TypeScript. ", 80)

	baseSystem := []interface{}{
		map[string]interface{}{
			"type": "text",
			"text": systemText,
			"cache_control": map[string]interface{}{
				"type": "ephemeral",
			},
		},
	}

	// Round 1: single user message.
	req1 := &ClaudeRequest{
		Model:    "claude-sonnet-4.5",
		System:   baseSystem,
		Messages: []ClaudeMessage{{Role: "user", Content: "question one"}},
	}
	profile1 := tracker.BuildClaudeProfile(req1, 2048)
	if profile1 == nil {
		t.Fatalf("profile1 should be built")
	}
	tracker.Update("acct-1", profile1)

	// Round 2: conversation continues with new messages. The latest user
	// message has no explicit cache_control; it should still hit the stored
	// prefix via the implicit message-end breakpoint.
	req2 := &ClaudeRequest{
		Model:  "claude-sonnet-4.5",
		System: baseSystem,
		Messages: []ClaudeMessage{
			{Role: "user", Content: "question one"},
			{Role: "assistant", Content: "answer one"},
			{Role: "user", Content: "follow-up question"},
		},
	}
	profile2 := tracker.BuildClaudeProfile(req2, 4096)
	if profile2 == nil {
		t.Fatalf("profile2 should be built")
	}
	result := tracker.Compute("acct-1", profile2)
	if result.CacheReadInputTokens == 0 {
		t.Fatalf("expected cache read via implicit message-end breakpoint, got %+v", result)
	}
}

// TestPromptCacheCrossAccountHit verifies the Cross-account Prompt Cache
// contract (ADR-0001): a prefix written under one Account is readable by a
// different Account, because entries are keyed globally by Cache Fingerprint
// rather than per Account.
func TestPromptCacheCrossAccountHit(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour, 0, 0)
	longSystem := strings.Repeat("You are a helpful coding assistant with deep knowledge of Go, Rust, Python, and TypeScript. ", 80)
	req := &ClaudeRequest{
		Model: "claude-sonnet-4.5",
		System: []interface{}{
			map[string]interface{}{
				"type": "text",
				"text": longSystem,
				"cache_control": map[string]interface{}{
					"type": "ephemeral",
				},
			},
		},
		Messages: []ClaudeMessage{{Role: "user", Content: "hello world"}},
	}

	profile := tracker.BuildClaudeProfile(req, 2048)
	if profile == nil {
		t.Fatalf("expected cache profile to be built")
	}

	// Account A writes the prefix.
	tracker.Update("acct-A", profile)

	// Account B (different Account) must hit the prefix A wrote.
	crossProfile := tracker.BuildClaudeProfile(req, 2048)
	result := tracker.Compute("acct-B", crossProfile)
	if result.CacheReadInputTokens == 0 {
		t.Fatalf("expected cross-account cache read on second account, got %+v", result)
	}
}

// TestPromptCacheStructuralSystemSkip verifies that a leading dynamic system
// block without cache_control (Claude Code injects a per-turn system[0]) is
// excluded from the fingerprint, so the cached prefix anchored at the
// cache_control system block still hits when only the leading block drifts.
func TestPromptCacheStructuralSystemSkip(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour, 0, 0)
	mainSystem := strings.Repeat("You are a helpful coding assistant with deep knowledge of Go, Rust, Python, and TypeScript. ", 80)

	build := func(leading string) *ClaudeRequest {
		return &ClaudeRequest{
			Model: "claude-sonnet-4.5",
			System: []interface{}{
				// Leading dynamic system block, no cache_control (not a billing header).
				map[string]interface{}{
					"type": "text",
					"text": leading,
				},
				// Stable cache_control-anchored main system.
				map[string]interface{}{
					"type": "text",
					"text": mainSystem,
					"cache_control": map[string]interface{}{
						"type": "ephemeral",
					},
				},
			},
			Messages: []ClaudeMessage{{Role: "user", Content: "hello world"}},
		}
	}

	req1 := build("Current session started at 2026-07-14T09:00:00Z; cwd=/home/a; branch=main.")
	profile1 := tracker.BuildClaudeProfile(req1, 2048)
	if profile1 == nil {
		t.Fatalf("profile1 should be built")
	}
	tracker.Update("acct-1", profile1)

	req2 := build("Current session started at 2026-07-14T11:22:33Z; cwd=/home/b; branch=feature-x.")
	profile2 := tracker.BuildClaudeProfile(req2, 2048)
	if profile2 == nil {
		t.Fatalf("profile2 should be built")
	}
	result := tracker.Compute("acct-1", profile2)
	if result.CacheReadInputTokens == 0 {
		t.Fatalf("expected cache read after leading dynamic system block drift, got %+v", result)
	}
}

// TestPromptCacheAutoPrefixWithoutCacheControl verifies auto-prefix behavior:
// even with no explicit cache_control anywhere, a large stable prefix produces
// message-end breakpoints so a repeated prefix estimates a cache hit.
func TestPromptCacheAutoPrefixWithoutCacheControl(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour, 0, 0)
	longSystem := strings.Repeat("You are a helpful coding assistant with deep knowledge of Go, Rust, Python, and TypeScript. ", 120)

	baseSystem := []interface{}{
		map[string]interface{}{
			"type": "text",
			"text": longSystem,
			// No cache_control at all.
		},
	}

	req1 := &ClaudeRequest{
		Model:    "claude-sonnet-4.5",
		System:   baseSystem,
		Messages: []ClaudeMessage{{Role: "user", Content: "first question about the codebase"}},
	}
	profile1 := tracker.BuildClaudeProfile(req1, 3000)
	if profile1 == nil {
		t.Fatalf("expected auto-prefix profile even without cache_control")
	}
	tracker.Update("acct-1", profile1)

	req2 := &ClaudeRequest{
		Model:  "claude-sonnet-4.5",
		System: baseSystem,
		Messages: []ClaudeMessage{
			{Role: "user", Content: "first question about the codebase"},
			{Role: "assistant", Content: "here is the answer"},
			{Role: "user", Content: "a follow-up question"},
		},
	}
	profile2 := tracker.BuildClaudeProfile(req2, 4000)
	if profile2 == nil {
		t.Fatalf("profile2 should be built")
	}
	result := tracker.Compute("acct-1", profile2)
	if result.CacheReadInputTokens == 0 {
		t.Fatalf("expected auto-prefix cache read without explicit cache_control, got %+v", result)
	}
}

// TestPromptCacheOpusMinTokenThreshold verifies that a breakpoint whose
// cumulative tokens fall between the generic floor (1024) and the Opus floor
// (4096) is cacheable for a Sonnet model but excluded for an Opus model.
func TestPromptCacheOpusMinTokenThreshold(t *testing.T) {
	// ~2000 tokens of system content: above the 1024 generic floor, below the
	// 4096 Opus floor.
	midSystem := strings.Repeat("token ", 2200)

	build := func(model string) *ClaudeRequest {
		return &ClaudeRequest{
			Model: model,
			System: []interface{}{
				map[string]interface{}{
					"type": "text",
					"text": midSystem,
					"cache_control": map[string]interface{}{
						"type": "ephemeral",
					},
				},
			},
			Messages: []ClaudeMessage{{Role: "user", Content: "hi"}},
		}
	}

	// Sonnet: breakpoint above 1024 → cacheable.
	sonnet := newPromptCacheTracker(time.Hour, 0, 0)
	sonnetProfile := build("claude-sonnet-4.5")
	p := sonnet.BuildClaudeProfile(sonnetProfile, 2200)
	if p == nil {
		t.Fatalf("sonnet profile should be built")
	}
	sonnet.Update("acct-1", p)
	if got := sonnet.Compute("acct-1", sonnet.BuildClaudeProfile(build("claude-sonnet-4.5"), 2200)); got.CacheReadInputTokens == 0 {
		t.Fatalf("expected sonnet breakpoint above 1024 to be cacheable, got %+v", got)
	}

	// Opus: same breakpoint below 4096 → excluded.
	opus := newPromptCacheTracker(time.Hour, 0, 0)
	opusProfile := build("claude-opus-4.1")
	po := opus.BuildClaudeProfile(opusProfile, 2200)
	if po == nil {
		t.Fatalf("opus profile should be built")
	}
	opus.Update("acct-1", po)
	if got := opus.Compute("acct-1", opus.BuildClaudeProfile(build("claude-opus-4.1"), 2200)); got.CacheReadInputTokens != 0 {
		t.Fatalf("expected opus breakpoint below 4096 to be excluded, got %+v", got)
	}
}

// singleBreakpointRequest builds a request that produces exactly one cache
// breakpoint: a single cache_control-anchored system block and no messages (a
// trailing user message would add a second, message-end breakpoint). One
// breakpoint per profile keeps the LRU entry count predictable so the eviction
// tests below can reason about it precisely.
func singleBreakpointRequest(marker string) *ClaudeRequest {
	longSystem := strings.Repeat("You are a helpful coding assistant with deep knowledge of Go, Rust, Python, and TypeScript. ", 80)
	return &ClaudeRequest{
		Model: "claude-sonnet-4.5",
		System: []interface{}{
			map[string]interface{}{
				"type": "text",
				"text": longSystem + marker,
				"cache_control": map[string]interface{}{
					"type": "ephemeral",
				},
			},
		},
	}
}

// TestPromptCacheLRUEviction verifies the in-memory LRU bound: once the number
// of distinct fingerprints exceeds maxEntries, the least-recently-used entry is
// evicted and no longer produces a cache read.
func TestPromptCacheLRUEviction(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour, 2, 0)

	tracker.Update("acct-1", tracker.BuildClaudeProfile(singleBreakpointRequest(" A"), 2048)) // {A}
	tracker.Update("acct-1", tracker.BuildClaudeProfile(singleBreakpointRequest(" B"), 2048)) // {A,B}
	tracker.Update("acct-1", tracker.BuildClaudeProfile(singleBreakpointRequest(" C"), 2048)) // {B,C} — A evicted (LRU, max 2)

	if got := tracker.Compute("acct-1", tracker.BuildClaudeProfile(singleBreakpointRequest(" A"), 2048)); got.CacheReadInputTokens != 0 {
		t.Fatalf("expected LRU-evicted entry A to miss, got %+v", got)
	}
	if got := tracker.Compute("acct-1", tracker.BuildClaudeProfile(singleBreakpointRequest(" C"), 2048)); got.CacheReadInputTokens == 0 {
		t.Fatalf("expected most-recent entry C to hit, got %+v", got)
	}
}

// TestPromptCacheLRUEvictsLeastRecentlyUsed proves the eviction order is by
// recency, not insertion (FIFO): after A and B are stored, a cache read on A
// makes A most-recently-used; inserting C then evicts B (the LRU entry), while
// A survives. A FIFO cache would wrongly evict A here.
func TestPromptCacheLRUEvictsLeastRecentlyUsed(t *testing.T) {
	tracker := newPromptCacheTracker(time.Hour, 2, 0)

	tracker.Update("acct-1", tracker.BuildClaudeProfile(singleBreakpointRequest(" A"), 2048)) // {A}
	tracker.Update("acct-1", tracker.BuildClaudeProfile(singleBreakpointRequest(" B"), 2048)) // {A,B}

	// Touch A so it becomes most-recently-used (Compute moves the hit to front).
	if got := tracker.Compute("acct-1", tracker.BuildClaudeProfile(singleBreakpointRequest(" A"), 2048)); got.CacheReadInputTokens == 0 {
		t.Fatalf("expected A to hit before eviction, got %+v", got)
	}

	tracker.Update("acct-1", tracker.BuildClaudeProfile(singleBreakpointRequest(" C"), 2048)) // evicts LRU = B

	if got := tracker.Compute("acct-1", tracker.BuildClaudeProfile(singleBreakpointRequest(" B"), 2048)); got.CacheReadInputTokens != 0 {
		t.Fatalf("expected least-recently-used entry B to be evicted, got %+v", got)
	}
	if got := tracker.Compute("acct-1", tracker.BuildClaudeProfile(singleBreakpointRequest(" A"), 2048)); got.CacheReadInputTokens == 0 {
		t.Fatalf("expected recently-used entry A to survive, got %+v", got)
	}
}

// TestPromptCacheMaxRatioCap verifies that reported cache-read tokens are capped
// at the configured ratio of total input tokens even when the matched prefix is
// larger, keeping the newest turn from appearing fully cache-served.
func TestPromptCacheMaxRatioCap(t *testing.T) {
	// Explicit low ratio so the cap is easy to assert.
	tracker := newPromptCacheTracker(time.Hour, 0, 0.5)
	longSystem := strings.Repeat("You are a helpful coding assistant with deep knowledge of Go, Rust, Python, and TypeScript. ", 200)

	req := &ClaudeRequest{
		Model: "claude-sonnet-4.5",
		System: []interface{}{
			map[string]interface{}{
				"type": "text",
				"text": longSystem,
				"cache_control": map[string]interface{}{
					"type": "ephemeral",
				},
			},
		},
		Messages: []ClaudeMessage{{Role: "user", Content: "hello world"}},
	}

	profile := tracker.BuildClaudeProfile(req, 0)
	if profile == nil {
		t.Fatalf("expected profile to be built")
	}
	total := profile.TotalInputTokens
	tracker.Update("acct-1", profile)

	second := tracker.Compute("acct-1", tracker.BuildClaudeProfile(req, 0))
	if second.CacheReadInputTokens == 0 {
		t.Fatalf("expected a cache read, got %+v", second)
	}
	if second.CacheReadInputTokens > int(float64(total)*0.5) {
		t.Fatalf("expected cache read capped at 50%% of %d total, got %d", total, second.CacheReadInputTokens)
	}
}
