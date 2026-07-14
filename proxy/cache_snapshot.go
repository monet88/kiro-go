package proxy

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"kiro-go/config"
	"kiro-go/logger"
)

// promptCacheSnapshotVersion is the on-disk schema version for the Prompt Cache
// Snapshot. Bumped only if the serialized shape changes incompatibly so an old
// file can be detected and ignored rather than mis-parsed.
const promptCacheSnapshotVersion = 1

// defaultPromptCacheSnapshotName is the Prompt Cache Snapshot filename, stored
// alongside config.json under the data directory (ADR-0001).
const defaultPromptCacheSnapshotName = "prompt_cache.json"

// promptCacheSnapshotFlushInterval is how often the in-memory Cross-account
// Prompt Cache is flushed to disk so entries survive a process restart. A
// moderate interval bounds data loss on crash without hammering the disk.
const promptCacheSnapshotFlushInterval = 60 * time.Second

// promptCacheSnapshotEntry is one persisted Cache Fingerprint entry. The
// fingerprint is hex-encoded (fixed 64 chars) so the snapshot is human-readable
// and stable across architectures. Timestamps are RFC3339Nano; TTL is stored in
// nanoseconds so refresh-on-hit keeps working after a reload.
type promptCacheSnapshotEntry struct {
	Fingerprint string    `json:"fingerprint"`
	ExpiresAt   time.Time `json:"expiresAt"`
	TTLNanos    int64     `json:"ttlNanos"`
}

// promptCacheSnapshot is the full on-disk Prompt Cache Snapshot document.
type promptCacheSnapshot struct {
	Version int                        `json:"version"`
	SavedAt time.Time                  `json:"savedAt"`
	Entries []promptCacheSnapshotEntry `json:"entries"`
}

// promptCacheSnapshotPath returns the Prompt Cache Snapshot path derived from
// the config data directory (sibling of config.json), e.g. data/prompt_cache.json.
func promptCacheSnapshotPath() string {
	return filepath.Join(config.GetConfigDir(), defaultPromptCacheSnapshotName)
}

// exportSnapshotLocked returns a serializable copy of all live (unexpired)
// entries. Caller must hold t.mu.
func (t *promptCacheTracker) exportSnapshotLocked(now time.Time) promptCacheSnapshot {
	entries := make([]promptCacheSnapshotEntry, 0, len(t.entries))
	// Walk the LRU front→back so the most-recently-used entries are written
	// first; on reload they are re-inserted in reverse to rebuild MRU order.
	for elem := t.lru.Front(); elem != nil; elem = elem.Next() {
		item := elem.Value.(*lruItem)
		if !item.Entry.ExpiresAt.After(now) {
			continue // skip already-expired entries
		}
		entries = append(entries, promptCacheSnapshotEntry{
			Fingerprint: hex.EncodeToString(item.Fingerprint[:]),
			ExpiresAt:   item.Entry.ExpiresAt,
			TTLNanos:    int64(item.Entry.TTL),
		})
	}
	return promptCacheSnapshot{
		Version: promptCacheSnapshotVersion,
		SavedAt: now,
		Entries: entries,
	}
}

// FlushSnapshot atomically writes the current cache to path using a temp file +
// rename with restrictive (0600) permissions. Expired entries are dropped. A nil
// tracker or empty path is a no-op.
func (t *promptCacheTracker) FlushSnapshot(path string) error {
	if t == nil || path == "" {
		return nil
	}

	// Serialize disk writes: two concurrent flushers would otherwise race on the
	// shared "<path>.tmp" file. Held for the whole snapshot+write so the on-disk
	// file always reflects a single coherent export.
	t.flushMu.Lock()
	defer t.flushMu.Unlock()

	now := time.Now()
	t.mu.Lock()
	t.pruneExpiredLocked(now)
	snap := t.exportSnapshotLocked(now)
	t.mu.Unlock()

	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}

	// Ensure the parent directory exists (data dir may be freshly provisioned).
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
	}

	// Atomic write: write to a temp file in the same directory, then rename.
	// A crash or full disk mid-write cannot leave a corrupt partial file as the
	// live snapshot; the previous good file survives until rename succeeds.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// LoadSnapshot loads a Prompt Cache Snapshot from path and merges live entries
// into the tracker. A missing file is not an error (cold start). Expired entries
// and entries beyond maxEntries are dropped. A malformed file or version
// mismatch is logged and ignored so a bad snapshot never blocks startup.
func (t *promptCacheTracker) LoadSnapshot(path string) error {
	if t == nil || path == "" {
		return nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // cold start, no snapshot yet
		}
		return err
	}

	var snap promptCacheSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		logger.Warnf("prompt cache snapshot %s is malformed, ignoring: %v", path, err)
		return nil
	}
	if snap.Version != promptCacheSnapshotVersion {
		logger.Warnf("prompt cache snapshot %s version %d != %d, ignoring", path, snap.Version, promptCacheSnapshotVersion)
		return nil
	}

	now := time.Now()
	loaded := 0

	t.mu.Lock()
	// Entries are serialized MRU→LRU (exportSnapshotLocked walks the LRU front→
	// back). If the snapshot holds more entries than maxEntries, keep only the
	// MRU prefix so we never allocate list/map nodes we would immediately evict.
	// evictOverflowLocked below is still a correctness backstop (the tracker may
	// already hold entries when LoadSnapshot runs).
	entries := snap.Entries
	if t.maxEntries > 0 && len(entries) > t.maxEntries {
		entries = entries[:t.maxEntries]
	}
	// Insert in reverse so the first (most-recently-used) snapshot entry ends up
	// at the front of the LRU after all PushFront calls.
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if !e.ExpiresAt.After(now) {
			continue // expired while at rest
		}
		raw, err := hex.DecodeString(e.Fingerprint)
		if err != nil || len(raw) != 32 {
			continue // skip corrupt fingerprint
		}
		var fp [32]byte
		copy(fp[:], raw)
		if _, ok := t.entries[fp]; ok {
			continue // already present (e.g. loaded twice); keep the newer live entry
		}
		elem := t.lru.PushFront(&lruItem{
			Fingerprint: fp,
			Entry: promptCacheEntry{
				ExpiresAt: e.ExpiresAt,
				TTL:       time.Duration(e.TTLNanos),
			},
		})
		t.entries[fp] = elem
		loaded++
	}
	t.evictOverflowLocked()
	t.mu.Unlock()

	// Log outside the lock so a large snapshot load does not extend mutex hold
	// time (Compute/Update contend on the same lock).
	if loaded > 0 {
		logger.Infof("prompt cache snapshot loaded %d live entries from %s", loaded, path)
	}
	return nil
}

// startSnapshotSaver periodically flushes the Prompt Cache Snapshot until stop
// is closed. When stop is closed (by Handler.Shutdown on SIGINT/SIGTERM) it
// performs one final flush before returning so the latest cache state is
// persisted on a graceful stop; between stops, durability comes from the
// periodic flush. Run in its own goroutine.
func (t *promptCacheTracker) startSnapshotSaver(path string, stop <-chan struct{}) {
	if t == nil || path == "" {
		return
	}
	ticker := time.NewTicker(promptCacheSnapshotFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := t.FlushSnapshot(path); err != nil {
				logger.Warnf("prompt cache snapshot flush failed: %v", err)
			}
		case <-stop:
			if err := t.FlushSnapshot(path); err != nil {
				logger.Warnf("prompt cache snapshot final flush failed: %v", err)
			}
			return
		}
	}
}
