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

func validateClaudeRequestShape(req *ClaudeRequest) string {
	if len(req.Messages) == 0 {
		return "messages must not be empty"
	}
	if msg := validateClaudeThinkingConfig(req.Thinking, req.MaxTokens); msg != "" {
		return msg
	}

	hasUserContext := false
	lastRole := ""
	for _, msg := range req.Messages {
		role := strings.TrimSpace(msg.Role)
		if role == "" {
			continue
		}
		lastRole = role
		if role != "user" {
			continue
		}

		text, images, toolResults := extractClaudeUserContent(msg.Content)
		if normalizeUserContent(text, len(images) > 0) != "" || len(toolResults) > 0 {
			hasUserContext = true
		}
	}

	if lastRole == "assistant" {
		return "assistant-prefill final message is not supported; last message must be user"
	}
	if !hasUserContext {
		return "at least one non-empty user message is required"
	}
	return ""
}

func validateClaudeThinkingConfig(thinking *ClaudeThinkingConfig, maxTokens int) string {
	if thinking == nil {
		return ""
	}

	kind := strings.ToLower(strings.TrimSpace(thinking.Type))
	switch kind {
	case "enabled":
		if maxTokens == 0 {
			return "thinking.type enabled cannot be used with max_tokens=0"
		}
		if thinking.BudgetTokens <= 0 {
			return "thinking.budget_tokens is required when thinking.type is enabled"
		}
		if thinking.BudgetTokens < 1024 {
			return "thinking.budget_tokens must be at least 1024"
		}
		if maxTokens > 0 && thinking.BudgetTokens >= maxTokens {
			return "thinking.budget_tokens must be less than max_tokens"
		}
	case "adaptive":
		if thinking.BudgetTokens != 0 {
			return "thinking.budget_tokens is not supported when thinking.type is adaptive"
		}
	case "disabled":
		if thinking.BudgetTokens != 0 {
			return "thinking.budget_tokens is not supported when thinking.type is disabled"
		}
	default:
		return "thinking.type must be one of: enabled, adaptive, disabled"
	}

	display := strings.ToLower(strings.TrimSpace(thinking.Display))
	if display != "" && display != "summarized" && display != "omitted" {
		return "thinking.display must be one of: summarized, omitted"
	}
	if kind == "disabled" && display != "" {
		return "thinking.display is not supported when thinking.type is disabled"
	}

	return ""
}

type claudeThinkingResponseOptions struct {
	Format      string
	OmitDisplay bool
}

func resolveClaudeThinkingResponseOptions(thinking *ClaudeThinkingConfig, defaultFormat string) claudeThinkingResponseOptions {
	opts := claudeThinkingResponseOptions{Format: defaultFormat}
	if opts.Format == "" {
		opts.Format = "thinking"
	}
	if thinking == nil {
		return opts
	}

	display := strings.ToLower(strings.TrimSpace(thinking.Display))
	switch display {
	case "summarized":
		opts.Format = "thinking"
	case "omitted":
		opts.Format = "thinking"
		opts.OmitDisplay = true
	}

	return opts
}

// handleCountTokens Token 计数（Claude Code 会调用）
func (h *Handler) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		if maxBytesExceeded(err) {
			h.sendClaudeError(w, 413, "invalid_request_error", "Request body too large")
			return
		}
		h.sendClaudeError(w, 400, "invalid_request_error", "Failed to read request body")
		return
	}
	logInboundRequestProbe("claude_count_tokens", r, body)

	var req ClaudeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendClaudeError(w, 400, "invalid_request_error", "Invalid JSON")
		return
	}
	if msg := validateClaudeThinkingConfig(req.Thinking, req.MaxTokens); msg != "" {
		h.sendClaudeError(w, 400, "invalid_request_error", msg)
		return
	}

	thinkingCfg := config.GetThinkingConfig()
	actualModel, thinking := resolveClaudeThinkingMode(req.Model, req.Thinking, thinkingCfg.Suffix)
	req.Model = actualModel
	effectiveReq := cloneClaudeRequestForThinking(&req, thinking)

	estimatedTokens := estimateClaudeRequestInputTokens(effectiveReq)
	if estimatedTokens < 1 {
		estimatedTokens = 1
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]int{"input_tokens": estimatedTokens})
}

// handleClaudeMessages Claude API 处理
func (h *Handler) handleClaudeMessages(w http.ResponseWriter, r *http.Request) {
	h.handleClaudeMessagesInternal(w, r)
}

func (h *Handler) handleClaudeMessagesInternal(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	// 读取请求
	body, err := io.ReadAll(r.Body)
	if err != nil {
		if maxBytesExceeded(err) {
			h.sendClaudeError(w, 413, "invalid_request_error", "Request body too large")
			return
		}
		h.sendClaudeError(w, 400, "invalid_request_error", "Failed to read request body")
		return
	}
	logInboundRequestProbe("claude", r, body)

	var req ClaudeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendClaudeError(w, 400, "invalid_request_error", "Invalid JSON: "+err.Error())
		return
	}
	if msg := validateClaudeRequestShape(&req); msg != "" {
		h.sendClaudeError(w, 400, "invalid_request_error", msg)
		return
	}

	// 解析模型和 thinking 模式
	thinkingCfg := config.GetThinkingConfig()
	actualModel, thinking := resolveClaudeThinkingMode(req.Model, req.Thinking, thinkingCfg.Suffix)
	req.Model = actualModel

	// WebSearch server-tool: a pure web_search request is adapted to a Kiro MCP
	// call and returned as the Anthropic server-tool response.
	if hasWebSearchTool(&req) {
		wsInputTokens := estimateClaudeRequestInputTokens(&req)
		h.handleClaudeWebSearch(r.Context(), w, &req, req.Model, wsInputTokens, apiKeyIDFromContext(r.Context()))
		return
	}

	effectiveReq := cloneClaudeRequestForThinking(&req, thinking)
	thinkingResponseOpts := resolveClaudeThinkingResponseOptions(req.Thinking, thinkingCfg.ClaudeFormat)
	estimatedInputTokens := estimateClaudeRequestInputTokens(effectiveReq)
	cacheProfile := h.promptCache.BuildClaudeProfile(effectiveReq, estimatedInputTokens)

	// 转换请求
	kiroPayload := ClaudeToKiro(&req, thinking)
	if _, err := declaredToolsFromPayload(kiroPayload); err != nil {
		writeInvalidDeclaredTool(w, "claude", err)
		return
	}

	// Stream or non-stream
	apiKeyID := apiKeyIDFromContext(r.Context())
	if req.Stream {
		h.handleClaudeStream(r.Context(), w, kiroPayload, req.Model, thinking, thinkingResponseOpts, estimatedInputTokens, cacheProfile, apiKeyID)
	} else {
		if apiKeyForbidsSyncRequests(apiKeyID) {
			h.sendClaudeError(w, 403, "permission_error", "This API key only permits streaming requests; set \"stream\": true.")
			return
		}
		h.handleClaudeNonStream(r.Context(), w, kiroPayload, req.Model, thinking, thinkingResponseOpts, estimatedInputTokens, cacheProfile, apiKeyID)
	}
}

// handleClaudeStream Claude 流式响应
func (h *Handler) handleClaudeStream(ctx context.Context, w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, thinkingOpts claudeThinkingResponseOptions, estimatedInputTokens int, cacheProfile *promptCacheProfile, apiKeyID string) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Disable reverse-proxy response buffering (nginx/Caddy-compatible) so each
	// SSE chunk reaches the client immediately instead of waiting for a buffer fill.
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendClaudeError(w, 500, "api_error", "Streaming not supported")
		return
	}

	// Stream Keepalive: emit SSE comment frames only while the client connection
	// is idle so intermediaries do not cut long stalls between real events.
	sse := startStreamSSE(w, flusher)
	defer sse.Stop()

	requestStartedAt := time.Now()

	// 获取 thinking 输出格式配置
	thinkingFormat := thinkingOpts.Format

	msgID := "msg_" + uuid.New().String()
	startInputTokens := estimatedInputTokens
	messageStarted := false
	var messageStartUsage promptCacheUsage

	// Per-attempt render state used after Account Routing returns, hoisted to
	// caller scope so terminal rendering (message_delta/message_stop) runs after
	// the slot is released. Block-tracking state stays inside the callback.
	var inputTokens, outputTokens int
	var credits float64
	var realInputTokens int
	var toolUses []KiroToolUse
	var rawContentBuilder strings.Builder
	var rawThinkingBuilder strings.Builder
	var firstTokenAt time.Time
	var cacheUsage promptCacheUsage
	var okAccount *config.Account

	ensureMessageStart := func() {
		if messageStarted {
			return
		}
		sse.WriteEvent("message_start", map[string]interface{}{
			"type": "message_start",
			"message": map[string]interface{}{
				"id":            msgID,
				"type":          "message",
				"role":          "assistant",
				"content":       []interface{}{},
				"model":         model,
				"stop_reason":   nil,
				"stop_sequence": nil,
				"usage":         buildClaudeUsageMap(startInputTokens, 0, messageStartUsage, cacheProfile != nil),
			},
		})
		messageStarted = true
	}

	outcome := h.runWithAccount(ctx, model, payload.RoutingAffinityKey, func(account *config.Account) attemptResult {
		// Reset per-attempt state so a failover starts clean.
		inputTokens, outputTokens = 0, 0
		credits = 0
		realInputTokens = 0
		toolUses = nil
		rawContentBuilder.Reset()
		rawThinkingBuilder.Reset()
		firstTokenAt = time.Time{}

		cacheUsage = h.promptCache.Compute(account.ID, cacheProfile)
		messageStartUsage = cacheUsage

		var nextContentIndex int
		activeBlockIndex := -1
		activeBlockType := ""

		closeActiveBlock := func() {
			if activeBlockIndex < 0 {
				return
			}
			sse.WriteEvent("content_block_stop", map[string]interface{}{
				"type":  "content_block_stop",
				"index": activeBlockIndex,
			})
			activeBlockIndex = -1
			activeBlockType = ""
		}

		startContentBlock := func(blockType string) {
			if activeBlockType == blockType {
				return
			}
			ensureMessageStart()
			closeActiveBlock()

			idx := nextContentIndex
			nextContentIndex++

			if blockType == "thinking" {
				sse.WriteEvent("content_block_start", map[string]interface{}{
					"type":  "content_block_start",
					"index": idx,
					"content_block": map[string]string{
						"type":     "thinking",
						"thinking": "",
					},
				})
			} else {
				sse.WriteEvent("content_block_start", map[string]interface{}{
					"type":  "content_block_start",
					"index": idx,
					"content_block": map[string]string{
						"type": "text",
						"text": "",
					},
				})
			}

			activeBlockIndex = idx
			activeBlockType = blockType
		}

		var thinkingStarted bool
		var eventThinkingOpen bool

		sendText := func(text string, thinkingState int) {
			if thinkingState == 0 {
				if text == "" {
					return
				}
				startContentBlock("text")
				sse.WriteEvent("content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": activeBlockIndex,
					"delta": map[string]string{"type": "text_delta", "text": text},
				})
				return
			}

			if !thinking {
				return
			}

			switch thinkingFormat {
			case "think":
				var outputText string
				switch thinkingState {
				case 1:
					outputText = "<think>" + text
				case 2:
					outputText = text
				case 3:
					outputText = text + "</think>"
				}
				if outputText == "" {
					return
				}
				startContentBlock("text")
				sse.WriteEvent("content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": activeBlockIndex,
					"delta": map[string]string{"type": "text_delta", "text": outputText},
				})
			case "reasoning_content":
				if text == "" {
					return
				}
				startContentBlock("text")
				sse.WriteEvent("content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": activeBlockIndex,
					"delta": map[string]string{"type": "text_delta", "text": text},
				})
			default:
				if thinkingOpts.OmitDisplay {
					if thinkingState == 1 {
						startContentBlock("thinking")
						return
					}
					if thinkingState == 3 {
						if activeBlockType != "thinking" {
							startContentBlock("thinking")
						}
						closeActiveBlock()
					}
					return
				}
				if thinkingState == 3 && text == "" {
					if activeBlockType == "thinking" {
						closeActiveBlock()
					}
					return
				}
				if text != "" {
					startContentBlock("thinking")
					sse.WriteEvent("content_block_delta", map[string]interface{}{
						"type":  "content_block_delta",
						"index": activeBlockIndex,
						"delta": map[string]string{"type": "thinking_delta", "thinking": text},
					})
				}
				if thinkingState == 3 && activeBlockType == "thinking" {
					closeActiveBlock()
				}
			}
		}

		tools, toolsErr := declaredToolsFromPayload(payload)
		if toolsErr != nil {
			// Should have been rejected pre-upstream; treat as caller-terminal non-penalizing.
			return attemptStop(toolsErr, false)
		}

		var streamStopReason string
		var sawValidatedTool bool
		err := streamAssistantFromKiro(ctx, account, payload, tools, func(ev assistantEvent) error {
			switch ev.kind {
			case assistantKindPlainText:
				if ev.text == "" {
					return nil
				}
				if firstTokenAt.IsZero() {
					firstTokenAt = time.Now()
				}
				rawContentBuilder.WriteString(ev.text)
				// Normalizer already separated tags; do not re-run thinkingSplitter.
				if eventThinkingOpen {
					sendText("", 3)
					eventThinkingOpen = false
					thinkingStarted = false
				}
				sendText(ev.text, 0)
			case assistantKindReasoning:
				if ev.text == "" {
					return nil
				}
				if firstTokenAt.IsZero() {
					firstTokenAt = time.Now()
				}
				rawThinkingBuilder.WriteString(ev.text)
				if thinking {
					if !thinkingStarted {
						sendText(ev.text, 1)
						thinkingStarted = true
						eventThinkingOpen = true
					} else {
						sendText(ev.text, 2)
					}
				}
			case assistantKindToolCall:
				if eventThinkingOpen {
					sendText("", 3)
					eventThinkingOpen = false
					thinkingStarted = false
				}
				rawContentBuilder.WriteString(ev.tool.Name)
				if b, mErr := json.Marshal(ev.tool.Input); mErr == nil {
					rawContentBuilder.Write(b)
				}
				tu := kiroToolFromNormalized(ev.tool)
				toolUses = append(toolUses, tu)
				sawValidatedTool = true
				ensureMessageStart()
				closeActiveBlock()
				idx := nextContentIndex
				nextContentIndex++
				writeClaudeToolBlock(sse, idx, ev.tool)
			case assistantKindTelemetry:
				if ev.hasCredits {
					credits = ev.credits
				}
				if ev.hasContext {
					realInputTokens = int(ev.contextPct * float64(getContextWindowSize(model)) / 100.0)
				}
			case assistantKindCompletion:
				inputTokens = ev.finalIn
				outputTokens = ev.finalOut
				if ev.finalCred > 0 {
					credits = ev.finalCred
				}
				streamStopReason = ev.stopReason
				_ = sawValidatedTool
			case assistantKindModelOutputError:
				return ev.err
			}
			return nil
		})
		if err != nil {
			if IsModelOutputError(err) {
				if !messageStarted {
					// Pre-commit: caller will render sanitized 502; no Account penalty/failover.
					return attemptStop(err, false)
				}
				// Post-commit: terminal, non-penalizing.
				return attemptStop(err, false)
			}
			if !messageStarted {
				return attemptRetry(err)
			}
			return attemptStop(err, true)
		}

		// Close any open thinking block while the slot is still held.
		if eventThinkingOpen {
			sendText("", 3)
			eventThinkingOpen = false
		}
		closeActiveBlock()
		if streamStopReason != "" {
			// Prefer normalizer stop reason when available; success path still derives from tools.
			_ = streamStopReason
		}
		okAccount = account
		return attemptSuccess()
	})

	if outcome.stopReason == routeStopCanceled {
		return
	}

	// Success: slot released; emit final usage + message_stop and record success.
	if outcome.stopReason == routeStopSuccess {
		account := okAccount
		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		outputContent, extractedReasoning := extractThinkingFromContent(rawContentBuilder.String())
		thinkingOutput := rawThinkingBuilder.String()
		if thinking && thinkingOutput == "" && extractedReasoning != "" {
			thinkingOutput = extractedReasoning
		}
		if !thinking {
			thinkingOutput = ""
		}
		outputTokens = estimateClaudeOutputTokens(outputContent, thinkingOutput, toolUses)

		h.recordSuccessForApiKey(apiKeyID, inputTokens, outputTokens, credits)
		var ttftMs int64
		if !firstTokenAt.IsZero() {
			ttftMs = firstTokenAt.Sub(requestStartedAt).Milliseconds()
		}
		recordRequestMetrics("claude", model, true, account, apiKeyID, true, http.StatusOK, "", inputTokens, outputTokens, credits, requestStartedAt, ttftMs)
		h.pool.RecordSuccess(account.ID)
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
		h.promptCache.Update(account.ID, cacheProfile)

		stopReason := "end_turn"
		if len(toolUses) > 0 {
			stopReason = "tool_use"
		}

		ensureMessageStart()
		sse.WriteEvent("message_delta", map[string]interface{}{
			"type": "message_delta",
			"delta": map[string]interface{}{
				"stop_reason": stopReason,
			},
			"usage": buildClaudeUsageMap(inputTokens, outputTokens, cacheUsage, cacheProfile != nil),
		})
		sse.WriteEvent("message_stop", map[string]interface{}{
			"type": "message_stop",
		})
		return
	}

	// Output already committed then upstream failed: close the SSE stream cleanly
	// now that the slot is released.
	if outcome.stopReason == routeStopCallerTerminal {
		if IsModelOutputError(outcome.lastErr) {
			// Model output defects are not Account failures.
			recordRequestMetrics("claude", model, true, outcome.lastAccount, apiKeyID, false, http.StatusBadGateway, "model_output_error", estimatedInputTokens, outputTokens, credits, requestStartedAt)
			if messageStarted || sse.Committed() {
				writeClaudeStreamModelOutputError(sse, sanitizedModelOutputMessage(outcome.lastErr))
				return
			}
			sse.Stop()
			writeModelOutputErrorJSON(w, "claude")
			return
		}
		h.recordFailure()
		statusCode, errType := metricsErrorDetails(outcome.lastErr, http.StatusInternalServerError, "api_error")
		recordRequestMetrics("claude", model, true, outcome.lastAccount, apiKeyID, false, statusCode, errType, estimatedInputTokens, outputTokens, credits, requestStartedAt)
		sse.WriteEvent("error", map[string]interface{}{
			"type":  "error",
			"error": map[string]string{"type": "api_error", "message": outcome.lastErr.Error()},
		})
		return
	}

	// Stop keepalive before any non-SSE error write so a late ping cannot race
	// WriteHeader/JSON. If a keepalive already committed the body, stay on SSE.
	sse.Stop()
	streamCommitted := messageStarted || sse.Committed()

	if outcome.stopReason == routeStopRoutingLimit {
		h.recordFailure()
		statusCode, errType := metricsErrorDetails(outcome.acquireErr, http.StatusTooManyRequests, "rate_limit_error")
		recordRequestMetrics("claude", model, true, nil, apiKeyID, false, statusCode, errType, estimatedInputTokens, 0, 0, requestStartedAt)
		if streamCommitted {
			ensureMessageStart()
			sse.WriteEvent("error", map[string]interface{}{
				"type":  "error",
				"error": map[string]string{"type": "rate_limit_error", "message": routingErrorMessage(outcome.acquireErr)},
			})
			return
		}
		h.sendClaudeError(w, 429, "rate_limit_error", routingErrorMessage(outcome.acquireErr))
		return
	}

	if outcome.lastErr == nil {
		recordRequestMetrics("claude", model, true, nil, apiKeyID, false, http.StatusServiceUnavailable, "no_available_accounts", estimatedInputTokens, 0, 0, requestStartedAt)
		if streamCommitted {
			ensureMessageStart()
			sse.WriteEvent("error", map[string]interface{}{
				"type":  "error",
				"error": map[string]string{"type": "api_error", "message": "No available accounts"},
			})
			return
		}
		h.sendClaudeError(w, 503, "api_error", "No available accounts")
		return
	}

	h.recordFailure()
	statusCode, errType := metricsErrorDetails(outcome.lastErr, http.StatusInternalServerError, "api_error")
	recordRequestMetrics("claude", model, true, outcome.lastAccount, apiKeyID, false, statusCode, errType, estimatedInputTokens, 0, 0, requestStartedAt)
	logRetryExhausted("claude_stream", model, statusCode, errType, outcome.lastErr)
	// If the stream already started (real events or keepalive), the SSE
	// headers/body are committed and the status line cannot change; emit an
	// error event and stop. Otherwise return the true status (e.g. 429 when the
	// pool is drained) instead of a blanket 500.
	clientMsg := improperlyFormedClientMessage(outcome.lastErr)
	if streamCommitted {
		ensureMessageStart()
		sse.WriteEvent("error", map[string]interface{}{
			"type":  "error",
			"error": map[string]string{"type": clientFacingClaudeErrorType(statusCode), "message": clientMsg},
		})
		return
	}
	h.sendClaudeError(w, statusCode, clientFacingClaudeErrorType(statusCode), clientMsg)
}

// handleClaudeNonStream Claude 非流式响应
func (h *Handler) handleClaudeNonStream(ctx context.Context, w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, thinkingOpts claudeThinkingResponseOptions, estimatedInputTokens int, cacheProfile *promptCacheProfile, apiKeyID string) {
	requestStartedAt := time.Now()

	// Per-attempt render state, hoisted to caller scope so the JSON response is
	// written after Account Routing releases the slot. Non-stream never commits
	// output mid-attempt, so every failure is a clean failover (attemptRetry).
	var content string
	var thinkingContent string
	var toolUses []KiroToolUse
	var inputTokens, outputTokens int
	var credits float64
	var realInputTokens int
	var cacheUsage promptCacheUsage
	var okAccount *config.Account

	outcome := h.runWithAccount(ctx, model, payload.RoutingAffinityKey, func(account *config.Account) attemptResult {
		content = ""
		thinkingContent = ""
		toolUses = nil
		inputTokens, outputTokens = 0, 0
		credits = 0
		realInputTokens = 0

		cacheUsage = h.promptCache.Compute(account.ID, cacheProfile)

		tools, toolsErr := declaredToolsFromPayload(payload)
		if toolsErr != nil {
			return attemptStop(toolsErr, false)
		}
		err := streamAssistantFromKiro(ctx, account, payload, tools, func(ev assistantEvent) error {
			switch ev.kind {
			case assistantKindPlainText:
				content += ev.text
			case assistantKindReasoning:
				if thinking {
					thinkingContent += ev.text
				}
			case assistantKindToolCall:
				toolUses = append(toolUses, kiroToolFromNormalized(ev.tool))
			case assistantKindTelemetry:
				if ev.hasCredits {
					credits = ev.credits
				}
				if ev.hasContext {
					realInputTokens = int(ev.contextPct * float64(getContextWindowSize(model)) / 100.0)
				}
			case assistantKindCompletion:
				inputTokens = ev.finalIn
				outputTokens = ev.finalOut
				if ev.finalCred > 0 {
					credits = ev.finalCred
				}
			case assistantKindModelOutputError:
				return ev.err
			}
			return nil
		})
		if err != nil {
			if IsModelOutputError(err) {
				return attemptStop(err, false)
			}
			return attemptRetry(err)
		}
		okAccount = account
		return attemptSuccess()
	})

	if outcome.stopReason == routeStopCanceled {
		return
	}
	if outcome.stopReason == routeStopCallerTerminal && IsModelOutputError(outcome.lastErr) {
		recordRequestMetrics("claude", model, false, outcome.lastAccount, apiKeyID, false, http.StatusBadGateway, "model_output_error", estimatedInputTokens, 0, 0, requestStartedAt)
		writeModelOutputErrorJSON(w, "claude")
		return
	}

	if outcome.stopReason == routeStopSuccess {
		account := okAccount
		thinkingFormat := thinkingOpts.Format
		finalContent, extractedReasoning := extractThinkingFromContent(content)
		rawThinkingContent := thinkingContent
		if thinking && rawThinkingContent == "" && extractedReasoning != "" {
			rawThinkingContent = extractedReasoning
		}
		if !thinking {
			rawThinkingContent = ""
		}

		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		outputTokens = estimateClaudeOutputTokens(finalContent, rawThinkingContent, toolUses)

		h.recordSuccessForApiKey(apiKeyID, inputTokens, outputTokens, credits)
		recordRequestMetrics("claude", model, false, account, apiKeyID, true, http.StatusOK, "", inputTokens, outputTokens, credits, requestStartedAt)
		h.pool.RecordSuccess(account.ID)
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
		h.promptCache.Update(account.ID, cacheProfile)

		responseThinkingContent := rawThinkingContent
		includeEmptyThinkingBlock := thinking && thinkingOpts.OmitDisplay && rawThinkingContent != ""
		if includeEmptyThinkingBlock {
			responseThinkingContent = ""
		}

		if thinking && responseThinkingContent != "" {
			switch thinkingFormat {
			case "think":
				finalContent = "<think>" + responseThinkingContent + "</think>" + finalContent
				responseThinkingContent = ""
			case "reasoning_content":
				finalContent = responseThinkingContent + finalContent
				responseThinkingContent = ""
			default:
			}
		}

		resp := KiroToClaudeResponse(finalContent, responseThinkingContent, includeEmptyThinkingBlock, toolUses, inputTokens, outputTokens, model)
		resp.Usage.InputTokens = billedClaudeInputTokens(inputTokens, cacheUsage)
		resp.Usage.CacheCreationInputTokens = cacheUsage.CacheCreationInputTokens
		resp.Usage.CacheReadInputTokens = cacheUsage.CacheReadInputTokens
		if cacheProfile != nil {
			resp.Usage.CacheCreation = &ClaudeCacheCreationUsage{
				Ephemeral5mInputTokens: cacheUsage.CacheCreation5mInputTokens,
				Ephemeral1hInputTokens: cacheUsage.CacheCreation1hInputTokens,
			}
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(resp)
		return
	}

	if outcome.stopReason == routeStopRoutingLimit {
		h.recordFailure()
		statusCode, errType := metricsErrorDetails(outcome.acquireErr, http.StatusTooManyRequests, "rate_limit_error")
		recordRequestMetrics("claude", model, false, nil, apiKeyID, false, statusCode, errType, estimatedInputTokens, 0, 0, requestStartedAt)
		h.sendClaudeError(w, 429, "rate_limit_error", routingErrorMessage(outcome.acquireErr))
		return
	}

	if outcome.lastErr == nil {
		recordRequestMetrics("claude", model, false, nil, apiKeyID, false, http.StatusServiceUnavailable, "no_available_accounts", estimatedInputTokens, 0, 0, requestStartedAt)
		h.sendClaudeError(w, 503, "api_error", "No available accounts")
		return
	}

	h.recordFailure()
	statusCode, errType := metricsErrorDetails(outcome.lastErr, http.StatusInternalServerError, "api_error")
	recordRequestMetrics("claude", model, false, outcome.lastAccount, apiKeyID, false, statusCode, errType, estimatedInputTokens, 0, 0, requestStartedAt)
	logRetryExhausted("claude", model, statusCode, errType, outcome.lastErr)
	h.sendClaudeError(w, statusCode, clientFacingClaudeErrorType(statusCode), improperlyFormedClientMessage(outcome.lastErr))
}

func (h *Handler) sendClaudeError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"type": "error",
		"error": map[string]string{
			"type":    errType,
			"message": message,
		},
	})
}
