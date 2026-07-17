package proxy

import (
	"encoding/json"
	"kiro-go/logger"

	"github.com/google/uuid"
)

// kiroSemanticExtractor converts decoded AWS Event Stream payloads into the
// closed Kiro Semantic Event vocabulary. It owns chunk normalization and the
// single-open-tool lifecycle used by current production streams (PR A).
// PR B will expand parallel-tool and complete-array behavior in the normalizer.
type kiroSemanticExtractor struct {
	lastAssistantContent string
	lastReasoningContent string
	inputTokens          int
	outputTokens         int
	openTool             *extractorToolState
}

type extractorToolState struct {
	toolUseID   string
	name        string
	generatedID bool
	started     bool
}

func newKiroSemanticExtractor() *kiroSemanticExtractor {
	return &kiroSemanticExtractor{}
}

// ingestJSONPayload consumes one decoded event payload and returns zero or more
// semantic events. Unknown event types are debug-logged and ignored. Recognized
// malformed payloads preserve current compatibility behavior (skip/continue).
func (e *kiroSemanticExtractor) ingestJSONPayload(eventType string, payloadBytes []byte) []kiroSemanticEvent {
	if len(payloadBytes) == 0 {
		return nil
	}
	var event map[string]interface{}
	if err := json.Unmarshal(payloadBytes, &event); err != nil {
		// Current production path silently continues on malformed JSON.
		return nil
	}

	var out []kiroSemanticEvent

	// Token usage can appear on any event; capture as a snapshot when values change.
	prevIn, prevOut := e.inputTokens, e.outputTokens
	e.inputTokens, e.outputTokens = updateTokensFromEvent(event, e.inputTokens, e.outputTokens)
	if e.inputTokens != prevIn || e.outputTokens != prevOut {
		out = append(out, newUsageSnapshot(e.inputTokens, e.outputTokens))
	}

	switch eventType {
	case "assistantResponseEvent":
		if content, ok := event["content"].(string); ok && content != "" {
			normalized := normalizeChunk(content, &e.lastAssistantContent)
			if normalized != "" {
				if ev, err := newPlainTextDelta(normalized); err == nil {
					out = append(out, ev)
				}
			}
		}
		// Complete toolUses[] array entries become start/input/stop semantics.
		if arr, ok := event["toolUses"].([]interface{}); ok {
			for _, item := range arr {
				m, ok := item.(map[string]interface{})
				if !ok {
					continue
				}
				out = append(out, e.ingestCompleteToolUse(m)...)
			}
		}
	case "reasoningContentEvent":
		if text, ok := event["text"].(string); ok && text != "" {
			normalized := normalizeChunk(text, &e.lastReasoningContent)
			if normalized != "" {
				if ev, err := newReasoningDelta(normalized); err == nil {
					out = append(out, ev)
				}
			}
		}
	case "toolUseEvent":
		out = append(out, e.ingestToolUseEvent(event)...)
	case "meteringEvent":
		if usage, ok := event["usage"].(float64); ok && usage != 0 {
			if ev, err := newCreditDelta(usage); err == nil {
				out = append(out, ev)
			}
		}
	case "contextUsageEvent":
		if pct, ok := event["contextUsagePercentage"].(float64); ok {
			out = append(out, newContextUsageSnapshot(pct))
		}
	case "metadataEvent":
		// Informative stop metadata. PR A compatibility adapter does not surface
		// this on callbacks; PR B will consume it for stop-reason reconciliation.
		reason := firstStringField(event, "stopReason", "stop_reason")
		out = append(out, newStopMetadata(reason))
	default:
		logger.Debugf("[EventStream] Unhandled event type=%q payload=%s", eventType, string(payloadBytes))
	}
	return out
}

func (e *kiroSemanticExtractor) ingestToolUseEvent(event map[string]interface{}) []kiroSemanticEvent {
	toolUseID := firstStringField(event, "toolUseId", "toolUseID", "tool_use_id", "id")
	name := firstStringField(event, "name", "toolName", "tool_name")
	isStop := firstBoolField(event, "stop", "isStop", "done")

	var out []kiroSemanticEvent

	// Identity transitions mirror handleToolUseEvent so callback parity holds.
	if toolUseID != "" && name != "" {
		if e.openTool == nil {
			e.openTool = &extractorToolState{toolUseID: toolUseID, name: name}
		} else if e.openTool.toolUseID != toolUseID {
			if e.openTool.generatedID && e.openTool.name == name {
				e.openTool.toolUseID = toolUseID
				e.openTool.generatedID = false
			} else {
				out = append(out, e.emitToolStop()...)
				e.openTool = &extractorToolState{toolUseID: toolUseID, name: name}
			}
		}
	} else if name != "" && e.openTool == nil {
		e.openTool = &extractorToolState{
			toolUseID:   "toolu_" + uuid.New().String(),
			name:        name,
			generatedID: true,
		}
	} else if name != "" && e.openTool != nil && e.openTool.name != name {
		out = append(out, e.emitToolStop()...)
		e.openTool = &extractorToolState{
			toolUseID:   "toolu_" + uuid.New().String(),
			name:        name,
			generatedID: true,
		}
	}

	if e.openTool == nil {
		return out
	}

	if !e.openTool.started {
		if ev, err := newToolStart(e.openTool.toolUseID, e.openTool.name); err == nil {
			out = append(out, ev)
		}
		e.openTool.started = true
	}

	// Input fragments. Object-shaped input replaces the buffer (same as today).
	if input, ok := event["input"].(string); ok && input != "" {
		if ev, err := newToolInput(e.openTool.toolUseID, e.openTool.name, input, false); err == nil {
			out = append(out, ev)
		}
	} else if inputObj, ok := event["input"].(map[string]interface{}); ok {
		data, _ := json.Marshal(inputObj)
		if len(data) > 0 {
			if ev, err := newToolInput(e.openTool.toolUseID, e.openTool.name, string(data), true); err == nil {
				out = append(out, ev)
			}
		}
	}

	if isStop {
		out = append(out, e.emitToolStop()...)
	}
	return out
}

func (e *kiroSemanticExtractor) emitToolStop() []kiroSemanticEvent {
	if e.openTool == nil || e.openTool.name == "" {
		e.openTool = nil
		return nil
	}
	id := e.openTool.toolUseID
	if id == "" {
		id = "toolu_" + uuid.New().String()
	}
	name := e.openTool.name
	e.openTool = nil
	if ev, err := newToolStop(id, name); err == nil {
		return []kiroSemanticEvent{ev}
	}
	return nil
}

// finish emits an implicit tool stop for any open tool, then the terminal boundary.
func (e *kiroSemanticExtractor) finish() []kiroSemanticEvent {
	var out []kiroSemanticEvent
	out = append(out, e.emitToolStop()...)
	out = append(out, newTerminalBoundary())
	return out
}

func (e *kiroSemanticExtractor) ingestCompleteToolUse(event map[string]interface{}) []kiroSemanticEvent {
	toolUseID := firstStringField(event, "toolUseId", "toolUseID", "tool_use_id", "id")
	name := firstStringField(event, "name", "toolName", "tool_name")
	if name == "" {
		return nil
	}
	if toolUseID == "" {
		toolUseID = "toolu_" + uuid.New().String()
	}
	var out []kiroSemanticEvent
	if ev, err := newToolStart(toolUseID, name); err == nil {
		out = append(out, ev)
	}
	if input, ok := event["input"].(string); ok && input != "" {
		if ev, err := newToolInput(toolUseID, name, input, false); err == nil {
			out = append(out, ev)
		}
	} else if inputObj, ok := event["input"].(map[string]interface{}); ok {
		data, _ := json.Marshal(inputObj)
		if len(data) > 0 {
			if ev, err := newToolInput(toolUseID, name, string(data), true); err == nil {
				out = append(out, ev)
			}
		}
	}
	if ev, err := newToolStop(toolUseID, name); err == nil {
		out = append(out, ev)
	}
	return out
}
