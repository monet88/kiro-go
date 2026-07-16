package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"kiro-go/logger"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

// WebSearch server-tool support.
//
// When an Anthropic client (e.g. Claude Code) sends a request whose tools list
// contains exactly one tool named "web_search", the upstream Kiro backend does
// not handle the Anthropic web_search server-tool directly. This module adapts
// such a request into a Kiro MCP (JSON-RPC tools/call) request, then rebuilds
// the MCP result into the Anthropic-standard SSE event sequence
// (server_tool_use + web_search_tool_result + text), so the client sees the
// expected server-tool behaviour.
//
// Ported from kiro.rs (src/anthropic/websearch.rs); the SSE event contract and
// ID formats are kept identical so existing Anthropic clients match the output.

const webSearchQueryPrefix = "Perform a web search for the query: "

// ==================== MCP request / response shapes ====================

type mcpRequest struct {
	ID      string       `json:"id"`
	JSONRPC string       `json:"jsonrpc"`
	Method  string       `json:"method"`
	Params  mcpReqParams `json:"params"`
}

type mcpReqParams struct {
	Name      string          `json:"name"`
	Arguments mcpReqArguments `json:"arguments"`
}

type mcpReqArguments struct {
	Query string `json:"query"`
}

type mcpResponse struct {
	Error  *mcpError  `json:"error"`
	ID     string     `json:"id"`
	Result *mcpResult `json:"result"`
}

type mcpError struct {
	Code    *int   `json:"code"`
	Message string `json:"message"`
}

type mcpResult struct {
	Content []mcpContent `json:"content"`
	IsError bool         `json:"isError"`
}

type mcpContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// webSearchResults is the JSON embedded in the MCP result's first text content.
type webSearchResults struct {
	Results      []webSearchResult `json:"results"`
	TotalResults *int              `json:"totalResults"`
	Query        *string           `json:"query"`
	Error        *string           `json:"error"`
}

type webSearchResult struct {
	Title         string  `json:"title"`
	URL           string  `json:"url"`
	Snippet       *string `json:"snippet"`
	PublishedDate *int64  `json:"publishedDate"`
	ID            *string `json:"id"`
	Domain        *string `json:"domain"`
}

// ==================== Pure detection / extraction ====================

// hasWebSearchTool reports whether the request is a pure WebSearch request:
// tools has exactly one entry whose name is "web_search".
func hasWebSearchTool(req *ClaudeRequest) bool {
	if req == nil {
		return false
	}
	return len(req.Tools) == 1 && req.Tools[0].Name == "web_search"
}

// extractWebSearchQuery reads the first text content of the first message and
// strips the "Perform a web search for the query: " prefix when present.
func extractWebSearchQuery(req *ClaudeRequest) (string, bool) {
	if req == nil || len(req.Messages) == 0 {
		return "", false
	}

	var text string
	switch content := req.Messages[0].Content.(type) {
	case string:
		text = content
	case []interface{}:
		if len(content) == 0 {
			return "", false
		}
		first, ok := content[0].(map[string]interface{})
		if !ok {
			return "", false
		}
		if t, _ := first["type"].(string); t != "text" {
			return "", false
		}
		text, _ = first["text"].(string)
	default:
		return "", false
	}

	query := strings.TrimPrefix(text, webSearchQueryPrefix)
	if query == "" {
		return "", false
	}
	return query, true
}

// ==================== ID generation ====================

const (
	idCharset22 = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	idCharset8  = "abcdefghijklmnopqrstuvwxyz0123456789"
)

func randomID(n int, charset string) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = charset[rand.Intn(len(charset))]
	}
	return string(b)
}

// newMCPRequestID returns an MCP request id.
// Format: web_search_tooluse_{22-char}_{millis}_{8-char}
func newMCPRequestID() string {
	return fmt.Sprintf("web_search_tooluse_%s_%d_%s",
		randomID(22, idCharset22),
		time.Now().UnixMilli(),
		randomID(8, idCharset8),
	)
}

// newServerToolUseID returns a server tool_use id.
// Format: srvtoolu_{32 hex chars}
func newServerToolUseID() string {
	return "srvtoolu_" + strings.ReplaceAll(uuid.New().String(), "-", "")[:32]
}

// ==================== MCP request body / response parsing ====================

func buildMCPRequestBody(requestID, query string) ([]byte, error) {
	return json.Marshal(mcpRequest{
		ID:      requestID,
		JSONRPC: "2.0",
		Method:  "tools/call",
		Params: mcpReqParams{
			Name:      "web_search",
			Arguments: mcpReqArguments{Query: query},
		},
	})
}

// parseMCPSearchResults extracts the embedded search-results JSON from an MCP
// response body. Returns nil results when the body cannot be interpreted.
func parseMCPSearchResults(body []byte) (*webSearchResults, error) {
	var resp mcpResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	if resp.Error != nil {
		code := -1
		if resp.Error.Code != nil {
			code = *resp.Error.Code
		}
		msg := resp.Error.Message
		if msg == "" {
			msg = "Unknown error"
		}
		return nil, fmt.Errorf("MCP error: %d - %s", code, msg)
	}
	if resp.Result == nil || len(resp.Result.Content) == 0 {
		return nil, nil
	}
	first := resp.Result.Content[0]
	if first.Type != "text" {
		return nil, nil
	}
	var results webSearchResults
	if err := json.Unmarshal([]byte(first.Text), &results); err != nil {
		return nil, err
	}
	return &results, nil
}

// ==================== Summary text ====================

// runeTruncate returns s truncated to at most n runes, appending "..." when it
// was shortened. It is UTF-8 safe (never splits a multi-byte rune).
func runeTruncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "..."
}

func generateWebSearchSummary(query string, results *webSearchResults) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Here are the search results for \"%s\":\n\n", query)

	if results != nil && len(results.Results) > 0 {
		for i, r := range results.Results {
			fmt.Fprintf(&sb, "%d. **%s**\n", i+1, r.Title)
			if r.Snippet != nil && *r.Snippet != "" {
				fmt.Fprintf(&sb, "   %s\n", runeTruncate(*r.Snippet, 200))
			}
			fmt.Fprintf(&sb, "   Source: %s\n\n", r.URL)
		}
	} else {
		sb.WriteString("No results found.\n")
	}

	sb.WriteString("\nPlease note that these are web search results and may not be fully accurate or up-to-date.")
	return sb.String()
}

// pageAgeFromMillis formats a published-date timestamp (ms) like "January 2, 2006".
func pageAgeFromMillis(ms *int64) interface{} {
	if ms == nil {
		return nil
	}
	return time.UnixMilli(*ms).UTC().Format("January 2, 2006")
}

// ==================== SSE event sequence ====================

type sseEvent struct {
	Event string
	Data  map[string]interface{}
}

// buildWebSearchEvents builds the Anthropic-standard SSE event sequence for a
// WebSearch response. The block indices and field names mirror the official
// Claude API contract (server_tool_use sends input in content_block_start in
// one shot; web_search_tool_result has no tool_use_id; message_delta has no
// stop_sequence and reports server_tool_use.web_search_requests).
func buildWebSearchEvents(model, query, toolUseID string, results *webSearchResults, inputTokens int) []sseEvent {
	messageID := "msg_" + strings.ReplaceAll(uuid.New().String(), "-", "")[:24]
	events := make([]sseEvent, 0, 16)

	// 1. message_start
	events = append(events, sseEvent{"message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":          messageID,
			"type":        "message",
			"role":        "assistant",
			"model":       model,
			"content":     []interface{}{},
			"stop_reason": nil,
			"usage": map[string]interface{}{
				"input_tokens":                inputTokens,
				"output_tokens":               0,
				"cache_creation_input_tokens": 0,
				"cache_read_input_tokens":     0,
			},
		},
	}})

	// 2. text block (search decision, index 0)
	events = append(events, sseEvent{"content_block_start", map[string]interface{}{
		"type":          "content_block_start",
		"index":         0,
		"content_block": map[string]interface{}{"type": "text", "text": ""},
	}})
	events = append(events, sseEvent{"content_block_delta", map[string]interface{}{
		"type":  "content_block_delta",
		"index": 0,
		"delta": map[string]interface{}{"type": "text_delta", "text": fmt.Sprintf("I'll search for \"%s\".", query)},
	}})
	events = append(events, sseEvent{"content_block_stop", map[string]interface{}{
		"type": "content_block_stop", "index": 0,
	}})

	// 3. server_tool_use block (index 1) - input sent in one shot
	events = append(events, sseEvent{"content_block_start", map[string]interface{}{
		"type":  "content_block_start",
		"index": 1,
		"content_block": map[string]interface{}{
			"id":    toolUseID,
			"type":  "server_tool_use",
			"name":  "web_search",
			"input": map[string]interface{}{"query": query},
		},
	}})
	events = append(events, sseEvent{"content_block_stop", map[string]interface{}{
		"type": "content_block_stop", "index": 1,
	}})

	// 4. web_search_tool_result block (index 2). Per the Anthropic contract this
	// block must carry tool_use_id referencing the server_tool_use.id so clients
	// can correlate the results with the tool call.
	searchContent := make([]interface{}, 0)
	if results != nil {
		for _, r := range results.Results {
			encrypted := ""
			if r.Snippet != nil {
				encrypted = *r.Snippet
			}
			searchContent = append(searchContent, map[string]interface{}{
				"type":              "web_search_result",
				"title":             r.Title,
				"url":               r.URL,
				"encrypted_content": encrypted,
				"page_age":          pageAgeFromMillis(r.PublishedDate),
			})
		}
	}
	events = append(events, sseEvent{"content_block_start", map[string]interface{}{
		"type":  "content_block_start",
		"index": 2,
		"content_block": map[string]interface{}{
			"type":        "web_search_tool_result",
			"tool_use_id": toolUseID,
			"content":     searchContent,
		},
	}})
	events = append(events, sseEvent{"content_block_stop", map[string]interface{}{
		"type": "content_block_stop", "index": 2,
	}})

	// 5. text block (summary, index 3) - delta chunked by 100 runes
	events = append(events, sseEvent{"content_block_start", map[string]interface{}{
		"type":          "content_block_start",
		"index":         3,
		"content_block": map[string]interface{}{"type": "text", "text": ""},
	}})
	summary := generateWebSearchSummary(query, results)
	for _, chunk := range chunkByRunes(summary, 100) {
		events = append(events, sseEvent{"content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": 3,
			"delta": map[string]interface{}{"type": "text_delta", "text": chunk},
		}})
	}
	events = append(events, sseEvent{"content_block_stop", map[string]interface{}{
		"type": "content_block_stop", "index": 3,
	}})

	// 6. message_delta - no stop_sequence; reports web_search_requests
	outputTokens := (len([]rune(summary)) + 3) / 4
	events = append(events, sseEvent{"message_delta", map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": "end_turn"},
		"usage": map[string]interface{}{
			"output_tokens":   outputTokens,
			"server_tool_use": map[string]interface{}{"web_search_requests": 1},
		},
	}})

	// 7. message_stop
	events = append(events, sseEvent{"message_stop", map[string]interface{}{
		"type": "message_stop",
	}})

	return events
}

// chunkByRunes splits s into chunks of at most n runes (UTF-8 safe).
func chunkByRunes(s string, n int) []string {
	if n <= 0 {
		return []string{s}
	}
	runes := []rune(s)
	if len(runes) == 0 {
		return nil
	}
	var chunks []string
	for i := 0; i < len(runes); i += n {
		end := i + n
		if end > len(runes) {
			end = len(runes)
		}
		chunks = append(chunks, string(runes[i:end]))
	}
	return chunks
}

// ==================== MCP call ====================

// mcpURLForEndpoint derives the /mcp URL from a generateAssistantResponse URL
// on the same host (e.g. https://q.us-east-1.amazonaws.com/mcp).
func mcpURLForEndpoint(apiURL string) string {
	if u, err := url.Parse(apiURL); err == nil && u.Host != "" {
		return u.Scheme + "://" + u.Host + "/mcp"
	}
	return "https://q.us-east-1.amazonaws.com/mcp"
}

// callKiroMCP sends a JSON-RPC MCP request for the given account and returns the
// raw response body. Non-200 responses are returned as a KiroAPIError.
func callKiroMCP(ctx context.Context, account *config.Account, requestBody []byte) ([]byte, error) {
	mcpURL := mcpURLForEndpoint(kiroEndpoints[0].URL)

	req, err := http.NewRequestWithContext(ctx, "POST", mcpURL, bytes.NewReader(requestBody))
	if err != nil {
		return nil, err
	}

	host := ""
	if u, perr := url.Parse(mcpURL); perr == nil {
		host = u.Host
	}
	headerValues := buildStreamingHeaderValues(account, host)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	applyKiroBaseHeaders(req, account, headerValues)
	req.Header.Set("Amz-Sdk-Request", "attempt=1; max=3")
	req.Header.Set("Amz-Sdk-Invocation-Id", uuid.New().String())
	if account != nil {
		if profileArn := strings.TrimSpace(account.ProfileArn); profileArn != "" {
			req.Header.Set("x-amzn-kiro-profile-arn", profileArn)
		}
	}

	resp, err := GetClientForProxy(ResolveAccountProxyURL(account)).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, &KiroAPIError{StatusCode: resp.StatusCode, Endpoint: "MCP", Body: strings.TrimSpace(string(body))}
	}
	return body, nil
}

// fetchWebSearchResults acquires an account, ensures its token, and performs the
// MCP search call. It returns:
//   - err != nil: could not perform the search at all (no account / routing limit);
//     the caller should return an HTTP error.
//   - failed == true (err == nil): an account was obtained but the MCP call or
//     result parsing failed; the caller degrades gracefully (emits a well-formed
//     "No results found" response) but records the request as a failure so the
//     upstream fault is visible in metrics rather than being masked as a 200.
//   - failed == false, err == nil: genuine success (results may be empty).
func (h *Handler) fetchWebSearchResults(ctx context.Context, query, apiKeyID, model string) (results *webSearchResults, account *config.Account, failed bool, err error) {
	body, buildErr := buildMCPRequestBody(newMCPRequestID(), query)
	if buildErr != nil {
		return nil, nil, false, buildErr
	}

	// Account Routing owns the acquire/token/release/failover/backoff loop. The
	// callback performs one MCP search against the routed Account and classifies
	// the outcome. web_search forwards apiKeyID as the affinity key, as before.
	var okResults *webSearchResults
	var okAccount *config.Account
	outcome := h.runWithAccount(ctx, model, apiKeyID, func(acct *config.Account) attemptResult {
		respBody, callErr := callKiroMCP(ctx, acct, body)
		if callErr != nil {
			if IsKiroRetryableAccountError(callErr) {
				// Account-attributable and retryable: penalise + fail over.
				return attemptRetry(callErr)
			}
			// Non-retryable upstream fault: still the Account's fault (matches the
			// prior handleAccountFailure call), but we cannot fail over — stop and
			// let the caller degrade gracefully.
			logger.Warnf("[WebSearch] MCP call failed (non-retryable): %v", callErr)
			return attemptStop(callErr, true)
		}

		parsed, parseErr := parseMCPSearchResults(respBody)
		if parseErr != nil {
			// Parsing is caller-local work: a parse failure is NOT the Account's
			// fault, so it must not penalise the Account. Stop and degrade.
			logger.Warnf("[WebSearch] failed to parse MCP results: %v", parseErr)
			return attemptStop(parseErr, false)
		}
		okResults = parsed
		okAccount = acct
		return attemptSuccess()
	})

	switch outcome.stopReason {
	case routeStopSuccess:
		return okResults, okAccount, false, nil
	case routeStopRoutingLimit, routeStopUnavailable:
		// Could not perform the search at all (queue full/timeout, or empty pool
		// with no attempt made): surface the acquire error so the caller renders
		// 429 (routing-limit) or 503 (no accounts).
		return nil, outcome.lastAccount, false, outcome.acquireErr
	default:
		// caller-terminal (non-retryable MCP / parse error), exhausted retries, or
		// a cancelled request: degrade gracefully but mark as failed for metrics.
		return nil, outcome.lastAccount, true, nil
	}
}

// ==================== Handler entry ====================

// handleClaudeWebSearch handles a pure web_search request: it performs the MCP
// search and emits the Anthropic server-tool response (streaming or not).
func (h *Handler) handleClaudeWebSearch(ctx context.Context, w http.ResponseWriter, req *ClaudeRequest, model string, estimatedInputTokens int, apiKeyID string) {
	requestStartedAt := time.Now()

	query, ok := extractWebSearchQuery(req)
	if !ok {
		recordRequestMetrics("claude", model, req.Stream, nil, apiKeyID, false, http.StatusBadRequest, "invalid_request_error", estimatedInputTokens, 0, 0, requestStartedAt)
		h.sendClaudeError(w, 400, "invalid_request_error", "Unable to extract search query from messages")
		return
	}

	results, account, failed, err := h.fetchWebSearchResults(ctx, query, apiKeyID, model)
	if err != nil {
		if isRoutingLimitError(err) {
			h.recordFailure()
			statusCode, errType := metricsErrorDetails(err, http.StatusTooManyRequests, "rate_limit_error")
			recordRequestMetrics("claude", model, req.Stream, nil, apiKeyID, false, statusCode, errType, estimatedInputTokens, 0, 0, requestStartedAt)
			h.sendClaudeError(w, 429, "rate_limit_error", routingErrorMessage(err))
			return
		}
		h.recordFailure()
		statusCode, errType := metricsErrorDetails(err, http.StatusServiceUnavailable, "no_available_accounts")
		recordRequestMetrics("claude", model, req.Stream, nil, apiKeyID, false, statusCode, errType, estimatedInputTokens, 0, 0, requestStartedAt)
		h.sendClaudeError(w, 503, "api_error", "No available accounts")
		return
	}

	toolUseID := newServerToolUseID()
	events := buildWebSearchEvents(model, query, toolUseID, results, estimatedInputTokens)

	// Estimate output tokens for metrics from the last message_delta.
	outputTokens := 0
	if len(events) >= 2 {
		if usage, ok := events[len(events)-2].Data["usage"].(map[string]interface{}); ok {
			if ot, ok := usage["output_tokens"].(int); ok {
				outputTokens = ot
			}
		}
	}

	// The MCP search failed but we still emit a well-formed (empty) response to
	// the client. Record it as a failure so the upstream fault is visible in
	// metrics instead of being masked as a successful 200.
	if failed {
		h.recordFailure()
		recordRequestMetrics("claude", model, req.Stream, account, apiKeyID, false, http.StatusBadGateway, "web_search_upstream_error", estimatedInputTokens, outputTokens, 0, requestStartedAt)
	} else {
		h.recordSuccessForApiKey(apiKeyID, estimatedInputTokens, outputTokens, 0)
		recordRequestMetrics("claude", model, req.Stream, account, apiKeyID, true, http.StatusOK, "", estimatedInputTokens, outputTokens, 0, requestStartedAt)
		if account != nil {
			h.pool.RecordSuccess(account.ID)
		}
	}

	if req.Stream {
		h.writeWebSearchStream(w, events)
		return
	}
	h.writeWebSearchJSON(w, model, query, toolUseID, results, estimatedInputTokens, outputTokens)
}

func (h *Handler) writeWebSearchStream(w http.ResponseWriter, events []sseEvent) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendClaudeError(w, 500, "api_error", "Streaming not supported")
		return
	}
	for _, e := range events {
		h.sendSSE(w, flusher, e.Event, e.Data)
	}
}

// writeWebSearchJSON assembles a non-streaming Claude message containing the
// same content blocks (text + server_tool_use + web_search_tool_result + text).
func (h *Handler) writeWebSearchJSON(w http.ResponseWriter, model, query, toolUseID string, results *webSearchResults, inputTokens, outputTokens int) {
	searchContent := make([]interface{}, 0)
	if results != nil {
		for _, r := range results.Results {
			encrypted := ""
			if r.Snippet != nil {
				encrypted = *r.Snippet
			}
			searchContent = append(searchContent, map[string]interface{}{
				"type":              "web_search_result",
				"title":             r.Title,
				"url":               r.URL,
				"encrypted_content": encrypted,
				"page_age":          pageAgeFromMillis(r.PublishedDate),
			})
		}
	}

	content := []interface{}{
		map[string]interface{}{"type": "text", "text": fmt.Sprintf("I'll search for \"%s\".", query)},
		map[string]interface{}{"id": toolUseID, "type": "server_tool_use", "name": "web_search", "input": map[string]interface{}{"query": query}},
		map[string]interface{}{"type": "web_search_tool_result", "tool_use_id": toolUseID, "content": searchContent},
		map[string]interface{}{"type": "text", "text": generateWebSearchSummary(query, results)},
	}

	resp := map[string]interface{}{
		"id":            "msg_" + strings.ReplaceAll(uuid.New().String(), "-", "")[:24],
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage": map[string]interface{}{
			"input_tokens":    inputTokens,
			"output_tokens":   outputTokens,
			"server_tool_use": map[string]interface{}{"web_search_requests": 1},
		},
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(resp)
}
