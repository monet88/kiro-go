# Kiro-Go Proxy

Domain language for the monet88 Kiro-Go proxy: credential kinds, cache, and stream behavior.

## Language

### Credentials

**Gateway API Key**:
A client credential that authorizes callers to use this proxy (`ApiKeyEntry` / `config.ApiKeys`, typically `sk-…`).
_Avoid_: API key (unqualified), proxy password, account key

**Account**:
An upstream Kiro credential held in the account pool and selected for outbound requests.
_Avoid_: user, login, key

**API-key Account**:
An Account whose upstream auth is a static Kiro API key bearer (`ksk_…`), with `AuthMethod=api_key` and no OAuth refresh.
_Avoid_: Gateway API Key, ApiKeyEntry, social account, API key (unqualified)

**Kiro API Key**:
The secret value of an API-key Account (`ksk_…`), stored in `KiroApiKey` as source of truth and mirrored into `AccessToken` on every write path.
_Avoid_: Gateway API Key, access token (unqualified)

**OAuth Account**:
An Account refreshed via OAuth-style tokens — `idc`, `social`, or `external_idp`.
_Avoid_: API-key Account

**AuthMethod**:
The Account field that selects upstream auth behavior: `idc`, `social`, `external_idp`, or (Wave C) `api_key`.

### Stream & payload

**Stream Keepalive**:
Periodic SSE comment frames sent while an upstream stream is idle so intermediaries do not cut the connection.

**Primary clients (ops assumption)**:
- Claude Code CLI → Anthropic Messages stream (`/v1/messages`)
- OpenAI Codex → OpenAI-compatible stream (`/v1/chat/completions` and/or `/v1/responses`)

**Tool Compression**:
Shrinking oversized tool definitions before the upstream Kiro request to avoid hard request rejection. Default threshold follows ngh1105 (20KB), overridable via `KIRO_TOOLS_COMPRESS_THRESHOLD_BYTES`.

**Wave A stream package** (clients: Claude Code CLI + OpenAI Codex):
- Stream Keepalive on Claude Messages, OpenAI chat completions, and Responses SSE paths
- Tool Compression before upstream send
- Preserve explicit `temperature: 0` via pointer fields on request structs
- Client-facing hint for opaque upstream "Improperly formed request"

### Cache

**Prompt Cache**:
The proxy-side tracker of prompt prefixes used to estimate or preserve cache hits across turns/requests.

**Cache Fingerprint**:
The identity of a cacheable prompt prefix entry inside the Prompt Cache.

**Cross-account Prompt Cache**:
Wave B stores Prompt Cache entries globally by Cache Fingerprint (not per Account), under the assumption that all Accounts in one proxy deployment share the same upstream cache/billing domain.
_Avoid_: per-account cache isolation (legacy local behavior)

**Prompt Cache Snapshot**:
The on-disk persistence of the Prompt Cache under the data directory (default `data/prompt_cache.json`), loaded on start and flushed periodically/atomically with restrictive file mode.
_Avoid_: embedding cache entries inside `config.json`

**Wave B cache package**:
- Cross-account Prompt Cache by Cache Fingerprint
- In-memory LRU with `PromptCacheMaxEntries` (default **131072**, config-overridable)
- Prompt Cache Snapshot on disk (default `data/prompt_cache.json`)
- `MaxPayloadBytes` configurable (default **2_000_000**), replacing the local hard ~900KiB limit
- `PromptCacheMaxRatio` default **0.85**, config + admin API alongside payload/entries
- Full structural fingerprint package from ngh1105: system-skip for leading dynamic `system[]` blocks, auto-prefix breakpoints without explicit `cache_control`, Opus min-cacheable tokens, strip volatile position/billing keys
- Operator knobs: config fields + admin settings API; light web wiring only if local settings page already has the pattern
- Keep local cache metrics/admin panel surface unless a later decision replaces it

**API-key Account invariants** (Wave C):
- `AuthMethod` canonical value is `api_key`
- Source of truth for the secret is `KiroApiKey`
- After every write path: `AccessToken == KiroApiKey`
- `IsApiKeyCredential()` true implies no OAuth refresh and no profile-ARN resolution

**Wave C operator surface (MVP)**:
- Add one API-key Account via admin API + UI
- Import one JSON account (`authMethod=api_key`, `kiroApiKey`)
- Mask `ksk_…` in logs/UI
- Bulk `apikeys-batch` is out of MVP (later wave)

### Delivery

**Port base**: monet88 codebase only (selective port from ngh1105; no wholesale replace).

**Implementation order**: Wave A (stream) → Wave B (cache) → Wave C (API-key Account MVP).

**VPS while coding**: keep current ngh1105 trial image until monet88+port image is smoke-tested.

**Cutover**: one switch to monet88 image with ports; preserve `./data`; use existing compose/env backups for rollback.

**Out of scope (this port)**:
- Prometheus rewrite
- Long ban/cooldown dispatch from ngh
- Reverted external_idp harden
- Bulk apikeys-batch
