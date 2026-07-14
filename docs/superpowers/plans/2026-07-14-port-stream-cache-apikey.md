# Plan: Port stream / cache / API-key Account (ngh1105 → monet88)

**Status:** agreed after grilling; implementation not started  
**Base:** monet88 (`main` @ local) — selective port, not wholesale replace  
**Donor:** ngh1105/main  
**Clients:** Claude Code CLI (`/v1/messages`), OpenAI Codex (`/v1/chat/completions`, `/v1/responses`)  
**Glossary:** `CONTEXT.md`  
**ADRs:** `docs/adr/0001-cross-account-prompt-cache.md`, `docs/adr/0002-kiro-api-key-dual-write.md`

## Order

1. **Wave A — stream**  
2. **Wave B — cache + payload knobs**  
3. **Wave C — API-key Account MVP**  
4. **Cutover VPS** once A+B+C smoke-pass (VPS stays on ngh trial image until then)

## Wave A — stream

**Goals**

- Stream Keepalive on Claude Messages, OpenAI chat stream, Responses SSE (shared helper; donor idle interval ~10s; comment `: keepalive\n\n` only when silent).
- Tool Compression before upstream send (`proxy/tool_compression.go` from donor; env `KIRO_TOOLS_COMPRESS_THRESHOLD_BYTES`).
- Preserve explicit `temperature: 0` via pointer fields on request structs.
- Client-facing hint for opaque upstream "Improperly formed request".

**Primary files (local layout)**

- `proxy/handler_stream.go` — shared SSE/keepalive helper
- `proxy/handler_claude.go`, `proxy/handler_openai.go`, `proxy/responses_handler.go` — wire keepalive
- New: `proxy/tool_compression.go` (+ tests)
- Request struct / translator temperature fields
- Error path next to existing Claude/OpenAI error helpers

**Validate**

- Unit tests for compression threshold/steps and temperature omitempty vs `0`
- Manual or handler test: idle stream emits keepalive comment without breaking event framing

## Wave B — cache

**Goals**

- Replace per-account map with Cross-account Prompt Cache + LRU
- Disk Prompt Cache Snapshot default `data/prompt_cache.json` (load start, periodic atomic flush, restrictive mode)
- Full structural package: system-skip, auto-prefix, Opus min, strip volatile keys
- Config + admin settings API: `maxPayloadBytes` (default 2_000_000), `promptCacheMaxEntries` (default 131072), `promptCacheMaxRatio` (default 0.85)
- Keep local cache metrics panel unless API shape forces a minimal adapt

**Primary files**

- `proxy/cache_tracker.go` (+ `cache_tracker_test.go`) — prefer port donor logic, rewire to monet88 Handler/metrics
- `config/config.go` / settings modules — new fields + getters/updaters
- `proxy/admin_settings.go` — expose knobs on existing GET/POST settings
- `main.go` / Handler ctor — Load + startSaveLoop path derived from config data dir
- Optional light `web/app.js` / locales only if settings page already has similar fields

**Validate**

- Tests: structural system-skip stability across turns; LRU eviction; Load/Save round-trip; ratio cap
- Settings GET shows defaults; POST persists and process reads new values

## Wave C — API-key Account MVP

**Goals**

- `AuthMethod=api_key`, `KiroApiKey` source of truth, dual-write `AccessToken` (ADR-0002)
- Headers / token type API_KEY; skip refresh + profile ARN when `IsApiKeyCredential()`
- Admin: add one + import one JSON + mask `ksk_…`
- No bulk batch in this wave

**Primary files**

- `config` Account type + `IsApiKeyCredential`
- `proxy/kiro_headers.go` / auth apply paths
- Account refresh / pool paths — early-return for API-key Account
- `proxy/admin_accounts.go` (+ import) and `web` account UI/mask
- Tests for dual-write invariant and no-refresh behavior

**Validate**

- Unit: create/import API-key Account → AccessToken == KiroApiKey; refresh skipped
- Mask in list/detail responses and logs

## Delivery / VPS

- Branch from monet88 `main` (e.g. `port/stream-cache-apikey`)
- Prefer small commits per wave (or one PR with three commits)
- VPS: keep `kiro-go:ngh1105-main` until monet88+port image smoke OK
- Cutover: build/push monet88 image → compose/env image tag → preserve `./data` → health + models + one Claude stream + one Codex stream
- Rollback: existing `*20260714-060205*` compose/env/image backups

## Out of scope

- Prometheus rewrite  
- Long ban/cooldown dispatch from ngh  
- Reverted external_idp harden  
- Bulk `apikeys-batch`  
- Wholesale replace monet88 with ngh monolith layout  

## Done when

- [ ] Wave A tests + stream smoke green  
- [ ] Wave B tests + cache snapshot file after traffic  
- [ ] Wave C tests + masked admin surfaces  
- [ ] Image built; VPS cutover optional (operator-triggered)  
- [ ] No scope creep from out-of-scope list  

## Next action after this doc

User says start Wave A → create branch → implement Wave A only first.
