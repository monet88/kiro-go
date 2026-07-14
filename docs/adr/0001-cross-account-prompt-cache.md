# Cross-account Prompt Cache

```yaml
status: accepted
```

Local Prompt Cache was keyed per Account. Claude Code multi-turn traffic across a pooled multi-account deployment then re-paid for the same stable prefixes after each Account switch, and structural fingerprint drift (dynamic leading `system[]`) made hits collapse even without failover.

**Decision:** Wave B uses a single global Prompt Cache keyed only by Cache Fingerprint (Cross-account Prompt Cache), with in-memory LRU (`PromptCacheMaxEntries`, default 131072), disk Prompt Cache Snapshot (`data/prompt_cache.json`), full structural fingerprint logic from ngh1105 (system-skip, auto-prefix, Opus min tokens, strip volatile keys), and `PromptCacheMaxRatio` default 0.85.

**Why not keep per-account isolation:** Isolation is safer if Accounts ever map to different upstream cache domains, but this deployment assumes one shared upstream/billing domain; isolation would leave the port's main win (pool + Claude Code hit rate) on the table.

**Consequences:** Cache metadata is shared across Accounts in one process; restart no longer cold-starts if the snapshot loads; operators must treat `prompt_cache.json` as deployment data (not secret credentials, but not disposable noise). Reverting to per-account maps later means reworking store layout, metrics, and tests.
