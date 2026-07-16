package proxy

import (
	"context"
	"encoding/json"
	"io"
	"kiro-go/config"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

func validateOpenAIRequestShape(req *OpenAIRequest) string {
	if len(req.Messages) == 0 {
		return "messages must not be empty"
	}

	hasNonSystem := false
	hasUserContext := false
	lastRole := ""
	for _, msg := range req.Messages {
		role := strings.TrimSpace(msg.Role)
		if role == "" {
			continue
		}
		if role != "system" {
			hasNonSystem = true
			lastRole = role
		}

		if role != "user" {
			continue
		}
		text, images := extractOpenAIUserContent(msg.Content)
		if normalizeUserContent(text, len(images) > 0) != "" {
			hasUserContext = true
		}
	}

	if !hasNonSystem {
		return "at least one non-system message is required"
	}
	if lastRole == "assistant" {
		return "assistant-prefill final message is not supported; last message must be user or tool"
	}
	if !hasUserContext {
		return "at least one non-empty user message is required"
	}
	return ""
}

// handleOpenAIChat OpenAI API 处理
func (h *Handler) handleOpenAIChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		if maxBytesExceeded(err) {
			h.sendOpenAIError(w, 413, "invalid_request_error", "Request body too large")
			return
		}
		h.sendOpenAIError(w, 400, "invalid_request_error", "Failed to read request body")
		return
	}

	logInboundRequestProbe("openai", r, body)

	var req OpenAIRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", "Invalid JSON")
		return
	}
	if msg := validateOpenAIRequestShape(&req); msg != "" {
		h.sendOpenAIError(w, 400, "invalid_request_error", msg)
		return
	}

	// 解析模型和 thinking 模式
	thinkingCfg := config.GetThinkingConfig()
	actualModel, thinking := ParseModelAndThinking(req.Model, thinkingCfg.Suffix)
	req.Model = actualModel
	estimatedInputTokens := estimateOpenAIRequestInputTokens(&req)

	kiroPayload := OpenAIToKiro(&req, thinking)

	apiKeyID := apiKeyIDFromContext(r.Context())
	if req.Stream {
		h.handleOpenAIStream(r.Context(), w, kiroPayload, req.Model, thinking, estimatedInputTokens, apiKeyID)
	} else {
		if apiKeyForbidsSyncRequests(apiKeyID) {
			h.sendOpenAIError(w, 403, "permission_error", "This API key only permits streaming requests; set \"stream\": true.")
			return
		}
		h.handleOpenAINonStream(r.Context(), w, kiroPayload, req.Model, thinking, estimatedInputTokens, apiKeyID)
	}
}

// handleOpenAIStream OpenAI 流式响应
func (h *Handler) handleOpenAIStream(ctx context.Context, w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, estimatedInputTokens int, apiKeyID string) {
	requestStartedAt := time.Now()
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Disable reverse-proxy response buffering (nginx/Caddy-compatible) so each
	// SSE chunk reaches the client immediately instead of waiting for a buffer fill.
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendOpenAIError(w, 500, "server_error", "Streaming not supported")
		return
	}

	// Stream Keepalive: idle SSE comments only; real chunks go through sse.WriteData.
	sse := startStreamSSE(w, flusher)
	defer sse.Stop()

	// 获取 thinking 输出格式配置
	thinkingFormat := config.GetThinkingConfig().OpenAIFormat

	chatID := "chatcmpl-" + uuid.New().String()

	// Per-attempt render state, hoisted to caller scope so terminal rendering
	// (final chunk / error close) runs after Account Routing releases the slot,
	// while token streaming still happens inside the callback under the slot.
	var toolCalls []ToolCall
	var toolCallIndex int
	var inputTokens, outputTokens int
	var credits float64
	var realInputTokens int
	var rawContentBuilder strings.Builder
	var rawReasoningBuilder strings.Builder
	var firstTokenAt time.Time
	var dropTagThinking bool
	var thinkingSource thinkingStreamSource
	var thinkingStarted bool
	var eventThinkingOpen bool
	responseStarted := false
	var okAccount *config.Account

	outcome := h.runWithAccount(ctx, model, payload.RoutingAffinityKey, func(account *config.Account) attemptResult {
		// Reset per-attempt state so a failover starts clean.
		toolCalls = nil
		toolCallIndex = 0
		inputTokens, outputTokens = 0, 0
		credits = 0
		realInputTokens = 0
		rawContentBuilder.Reset()
		rawReasoningBuilder.Reset()
		firstTokenAt = time.Time{}
		dropTagThinking = false
		thinkingSource = thinkingSourceUnknown
		thinkingStarted = false
		eventThinkingOpen = false
		responseStarted = false

		sendChunk := func(content string, thinkingState int) {
			if content == "" && thinkingState == 2 {
				return
			}

			var chunk map[string]interface{}

			if thinkingState > 0 {
				if !thinking {
					return
				}
				switch thinkingFormat {
				case "thinking":
					var text string
					switch thinkingState {
					case 1:
						text = "<thinking>" + content
					case 2:
						text = content
					case 3:
						text = content + "</thinking>"
					}
					if text == "" {
						return
					}
					chunk = map[string]interface{}{
						"id":      chatID,
						"object":  "chat.completion.chunk",
						"created": time.Now().Unix(),
						"model":   model,
						"choices": []map[string]interface{}{{
							"index":         0,
							"delta":         map[string]string{"content": text},
							"finish_reason": nil,
						}},
					}
				case "think":
					var text string
					switch thinkingState {
					case 1:
						text = "<think>" + content
					case 2:
						text = content
					case 3:
						text = content + "</think>"
					}
					if text == "" {
						return
					}
					chunk = map[string]interface{}{
						"id":      chatID,
						"object":  "chat.completion.chunk",
						"created": time.Now().Unix(),
						"model":   model,
						"choices": []map[string]interface{}{{
							"index":         0,
							"delta":         map[string]string{"content": text},
							"finish_reason": nil,
						}},
					}
				default:
					if content == "" {
						return
					}
					chunk = map[string]interface{}{
						"id":      chatID,
						"object":  "chat.completion.chunk",
						"created": time.Now().Unix(),
						"model":   model,
						"choices": []map[string]interface{}{{
							"index":         0,
							"delta":         map[string]string{"reasoning_content": content},
							"finish_reason": nil,
						}},
					}
				}
			} else {
				if content == "" {
					return
				}
				chunk = map[string]interface{}{
					"id":      chatID,
					"object":  "chat.completion.chunk",
					"created": time.Now().Unix(),
					"model":   model,
					"choices": []map[string]interface{}{{
						"index":         0,
						"delta":         map[string]string{"content": content},
						"finish_reason": nil,
					}},
				}
			}
			data, _ := json.Marshal(chunk)
			sse.WriteData(string(data))
			responseStarted = true
		}

		splitter := &thinkingSplitter{
			onPlain: func(t string) { sendChunk(t, 0) },
			onOpen: func() {
				dropTagThinking = !allowTagSource(&thinkingSource)
				thinkingStarted = false
			},
			onThinking: func(t string) {
				if dropTagThinking {
					return
				}
				if !thinkingStarted {
					sendChunk(t, 1)
					thinkingStarted = true
				} else {
					sendChunk(t, 2)
				}
			},
			onClose: func() {
				wasDrop := dropTagThinking
				dropTagThinking = false
				if wasDrop {
					return
				}
				if !thinkingStarted {
					sendChunk("", 1)
				}
				sendChunk("", 3)
				thinkingStarted = false
			},
		}

		processText := func(text string, isThinking bool, forceFlush bool) {
			if isThinking && !thinking {
				return
			}

			if isThinking {
				if !allowReasoningSource(&thinkingSource) {
					return
				}
				if !thinkingStarted {
					sendChunk(text, 1)
					thinkingStarted = true
					eventThinkingOpen = true
				} else {
					sendChunk(text, 2)
				}
				return
			}

			if eventThinkingOpen {
				sendChunk("", 3)
				eventThinkingOpen = false
				thinkingStarted = false
			}

			splitter.push(text)
			if forceFlush {
				splitter.flush()
			}
		}

		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if text == "" {
					return
				}
				if firstTokenAt.IsZero() {
					firstTokenAt = time.Now()
				}
				if isThinking {
					rawReasoningBuilder.WriteString(text)
				} else {
					rawContentBuilder.WriteString(text)
				}
				processText(text, isThinking, false)
			},
			OnToolUse: func(tu KiroToolUse) {
				processText("", false, true)

				args, _ := json.Marshal(tu.Input)
				rawContentBuilder.WriteString(tu.Name)
				rawContentBuilder.Write(args)
				tc := ToolCall{ID: tu.ToolUseID, Type: "function"}
				tc.Function.Name = tu.Name
				tc.Function.Arguments = string(args)
				toolCalls = append(toolCalls, tc)

				chunk := map[string]interface{}{
					"id":      chatID,
					"object":  "chat.completion.chunk",
					"created": time.Now().Unix(),
					"model":   model,
					"choices": []map[string]interface{}{{
						"index": 0,
						"delta": map[string]interface{}{
							"tool_calls": []map[string]interface{}{{
								"index": toolCallIndex,
								"id":    tu.ToolUseID,
								"type":  "function",
								"function": map[string]string{
									"name":      tu.Name,
									"arguments": string(args),
								},
							}},
						},
						"finish_reason": nil,
					}},
				}
				toolCallIndex++
				data, _ := json.Marshal(chunk)
				sse.WriteData(string(data))
				responseStarted = true
			},
			OnComplete: func(inTok, outTok int) {
				inputTokens = inTok
				outputTokens = outTok
			},
			OnCredits: func(c float64) {
				credits = c
			},
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
			},
		}

		err := CallKiroAPI(ctx, account, payload, callback)
		if err != nil {
			// Before any semantic model output: fail over to another Account.
			if !responseStarted {
				return attemptRetry(err)
			}
			// Output already committed: we cannot change the HTTP status or fail
			// over. Stop; the caller renders the SSE close after the slot is
			// released (terminal frames must not hold the routing slot).
			return attemptStop(err, true)
		}

		// Flush any remaining buffered model output while the slot is still held
		// (this is model output, not a terminal control frame).
		processText("", false, true)
		if eventThinkingOpen {
			sendChunk("", 3)
		}
		okAccount = account
		return attemptSuccess()
	})

	if outcome.stopReason == routeStopCanceled {
		return
	}

	// Success: the slot is released; write the terminal control frame + [DONE]
	// and record success bookkeeping now.
	if outcome.stopReason == routeStopSuccess {
		account := okAccount
		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		outputContent, extractedReasoning := extractThinkingFromContent(rawContentBuilder.String())
		reasoningOutput := rawReasoningBuilder.String()
		if thinking && reasoningOutput == "" && extractedReasoning != "" {
			reasoningOutput = extractedReasoning
		}
		if !thinking {
			reasoningOutput = ""
		}
		outputTokens = estimateApproxTokens(outputContent) + estimateApproxTokens(reasoningOutput)
		for _, tc := range toolCalls {
			outputTokens += estimateApproxTokens(tc.Function.Name)
			outputTokens += estimateApproxTokens(tc.Function.Arguments)
		}

		h.recordSuccessForApiKey(apiKeyID, inputTokens, outputTokens, credits)
		var ttftMs int64
		if !firstTokenAt.IsZero() {
			ttftMs = firstTokenAt.Sub(requestStartedAt).Milliseconds()
		}
		recordRequestMetrics("openai", model, true, account, apiKeyID, true, http.StatusOK, "", inputTokens, outputTokens, credits, requestStartedAt, ttftMs)
		h.pool.RecordSuccess(account.ID)
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)

		finishReason := "stop"
		if len(toolCalls) > 0 {
			finishReason = "tool_calls"
		}

		chunk := map[string]interface{}{
			"id":      chatID,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []map[string]interface{}{{
				"index":         0,
				"delta":         map[string]interface{}{},
				"finish_reason": finishReason,
			}},
			"usage": map[string]int{
				"prompt_tokens":     inputTokens,
				"completion_tokens": outputTokens,
				"total_tokens":      inputTokens + outputTokens,
			},
		}
		data, _ := json.Marshal(chunk)
		sse.WriteData(string(data))
		sse.WriteData("[DONE]")
		return
	}

	// Output already committed then the upstream failed: close the SSE stream
	// cleanly now that the slot is released.
	if outcome.stopReason == routeStopCallerTerminal {
		h.recordFailure()
		statusCode, errType := metricsErrorDetails(outcome.lastErr, http.StatusInternalServerError, "api_error")
		recordRequestMetrics("openai", model, true, outcome.lastAccount, apiKeyID, false, statusCode, errType, estimatedInputTokens, outputTokens, credits, requestStartedAt)
		closeChunk := map[string]interface{}{
			"id":      chatID,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []map[string]interface{}{{
				"index":         0,
				"delta":         map[string]interface{}{},
				"finish_reason": "stop",
			}},
		}
		if data, mErr := json.Marshal(closeChunk); mErr == nil {
			sse.WriteData(string(data))
		}
		sse.WriteData("[DONE]")
		return
	}

	// Stop keepalive before any non-SSE error write so a late ping cannot race
	// WriteHeader/JSON. If a keepalive already committed the body, stay on SSE.
	sse.Stop()
	streamCommitted := sse.Committed()

	writeOpenAIStreamError := func(msg string) {
		errChunk := map[string]interface{}{
			"id":      chatID,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []map[string]interface{}{{
				"index":         0,
				"delta":         map[string]interface{}{},
				"finish_reason": "error",
			}},
			"error": map[string]string{"message": msg},
		}
		if data, mErr := json.Marshal(errChunk); mErr == nil {
			sse.WriteData(string(data))
		}
		sse.WriteData("[DONE]")
	}

	if outcome.stopReason == routeStopRoutingLimit {
		h.recordFailure()
		statusCode, errType := metricsErrorDetails(outcome.acquireErr, http.StatusTooManyRequests, "rate_limit_error")
		recordRequestMetrics("openai", model, true, nil, apiKeyID, false, statusCode, errType, estimatedInputTokens, 0, 0, requestStartedAt)
		if streamCommitted {
			writeOpenAIStreamError(routingErrorMessage(outcome.acquireErr))
			return
		}
		h.sendOpenAIError(w, 429, "rate_limit_error", routingErrorMessage(outcome.acquireErr))
		return
	}

	if outcome.lastErr == nil {
		recordRequestMetrics("openai", model, true, nil, apiKeyID, false, http.StatusServiceUnavailable, "no_available_accounts", estimatedInputTokens, 0, 0, requestStartedAt)
		if streamCommitted {
			writeOpenAIStreamError("No available accounts")
			return
		}
		h.sendOpenAIError(w, 503, "server_error", "No available accounts")
		return
	}

	h.recordFailure()
	statusCode, errType := metricsErrorDetails(outcome.lastErr, http.StatusInternalServerError, "server_error")
	recordRequestMetrics("openai", model, true, outcome.lastAccount, apiKeyID, false, statusCode, errType, estimatedInputTokens, 0, 0, requestStartedAt)
	logRetryExhausted("openai", model, statusCode, errType, outcome.lastErr)
	if streamCommitted {
		writeOpenAIStreamError(improperlyFormedClientMessage(outcome.lastErr))
		return
	}
	h.sendOpenAIError(w, statusCode, clientFacingOpenAIErrorType(statusCode), improperlyFormedClientMessage(outcome.lastErr))
}

// handleOpenAINonStream OpenAI 非流式响应
func (h *Handler) handleOpenAINonStream(ctx context.Context, w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, estimatedInputTokens int, apiKeyID string) {
	requestStartedAt := time.Now()

	// Per-attempt render state, hoisted to caller scope so the JSON response is
	// written after Account Routing releases the slot. Non-stream never commits
	// output mid-attempt, so every failure is a clean failover (attemptRetry).
	var content string
	var reasoningContent string
	var toolUses []KiroToolUse
	var inputTokens, outputTokens int
	var credits float64
	var realInputTokens int
	var okAccount *config.Account

	outcome := h.runWithAccount(ctx, model, payload.RoutingAffinityKey, func(account *config.Account) attemptResult {
		content = ""
		reasoningContent = ""
		toolUses = nil
		inputTokens, outputTokens = 0, 0
		credits = 0
		realInputTokens = 0

		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if isThinking {
					reasoningContent += text
				} else {
					content += text
				}
			},
			OnToolUse:  func(tu KiroToolUse) { toolUses = append(toolUses, tu) },
			OnComplete: func(inTok, outTok int) { inputTokens = inTok; outputTokens = outTok },
			OnCredits:  func(c float64) { credits = c },
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
			},
		}

		if err := CallKiroAPI(ctx, account, payload, callback); err != nil {
			return attemptRetry(err)
		}
		okAccount = account
		return attemptSuccess()
	})

	if outcome.stopReason == routeStopCanceled {
		return
	}

	if outcome.stopReason == routeStopSuccess {
		account := okAccount
		finalContent, extractedReasoning := extractThinkingFromContent(content)
		if thinking && reasoningContent == "" && extractedReasoning != "" {
			reasoningContent = extractedReasoning
		} else if !thinking {
			reasoningContent = ""
		}

		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		outputTokens = estimateOpenAIOutputTokens(finalContent, reasoningContent, toolUses)

		h.recordSuccessForApiKey(apiKeyID, inputTokens, outputTokens, credits)
		recordRequestMetrics("openai", model, false, account, apiKeyID, true, http.StatusOK, "", inputTokens, outputTokens, credits, requestStartedAt)
		h.pool.RecordSuccess(account.ID)
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)

		thinkingFormat := config.GetThinkingConfig().OpenAIFormat
		resp := KiroToOpenAIResponseWithReasoning(finalContent, reasoningContent, toolUses, inputTokens, outputTokens, model, thinkingFormat)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(resp)
		return
	}

	if outcome.stopReason == routeStopRoutingLimit {
		h.recordFailure()
		statusCode, errType := metricsErrorDetails(outcome.acquireErr, http.StatusTooManyRequests, "rate_limit_error")
		recordRequestMetrics("openai", model, false, nil, apiKeyID, false, statusCode, errType, estimatedInputTokens, 0, 0, requestStartedAt)
		h.sendOpenAIError(w, 429, "rate_limit_error", routingErrorMessage(outcome.acquireErr))
		return
	}

	if outcome.lastErr == nil {
		recordRequestMetrics("openai", model, false, nil, apiKeyID, false, http.StatusServiceUnavailable, "no_available_accounts", estimatedInputTokens, 0, 0, requestStartedAt)
		h.sendOpenAIError(w, 503, "server_error", "No available accounts")
		return
	}

	h.recordFailure()
	statusCode, errType := metricsErrorDetails(outcome.lastErr, http.StatusInternalServerError, "server_error")
	recordRequestMetrics("openai", model, false, outcome.lastAccount, apiKeyID, false, statusCode, errType, estimatedInputTokens, 0, 0, requestStartedAt)
	logRetryExhausted("openai", model, statusCode, errType, outcome.lastErr)
	h.sendOpenAIError(w, statusCode, clientFacingOpenAIErrorType(statusCode), improperlyFormedClientMessage(outcome.lastErr))
}

func (h *Handler) sendOpenAIError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"type":    errType,
			"message": message,
		},
	})
}
