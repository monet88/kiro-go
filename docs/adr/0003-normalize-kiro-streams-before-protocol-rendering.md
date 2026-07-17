# Normalize Kiro streams before protocol rendering

```yaml
status: accepted
```

Kiro-Go currently decodes AWS Event Stream frames directly into protocol-specific callbacks, so thinking arbitration, tool accumulation, completion, and error behavior are repeated across Claude Messages, OpenAI Chat Completions, and OpenAI Responses. Embedded Tool Narration recovery and truncated tool-input repair would make those paths diverge further if added independently.

**Decision:** streaming passes through two internal, unexported vocabularies before protocol rendering. The Kiro Semantic Event vocabulary represents normalized upstream meaning (plain/reasoning deltas, tool start/input/stop, typed usage/credit/context updates, stop metadata, and upstream errors). An attempt-local Assistant Normalizer converts those events into client-visible Assistant Events after first-source-wins thinking arbitration, parallel tool accumulation, conservative JSON repair, schema-directed coercion, full Declared Tool schema validation, and duplicate reconciliation. Claude, Chat Completions, and Responses adapters render only Assistant Events.

**Tool safety policy:** Embedded Tool Narration is a fallback signal, not an authoritative invocation. It is recoverable only from plain assistant text, on a standalone line outside code fences, for a Declared Tool, with already-valid JSON and schema-valid input. Incomplete or invalid narration remains text. A Structured Tool Event is authoritative intent, but an undeclared, unrepairable, or schema-invalid event becomes a Rejected Tool Attempt and a caller-terminal protocol error; it is not emitted as an empty tool call and does not trigger Account failover.

**Streaming policy:** tool arguments are buffered until tool stop or EOF, then repaired and validated before any tool bytes reach the client. Parallel tools are tracked by stable identity and emitted in first-seen order. Embedded/structured duplicates reconcile one-to-one by canonical fingerprint after name restoration and schema-directed normalization. Completion waits for the terminal stream boundary so late usage can be included exactly once.

**Schema policy:** PR B adds `github.com/santhosh-tekuri/jsonschema/v6` at `v6.0.2`, compatible with Go 1.21. Schemas compile once per request and are reused across Account attempts. Internal `$ref` values are allowed; external resolution and all schema-triggered network/filesystem I/O are forbidden. An invalid Declared Tool schema rejects the client request before any upstream call.

**Delivery:** implement in two stacked changes. PR A introduces Kiro Semantic Events behind a compatibility adapter and proves callback parity. PR B adds the Assistant Normalizer, tool recovery/repair/validation, strict error semantics, protocol adapters, and removes the legacy callback path.

**Consequences:** tool-call latency extends to the tool lifecycle boundary instead of streaming unvalidated partial arguments. The design adds internal types and one validation dependency, but centralizes correctness and prevents invalid tool calls from becoming irreversible client-visible actions. Porting the entire sub2api compatibility stack or CLIProxyAPI translator matrix remains out of scope; only their proven stream-state patterns are adopted.