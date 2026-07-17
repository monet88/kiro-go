package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func mustTools(t *testing.T, schemas map[string]interface{}, names map[string]string) *declaredToolSet {
	t.Helper()
	set, err := buildDeclaredToolSet(names, schemas)
	if err != nil {
		t.Fatalf("tools: %v", err)
	}
	return set
}

func collect(n *assistantNormalizer, events ...kiroSemanticEvent) []assistantEvent {
	var out []assistantEvent
	for _, ev := range events {
		out = append(out, n.handle(ev)...)
	}
	return out
}

func kindsOf(evs []assistantEvent) []string {
	var s []string
	for _, e := range evs {
		switch e.kind {
		case assistantKindPlainText:
			s = append(s, "text:"+e.text)
		case assistantKindReasoning:
			s = append(s, "reasoning:"+e.text)
		case assistantKindToolCall:
			raw, _ := json.Marshal(e.tool.Input)
			s = append(s, fmt.Sprintf("tool:%s:%s:%s", e.tool.ID, e.tool.Name, string(raw)))
		case assistantKindTelemetry:
			if e.hasCredits {
				s = append(s, fmt.Sprintf("credits:%.2f", e.credits))
			}
			if e.hasContext {
				s = append(s, fmt.Sprintf("context:%.2f", e.contextPct))
			}
		case assistantKindCompletion:
			s = append(s, fmt.Sprintf("complete:%s:%d:%d", e.stopReason, e.finalIn, e.finalOut))
		case assistantKindModelOutputError:
			s = append(s, "error:"+e.err.Error())
		}
	}
	return s
}

func TestNormalizerTextReasoningCompletionLateUsage(t *testing.T) {
	n := newAssistantNormalizer(nil)
	out := collect(n,
		mustPlain("Hello"),
		mustReason("think"),
		mustUsage(10, 2),
		mustCredit(1.5),
		mustContext(12.5),
		mustUsage(15, 7), // late usage
		mustStop("END_TURN"),
		newTerminalBoundary(),
	)
	ks := kindsOf(out)
	if !containsPrefix(ks, "text:Hello") || !containsPrefix(ks, "reasoning:think") {
		t.Fatalf("missing text/reasoning: %v", ks)
	}
	// Completion waits for terminal and uses late usage once.
	last := ks[len(ks)-1]
	if last != "complete:end_turn:15:7" {
		t.Fatalf("completion: got %q want complete:end_turn:15:7; full=%v", last, ks)
	}
}

func TestNormalizerThinkingFirstSourceWinsEvent(t *testing.T) {
	n := newAssistantNormalizer(nil)
	out := collect(n,
		mustReason("from-event"),
		mustPlain("<thinking>\ntag-should-drop\n</thinking>\n\nvisible"),
		newTerminalBoundary(),
	)
	ks := kindsOf(out)
	joined := strings.Join(ks, "|")
	if !strings.Contains(joined, "reasoning:from-event") {
		t.Fatalf("missing event reasoning: %v", ks)
	}
	if strings.Contains(joined, "tag-should-drop") {
		t.Fatalf("tag reasoning should lose: %v", ks)
	}
	if !strings.Contains(joined, "visible") {
		t.Fatalf("plain after tag missing: %v", ks)
	}
}

func TestNormalizerThinkingFirstSourceWinsTag(t *testing.T) {
	n := newAssistantNormalizer(nil)
	out := collect(n,
		mustPlain("<thinking>\nfrom-tag\n</thinking>\n\n"),
		mustReason("from-event-should-drop"),
		newTerminalBoundary(),
	)
	ks := kindsOf(out)
	joined := strings.Join(ks, "|")
	if !strings.Contains(joined, "reasoning:from-tag") {
		t.Fatalf("missing tag reasoning: %v", ks)
	}
	if strings.Contains(joined, "from-event-should-drop") {
		t.Fatalf("event reasoning should lose: %v", ks)
	}
}

func TestNormalizerMaxTokensOutranks(t *testing.T) {
	n := newAssistantNormalizer(nil)
	out := collect(n, mustPlain("x"), mustStop("MAX_TOKENS"), newTerminalBoundary())
	ks := kindsOf(out)
	if ks[len(ks)-1] != "complete:max_tokens:0:0" {
		t.Fatalf("got %v", ks)
	}
}

func TestNormalizerResetClearsAttemptState(t *testing.T) {
	tools := mustTools(t, map[string]interface{}{
		"lookup": map[string]interface{}{"type": "object"},
	}, nil)
	n := newAssistantNormalizer(tools)
	_ = collect(n, mustPlain("hi"), mustCredit(1), mustUsage(3, 4))
	n.reset()
	out := collect(n, newTerminalBoundary())
	ks := kindsOf(out)
	if ks[len(ks)-1] != "complete:end_turn:0:0" {
		t.Fatalf("reset failed to clear usage: %v", ks)
	}
}

func TestNormalizerStructuredToolValidated(t *testing.T) {
	tools := mustTools(t, map[string]interface{}{
		"lookup": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"q": map[string]interface{}{"type": "string"},
			},
			"required": []interface{}{"q"},
		},
	}, map[string]string{"lookup": "Lookup"})
	n := newAssistantNormalizer(tools)
	out := collect(n,
		mustToolStart("toolu_1", "lookup"),
		mustToolInput("toolu_1", "lookup", `{"q":"hi"}`, false),
		mustToolStop("toolu_1", "lookup"),
		mustStop("TOOL_USE"),
		newTerminalBoundary(),
	)
	ks := kindsOf(out)
	if !containsPrefix(ks, `tool:toolu_1:Lookup:{"q":"hi"}`) {
		t.Fatalf("tool call: %v", ks)
	}
	if ks[len(ks)-1] != "complete:tool_use:0:0" {
		t.Fatalf("stop reason: %v", ks)
	}
}

func TestNormalizerInterleavedToolsEmitFirstSeenOrder(t *testing.T) {
	tools := mustTools(t, map[string]interface{}{
		"a": map[string]interface{}{"type": "object"},
		"b": map[string]interface{}{"type": "object"},
	}, nil)
	n := newAssistantNormalizer(tools)
	out := collect(n,
		mustToolStart("id_a", "a"),
		mustToolInput("id_a", "a", `{}`, true),
		mustToolStart("id_b", "b"),
		mustToolInput("id_b", "b", `{}`, true),
		// b completes first
		mustToolStop("id_b", "b"),
		mustToolStop("id_a", "a"),
		newTerminalBoundary(),
	)
	var toolsOut []string
	for _, e := range out {
		if e.kind == assistantKindToolCall {
			toolsOut = append(toolsOut, e.tool.ID)
		}
	}
	if len(toolsOut) != 2 || toolsOut[0] != "id_a" || toolsOut[1] != "id_b" {
		t.Fatalf("order: %v full=%v", toolsOut, kindsOf(out))
	}
}

func TestNormalizerIDUpgradeAndGenerated(t *testing.T) {
	tools := mustTools(t, map[string]interface{}{
		"lookup": map[string]interface{}{"type": "object"},
	}, nil)
	n := newAssistantNormalizer(tools)
	out := collect(n,
		mustToolStart("", "lookup"),
		mustToolInput("", "lookup", `{"x":1}`, true),
		mustToolStop("toolu_real", "lookup"),
		newTerminalBoundary(),
	)
	var id string
	for _, e := range out {
		if e.kind == assistantKindToolCall {
			id = e.tool.ID
		}
	}
	if id != "toolu_real" {
		t.Fatalf("expected upgraded id, got %q in %v", id, kindsOf(out))
	}
}

func TestNormalizerConflictingIdentityFails(t *testing.T) {
	tools := mustTools(t, map[string]interface{}{
		"a": map[string]interface{}{"type": "object"},
		"b": map[string]interface{}{"type": "object"},
	}, nil)
	n := newAssistantNormalizer(tools)
	out := collect(n,
		mustToolStart("same", "a"),
		mustToolStart("same", "b"),
	)
	if !hasError(out) {
		t.Fatalf("expected identity conflict: %v", kindsOf(out))
	}
	if !IsModelOutputError(n.failErr) {
		t.Fatalf("expected model output error, got %v", n.failErr)
	}
}

func TestNormalizerRepairAndSchemaFailure(t *testing.T) {
	tools := mustTools(t, map[string]interface{}{
		"lookup": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"q": map[string]interface{}{"type": "string"},
			},
			"required": []interface{}{"q"},
		},
	}, nil)
	n := newAssistantNormalizer(tools)
	// trailing comma repair should succeed
	out := collect(n,
		mustToolStart("t1", "lookup"),
		mustToolInput("t1", "lookup", `{"q":"x",}`, false),
		mustToolStop("t1", "lookup"),
		newTerminalBoundary(),
	)
	if !containsPrefix(kindsOf(out), `tool:t1:lookup:{"q":"x"}`) {
		t.Fatalf("repair path: %v", kindsOf(out))
	}

	n2 := newAssistantNormalizer(tools)
	out2 := collect(n2,
		mustToolStart("t2", "lookup"),
		mustToolInput("t2", "lookup", `{}`, false),
		mustToolStop("t2", "lookup"),
	)
	if !hasError(out2) {
		t.Fatalf("expected schema failure: %v", kindsOf(out2))
	}
}

func TestNormalizerUndeclaredStructuredIsModelOutputError(t *testing.T) {
	tools := mustTools(t, map[string]interface{}{
		"other": map[string]interface{}{"type": "object"},
	}, nil)
	n := newAssistantNormalizer(tools)
	out := collect(n,
		mustToolStart("t", "nope"),
		mustToolInput("t", "nope", `{}`, true),
		mustToolStop("t", "nope"),
	)
	if !hasError(out) || !IsModelOutputError(n.failErr) {
		t.Fatalf("expected rejected tool: %v err=%v", kindsOf(out), n.failErr)
	}
	if strings.Contains(n.failErr.Error(), "{") {
		t.Fatalf("error leaked raw args: %v", n.failErr)
	}
}

func TestNormalizerOversizedInput(t *testing.T) {
	tools := mustTools(t, map[string]interface{}{
		"lookup": map[string]interface{}{"type": "object"},
	}, nil)
	n := newAssistantNormalizer(tools)
	big := strings.Repeat("a", structuredToolInputCapBytes+1)
	out := collect(n,
		mustToolStart("t", "lookup"),
		mustToolInput("t", "lookup", `{"q":"`+big+`"}`, false),
	)
	if !hasError(out) {
		t.Fatalf("expected size cap error: %v", kindsOf(out))
	}
}

func TestNormalizerNarrationRecoveryAndGrace(t *testing.T) {
	tools := mustTools(t, map[string]interface{}{
		"lookup": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"q": map[string]interface{}{"type": "string"},
			},
			"required": []interface{}{"q"},
		},
	}, map[string]string{"lookup": "Lookup"})
	n := newAssistantNormalizer(tools)
	// Complete narration in one line becomes tool call (no visible narration).
	out := collect(n,
		mustPlain(`[Called lookup with args: {"q":"hi"}]`+"\n"),
		newTerminalBoundary(),
	)
	ks := kindsOf(out)
	found := false
	for _, e := range out {
		if e.kind == assistantKindToolCall && e.tool.Name == "Lookup" {
			found = true
			if e.tool.Input["q"] != "hi" {
				t.Fatalf("input: %#v", e.tool.Input)
			}
		}
		if e.kind == assistantKindPlainText && strings.Contains(e.text, "[Called") {
			t.Fatalf("narration should not be visible: %v", ks)
		}
	}
	if !found {
		t.Fatalf("expected narrated tool: %v", ks)
	}
}

func TestNormalizerNarrationHeldAcrossChunks(t *testing.T) {
	tools := mustTools(t, map[string]interface{}{
		"lookup": map[string]interface{}{"type": "object"},
	}, nil)
	n := newAssistantNormalizer(tools)
	out1 := collect(n, mustPlain(`[Called lookup with args: {"a":`))
	if len(out1) != 0 {
		t.Fatalf("premature output: %v", kindsOf(out1))
	}
	out2 := collect(n, mustPlain(`1}]`+"\n"), newTerminalBoundary())
	if !hasTool(out2) {
		t.Fatalf("expected tool after complete: %v", kindsOf(out2))
	}
}

func TestNormalizerIncompleteNarrationAtEOFIsText(t *testing.T) {
	tools := mustTools(t, map[string]interface{}{
		"lookup": map[string]interface{}{"type": "object"},
	}, nil)
	n := newAssistantNormalizer(tools)
	out := collect(n, mustPlain(`[Called lookup with args: {"a":`), newTerminalBoundary())
	joined := strings.Join(kindsOf(out), "|")
	if hasTool(out) {
		t.Fatalf("should not recover incomplete: %v", kindsOf(out))
	}
	if !strings.Contains(joined, "[Called") {
		t.Fatalf("expected text fallback: %v", kindsOf(out))
	}
}

func TestNormalizerNarrationInFenceRemainsText(t *testing.T) {
	tools := mustTools(t, map[string]interface{}{
		"lookup": map[string]interface{}{"type": "object"},
	}, nil)
	n := newAssistantNormalizer(tools)
	out := collect(n,
		mustPlain("```\n"+`[Called lookup with args: {}]`+"\n```\n"),
		newTerminalBoundary(),
	)
	if hasTool(out) {
		t.Fatalf("fenced narration must stay text: %v", kindsOf(out))
	}
}

func TestNormalizerUndeclaredNarrationRemainsText(t *testing.T) {
	tools := mustTools(t, map[string]interface{}{
		"other": map[string]interface{}{"type": "object"},
	}, nil)
	n := newAssistantNormalizer(tools)
	out := collect(n,
		mustPlain(`[Called nope with args: {}]`+"\n"),
		newTerminalBoundary(),
	)
	if hasTool(out) || hasError(out) {
		t.Fatalf("undeclared narration must be text only: %v", kindsOf(out))
	}
}

func TestNormalizerStructuredPrefersOverNarrationGrace(t *testing.T) {
	tools := mustTools(t, map[string]interface{}{
		"lookup": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"q": map[string]interface{}{"type": "string"},
			},
			"required": []interface{}{"q"},
		},
	}, nil)
	n := newAssistantNormalizer(tools)
	out := collect(n,
		mustPlain(`[Called lookup with args: {"q":"hi"}]`+"\n"),
		// telemetry must not expire grace
		mustCredit(0.5),
		mustToolStart("toolu_real", "lookup"),
		mustToolInput("toolu_real", "lookup", `{"q":"hi"}`, false),
		mustToolStop("toolu_real", "lookup"),
		newTerminalBoundary(),
	)
	var toolsOut []normalizedToolCall
	for _, e := range out {
		if e.kind == assistantKindToolCall {
			toolsOut = append(toolsOut, e.tool)
		}
	}
	if len(toolsOut) != 1 || toolsOut[0].ID != "toolu_real" {
		t.Fatalf("expected one structured id tool: %#v / %v", toolsOut, kindsOf(out))
	}
}

func TestNormalizerOneToOneDedupNotGlobal(t *testing.T) {
	tools := mustTools(t, map[string]interface{}{
		"lookup": map[string]interface{}{"type": "object"},
	}, nil)
	n := newAssistantNormalizer(tools)
	out := collect(n,
		mustToolStart("id1", "lookup"),
		mustToolInput("id1", "lookup", `{}`, true),
		mustToolStop("id1", "lookup"),
		mustToolStart("id2", "lookup"),
		mustToolInput("id2", "lookup", `{}`, true),
		mustToolStop("id2", "lookup"),
		newTerminalBoundary(),
	)
	var ids []string
	for _, e := range out {
		if e.kind == assistantKindToolCall {
			ids = append(ids, e.tool.ID)
		}
	}
	if len(ids) != 2 {
		t.Fatalf("two structured ids must remain two calls: %v", ids)
	}
}

func TestNormalizerEOFFinalizesOpenTool(t *testing.T) {
	tools := mustTools(t, map[string]interface{}{
		"lookup": map[string]interface{}{"type": "object"},
	}, nil)
	n := newAssistantNormalizer(tools)
	out := collect(n,
		mustToolStart("t", "lookup"),
		mustToolInput("t", "lookup", `{}`, true),
		newTerminalBoundary(),
	)
	if !hasTool(out) {
		t.Fatalf("EOF should finalize: %v", kindsOf(out))
	}
}

// --- helpers ---

func mustPlain(s string) kiroSemanticEvent {
	ev, err := newPlainTextDelta(s)
	if err != nil {
		panic(err)
	}
	return ev
}
func mustReason(s string) kiroSemanticEvent {
	ev, err := newReasoningDelta(s)
	if err != nil {
		panic(err)
	}
	return ev
}
func mustUsage(in, out int) kiroSemanticEvent { return newUsageSnapshot(in, out) }
func mustCredit(c float64) kiroSemanticEvent {
	ev, err := newCreditDelta(c)
	if err != nil {
		panic(err)
	}
	return ev
}
func mustContext(p float64) kiroSemanticEvent { return newContextUsageSnapshot(p) }
func mustStop(r string) kiroSemanticEvent     { return newStopMetadata(r) }
func mustToolStart(id, name string) kiroSemanticEvent {
	if id == "" {
		// constructor requires id; normalizer synthesizes on empty via start with gen path.
		// Use a temporary and let upgrade happen — for empty start tests call onToolStart via special event.
		// Emit with placeholder that normalizer treats... better: allow empty by constructing manually.
		return kiroSemanticEvent{kind: kiroKindToolStart, toolID: "", toolName: name}
	}
	ev, err := newToolStart(id, name)
	if err != nil {
		panic(err)
	}
	return ev
}
func mustToolInput(id, name, input string, replace bool) kiroSemanticEvent {
	if id == "" {
		return kiroSemanticEvent{kind: kiroKindToolInput, toolID: "", toolName: name, toolInput: input, replace: replace}
	}
	ev, err := newToolInput(id, name, input, replace)
	if err != nil {
		panic(err)
	}
	return ev
}
func mustToolStop(id, name string) kiroSemanticEvent {
	if id == "" {
		return kiroSemanticEvent{kind: kiroKindToolStop, toolID: "", toolName: name}
	}
	ev, err := newToolStop(id, name)
	if err != nil {
		panic(err)
	}
	return ev
}

func containsPrefix(ss []string, want string) bool {
	for _, s := range ss {
		if s == want || strings.HasPrefix(s, want) {
			return true
		}
	}
	return false
}
func hasError(evs []assistantEvent) bool {
	for _, e := range evs {
		if e.kind == assistantKindModelOutputError {
			return true
		}
	}
	return false
}
func hasTool(evs []assistantEvent) bool {
	for _, e := range evs {
		if e.kind == assistantKindToolCall {
			return true
		}
	}
	return false
}
