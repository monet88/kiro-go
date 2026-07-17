package proxy

// assistantEventKind is the closed client-visible vocabulary after normalization.
type assistantEventKind int

const (
	assistantKindPlainText assistantEventKind = iota
	assistantKindReasoning
	assistantKindToolCall
	assistantKindTelemetry
	assistantKindCompletion
	assistantKindModelOutputError
)

// assistantEvent is one client-visible unit. Protocol adapters render only these.
type assistantEvent struct {
	kind assistantEventKind

	text string

	// tool call
	tool normalizedToolCall

	// telemetry (optional; emitted when handlers need intermediate updates)
	inputTokens  int
	outputTokens int
	credits      float64
	contextPct   float64
	hasUsage     bool
	hasCredits   bool
	hasContext   bool

	// completion
	stopReason string
	finalIn    int
	finalOut   int
	finalCred  float64

	// model output error (sanitized; never carries raw tool args)
	err error
}

// normalizedToolCall is the single client-visible tool invocation after name
// restoration, repair, schema-directed coercion, validation, and reconciliation.
type normalizedToolCall struct {
	ID    string
	Name  string // canonical/client-facing
	Input map[string]interface{}
}

// modelOutputError is a caller-terminal, non-penalizing failure caused by model
// output defects (Rejected Tool Attempt, identity conflict, oversized input).
// It must not trigger Account failover or Account health penalties.
type modelOutputError struct {
	// Code is a stable machine-readable class (no raw arguments).
	Code string
	// Message is a sanitized human-readable description.
	Message string
}

func (e *modelOutputError) Error() string {
	if e == nil {
		return ""
	}
	if e.Code == "" {
		return e.Message
	}
	if e.Message == "" {
		return e.Code
	}
	return e.Code + ": " + e.Message
}

func newModelOutputError(code, message string) *modelOutputError {
	return &modelOutputError{Code: code, Message: message}
}

// IsModelOutputError reports whether err is (or wraps) a model-output defect.
func IsModelOutputError(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := err.(*modelOutputError); ok {
		return true
	}
	// unwrap one level of fmt.Errorf %w if needed via type assert chain
	type unwrapper interface{ Unwrap() error }
	for {
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
		if err == nil {
			return false
		}
		if _, ok := err.(*modelOutputError); ok {
			return true
		}
	}
}
