# Plan: Kiro semantic events and tool-call normalization

- **Status:** agreed after grilling; production implementation not started
- **Base:** `main` at `d068cc4`
- **Primary donor:** `.ref/sub2api/backend/internal/pkg/kiro/translator.go`
- **Pattern reference:** `F:/CLIProxyAPI` streaming translators and tests
- **Clients:** Claude Code CLI (`/v1/messages`) and OpenAI Codex (`/v1/chat/completions`, `/v1/responses`)
- **Glossary:** `CONTEXT.md`
- **ADR:** `docs/adr/0003-normalize-kiro-streams-before-protocol-rendering.md`

## Goal

Create one normalization pipeline for all Kiro stream shapes and all three client protocols, then add:

1. Embedded Tool Narration recovery.
2. Conservative repair of truncated Structured Tool Event input.
3. Full Declared Tool schema validation before client-visible tool output.
4. Parallel/interleaved tool lifecycle support.

The implementation is split into two stacked PRs so the stream refactor has a behavior-parity checkpoint before correctness behavior changes are enabled.

## Canonical flow

```text
AWS Event Stream bytes
  -> frame decoder
  -> Kiro Semantic Extractor
  -> Kiro Semantic Events
  -> attempt-local Assistant Normalizer
  -> Assistant Events
  -> Claude / Chat Completions / Responses adapter
  -> client wire events
```

### Kiro Semantic Events

Use unexported tagged payload structs with constructors that enforce kind/payload invariants. The vocabulary covers:

- Plain assistant text delta.
- Reasoning delta.
- Structured tool start, input, and stop.
- Token usage snapshot.
- Credit delta.
- Context-usage snapshot.
- Stop metadata.
- Typed upstream/model-output error.
- Terminal stream boundary.

The extractor owns AWS wire variants and cumulative/overlapping chunk normalization. Unknown event types are debug-logged and ignored. In PR A, known malformed events preserve current compatibility behavior; PR B makes recognized malformed events typed errors.

### Assistant Events

The Assistant Normalizer emits only client-visible meaning:

- Plain text delta.
- Reasoning delta.
- Normalized Tool Call.
- Typed telemetry update when needed by current handlers.
- Completion with reconciled stop reason and final usage.
- Caller-terminal model-output error.

Recovery provenance stays inside normalizer state and diagnostics. Protocol adapters never branch on `embedded` versus `structured`.

## Shared invariants

### Thinking

- Move the real-tag thinking splitter and source arbitration into the Assistant Normalizer.
- Preserve current first-source-wins behavior between explicit reasoning events and real `<thinking>` tag blocks.
- Embedded Tool Narration is inspected only after text is classified as plain assistant text.
- Protocol adapters decide how to render or suppress reasoning for the requested mode; they do not parse thinking tags.

### Declared Tools and schemas

- Build a request-scoped Declared Tool set after request translation has established canonical/original name mappings.
- Compile each schema once and reuse it across Account attempts.
- Invalid schemas return protocol-appropriate HTTP 400 before the upstream call.
- Allow only internal `$ref` values. Reject external URI/file references without resolution.
- For schema `{}` or a schema without a top-level type, require a JSON object and preserve values unchanged.
- Apply only deterministic schema-directed coercion, then full JSON Schema validation.
- Do not use field-name heuristics in the Normalized Tool Call path.

### Structured Tool Events

- Support both lifecycle `toolUseEvent` frames and complete `assistantResponseEvent.toolUses[]` shapes.
- Normalize complete array entries into start/input/stop semantics.
- Track open tools in a map by stable tool-use ID; permit at most one anonymous open tool.
- If ID is absent at start, synthesize one and upgrade to a real ID if it arrives before stop and the name matches.
- A repeated ID with conflicting name or input is a Model Output Error.
- Preserve first-seen output order, not completion order. Ready calls drain by their assigned ordinal.
- EOF is an implicit stop for open tools. Finalize in first-seen order through the same repair and validation path.
- Buffer at most 1 MiB raw input per Structured Tool Event. Exceeding the cap is a Model Output Error.
- Do not emit partial tool arguments. After validation, adapters may render start/delta/stop frames contiguously when their protocol requires them.

### JSON repair

Repair Structured Tool Event input only. Embedded Tool Narration must contain an already-valid JSON object.

Allowed deterministic repairs:

- Escape raw newline, carriage return, and tab characters inside strings.
- Remove a trailing comma immediately before `}` or `]`.
- Close an unterminated string.
- Close unmatched object/array containers when balance is positive.

Reject instead of guessing when balance is negative, tokens are otherwise invalid, parsing still fails, the result is not an object, or schema validation fails. Never rename fields, delete values, invent defaults, or include raw arguments in client-facing errors.

### Embedded Tool Narration

- Recognize `[Called <name> with args: {...}]` only on a standalone line, allowing surrounding whitespace, outside fenced code blocks.
- The tool name must resolve to a Declared Tool.
- JSON must already parse as an object and pass the same normalization/schema pipeline.
- Invalid or undeclared narration remains original assistant text and does not fail the request.
- Hold an incomplete candidate across chunks, capped at 64 KiB. On cap overflow, degrade the full candidate to plain text.
- At EOF, emit incomplete narration verbatim as plain text.

### Reconciliation and ordering

- Canonical fingerprint: restored Declared Tool name plus canonical JSON after schema-directed normalization.
- Deduplicate one-to-one only across sources. Two structured calls with distinct IDs remain two calls; two separate narration lines remain two calls.
- Grace window lasts until the next semantic output boundary; telemetry does not expire it.
- If the next output is a matching Structured Tool Event, hold the narration candidate through tool stop and emit the structured call with its real ID.
- If the next output is text/reasoning, a non-matching tool, completion, or EOF, commit the narration candidate before that output.
- If a duplicate from the other source arrives later, consume one unmatched fingerprint entry and suppress only that duplicate.

### Errors, routing, and completion

- A Structured Tool Event for an undeclared tool, invalid input, conflicting identity, or exceeded cap becomes a Rejected Tool Attempt.
- Rejected Tool Attempts are caller-terminal Model Output Errors, not Account-attributable failures. Do not fail over or penalize the Account.
- Before any response bytes/status are committed, map Model Output Error to a sanitized HTTP 502 upstream error.
- After stream commitment, use the existing protocol-specific terminal error shape: Claude error event, OpenAI Chat stream error, or Responses `response.failed`, then close once.
- Upstream explicit truncation/max-token metadata outranks inferred end-turn.
- A tool-use stop reason is valid only when at least one Normalized Tool Call was emitted.
- Completion waits for EOF/terminal marker so late usage is aggregated and emitted exactly once.
- All parser, thinking, recovery, reconciliation, and open-tool state is attempt-local and resets before Account failover. Compiled Declared Tool schemas are request-local and survive failover.

## PR A: Kiro Semantic Extractor with parity adapter

**Suggested branch:** `feat/kiro-semantic-events`

### Scope

- Add unexported Kiro Semantic Event kinds, payloads, and constructors under `proxy/`.
- Split AWS frame decoding from semantic extraction in `proxy/kiro_eventstream.go`.
- Extract normalized plain/reasoning deltas, lifecycle tool events, telemetry, stop metadata, and terminal boundary.
- Keep callback-based handlers working through a compatibility adapter to `KiroStreamCallback`.
- Preserve current externally visible behavior, including first-source thinking arbitration remaining in handlers for this PR.
- Do not enable Embedded Tool Narration recovery, JSON repair, full schema validation, strict known-malformed errors, or new complete-tool-array behavior yet.

### Parity tests

Capture callback traces from the current implementation as golden expectations, then run the new extractor plus compatibility adapter against the same binary AWS Event Stream fixtures.

Fixtures must cover:

- Cumulative, overlapping, duplicate, and true-delta assistant content.
- Explicit reasoning events.
- Multipart tool input with and without an initial ID.
- Usage snapshots, credit deltas, context usage, and late usage.
- Metadata stop reasons.
- Unknown event type.
- EOF with an open tool.
- Context cancellation and truncated frames.

Assert ordered callbacks, final token/credit values, generated/real ID behavior, and returned errors. Keep fixture builders deterministic and avoid live upstream dependencies.

### PR A done when

- Package-scoped extractor/parity tests pass.
- Existing proxy tests pass without handler output changes.
- `go build ./...`, `go vet ./proxy/...`, and `go test ./...` pass.
- The production stream path has one decoder/extractor implementation plus the compatibility adapter, not parallel decoders.

## PR B: Assistant Normalizer and tool correctness

**Suggested branch:** `feat/tool-call-normalization`, stacked on PR A.

### Scope

- Add `github.com/santhosh-tekuri/jsonschema/v6@v6.0.2`.
- Add request-scoped Declared Tool compilation and pre-upstream request validation.
- Add attempt-local Assistant Normalizer with thinking arbitration, parallel tool state, repair, schema validation, recovery, reconciliation, stop policy, and final usage aggregation.
- Move current tool name restoration and schema-directed input normalization out of the `CallKiroAPI` callback wrapper into the normalizer.
- Move thinking splitter/source arbitration out of Claude, Chat, and Responses handlers into the normalizer.
- Add Claude, Chat Completions, Responses, and non-stream collectors as Assistant Event adapters.
- Enable complete `assistantResponseEvent.toolUses[]` support and interleaved lifecycle tools.
- Replace compatibility callbacks in handlers, admin probes, and tests; remove `KiroStreamCallback` and its input-normalization wrapper when no usages remain.
- Enable strict known-malformed event errors and protocol-specific Rejected Tool Attempt rendering.

### Normalizer state-machine tests

Use table-driven Kiro Semantic Event sequences and assert ordered Assistant Events, final completion/error, and reset behavior.

Required cases:

- Complete narration in one chunk/line becomes one Normalized Tool Call with no visible narration.
- Narration split across chunks remains pending without premature output.
- Incomplete narration at EOF remains text.
- Prefix in prose, inline code, fenced code, or thinking remains text.
- Undeclared or schema-invalid narration remains text.
- Narration followed by matching structured lifecycle prefers the structured ID.
- Structured call followed by matching narration suppresses only the narration duplicate.
- Two identical structured calls with different IDs remain two calls.
- Two identical narration lines remain two calls.
- Cross-source duplicate matching is one-to-one.
- Telemetry does not expire the grace window.
- Non-matching output flushes the candidate first.
- Interleaved tools complete out of order but emit in first-seen order.
- Missing ID synthesizes once and upgrades before stop.
- Same ID with conflicting content fails.
- Complete tool array and lifecycle duplicate reconcile once.
- Each allowed JSON repair succeeds when the repaired result passes schema.
- Negative balance, invalid tokens, non-object result, missing required field, bad type, enum violation, nested violation, and forbidden additional property fail.
- Schema-directed scalar/object/array coercion occurs before validation; field-name heuristic coercion does not.
- Internal `$ref` works; external `$ref` rejects the request without I/O.
- 64 KiB narration overflow degrades to text.
- 1 MiB structured input overflow becomes a Model Output Error.
- Failover reset discards all attempt-local pending/open/reconciliation state.
- Completion waits for late usage and emits once.

### Protocol adapter tests

- Claude stream: valid tool emits `content_block_start`, one `input_json_delta`, and `content_block_stop`; invalid tool emits a Claude error event and no tool block.
- OpenAI Chat stream: valid tool emits one indexed `tool_calls` delta and finish reason `tool_calls`; invalid tool emits the existing stream error shape and no partial tool call.
- Responses stream: valid tool emits the required function-call lifecycle in monotonic output/sequence order; invalid tool emits `response.failed` and no `response.completed`.
- Non-stream variants produce equivalent text/reasoning/tools/usage and sanitized HTTP 502 on Model Output Error.
- All three protocols produce equivalent Normalized Tool Calls for the same semantic fixture.

### PR B done when

- No production handler parses thinking tags or accumulates raw tool input independently.
- No production path emits unvalidated partial tool arguments or substitutes `{}` after JSON parse failure.
- No `KiroStreamCallback` production usages remain.
- Package-scoped tests, handler tests, `go build ./...`, `go vet ./proxy/...`, and `go test ./...` pass.
- A final Go review checks ordering, memory caps, error classification, schema I/O policy, and Account Routing interaction.

## Patterns adopted from references

### sub2api

- Embedded narration parser shape and chunk-pending concept.
- Conservative JSON balancing/repair primitives.
- Semantic extraction before protocol rendering.
- Tool finalization at lifecycle/EOF boundaries.

Intentional deviations:

- Incomplete narration returns to text instead of being dropped at EOF.
- Recovery requires a standalone line, plain text, a Declared Tool, and schema-valid input.
- Dedup is one-to-one across sources, not global by `name + args`.
- Full request-declared schema validation replaces hard-coded required-field tables.

### CLIProxyAPI

- Per-tool state keyed by stable index/identity for parallel calls.
- Buffer multipart arguments until a lifecycle boundary.
- Preserve first-seen output ordering with allocated output indices.
- Defer terminal completion until late usage can be collected exactly once.
- Keep provider/protocol rendering downstream of canonical state.

Intentional deviation:

- Kiro-Go does not stream partial tool arguments before validation; correctness and recoverability take priority over argument-token latency.

## Out of scope

- Whole-sale port of sub2api `apicompat`.
- Whole-sale port of CLIProxyAPI translators or its Go 1.26 architecture.
- Thinking request injection, structured-output virtual tools, local stop-sequence enforcement, or tool-result compaction.
- Global compiled-schema cache.
- External JSON Schema reference resolution.
- Changes to Account health/failover policy beyond classifying Model Output Error as caller-terminal and non-penalizing.

## Next action

When implementation is authorized, create `feat/kiro-semantic-events` from clean `main` and implement PR A only. Do not start PR B until PR A parity tests and full validation are green.