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
	excluded := make(map[string]bool)
	var lastErr error
	var lastAccount *config.Account
	// Panic safety net: guarantees the routing slot is released even if a panic
	// unwinds the stack. release is sync.Once-idempotent.
	var activeRelease func()
	defer func() {
		if activeRelease != nil {
			activeRelease()
		}
	}()

	for attempt := 0; attempt < getAccountRetryAttempts(); attempt++ {
		account, release, acquireErr := h.acquireRouteAccount(ctx, model, excluded, payload.RoutingAffinityKey)
		if acquireErr != nil {
			if isRoutingLimitError(acquireErr) {
				h.recordFailure()
				statusCode, errType := metricsErrorDetails(acquireErr, http.StatusTooManyRequests, "rate_limit_error")
				recordRequestMetrics("responses", model, false, nil, apiKeyID, false, statusCode, errType, estimatedInputTokens, 0, 0, requestStartedAt)
				h.sendOpenAIError(w, 429, "rate_limit_error", routingErrorMessage(acquireErr))
				return
			}
			break
		}
		activeRelease = release
		if err := h.ensureValidToken(account); err != nil {
			release()
			lastErr = err
			lastAccount = account
			h.handleAccountError(account, excluded, err)
			if shouldBackoffBeforeRetry(err) {
				time.Sleep(retryBackoffAfterRateLimit())
			}
			continue
		}

		var content, reasoningContent string
		var toolUses []KiroToolUse
		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int

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

		err := CallKiroAPI(ctx, account, payload, callback)
		release()
		if err != nil {
			lastErr = err
			lastAccount = account
			h.handleAccountError(account, excluded, err)
			if shouldBackoffBeforeRetry(err) {
				time.Sleep(retryBackoffAfterRateLimit())
			}
			continue
		}

		finalContent, _ := extractThinkingFromContent(content)
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

		respObj := buildResponsesObject(respID, model, finalContent, toolUses, inputTokens, outputTokens, req)
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

	if lastErr == nil {
		recordRequestMetrics("responses", model, false, nil, apiKeyID, false, http.StatusServiceUnavailable, "no_available_accounts", estimatedInputTokens, 0, 0, requestStartedAt)
		h.sendOpenAIError(w, 503, "server_error", "No available accounts")
		return
	}
	h.recordFailure()
	statusCode, errType := metricsErrorDetails(lastErr, http.StatusInternalServerError, "server_error")
	recordRequestMetrics("responses", model, false, lastAccount, apiKeyID, false, statusCode, errType, estimatedInputTokens, 0, 0, requestStartedAt)
	logRetryExhausted("responses", model, statusCode, errType, lastErr)
	h.sendOpenAIError(w, statusCode, clientFacingOpenAIErrorType(statusCode), improperlyFormedClientMessage(lastErr))
}

func buildResponsesObject(
	id, model, content string, toolUses []KiroToolUse,
	inputTokens, outputTokens int, req *ResponsesRequest,
) *ResponsesObject {
	output := make([]ResponseOutputItem, 0, 1+len(toolUses))

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

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendOpenAIError(w, 500, "server_error", "Streaming not supported")
		return
	}

	send := func(eventName string, payload interface{}) {
		data, err := json.Marshal(payload)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventName, string(data))
		flusher.Flush()
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

	excluded := make(map[string]bool)
	var lastErr error
	var lastAccount *config.Account
	responseStarted := false
	// Panic safety net: guarantees the routing slot is released even if a panic
	// unwinds the stack. release is sync.Once-idempotent.
	var activeRelease func()
	defer func() {
		if activeRelease != nil {
			activeRelease()
		}
	}()

	for attempt := 0; attempt < getAccountRetryAttempts(); attempt++ {
		account, release, acquireErr := h.acquireRouteAccount(ctx, model, excluded, payload.RoutingAffinityKey)
		if acquireErr != nil {
			if isRoutingLimitError(acquireErr) {
				h.recordFailure()
				statusCode, errType := metricsErrorDetails(acquireErr, http.StatusTooManyRequests, "rate_limit_error")
				recordRequestMetrics("responses", model, true, nil, apiKeyID, false, statusCode, errType, estimatedInputTokens, 0, 0, requestStartedAt)
				send("response.failed", map[string]interface{}{
					"type": "response.failed",
					"response": map[string]interface{}{
						"id":     respID,
						"status": "failed",
						"error":  map[string]string{"type": "rate_limit_error", "message": routingErrorMessage(acquireErr)},
					},
				})
				return
			}
			break
		}
		activeRelease = release
		if err := h.ensureValidToken(account); err != nil {
			release()
			lastErr = err
			lastAccount = account
			h.handleAccountError(account, excluded, err)
			if shouldBackoffBeforeRetry(err) {
				time.Sleep(retryBackoffAfterRateLimit())
			}
			continue
		}

		send("response.in_progress", map[string]interface{}{
			"type":     "response.in_progress",
			"response": initial,
		})

		var (
			fullText        strings.Builder
			reasoningText   strings.Builder
			toolUses        []KiroToolUse
			inputTokens     int
			outputTokens    int
			credits         float64
			realInputTokens int
			firstTokenAt    time.Time
		)

		messageItemID := generateOutputItemID("msg")
		messageStarted := false
		outputIndex := 0
		contentIndex := 0

		ensureMessageStarted := func() {
			if messageStarted {
				return
			}
			messageStarted = true
			send("response.output_item.added", map[string]interface{}{
				"type":         "response.output_item.added",
				"output_index": outputIndex,
				"item": map[string]interface{}{
					"id":      messageItemID,
					"type":    "message",
					"role":    "assistant",
					"status":  "in_progress",
					"content": []map[string]interface{}{},
				},
			})
			send("response.content_part.added", map[string]interface{}{
				"type":          "response.content_part.added",
				"item_id":       messageItemID,
				"output_index":  outputIndex,
				"content_index": contentIndex,
				"part": map[string]interface{}{
					"type": "output_text",
					"text": "",
				},
			})
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
					reasoningText.WriteString(text)
					return
				}
				fullText.WriteString(text)
				ensureMessageStarted()
				send("response.output_text.delta", map[string]interface{}{
					"type":          "response.output_text.delta",
					"item_id":       messageItemID,
					"output_index":  outputIndex,
					"content_index": contentIndex,
					"delta":         text,
				})
				responseStarted = true
			},
			OnToolUse: func(tu KiroToolUse) {
				if messageStarted {
					send("response.content_part.done", map[string]interface{}{
						"type":          "response.content_part.done",
						"item_id":       messageItemID,
						"output_index":  outputIndex,
						"content_index": contentIndex,
						"part": map[string]interface{}{
							"type": "output_text",
							"text": fullText.String(),
						},
					})
					send("response.output_item.done", map[string]interface{}{
						"type":         "response.output_item.done",
						"output_index": outputIndex,
						"item": map[string]interface{}{
							"id":     messageItemID,
							"type":   "message",
							"role":   "assistant",
							"status": "completed",
							"content": []map[string]interface{}{{
								"type": "output_text",
								"text": fullText.String(),
							}},
						},
					})
					messageStarted = false
					outputIndex++
				}

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
		release()
		if err != nil {
			if !responseStarted {
				lastErr = err
				h.handleAccountError(account, excluded, err)
				if shouldBackoffBeforeRetry(err) {
					time.Sleep(retryBackoffAfterRateLimit())
				}
				continue
			}
			statusCode, errType := metricsErrorDetails(err, http.StatusInternalServerError, "server_error")
			recordRequestMetrics("responses", model, true, account, apiKeyID, false, statusCode, errType, estimatedInputTokens, outputTokens, credits, requestStartedAt)
			send("response.failed", map[string]interface{}{
				"type": "response.failed",
				"response": map[string]interface{}{
					"id":     respID,
					"status": "failed",
					"error": map[string]string{
						"type":    "server_error",
						"message": err.Error(),
					},
				},
			})
			// Match the success path: terminate the SSE stream with [DONE] so
			// clients stop reading instead of hanging.
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
			h.recordFailure()
			return
		}

		finalContent, _ := extractThinkingFromContent(fullText.String())
		reasoning := reasoningText.String()
		if !thinking {
			reasoning = ""
		}

		if messageStarted {
			send("response.content_part.done", map[string]interface{}{
				"type":          "response.content_part.done",
				"item_id":       messageItemID,
				"output_index":  outputIndex,
				"content_index": contentIndex,
				"part": map[string]interface{}{
					"type": "output_text",
					"text": finalContent,
				},
			})
			send("response.output_item.done", map[string]interface{}{
				"type":         "response.output_item.done",
				"output_index": outputIndex,
				"item": map[string]interface{}{
					"id":     messageItemID,
					"type":   "message",
					"role":   "assistant",
					"status": "completed",
					"content": []map[string]interface{}{{
						"type": "output_text",
						"text": finalContent,
					}},
				},
			})
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

		respObj := buildResponsesObject(respID, model, finalContent, toolUses, inputTokens, outputTokens, req)
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
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}

	if lastErr == nil {
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
	statusCode, errType := metricsErrorDetails(lastErr, http.StatusInternalServerError, "server_error")
	recordRequestMetrics("responses", model, true, lastAccount, apiKeyID, false, statusCode, errType, estimatedInputTokens, 0, 0, requestStartedAt)
	logRetryExhausted("responses", model, statusCode, errType, lastErr)
	send("response.failed", map[string]interface{}{
		"type": "response.failed",
		"response": map[string]interface{}{
			"id":     respID,
			"status": "failed",
			"error": map[string]string{
				"type":    clientFacingOpenAIErrorType(statusCode),
				"message": improperlyFormedClientMessage(lastErr),
			},
		},
	})
}
