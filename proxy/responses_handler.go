package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"net/http"
	"strings"
	"time"
)

const defaultResponsesModel = "claude-sonnet-4.5"

func (h *Handler) handleOpenAIResponses(w http.ResponseWriter, r *http.Request) {
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

	logInboundRequestProbe("responses", r, body)

	var req ResponsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", "Invalid JSON")
		return
	}

	if strings.TrimSpace(req.Model) == "" {
		req.Model = defaultResponsesModel
	}

	storedInputCopy := append(json.RawMessage(nil), req.Input...)

	storeResponse := true
	if req.Store != nil {
		storeResponse = *req.Store
	}

	var historyMessages []OpenAIMessage
	if req.PreviousResponseID != "" {
		prev, loadErr := loadResponse(req.PreviousResponseID)
		if loadErr != nil {
			h.sendOpenAIError(w, 404, "invalid_request_error",
				fmt.Sprintf("previous_response_id not found: %v", loadErr))
			return
		}
		historyMessages = expandPreviousResponseHistory(prev)
	}

	inputMessages, err := parseResponsesInput(req.Input)
	if err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", err.Error())
		return
	}

	finalMessages := make([]OpenAIMessage, 0, len(historyMessages)+len(inputMessages)+1)
	finalMessages = append(finalMessages, historyMessages...)
	if strings.TrimSpace(req.Instructions) != "" {
		// New instructions on this turn always take effect, even when
		// continuing from previous_response_id. Place them after the
		// expanded history so they apply to the current and future turns,
		// while ancestor instructions (re-emitted by expandPreviousResponseHistory)
		// stay in scope for the historical exchanges they shaped.
		finalMessages = append(finalMessages, OpenAIMessage{
			Role:    "system",
			Content: req.Instructions,
		})
	}
	finalMessages = append(finalMessages, inputMessages...)

	if len(finalMessages) == 0 {
		h.sendOpenAIError(w, 400, "invalid_request_error", "input must contain at least one message")
		return
	}

	hasUser := false
	for _, m := range finalMessages {
		if m.Role == "user" {
			hasUser = true
			break
		}
	}
	if !hasUser {
		h.sendOpenAIError(w, 400, "invalid_request_error", "input must contain at least one user message")
		return
	}

	openaiReq := &OpenAIRequest{
		Model:    req.Model,
		Messages: finalMessages,
		Stream:   req.Stream,
		Tools:    req.Tools,
	}
	if req.Temperature != nil {
		openaiReq.Temperature = req.Temperature
	}
	if req.MaxOutputTokens != nil {
		openaiReq.MaxTokens = *req.MaxOutputTokens
	}

	thinkingCfg := config.GetThinkingConfig()
	actualModel, thinking := ParseModelAndThinking(req.Model, thinkingCfg.Suffix)
	openaiReq.Model = actualModel

	estimatedInputTokens := estimateOpenAIRequestInputTokens(openaiReq)
	kiroPayload := OpenAIToKiro(openaiReq, thinking)

	apiKeyID := apiKeyIDFromContext(r.Context())
	respID := generateResponseID()

	if req.Stream {
		h.handleResponsesStream(r.Context(), w, kiroPayload, actualModel, thinking, estimatedInputTokens,
			apiKeyID, respID, &req, storedInputCopy, storeResponse)
		return
	}

	if apiKeyForbidsSyncRequests(apiKeyID) {
		h.sendOpenAIError(w, 403, "permission_error", "This API key only permits streaming requests; set \"stream\": true.")
		return
	}

	h.handleResponsesNonStream(r.Context(), w, kiroPayload, actualModel, thinking, estimatedInputTokens,
		apiKeyID, respID, &req, storedInputCopy, storeResponse)
}

func (h *Handler) handleResponsesNonStream(
	ctx context.Context,
	w http.ResponseWriter, payload *KiroPayload, model string, thinking bool,
	estimatedInputTokens int, apiKeyID, respID string,
	req *ResponsesRequest, storedInput json.RawMessage, storeResponse bool,
) {
	requestStartedAt := time.Now()

	// Per-attempt render state, hoisted to caller scope so the JSON response is
	// written after Account Routing releases the slot. Non-stream never commits
	// output mid-attempt, so every failure is a clean failover (attemptRetry).
	var content, reasoningContent string
	var toolUses []KiroToolUse
	var inputTokens, outputTokens int
	var credits float64
	var realInputTokens int
	var okAccount *config.Account

	outcome := h.runWithAccount(ctx, model, payload.RoutingAffinityKey, func(account *config.Account) attemptResult {
		content, reasoningContent = "", ""
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
		// Explicit reasoningContentEvent wins; fall back to inline <thinking>
		// tags extracted from the assistant text (mirrors the OpenAI handler).
		if thinking && reasoningContent == "" && extractedReasoning != "" {
			reasoningContent = extractedReasoning
		}
		if !thinking {
			reasoningContent = ""
		}

		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		outputTokens = estimateOpenAIOutputTokens(finalContent, reasoningContent, toolUses)

		h.recordSuccessForApiKey(apiKeyID, inputTokens, outputTokens, credits)
		recordRequestMetrics("responses", model, false, account, apiKeyID, true, http.StatusOK, "", inputTokens, outputTokens, credits, requestStartedAt)
		h.pool.RecordSuccess(account.ID)
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)

		respObj := buildResponsesObject(respID, model, finalContent, reasoningContent, toolUses, inputTokens, outputTokens, req)
		respObj.StoredInput = storedInput
		respObj.Instructions = req.Instructions

		if storeResponse {
			if saveErr := saveResponse(respObj); saveErr != nil {
				logResponsesPersistFailure(respObj.ID, saveErr)
			}
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(respObj)
		return
	}

	if outcome.stopReason == routeStopRoutingLimit {
		h.recordFailure()
		statusCode, errType := metricsErrorDetails(outcome.acquireErr, http.StatusTooManyRequests, "rate_limit_error")
		recordRequestMetrics("responses", model, false, nil, apiKeyID, false, statusCode, errType, estimatedInputTokens, 0, 0, requestStartedAt)
		h.sendOpenAIError(w, 429, "rate_limit_error", routingErrorMessage(outcome.acquireErr))
		return
	}

	if outcome.lastErr == nil {
		recordRequestMetrics("responses", model, false, nil, apiKeyID, false, http.StatusServiceUnavailable, "no_available_accounts", estimatedInputTokens, 0, 0, requestStartedAt)
		h.sendOpenAIError(w, 503, "server_error", "No available accounts")
		return
	}
	h.recordFailure()
	statusCode, errType := metricsErrorDetails(outcome.lastErr, http.StatusInternalServerError, "server_error")
	recordRequestMetrics("responses", model, false, outcome.lastAccount, apiKeyID, false, statusCode, errType, estimatedInputTokens, 0, 0, requestStartedAt)
	logRetryExhausted("responses", model, statusCode, errType, outcome.lastErr)
	h.sendOpenAIError(w, statusCode, clientFacingOpenAIErrorType(statusCode), improperlyFormedClientMessage(outcome.lastErr))
}

func buildResponsesObject(
	id, model, content, reasoning string, toolUses []KiroToolUse,
	inputTokens, outputTokens int, req *ResponsesRequest,
) *ResponsesObject {
	output := make([]ResponseOutputItem, 0, 2+len(toolUses))

	// A reasoning item precedes the message item (mirrors OpenAI's own Responses
	// ordering: reasoning summary, then the assistant message).
	if strings.TrimSpace(reasoning) != "" {
		output = append(output, ResponseOutputItem{
			ID:     generateOutputItemID("rs"),
			Type:   "reasoning",
			Status: "completed",
			Summary: []ResponseSummaryPart{{
				Type: "summary_text",
				Text: reasoning,
			}},
		})
	}

	if strings.TrimSpace(content) != "" {
		output = append(output, ResponseOutputItem{
			ID:     generateOutputItemID("msg"),
			Type:   "message",
			Role:   "assistant",
			Status: "completed",
			Content: []ResponseContentPart{{
				Type: "output_text",
				Text: content,
			}},
		})
	}

	for _, tu := range toolUses {
		args, _ := json.Marshal(tu.Input)
		output = append(output, ResponseOutputItem{
			ID:        generateOutputItemID("fc"),
			Type:      "function_call",
			Status:    "completed",
			CallID:    tu.ToolUseID,
			Name:      tu.Name,
			Arguments: string(args),
		})
	}

	if len(output) == 0 {
		output = append(output, ResponseOutputItem{
			ID:     generateOutputItemID("msg"),
			Type:   "message",
			Role:   "assistant",
			Status: "completed",
			Content: []ResponseContentPart{{
				Type: "output_text",
				Text: "",
			}},
		})
	}

	return &ResponsesObject{
		ID:                 id,
		Object:             "response",
		CreatedAt:          time.Now().Unix(),
		Status:             "completed",
		Model:              model,
		Output:             output,
		Usage:              ResponsesUsage{InputTokens: inputTokens, OutputTokens: outputTokens, TotalTokens: inputTokens + outputTokens},
		PreviousResponseID: req.PreviousResponseID,
		Metadata:           req.Metadata,
	}
}

func (h *Handler) handleResponsesStream(
	ctx context.Context,
	w http.ResponseWriter, payload *KiroPayload, model string, thinking bool,
	estimatedInputTokens int, apiKeyID, respID string,
	req *ResponsesRequest, storedInput json.RawMessage, storeResponse bool,
) {
	requestStartedAt := time.Now()
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Disable reverse-proxy response buffering so SSE chunks flush promptly.
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendOpenAIError(w, 500, "server_error", "Streaming not supported")
		return
	}

	// Stream Keepalive: idle SSE comments only; events go through sse.WriteEvent.
	sse := startStreamSSE(w, flusher)
	defer sse.Stop()

	send := func(eventName string, payload interface{}) {
		sse.WriteEvent(eventName, payload)
	}

	createdAt := time.Now().Unix()
	initial := &ResponsesObject{
		ID:                 respID,
		Object:             "response",
		CreatedAt:          createdAt,
		Status:             "in_progress",
		Model:              model,
		Output:             []ResponseOutputItem{},
		Usage:              ResponsesUsage{},
		PreviousResponseID: req.PreviousResponseID,
		Metadata:           req.Metadata,
	}
	send("response.created", map[string]interface{}{
		"type":     "response.created",
		"response": initial,
	})

	// Per-attempt render state, hoisted to caller scope so terminal rendering
	// (response.completed / response.failed + [DONE]) runs after Account Routing
	// releases the slot, while streaming events still emit inside the callback
	// under the slot. responseStarted is the semantic-output commit flag: the
	// response.created / response.in_progress preamble does NOT set it, so a
	// failure before any real output still fails over.
	var (
		msgText         strings.Builder
		reasoningText   strings.Builder
		currentItemText strings.Builder
		toolUses        []KiroToolUse
		inputTokens     int
		outputTokens    int
		credits         float64
		realInputTokens int
		firstTokenAt    time.Time
		finalContent    string
		currentItemType string
		currentItemID   string
		outputIndex     int
		thinkingSource  thinkingStreamSource
		dropTagThinking bool
		responseStarted bool
		okAccount       *config.Account
	)

	outcome := h.runWithAccount(ctx, model, payload.RoutingAffinityKey, func(account *config.Account) attemptResult {
		// Reset per-attempt state so a failover starts clean.
		msgText.Reset()
		reasoningText.Reset()
		currentItemText.Reset()
		toolUses = nil
		inputTokens, outputTokens = 0, 0
		credits = 0
		realInputTokens = 0
		firstTokenAt = time.Time{}
		finalContent = ""
		currentItemType = ""
		currentItemID = ""
		outputIndex = 0
		thinkingSource = thinkingSourceUnknown
		dropTagThinking = false
		responseStarted = false

		send("response.in_progress", map[string]interface{}{
			"type":     "response.in_progress",
			"response": initial,
		})

		// Item lifecycle helpers. Reasoning and message are SEPARATE output items
		// (mirrors OpenAI's own Responses shape and sub2api): a reasoning item
		// carries summary_text parts, a message item carries output_text parts.
		// Only one item is open at a time; switching kind closes the current one
		// and opens the next at a fresh output_index.
		openReasoningItem := func() {
			currentItemType = "reasoning"
			currentItemID = generateOutputItemID("rs")
			currentItemText.Reset()
			send("response.output_item.added", map[string]interface{}{
				"type":         "response.output_item.added",
				"output_index": outputIndex,
				"item": map[string]interface{}{
					"id":      currentItemID,
					"type":    "reasoning",
					"status":  "in_progress",
					"summary": []map[string]interface{}{},
				},
			})
			send("response.reasoning_summary_part.added", map[string]interface{}{
				"type":          "response.reasoning_summary_part.added",
				"item_id":       currentItemID,
				"output_index":  outputIndex,
				"summary_index": 0,
				"part":          map[string]interface{}{"type": "summary_text", "text": ""},
			})
		}
		openMessageItem := func() {
			currentItemType = "message"
			currentItemID = generateOutputItemID("msg")
			currentItemText.Reset()
			send("response.output_item.added", map[string]interface{}{
				"type":         "response.output_item.added",
				"output_index": outputIndex,
				"item": map[string]interface{}{
					"id":      currentItemID,
					"type":    "message",
					"role":    "assistant",
					"status":  "in_progress",
					"content": []map[string]interface{}{},
				},
			})
			send("response.content_part.added", map[string]interface{}{
				"type":          "response.content_part.added",
				"item_id":       currentItemID,
				"output_index":  outputIndex,
				"content_index": 0,
				"part":          map[string]interface{}{"type": "output_text", "text": ""},
			})
		}
		closeItem := func() {
			switch currentItemType {
			case "message":
				text := currentItemText.String()
				send("response.output_text.done", map[string]interface{}{
					"type":          "response.output_text.done",
					"item_id":       currentItemID,
					"output_index":  outputIndex,
					"content_index": 0,
					"text":          text,
				})
				send("response.content_part.done", map[string]interface{}{
					"type":          "response.content_part.done",
					"item_id":       currentItemID,
					"output_index":  outputIndex,
					"content_index": 0,
					"part":          map[string]interface{}{"type": "output_text", "text": text},
				})
				send("response.output_item.done", map[string]interface{}{
					"type":         "response.output_item.done",
					"output_index": outputIndex,
					"item": map[string]interface{}{
						"id":      currentItemID,
						"type":    "message",
						"role":    "assistant",
						"status":  "completed",
						"content": []map[string]interface{}{{"type": "output_text", "text": text}},
					},
				})
			case "reasoning":
				text := currentItemText.String()
				send("response.reasoning_summary_text.done", map[string]interface{}{
					"type":          "response.reasoning_summary_text.done",
					"item_id":       currentItemID,
					"output_index":  outputIndex,
					"summary_index": 0,
					"text":          text,
				})
				send("response.reasoning_summary_part.done", map[string]interface{}{
					"type":          "response.reasoning_summary_part.done",
					"item_id":       currentItemID,
					"output_index":  outputIndex,
					"summary_index": 0,
					"part":          map[string]interface{}{"type": "summary_text", "text": text},
				})
				send("response.output_item.done", map[string]interface{}{
					"type":         "response.output_item.done",
					"output_index": outputIndex,
					"item": map[string]interface{}{
						"id":      currentItemID,
						"type":    "reasoning",
						"status":  "completed",
						"summary": []map[string]interface{}{{"type": "summary_text", "text": text}},
					},
				})
			default:
				return
			}
			currentItemType = ""
			currentItemID = ""
			currentItemText.Reset()
			outputIndex++
		}

		emitText := func(text string) {
			if text == "" {
				return
			}
			if currentItemType != "message" {
				closeItem()
				openMessageItem()
			}
			msgText.WriteString(text)
			currentItemText.WriteString(text)
			send("response.output_text.delta", map[string]interface{}{
				"type":          "response.output_text.delta",
				"item_id":       currentItemID,
				"output_index":  outputIndex,
				"content_index": 0,
				"delta":         text,
			})
			responseStarted = true
		}
		emitReasoning := func(text string) {
			if text == "" || !thinking {
				return
			}
			if currentItemType != "reasoning" {
				closeItem()
				openReasoningItem()
			}
			reasoningText.WriteString(text)
			currentItemText.WriteString(text)
			send("response.reasoning_summary_text.delta", map[string]interface{}{
				"type":          "response.reasoning_summary_text.delta",
				"item_id":       currentItemID,
				"output_index":  outputIndex,
				"summary_index": 0,
				"delta":         text,
			})
			responseStarted = true
		}

		// Splitter separates plain assistant text from inline <thinking> blocks
		// so a literal tag never opens a phantom reasoning item, and real
		// reasoning is routed into a reasoning output item instead of leaking
		// raw <thinking> markers into output_text deltas (the previous bug).
		splitter := &thinkingSplitter{
			onPlain: func(t string) { emitText(t) },
			onOpen:  func() { dropTagThinking = !allowTagSource(&thinkingSource) },
			onThinking: func(t string) {
				if dropTagThinking {
					return
				}
				emitReasoning(t)
			},
			onClose: func() {
				wasDrop := dropTagThinking
				dropTagThinking = false
				if wasDrop {
					return
				}
				if currentItemType == "reasoning" {
					closeItem()
				}
			},
		}

		processText := func(text string, isThinking bool, forceFlush bool) {
			if isThinking {
				if !thinking {
					return
				}
				if !allowReasoningSource(&thinkingSource) {
					return
				}
				emitReasoning(text)
				return
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
				processText(text, isThinking, false)
			},
			OnToolUse: func(tu KiroToolUse) {
				// Flush buffered tagged text and close any open reasoning/message
				// item before the function_call item opens.
				processText("", false, true)
				closeItem()

				toolUses = append(toolUses, tu)
				args, _ := json.Marshal(tu.Input)
				fcID := generateOutputItemID("fc")
				send("response.output_item.added", map[string]interface{}{
					"type":         "response.output_item.added",
					"output_index": outputIndex,
					"item": map[string]interface{}{
						"id":        fcID,
						"type":      "function_call",
						"status":    "in_progress",
						"call_id":   tu.ToolUseID,
						"name":      tu.Name,
						"arguments": "",
					},
				})
				send("response.function_call_arguments.delta", map[string]interface{}{
					"type":         "response.function_call_arguments.delta",
					"item_id":      fcID,
					"output_index": outputIndex,
					"delta":        string(args),
				})
				send("response.output_item.done", map[string]interface{}{
					"type":         "response.output_item.done",
					"output_index": outputIndex,
					"item": map[string]interface{}{
						"id":        fcID,
						"type":      "function_call",
						"status":    "completed",
						"call_id":   tu.ToolUseID,
						"name":      tu.Name,
						"arguments": string(args),
					},
				})
				outputIndex++
				responseStarted = true
			},
			OnComplete: func(inTok, outTok int) { inputTokens = inTok; outputTokens = outTok },
			OnCredits:  func(c float64) { credits = c },
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
			},
		}

		err := CallKiroAPI(ctx, account, payload, callback)
		if err != nil {
			// Before any semantic model output: fail over to another Account.
			// The response.created/in_progress preamble is not model output, so
			// its emission alone does not block failover.
			if !responseStarted {
				return attemptRetry(err)
			}
			// Semantic output already committed: cannot fail over. Stop; the
			// caller closes the SSE stream after the slot is released.
			return attemptStop(err, true)
		}

		// Flush buffered model output and close the open item while the slot is
		// still held (model output, not a terminal control frame).
		processText("", false, true)
		closeItem()
		finalContent = msgText.String()
		okAccount = account
		return attemptSuccess()
	})

	if outcome.stopReason == routeStopCanceled {
		return
	}

	// Success: slot released; compute usage, persist, and emit the terminal
	// response.completed + [DONE].
	if outcome.stopReason == routeStopSuccess {
		account := okAccount
		reasoning := reasoningText.String()
		if !thinking {
			reasoning = ""
		}

		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		outputTokens = estimateOpenAIOutputTokens(finalContent, reasoning, toolUses)

		h.recordSuccessForApiKey(apiKeyID, inputTokens, outputTokens, credits)
		var ttftMs int64
		if !firstTokenAt.IsZero() {
			ttftMs = firstTokenAt.Sub(requestStartedAt).Milliseconds()
		}
		recordRequestMetrics("responses", model, true, account, apiKeyID, true, http.StatusOK, "", inputTokens, outputTokens, credits, requestStartedAt, ttftMs)
		h.pool.RecordSuccess(account.ID)
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)

		respObj := buildResponsesObject(respID, model, finalContent, reasoning, toolUses, inputTokens, outputTokens, req)
		respObj.CreatedAt = createdAt
		respObj.StoredInput = storedInput
		respObj.Instructions = req.Instructions

		if storeResponse {
			if saveErr := saveResponse(respObj); saveErr != nil {
				logResponsesPersistFailure(respObj.ID, saveErr)
			}
		}

		send("response.completed", map[string]interface{}{
			"type":     "response.completed",
			"response": respObj,
		})
		sse.WriteData("[DONE]")
		return
	}

	// Semantic output already committed then upstream failed: emit response.failed
	// + [DONE] now that the slot is released.
	if outcome.stopReason == routeStopCallerTerminal {
		h.recordFailure()
		statusCode, errType := metricsErrorDetails(outcome.lastErr, http.StatusInternalServerError, "server_error")
		recordRequestMetrics("responses", model, true, outcome.lastAccount, apiKeyID, false, statusCode, errType, estimatedInputTokens, outputTokens, credits, requestStartedAt)
		send("response.failed", map[string]interface{}{
			"type": "response.failed",
			"response": map[string]interface{}{
				"id":     respID,
				"status": "failed",
				"error": map[string]string{
					"type":    "server_error",
					"message": outcome.lastErr.Error(),
				},
			},
		})
		sse.WriteData("[DONE]")
		return
	}

	if outcome.stopReason == routeStopRoutingLimit {
		h.recordFailure()
		statusCode, errType := metricsErrorDetails(outcome.acquireErr, http.StatusTooManyRequests, "rate_limit_error")
		recordRequestMetrics("responses", model, true, nil, apiKeyID, false, statusCode, errType, estimatedInputTokens, 0, 0, requestStartedAt)
		send("response.failed", map[string]interface{}{
			"type": "response.failed",
			"response": map[string]interface{}{
				"id":     respID,
				"status": "failed",
				"error":  map[string]string{"type": "rate_limit_error", "message": routingErrorMessage(outcome.acquireErr)},
			},
		})
		return
	}

	if outcome.lastErr == nil {
		recordRequestMetrics("responses", model, true, nil, apiKeyID, false, http.StatusServiceUnavailable, "no_available_accounts", estimatedInputTokens, 0, 0, requestStartedAt)
		send("response.failed", map[string]interface{}{
			"type": "response.failed",
			"response": map[string]interface{}{
				"id":     respID,
				"status": "failed",
				"error": map[string]string{
					"type":    "server_error",
					"message": "No available accounts",
				},
			},
		})
		return
	}
	h.recordFailure()
	statusCode, errType := metricsErrorDetails(outcome.lastErr, http.StatusInternalServerError, "server_error")
	recordRequestMetrics("responses", model, true, outcome.lastAccount, apiKeyID, false, statusCode, errType, estimatedInputTokens, 0, 0, requestStartedAt)
	logRetryExhausted("responses", model, statusCode, errType, outcome.lastErr)
	send("response.failed", map[string]interface{}{
		"type": "response.failed",
		"response": map[string]interface{}{
			"id":     respID,
			"status": "failed",
			"error": map[string]string{
				"type":    clientFacingOpenAIErrorType(statusCode),
				"message": improperlyFormedClientMessage(outcome.lastErr),
			},
		},
	})
}
