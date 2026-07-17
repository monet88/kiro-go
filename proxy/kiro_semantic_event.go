package proxy

import "fmt"

// kiroSemanticKind is the closed vocabulary of normalized upstream stream meaning.
// Unexported: protocol adapters and handlers must not branch on raw AWS event types.
type kiroSemanticKind int

const (
	kiroKindPlainText kiroSemanticKind = iota
	kiroKindReasoning
	kiroKindToolStart
	kiroKindToolInput
	kiroKindToolStop
	kiroKindUsage
	kiroKindCredit
	kiroKindContextUsage
	kiroKindStopMeta
	kiroKindError
	kiroKindTerminal
)

// kiroSemanticEvent is one unit of normalized Kiro stream meaning.
// Constructors enforce kind/payload invariants; zero values are invalid.
type kiroSemanticEvent struct {
	kind kiroSemanticKind

	// text kinds
	text string

	// structured tool kinds
	toolID    string
	toolName  string
	toolInput string // raw fragment or full replacement JSON
	replace   bool   // when true, toolInput replaces the buffer (object-shaped input)

	// telemetry
	inputTokens  int
	outputTokens int
	credits      float64
	contextPct   float64

	// stop / error / terminal
	stopReason string
	err        error
}

func newPlainTextDelta(text string) (kiroSemanticEvent, error) {
	if text == "" {
		return kiroSemanticEvent{}, fmt.Errorf("plain text delta requires non-empty text")
	}
	return kiroSemanticEvent{kind: kiroKindPlainText, text: text}, nil
}

func newReasoningDelta(text string) (kiroSemanticEvent, error) {
	if text == "" {
		return kiroSemanticEvent{}, fmt.Errorf("reasoning delta requires non-empty text")
	}
	return kiroSemanticEvent{kind: kiroKindReasoning, text: text}, nil
}

func newToolStart(toolID, name string) (kiroSemanticEvent, error) {
	if name == "" {
		return kiroSemanticEvent{}, fmt.Errorf("tool start requires name")
	}
	if toolID == "" {
		return kiroSemanticEvent{}, fmt.Errorf("tool start requires tool id")
	}
	return kiroSemanticEvent{kind: kiroKindToolStart, toolID: toolID, toolName: name}, nil
}

func newToolInput(toolID, name, input string, replace bool) (kiroSemanticEvent, error) {
	if name == "" {
		return kiroSemanticEvent{}, fmt.Errorf("tool input requires name")
	}
	if toolID == "" {
		return kiroSemanticEvent{}, fmt.Errorf("tool input requires tool id")
	}
	if input == "" {
		return kiroSemanticEvent{}, fmt.Errorf("tool input requires non-empty input")
	}
	return kiroSemanticEvent{
		kind:      kiroKindToolInput,
		toolID:    toolID,
		toolName:  name,
		toolInput: input,
		replace:   replace,
	}, nil
}

func newToolStop(toolID, name string) (kiroSemanticEvent, error) {
	if name == "" {
		return kiroSemanticEvent{}, fmt.Errorf("tool stop requires name")
	}
	if toolID == "" {
		return kiroSemanticEvent{}, fmt.Errorf("tool stop requires tool id")
	}
	return kiroSemanticEvent{kind: kiroKindToolStop, toolID: toolID, toolName: name}, nil
}

func newUsageSnapshot(inputTokens, outputTokens int) kiroSemanticEvent {
	return kiroSemanticEvent{
		kind:         kiroKindUsage,
		inputTokens:  inputTokens,
		outputTokens: outputTokens,
	}
}

func newCreditDelta(credits float64) (kiroSemanticEvent, error) {
	if credits == 0 {
		return kiroSemanticEvent{}, fmt.Errorf("credit delta requires non-zero credits")
	}
	return kiroSemanticEvent{kind: kiroKindCredit, credits: credits}, nil
}

func newContextUsageSnapshot(pct float64) kiroSemanticEvent {
	return kiroSemanticEvent{kind: kiroKindContextUsage, contextPct: pct}
}

func newStopMetadata(reason string) kiroSemanticEvent {
	return kiroSemanticEvent{kind: kiroKindStopMeta, stopReason: reason}
}

func newStreamError(err error) (kiroSemanticEvent, error) {
	if err == nil {
		return kiroSemanticEvent{}, fmt.Errorf("stream error requires non-nil error")
	}
	return kiroSemanticEvent{kind: kiroKindError, err: err}, nil
}

func newTerminalBoundary() kiroSemanticEvent {
	return kiroSemanticEvent{kind: kiroKindTerminal}
}
