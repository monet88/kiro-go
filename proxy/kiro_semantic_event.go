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

// toolInputMode distinguishes appending a streamed input fragment from
// replacing the buffered input wholesale (object-shaped input arrives complete).
type toolInputMode int

const (
	toolInputAppend toolInputMode = iota
	toolInputReplace
)

// kiroErrorClass tags a semantic stream error by fault domain so downstream
// normalization can decide routing: an upstream/service fault is retryable and
// may trigger Account failover, while a model-output fault is caller-terminal
// and non-penalizing (per ADR-0003).
type kiroErrorClass int

const (
	// kiroErrorUpstream is a transport/service fault surfaced mid-stream by the
	// upstream (exception frames, invalid-state events). Callers may retry/failover.
	kiroErrorUpstream kiroErrorClass = iota
	// kiroErrorModelOutput is a defect in the model's own output that cannot be
	// recovered. Callers must not penalize the Account or fail over.
	kiroErrorModelOutput
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
	toolInput string        // raw fragment or full replacement JSON
	inputMode toolInputMode // toolInputReplace when toolInput replaces the buffer (object-shaped input)

	// telemetry
	inputTokens  int
	outputTokens int
	credits      float64
	contextPct   float64

	// stop / error / terminal
	stopReason string
	err        error
	errClass   kiroErrorClass
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

func newToolInput(toolID, name, input string, mode toolInputMode) (kiroSemanticEvent, error) {
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
		inputMode: mode,
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

func newUsageSnapshot(inputTokens, outputTokens int) (kiroSemanticEvent, error) {
	if inputTokens < 0 || outputTokens < 0 {
		return kiroSemanticEvent{}, fmt.Errorf("usage snapshot requires non-negative token counts")
	}
	return kiroSemanticEvent{
		kind:         kiroKindUsage,
		inputTokens:  inputTokens,
		outputTokens: outputTokens,
	}, nil
}

func newCreditDelta(credits float64) (kiroSemanticEvent, error) {
	if credits == 0 {
		return kiroSemanticEvent{}, fmt.Errorf("credit delta requires non-zero credits")
	}
	return kiroSemanticEvent{kind: kiroKindCredit, credits: credits}, nil
}

func newContextUsageSnapshot(pct float64) (kiroSemanticEvent, error) {
	if pct < 0 || pct > 100 {
		return kiroSemanticEvent{}, fmt.Errorf("context usage percentage %g outside [0,100]", pct)
	}
	return kiroSemanticEvent{kind: kiroKindContextUsage, contextPct: pct}, nil
}

func newStopMetadata(reason string) (kiroSemanticEvent, error) {
	if reason == "" {
		return kiroSemanticEvent{}, fmt.Errorf("stop metadata requires a non-empty reason")
	}
	return kiroSemanticEvent{kind: kiroKindStopMeta, stopReason: reason}, nil
}

// newStreamError builds a typed semantic error. The class distinguishes an
// upstream/service fault (retryable, may trigger Account failover) from a
// model-output fault (caller-terminal, non-penalizing) per ADR-0003.
func newStreamError(class kiroErrorClass, err error) (kiroSemanticEvent, error) {
	if err == nil {
		return kiroSemanticEvent{}, fmt.Errorf("stream error requires non-nil error")
	}
	if class != kiroErrorUpstream && class != kiroErrorModelOutput {
		return kiroSemanticEvent{}, fmt.Errorf("stream error requires a valid class")
	}
	return kiroSemanticEvent{kind: kiroKindError, err: err, errClass: class}, nil
}

func newTerminalBoundary() kiroSemanticEvent {
	return kiroSemanticEvent{kind: kiroKindTerminal}
}
