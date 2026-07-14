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
	excluded := make(map[string]bool)
	var lastErr error
	var lastAccount *config.Account
	messageStarted := false
	var messageStartUsage promptCacheUsage
	// Panic safety net: guarantees the routing slot is released even if a panic
	// unwinds the stack. release is sync.Once-idempotent, so the manual release()
	// calls below still drive normal failover.
	var activeRelease func()
	defer func() {
		if activeRelease != nil {
			activeRelease()
		}
	}()

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

	for attempt := 0; attempt < getAccountRetryAttempts(); attempt++ {
		account, release, acquireErr := h.acquireRouteAccount(ctx, model, excluded, payload.RoutingAffinityKey)
		if acquireErr != nil {
			if isRoutingLimitError(acquireErr) {
				h.recordFailure()
				statusCode, errType := metricsErrorDetails(acquireErr, http.StatusTooManyRequests, "rate_limit_error")
				recordRequestMetrics("claude", model, true, nil, apiKeyID, false, statusCode, errType, estimatedInputTokens, 0, 0, requestStartedAt)

				sse.WriteEvent("error", map[string]interface{}{
					"type":  "error",
					"error": map[string]string{"type": "rate_limit_error", "message": routingErrorMessage(acquireErr)},
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
			continue
		}
		cacheUsage := h.promptCache.Compute(account.ID, cacheProfile)
		messageStartUsage = cacheUsage

		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int
		var toolUses []KiroToolUse
		var nextContentIndex int
		var rawContentBuilder strings.Builder
		var rawThinkingBuilder strings.Builder
		var firstTokenAt time.Time
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

		var textBuffer string
		var inThinkingBlock bool
		var dropTagThinking bool
		var thinkingSource thinkingStreamSource
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

		processClaudeText := func(text string, isThinking bool, forceFlush bool) {
			if isThinking && !thinking {
				return
			}

			if isThinking {
				if !allowReasoningSource(&thinkingSource) {
					return
				}
				if !thinkingStarted {
					sendText(text, 1)
					thinkingStarted = true
					eventThinkingOpen = true
				} else {
					sendText(text, 2)
				}
				return
			}

			if eventThinkingOpen {
				sendText("", 3)
				eventThinkingOpen = false
				thinkingStarted = false
			}

			textBuffer += text

			for {
				if !inThinkingBlock {
					thinkingStart := strings.Index(textBuffer, "<thinking>")
					if thinkingStart != -1 {
						if thinkingStart > 0 {
							sendText(textBuffer[:thinkingStart], 0)
						}
						textBuffer = textBuffer[thinkingStart+10:]
						inThinkingBlock = true
						dropTagThinking = !allowTagSource(&thinkingSource)
						thinkingStarted = false
					} else if forceFlush || len([]rune(textBuffer)) > 50 {
						runes := []rune(textBuffer)
						safeLen := len(runes)
						if !forceFlush {
							safeLen = max(0, len(runes)-15)
						}
						if safeLen > 0 {
							sendText(string(runes[:safeLen]), 0)
							textBuffer = string(runes[safeLen:])
						}
						break
					} else {
						break
					}
				} else {
					thinkingEnd := strings.Index(textBuffer, "</thinking>")
					if thinkingEnd != -1 {
						content := textBuffer[:thinkingEnd]
						if !dropTagThinking {
							if !thinkingStarted {
								sendText(content, 1)
								sendText("", 3)
							} else {
								sendText(content, 3)
							}
						}
						textBuffer = textBuffer[thinkingEnd+11:]
						inThinkingBlock = false
						dropTagThinking = false
						thinkingStarted = false
					} else if forceFlush {
						if textBuffer != "" {
							if !dropTagThinking {
								if !thinkingStarted {
									sendText(textBuffer, 1)
									sendText("", 3)
								} else {
									sendText(textBuffer, 3)
								}
							}
							textBuffer = ""
						}
						inThinkingBlock = false
						dropTagThinking = false
						thinkingStarted = false
						break
					} else {
						runes := []rune(textBuffer)
						if len(runes) > 20 {
							safeLen := len(runes) - 15
							if safeLen > 0 {
								if !dropTagThinking {
									if !thinkingStarted {
										sendText(string(runes[:safeLen]), 1)
										thinkingStarted = true
									} else {
										sendText(string(runes[:safeLen]), 2)
									}
								}
								textBuffer = string(runes[safeLen:])
							}
						}
						break
					}
				}
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
					rawThinkingBuilder.WriteString(text)
				} else {
					rawContentBuilder.WriteString(text)
				}
				processClaudeText(text, isThinking, false)
			},
			OnToolUse: func(tu KiroToolUse) {
				processClaudeText("", false, true)
				rawContentBuilder.WriteString(tu.Name)
				if b, err := json.Marshal(tu.Input); err == nil {
					rawContentBuilder.Write(b)
				}

				toolUses = append(toolUses, tu)
				ensureMessageStart()
				closeActiveBlock()

				idx := nextContentIndex
				nextContentIndex++

				sse.WriteEvent("content_block_start", map[string]interface{}{
					"type":  "content_block_start",
					"index": idx,
					"content_block": map[string]interface{}{
						"type":  "tool_use",
						"id":    tu.ToolUseID,
						"name":  tu.Name,
						"input": map[string]interface{}{},
					},
				})

				inputJSON, _ := json.Marshal(tu.Input)
				sse.WriteEvent("content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": idx,
					"delta": map[string]interface{}{
						"type":         "input_json_delta",
						"partial_json": string(inputJSON),
					},
				})

				sse.WriteEvent("content_block_stop", map[string]interface{}{
					"type":  "content_block_stop",
					"index": idx,
				})
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
		release()
		if err != nil {
			lastErr = err
			lastAccount = account
			h.handleAccountError(account, excluded, err)
			if !messageStarted {
				if shouldBackoffBeforeRetry(err) {
					time.Sleep(retryBackoffAfterRateLimit())
				}
				continue
			}
			h.recordFailure()
			statusCode, errType := metricsErrorDetails(err, http.StatusInternalServerError, "api_error")
			recordRequestMetrics("claude", model, true, account, apiKeyID, false, statusCode, errType, estimatedInputTokens, outputTokens, credits, requestStartedAt)
			sse.WriteEvent("error", map[string]interface{}{
				"type":  "error",
				"error": map[string]string{"type": "api_error", "message": err.Error()},
			})
			return
		}

		processClaudeText("", false, true)
		if eventThinkingOpen {
			sendText("", 3)
		}
		closeActiveBlock()

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

	// Stop keepalive before any non-SSE error write so a late ping cannot race
	// WriteHeader/JSON. If a keepalive already committed the body, stay on SSE.
	sse.Stop()
	streamCommitted := messageStarted || sse.Committed()

	if lastErr == nil {
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
	statusCode, errType := metricsErrorDetails(lastErr, http.StatusInternalServerError, "api_error")
	recordRequestMetrics("claude", model, true, lastAccount, apiKeyID, false, statusCode, errType, estimatedInputTokens, 0, 0, requestStartedAt)
	logRetryExhausted("claude_stream", model, statusCode, errType, lastErr)
	// If the stream already started (real events or keepalive), the status line
	// cannot change; emit an error event and stop. Otherwise return the true
	// status (e.g. 429 when the pool is drained) instead of a blanket 500.
	if streamCommitted {
		ensureMessageStart()
		sse.WriteEvent("error", map[string]interface{}{
			"type":  "error",
			"error": map[string]string{"type": clientFacingClaudeErrorType(statusCode), "message": lastErr.Error()},
		})
		return
	}
	h.sendClaudeError(w, statusCode, clientFacingClaudeErrorType(statusCode), lastErr.Error())
}

// handleClaudeNonStream Claude 非流式响应
func (h *Handler) handleClaudeNonStream(ctx context.Context, w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, thinkingOpts claudeThinkingResponseOptions, estimatedInputTokens int, cacheProfile *promptCacheProfile, apiKeyID string) {
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
				recordRequestMetrics("claude", model, false, nil, apiKeyID, false, statusCode, errType, estimatedInputTokens, 0, 0, requestStartedAt)

				h.sendClaudeError(w, 429, "rate_limit_error", routingErrorMessage(acquireErr))
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
			continue
		}
		cacheUsage := h.promptCache.Compute(account.ID, cacheProfile)

		var content string
		var thinkingContent string
		var toolUses []KiroToolUse
		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int

		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if isThinking {
					thinkingContent += text
				} else {
					content += text
				}
			},
			OnToolUse: func(tu KiroToolUse) {
				toolUses = append(toolUses, tu)
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

	if lastErr == nil {
		recordRequestMetrics("claude", model, false, nil, apiKeyID, false, http.StatusServiceUnavailable, "no_available_accounts", estimatedInputTokens, 0, 0, requestStartedAt)
		h.sendClaudeError(w, 503, "api_error", "No available accounts")
		return
	}

	h.recordFailure()
	statusCode, errType := metricsErrorDetails(lastErr, http.StatusInternalServerError, "api_error")
	recordRequestMetrics("claude", model, false, lastAccount, apiKeyID, false, statusCode, errType, estimatedInputTokens, 0, 0, requestStartedAt)
	logRetryExhausted("claude", model, statusCode, errType, lastErr)
	h.sendClaudeError(w, statusCode, clientFacingClaudeErrorType(statusCode), lastErr.Error())
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
