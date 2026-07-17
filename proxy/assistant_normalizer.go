package proxy

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

const structuredToolInputCapBytes = 1 << 20 // 1 MiB

// assistantNormalizer is attempt-local. Construct or Reset before each Account
// attempt. Request-scoped declared tools are injected and survive failover.
type assistantNormalizer struct {
	tools *declaredToolSet

	// thinking arbitration: first source wins between explicit reasoning and tags
	thinkingSource string // "", "event", "tag"
	splitter       thinkingSplitter
	tagOpen        bool

	// structured tools
	openByID    map[string]*openTool
	anonOpen    *openTool // at most one anonymous (generated-id) open tool
	nextOrdinal int
	readyCalls  []readyCall
	nextEmitOrd int

	// fingerprints for one-to-one cross-source reconciliation
	structuredFP map[string]int
	narratedFP   map[string]int

	// narration pending state
	narrBuf     strings.Builder
	narrPending *pendingNarration // complete candidate in grace window
	fence       fenceTracker

	// telemetry / stop / completion
	inputTokens  int
	outputTokens int
	credits      float64
	contextPct   float64
	hasContext   bool
	stopReason   string
	sawToolCall  bool
	completed    bool
	failed       bool
	failErr      error

	out []assistantEvent
}

type openTool struct {
	id          string
	name        string // upstream name as seen
	generatedID bool
	ordinal     int
	buf         strings.Builder
	started     bool
}

type pendingNarration struct {
	name     string
	argsJSON string
	input    map[string]interface{}
	fp       string
	tool     *declaredTool
}

type readyCall struct {
	ordinal int
	call    normalizedToolCall
	fp      string
}

func newAssistantNormalizer(tools *declaredToolSet) *assistantNormalizer {
	n := &assistantNormalizer{tools: tools}
	n.reset()
	return n
}

// reset clears all attempt-local state. Compiled schemas are not touched.
func (n *assistantNormalizer) reset() {
	n.thinkingSource = ""
	n.tagOpen = false
	n.openByID = make(map[string]*openTool)
	n.anonOpen = nil
	n.nextOrdinal = 0
	n.readyCalls = nil
	n.nextEmitOrd = 0
	n.structuredFP = make(map[string]int)
	n.narratedFP = make(map[string]int)
	n.narrBuf.Reset()
	n.narrPending = nil
	n.fence = fenceTracker{}
	n.inputTokens = 0
	n.outputTokens = 0
	n.credits = 0
	n.contextPct = 0
	n.hasContext = false
	n.stopReason = ""
	n.sawToolCall = false
	n.completed = false
	n.failed = false
	n.failErr = nil
	n.out = nil
	n.splitter = thinkingSplitter{}
	n.wireSplitter()
}

func (n *assistantNormalizer) wireSplitter() {
	n.splitter.onPlain = func(text string) {
		n.handlePlainAfterThinking(text)
	}
	n.splitter.onOpen = func() {
		if n.thinkingSource == "" {
			n.thinkingSource = "tag"
		}
		n.tagOpen = true
	}
	n.splitter.onThinking = func(text string) {
		if n.thinkingSource == "tag" && text != "" {
			n.out = append(n.out, assistantEvent{kind: assistantKindReasoning, text: text})
		}
	}
	n.splitter.onClose = func() {
		n.tagOpen = false
	}
}

// handle consumes one Kiro Semantic Event and returns zero or more Assistant Events.
func (n *assistantNormalizer) handle(ev kiroSemanticEvent) []assistantEvent {
	if n.failed || n.completed {
		return nil
	}
	n.out = nil
	switch ev.kind {
	case kiroKindPlainText:
		n.onPlainText(ev.text)
	case kiroKindReasoning:
		n.onReasoning(ev.text)
	case kiroKindToolStart:
		n.onToolStart(ev.toolID, ev.toolName)
	case kiroKindToolInput:
		n.onToolInput(ev.toolID, ev.toolName, ev.toolInput, ev.replace)
	case kiroKindToolStop:
		n.onToolStop(ev.toolID, ev.toolName)
	case kiroKindUsage:
		n.inputTokens = ev.inputTokens
		n.outputTokens = ev.outputTokens
	case kiroKindCredit:
		n.credits += ev.credits
		n.out = append(n.out, assistantEvent{
			kind:       assistantKindTelemetry,
			credits:    n.credits,
			hasCredits: true,
		})
	case kiroKindContextUsage:
		n.contextPct = ev.contextPct
		n.hasContext = true
		n.out = append(n.out, assistantEvent{
			kind:       assistantKindTelemetry,
			contextPct: ev.contextPct,
			hasContext: true,
		})
	case kiroKindStopMeta:
		if ev.stopReason != "" {
			n.stopReason = ev.stopReason
		}
	case kiroKindError:
		if ev.err != nil {
			n.fail(ev.err)
		}
	case kiroKindTerminal:
		n.onTerminal()
	}
	out := n.out
	n.out = nil
	return out
}

func (n *assistantNormalizer) onPlainText(text string) {
	if text == "" {
		return
	}
	n.splitter.push(text)
}

func (n *assistantNormalizer) onReasoning(text string) {
	if text == "" {
		return
	}
	// Reasoning is a non-matching output boundary for pending narration.
	n.commitPendingNarrationIfAny()
	if n.thinkingSource == "" {
		n.thinkingSource = "event"
	}
	if n.thinkingSource == "event" {
		n.out = append(n.out, assistantEvent{kind: assistantKindReasoning, text: text})
	}
}

func (n *assistantNormalizer) handlePlainAfterThinking(text string) {
	if text == "" {
		return
	}
	n.feedNarrationText(text)
}

func (n *assistantNormalizer) feedNarrationText(text string) {
	if n.narrBuf.Len() > 0 {
		text = n.narrBuf.String() + text
		n.narrBuf.Reset()
	}
	lines, rest := splitCompleteLines(text)
	for _, line := range lines {
		n.consumeNarrationLine(line)
	}
	if rest == "" {
		return
	}
	if n.narrBuf.Len()+len(rest) > narrationCapBytes {
		overflow := rest
		n.commitPendingNarrationIfAny()
		n.emitPlain(overflow)
		return
	}
	trim := strings.TrimSpace(rest)
	if strings.HasPrefix(trim, "[Called") || strings.HasPrefix(trim, "[") {
		n.narrBuf.WriteString(rest)
		return
	}
	n.emitPlain(rest)
}

func (n *assistantNormalizer) consumeNarrationLine(line string) {
	if n.fence.lineInFence(line) || looksLikeInlineCode(line) {
		n.commitPendingNarrationIfAny()
		n.emitPlain(line + "\n")
		return
	}
	name, argsJSON, ok := tryParseCompleteNarrationLine(line)
	if !ok {
		n.commitPendingNarrationIfAny()
		n.emitPlain(line + "\n")
		return
	}
	n.onCompleteNarrationCandidate(name, argsJSON)
}

func (n *assistantNormalizer) onCompleteNarrationCandidate(name, argsJSON string) {
	if n.tools == nil {
		n.commitPendingNarrationIfAny()
		n.emitPlain(fmt.Sprintf("[Called %s with args: %s]\n", name, argsJSON))
		return
	}
	dt := n.tools.lookup(name)
	if dt == nil {
		n.commitPendingNarrationIfAny()
		n.emitPlain(fmt.Sprintf("[Called %s with args: %s]\n", name, argsJSON))
		return
	}
	var input map[string]interface{}
	if err := json.Unmarshal([]byte(argsJSON), &input); err != nil || input == nil {
		n.commitPendingNarrationIfAny()
		n.emitPlain(fmt.Sprintf("[Called %s with args: %s]\n", name, argsJSON))
		return
	}
	normalized, err := dt.validateToolInput(input)
	if err != nil {
		n.commitPendingNarrationIfAny()
		n.emitPlain(fmt.Sprintf("[Called %s with args: %s]\n", name, argsJSON))
		return
	}
	fp := toolFingerprint(dt.CanonicalName, normalized)
	// Grace window: hold until next semantic output boundary.
	n.narrPending = &pendingNarration{
		name:     name,
		argsJSON: argsJSON,
		input:    normalized,
		fp:       fp,
		tool:     dt,
	}
}

func (n *assistantNormalizer) commitPendingNarrationIfAny() {
	p := n.narrPending
	if p == nil {
		return
	}
	n.narrPending = nil
	if n.structuredFP[p.fp] > 0 {
		n.structuredFP[p.fp]--
		return
	}
	id := "toolu_" + uuid.New().String()
	n.emitToolCall(normalizedToolCall{ID: id, Name: p.tool.CanonicalName, Input: p.input})
	n.narratedFP[p.fp]++
}

func (n *assistantNormalizer) emitPlain(text string) {
	if text == "" {
		return
	}
	n.out = append(n.out, assistantEvent{kind: assistantKindPlainText, text: text})
}

func (n *assistantNormalizer) onToolStart(id, name string) {
	if n.narrPending != nil && !n.pendingCouldMatch(name) {
		n.commitPendingNarrationIfAny()
	}
	if id == "" {
		gen := "toolu_" + uuid.New().String()
		if n.anonOpen != nil {
			n.finalizeOpenTool(n.anonOpen)
		}
		n.anonOpen = &openTool{
			id:          gen,
			name:        name,
			generatedID: true,
			ordinal:     n.nextOrdinal,
			started:     true,
		}
		n.nextOrdinal++
		return
	}
	if existing, ok := n.openByID[id]; ok {
		if existing.name != "" && name != "" && existing.name != name {
			n.fail(newModelOutputError("tool_identity_conflict", "conflicting tool identity in stream"))
			return
		}
		if name != "" {
			existing.name = name
		}
		return
	}
	if n.anonOpen != nil && n.anonOpen.generatedID && (name == "" || n.anonOpen.name == name) {
		n.anonOpen.id = id
		n.anonOpen.generatedID = false
		if name != "" {
			n.anonOpen.name = name
		}
		n.openByID[id] = n.anonOpen
		n.anonOpen = nil
		return
	}
	ot := &openTool{id: id, name: name, ordinal: n.nextOrdinal, started: true}
	n.nextOrdinal++
	n.openByID[id] = ot
}

func (n *assistantNormalizer) pendingCouldMatch(name string) bool {
	if n.narrPending == nil || n.tools == nil {
		return false
	}
	dt := n.tools.lookup(name)
	if dt == nil {
		return false
	}
	return dt.CanonicalName == n.narrPending.tool.CanonicalName
}

func (n *assistantNormalizer) onToolInput(id, name, input string, replace bool) {
	ot := n.findOpenTool(id, name)
	if ot == nil {
		n.onToolStart(id, name)
		ot = n.findOpenTool(id, name)
		if ot == nil {
			return
		}
	}
	if id != "" && ot.generatedID {
		ot.id = id
		ot.generatedID = false
		n.openByID[id] = ot
		if n.anonOpen == ot {
			n.anonOpen = nil
		}
	}
	if replace {
		ot.buf.Reset()
	}
	if ot.buf.Len()+len(input) > structuredToolInputCapBytes {
		n.fail(newModelOutputError("tool_input_too_large", "structured tool input exceeded size limit"))
		return
	}
	ot.buf.WriteString(input)
}

func (n *assistantNormalizer) onToolStop(id, name string) {
	ot := n.findOpenTool(id, name)
	if ot == nil {
		n.onToolStart(id, name)
		ot = n.findOpenTool(id, name)
	}
	if ot == nil {
		return
	}
	if id != "" && ot.generatedID {
		ot.id = id
		ot.generatedID = false
		n.openByID[id] = ot
		if n.anonOpen == ot {
			n.anonOpen = nil
		}
	}
	if name != "" && ot.name == "" {
		ot.name = name
	}
	n.finalizeOpenTool(ot)
}

func (n *assistantNormalizer) findOpenTool(id, name string) *openTool {
	if id != "" {
		if ot := n.openByID[id]; ot != nil {
			return ot
		}
	}
	if n.anonOpen != nil {
		if name == "" || n.anonOpen.name == name || n.anonOpen.name == "" {
			return n.anonOpen
		}
	}
	if name != "" {
		for _, ot := range n.openByID {
			if ot.name == name {
				return ot
			}
		}
	}
	return nil
}

func (n *assistantNormalizer) finalizeOpenTool(ot *openTool) {
	if ot == nil || n.failed {
		return
	}
	if n.anonOpen == ot {
		n.anonOpen = nil
	}
	delete(n.openByID, ot.id)

	raw := ot.buf.String()
	input, err := repairStructuredToolJSON(raw)
	if err != nil {
		n.fail(newModelOutputError("rejected_tool_attempt", "structured tool input could not be repaired"))
		return
	}
	if n.tools == nil {
		n.fail(newModelOutputError("rejected_tool_attempt", "structured tool is not a declared tool"))
		return
	}
	dt := n.tools.lookup(ot.name)
	if dt == nil {
		n.fail(newModelOutputError("rejected_tool_attempt", "structured tool is not a declared tool"))
		return
	}
	normalized, err := dt.validateToolInput(input)
	if err != nil {
		n.fail(newModelOutputError("rejected_tool_attempt", "structured tool input failed schema validation"))
		return
	}
	fp := toolFingerprint(dt.CanonicalName, normalized)

	// Matching pending narration: structured wins with real ID.
	if n.narrPending != nil && n.narrPending.fp == fp {
		n.narrPending = nil
	} else if n.narratedFP[fp] > 0 {
		// Narration already emitted this fingerprint — suppress structured duplicate.
		n.narratedFP[fp]--
		return
	}

	n.structuredFP[fp]++
	n.enqueueReady(ot.ordinal, normalizedToolCall{
		ID:    ot.id,
		Name:  dt.CanonicalName,
		Input: normalized,
	}, fp)
}

func (n *assistantNormalizer) enqueueReady(ordinal int, call normalizedToolCall, fp string) {
	n.readyCalls = append(n.readyCalls, readyCall{ordinal: ordinal, call: call, fp: fp})
	// Keep sorted by ordinal (small N).
	for i := len(n.readyCalls) - 1; i > 0; i-- {
		if n.readyCalls[i].ordinal < n.readyCalls[i-1].ordinal {
			n.readyCalls[i], n.readyCalls[i-1] = n.readyCalls[i-1], n.readyCalls[i]
			continue
		}
		break
	}
	n.drainReady()
}

func (n *assistantNormalizer) drainReady() {
	for len(n.readyCalls) > 0 && n.readyCalls[0].ordinal == n.nextEmitOrd {
		rc := n.readyCalls[0]
		n.readyCalls = n.readyCalls[1:]
		n.nextEmitOrd++
		n.emitToolCall(rc.call)
	}
	// If earlier ordinals failed/skipped, still progress when only later ordinals remain?
	// First-seen order requires waiting for earlier ordinals to complete. At terminal,
	// all open tools finalize so ordinals fill in.
}

func (n *assistantNormalizer) emitToolCall(call normalizedToolCall) {
	n.sawToolCall = true
	n.out = append(n.out, assistantEvent{kind: assistantKindToolCall, tool: call})
}

func (n *assistantNormalizer) onTerminal() {
	n.splitter.flush()
	if n.narrBuf.Len() > 0 {
		// Incomplete narration at EOF remains text. Also flush any complete pending first?
		// Spec: incomplete candidate at terminal boundary emitted verbatim as text.
		// Complete pending (held in grace) should commit as tool if no structured match.
		n.commitPendingNarrationIfAny()
		n.emitPlain(n.narrBuf.String())
		n.narrBuf.Reset()
	} else {
		n.commitPendingNarrationIfAny()
	}

	var open []*openTool
	if n.anonOpen != nil {
		open = append(open, n.anonOpen)
	}
	for _, ot := range n.openByID {
		open = append(open, ot)
	}
	for i := 0; i < len(open); i++ {
		for j := i + 1; j < len(open); j++ {
			if open[j].ordinal < open[i].ordinal {
				open[i], open[j] = open[j], open[i]
			}
		}
	}
	for _, ot := range open {
		n.finalizeOpenTool(ot)
	}
	// Force-drain any remaining ready calls in ordinal order even if gaps remain.
	for len(n.readyCalls) > 0 {
		rc := n.readyCalls[0]
		n.readyCalls = n.readyCalls[1:]
		if rc.ordinal >= n.nextEmitOrd {
			n.nextEmitOrd = rc.ordinal + 1
		}
		n.emitToolCall(rc.call)
	}
	if n.failed {
		return
	}
	n.completed = true
	n.out = append(n.out, assistantEvent{
		kind:       assistantKindCompletion,
		stopReason: n.reconcileStopReason(),
		finalIn:    n.inputTokens,
		finalOut:   n.outputTokens,
		finalCred:  n.credits,
	})
}

func (n *assistantNormalizer) reconcileStopReason() string {
	up := strings.ToUpper(strings.TrimSpace(n.stopReason))
	switch up {
	case "MAX_TOKENS", "MAXTOKENS", "LENGTH", "TRUNCATED":
		return "max_tokens"
	case "TOOL_USE", "TOOLUSE":
		if n.sawToolCall {
			return "tool_use"
		}
		return "end_turn"
	}
	if n.sawToolCall {
		return "tool_use"
	}
	return "end_turn"
}

func (n *assistantNormalizer) fail(err error) {
	if n.failed {
		return
	}
	n.failed = true
	n.failErr = err
	n.out = append(n.out, assistantEvent{kind: assistantKindModelOutputError, err: err})
}

func toolFingerprint(canonicalName string, input map[string]interface{}) string {
	raw, _ := json.Marshal(input)
	return canonicalName + "\x00" + string(raw)
}
