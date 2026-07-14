package proxy

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// buildLongCacheReq constructs a Claude request with a single long
// cache_control system block so BuildClaudeProfile yields a cacheable
// breakpoint above the minimum-token threshold.
func buildLongCacheReq(marker string) *ClaudeRequest {
	longSystem := marker + " " + repeatStr("You are a helpful coding assistant with deep knowledge of Go, Rust, Python, and TypeScript. ", 80)
	return &ClaudeRequest{
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
}

func repeatStr(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}

// TestPromptCacheSnapshotRoundTrip verifies that entries written by one tracker
// are reloaded by a fresh tracker and immediately produce a cache read, i.e. the
// Prompt Cache survives a simulated process/tracker restart via the snapshot.
func TestPromptCacheSnapshotRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prompt_cache.json")

	writer := newPromptCacheTracker(time.Hour, 0, 0)
	req := buildLongCacheReq("round-trip")
	profile := writer.BuildClaudeProfile(req, 2048)
	if profile == nil {
		t.Fatalf("expected cache profile to be built")
	}
	writer.Update("acct-1", profile)

	if err := writer.FlushSnapshot(path); err != nil {
		t.Fatalf("flush snapshot: %v", err)
	}

	// Fresh tracker (simulates a process restart): cold before load.
	reader := newPromptCacheTracker(time.Hour, 0, 0)
	cold := reader.Compute("acct-2", profile)
	if cold.CacheReadInputTokens != 0 {
		t.Fatalf("expected cold tracker to have zero cache reads, got %+v", cold)
	}

	if err := reader.LoadSnapshot(path); err != nil {
		t.Fatalf("load snapshot: %v", err)
	}

	// Cross-account: written under acct-1, read under acct-2 (ADR-0001).
	warm := reader.Compute("acct-2", profile)
	if warm.CacheReadInputTokens <= 0 {
		t.Fatalf("expected cache read after snapshot reload, got %+v", warm)
	}
}

// TestPromptCacheSnapshotSkipsExpired verifies that entries whose TTL elapsed
// while the snapshot was at rest are not resurrected on load.
func TestPromptCacheSnapshotSkipsExpired(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prompt_cache.json")

	writer := newPromptCacheTracker(time.Hour, 0, 0)
	// Force a very short TTL so the entry expires before reload.
	req := buildLongCacheReq("expiry")
	profile := writer.BuildClaudeProfile(req, 2048)
	if profile == nil {
		t.Fatalf("expected cache profile")
	}
	for i := range profile.Breakpoints {
		profile.Breakpoints[i].TTL = time.Millisecond
	}
	writer.Update("acct-1", profile)
	if err := writer.FlushSnapshot(path); err != nil {
		t.Fatalf("flush: %v", err)
	}

	time.Sleep(5 * time.Millisecond)

	reader := newPromptCacheTracker(time.Hour, 0, 0)
	if err := reader.LoadSnapshot(path); err != nil {
		t.Fatalf("load: %v", err)
	}

	// Rebuild a fresh profile with a normal TTL for the read probe.
	readProfile := reader.BuildClaudeProfile(buildLongCacheReq("expiry"), 2048)
	got := reader.Compute("acct-2", readProfile)
	if got.CacheReadInputTokens != 0 {
		t.Fatalf("expected expired entry to be skipped on load, got %+v", got)
	}
}

// TestPromptCacheSnapshotMissingFile verifies a cold start (no snapshot yet) is
// not treated as an error.
func TestPromptCacheSnapshotMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does_not_exist.json")
	tracker := newPromptCacheTracker(time.Hour, 0, 0)
	if err := tracker.LoadSnapshot(path); err != nil {
		t.Fatalf("missing snapshot should not error, got %v", err)
	}
}

// TestPromptCacheSnapshotMalformedIgnored verifies a corrupt snapshot file does
// not block startup and leaves the tracker empty.
func TestPromptCacheSnapshotMalformedIgnored(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prompt_cache.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0600); err != nil {
		t.Fatalf("seed malformed file: %v", err)
	}
	tracker := newPromptCacheTracker(time.Hour, 0, 0)
	if err := tracker.LoadSnapshot(path); err != nil {
		t.Fatalf("malformed snapshot should be ignored, got %v", err)
	}
	if len(tracker.entries) != 0 {
		t.Fatalf("expected empty tracker after malformed load, got %d entries", len(tracker.entries))
	}
}

// TestPromptCacheSnapshotAtomicNoPartial verifies FlushSnapshot leaves no
// leftover temp file and the produced file is valid on its own.
func TestPromptCacheSnapshotAtomicNoPartial(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prompt_cache.json")
	tracker := newPromptCacheTracker(time.Hour, 0, 0)
	profile := tracker.BuildClaudeProfile(buildLongCacheReq("atomic"), 2048)
	tracker.Update("acct-1", profile)
	if err := tracker.FlushSnapshot(path); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file %s.tmp should not survive a successful flush", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("snapshot file %s should exist after flush: %v", path, err)
	}
	// Unix permission bits are only meaningful on non-Windows; Windows maps the
	// mode loosely, so restrict the 0600 assertion to platforms where it holds.
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("snapshot file mode = %o, want 0600 (restrictive)", perm)
		}
	}
}
