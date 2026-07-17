package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

func mapStopReasonClaude(reason string, sawTool bool) string {
	switch reason {
	case "max_tokens":
		return "max_tokens"
	case "tool_use":
		return "tool_use"
	default:
		if sawTool {
			return "tool_use"
		}
		return "end_turn"
	}
}

func mapStopReasonOpenAI(reason string, sawTool bool) string {
	switch reason {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		if sawTool {
			return "tool_calls"
		}
		return "stop"
	}
}

func writeClaudeStreamModelOutputError(sse *streamSSE, msg string) {
	if msg == "" {
		msg = "upstream model output error"
	}
	sse.WriteEvent("error", map[string]interface{}{
		"type": "error",
		"error": map[string]interface{}{
			"type":    "api_error",
			"message": msg,
		},
	})
}

func writeOpenAIStreamModelOutputError(sse *streamSSE, msg string) {
	if msg == "" {
		msg = "upstream model output error"
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"error": map[string]interface{}{
			"message": msg,
			"type":    "server_error",
		},
	})
	sse.WriteData(string(payload))
}

func writeResponsesStreamFailed(sse *streamSSE, respID, model, msg string) {
	if msg == "" {
		msg = "upstream model output error"
	}
	sse.WriteEvent("response.failed", map[string]interface{}{
		"type": "response.failed",
		"response": map[string]interface{}{
			"id":     respID,
			"object": "response",
			"status": "failed",
			"model":  model,
			"error": map[string]interface{}{
				"code":    "server_error",
				"message": msg,
			},
		},
	})
}

// kiroToolFromNormalized bridges to existing handler accumulators that still
// use KiroToolUse for token estimates / storage.
func kiroToolFromNormalized(c normalizedToolCall) KiroToolUse {
	return KiroToolUse{ToolUseID: c.ID, Name: c.Name, Input: c.Input}
}

// writeClaudeToolBlock emits start + one full input_json_delta + stop contiguously.
func writeClaudeToolBlock(sse *streamSSE, index int, call normalizedToolCall) {
	sse.WriteEvent("content_block_start", map[string]interface{}{
		"type":  "content_block_start",
		"index": index,
		"content_block": map[string]interface{}{
			"type":  "tool_use",
			"id":    call.ID,
			"name":  call.Name,
			"input": map[string]interface{}{},
		},
	})
	inputJSON, _ := json.Marshal(call.Input)
	sse.WriteEvent("content_block_delta", map[string]interface{}{
		"type":  "content_block_delta",
		"index": index,
		"delta": map[string]interface{}{
			"type":         "input_json_delta",
			"partial_json": string(inputJSON),
		},
	})
	sse.WriteEvent("content_block_stop", map[string]interface{}{
		"type":  "content_block_stop",
		"index": index,
	})
}

func writeOpenAIToolCallChunk(sse *streamSSE, chatID, model string, index int, call normalizedToolCall) {
	args, _ := json.Marshal(call.Input)
	chunk := map[string]interface{}{
		"id":      chatID,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]interface{}{{
			"index": 0,
			"delta": map[string]interface{}{
				"tool_calls": []map[string]interface{}{{
					"index": index,
					"id":    call.ID,
					"type":  "function",
					"function": map[string]interface{}{
						"name":      call.Name,
						"arguments": string(args),
					},
				}},
			},
			"finish_reason": nil,
		}},
	}
	raw, _ := json.Marshal(chunk)
	sse.WriteData(string(raw))
}

func invalidDeclaredToolHTTPMessage(err error) string {
	if err == nil {
		return "invalid tool schema"
	}
	return fmt.Sprintf("invalid tool schema: %v", err)
}

func writeInvalidDeclaredTool(w http.ResponseWriter, protocol string, err error) {
	msg := invalidDeclaredToolHTTPMessage(err)
	switch protocol {
	case "claude":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(fmt.Sprintf(`{"type":"error","error":{"type":"invalid_request_error","message":%q}}`, msg)))
	default:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(fmt.Sprintf(`{"error":{"message":%q,"type":"invalid_request_error"}}`, msg)))
	}
}
