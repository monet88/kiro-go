// Package proxy is the core proxy layer for the Kiro API.
// It handles streaming API calls to the Kiro backend and parses AWS Event Stream responses.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// Endpoint configuration (auto-fallback on quota exhaustion).
type kiroEndpoint struct {
	URL       string
	Origin    string
	AmzTarget string
	Name      string
}

var kiroEndpoints = []kiroEndpoint{
	{
		URL:       "https://q.us-east-1.amazonaws.com/generateAssistantResponse",
		Origin:    "AI_EDITOR",
		AmzTarget: "",
		Name:      "Kiro IDE",
	},
	{
		URL:       "https://codewhisperer.us-east-1.amazonaws.com/generateAssistantResponse",
		Origin:    "AI_EDITOR",
		AmzTarget: "AmazonCodeWhispererStreamingService.GenerateAssistantResponse",
		Name:      "CodeWhisperer",
	},
	{
		URL:       "https://q.us-east-1.amazonaws.com/generateAssistantResponse",
		Origin:    "AI_EDITOR",
		AmzTarget: "AmazonQDeveloperStreamingService.SendMessage",
		Name:      "AmazonQ",
	},
}

// Global HTTP clients, swappable at runtime to apply proxy reconfiguration without restart.
var kiroHttpStore atomic.Pointer[http.Client]
var kiroRestHttpStore atomic.Pointer[http.Client]

// proxyClientCache caches http.Client instances keyed by proxy URL for per-account proxy support.
var proxyClientCache sync.Map

func init() {
	InitKiroHttpClient("")
}

// GetClientForProxy returns an http.Client configured for the given proxy URL.
// If proxyURL is empty, returns the global kiro HTTP client.
func GetClientForProxy(proxyURL string) *http.Client {
	if proxyURL == "" {
		return kiroHttpStore.Load()
	}
	if cached, ok := proxyClientCache.Load(proxyURL); ok {
		return cached.(*http.Client)
	}
	client := &http.Client{
		Timeout:   5 * time.Minute,
		Transport: buildKiroTransport(proxyURL),
	}
	proxyClientCache.Store(proxyURL, client)
	return client
}

// GetRestClientForProxy returns a rest http.Client (30s timeout) for the given proxy URL.
// If proxyURL is empty, returns the global kiro REST HTTP client.
func GetRestClientForProxy(proxyURL string) *http.Client {
	if proxyURL == "" {
		return kiroRestHttpStore.Load()
	}
	cacheKey := "rest:" + proxyURL
	if cached, ok := proxyClientCache.Load(cacheKey); ok {
		return cached.(*http.Client)
	}
	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: buildKiroTransport(proxyURL),
	}
	proxyClientCache.Store(cacheKey, client)
	return client
}

// ResolveAccountProxyURL returns the effective proxy URL for an account.
// Falls back to global config.GetProxyURL() if the account has no per-account proxy.
func ResolveAccountProxyURL(account *config.Account) string {
	if account != nil && account.ProxyURL != "" {
		return account.ProxyURL
	}
	return config.GetProxyURL()
}

// buildKiroTransport constructs an HTTP Transport with optional outbound proxy support.
func buildKiroTransport(proxyURL string) *http.Transport {
	t := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  false,
		ForceAttemptHTTP2:   true,
	}
	if proxyURL != "" {
		if u, err := url.Parse(proxyURL); err == nil {
			t.Proxy = http.ProxyURL(u)
			// Proxied connections cannot negotiate HTTP/2.
			t.ForceAttemptHTTP2 = false
		}
	} else {
		t.Proxy = http.ProxyFromEnvironment
	}
	return t
}

// InitKiroHttpClient initializes (or reinitializes) the HTTP clients used for Kiro API requests.
func InitKiroHttpClient(proxyURL string) {
	client := &http.Client{
		Timeout:   5 * time.Minute,
		Transport: buildKiroTransport(proxyURL),
	}
	kiroHttpStore.Store(client)

	restClient := &http.Client{
		Timeout:   30 * time.Second,
		Transport: buildKiroTransport(proxyURL),
	}
	kiroRestHttpStore.Store(restClient)
}

// ==================== Request Structs ====================

// KiroPayload is the top-level request body sent to the Kiro API.
type KiroPayload struct {
	ConversationState struct {
		AgentContinuationId string `json:"agentContinuationId,omitempty"`
		AgentTaskType       string `json:"agentTaskType,omitempty"`
		ChatTriggerType     string `json:"chatTriggerType"`
		ConversationID      string `json:"conversationId"`
		CurrentMessage      struct {
			UserInputMessage KiroUserInputMessage `json:"userInputMessage"`
		} `json:"currentMessage"`
		History []KiroHistoryMessage `json:"history,omitempty"`
	} `json:"conversationState"`
	ProfileArn      string           `json:"profileArn,omitempty"`
	InferenceConfig *InferenceConfig `json:"inferenceConfig,omitempty"`

	// ToolNameMap maps sanitized tool names (sent to Kiro) back to the
	// original names supplied by the client. Used to restore original names
	// in tool_use responses so the client can match them to its tool registry.
	// Not serialized to the Kiro API request body.
	ToolNameMap map[string]string `json:"-"`

	// ToolSchemas maps names sent to Kiro back to the original client schema.
	// It lets the proxy coerce Kiro-generated tool inputs before returning them
	// to strict clients such as Claude Code.
	// Not serialized to the Kiro API request body.
	ToolSchemas map[string]interface{} `json:"-"`

	// RoutingAffinityKey is a stable per-conversation fingerprint used to pin
	// all turns of the same conversation to the same upstream account, so the
	// account's prompt cache can be reused across turns. Empty when the request
	// has no stable conversation anchor (single-shot / synthetic), in which case
	// routing falls back to normal load balancing. Not serialized to Kiro.
	RoutingAffinityKey string `json:"-"`
}

type KiroUserInputMessage struct {
	Content                 string                   `json:"content"`
	ModelID                 string                   `json:"modelId,omitempty"`
	Origin                  string                   `json:"origin"`
	Images                  []KiroImage              `json:"images,omitempty"`
	UserInputMessageContext *UserInputMessageContext `json:"userInputMessageContext,omitempty"`
}

type UserInputMessageContext struct {
	Tools       []KiroToolWrapper `json:"tools,omitempty"`
	ToolResults []KiroToolResult  `json:"toolResults,omitempty"`
}

type KiroToolWrapper struct {
	ToolSpecification struct {
		Name        string      `json:"name"`
		Description string      `json:"description"`
		InputSchema InputSchema `json:"inputSchema"`
	} `json:"toolSpecification"`
}

type InputSchema struct {
	JSON interface{} `json:"json"`
}

type KiroToolResult struct {
	ToolUseID string              `json:"toolUseId"`
	Content   []KiroResultContent `json:"content"`
	Status    string              `json:"status"`
}

type KiroResultContent struct {
	Text string `json:"text"`
}

type KiroImage struct {
	Format string `json:"format"`
	Source struct {
		Bytes string `json:"bytes"`
	} `json:"source"`
}

type KiroHistoryMessage struct {
	UserInputMessage         *KiroUserInputMessage         `json:"userInputMessage,omitempty"`
	AssistantResponseMessage *KiroAssistantResponseMessage `json:"assistantResponseMessage,omitempty"`
}

type KiroAssistantResponseMessage struct {
	Content  string        `json:"content"`
	ToolUses []KiroToolUse `json:"toolUses,omitempty"`
}

type KiroToolUse struct {
	ToolUseID string                 `json:"toolUseId"`
	Name      string                 `json:"name"`
	Input     map[string]interface{} `json:"input"`
}

type InferenceConfig struct {
	MaxTokens   int      `json:"maxTokens,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        float64  `json:"topP,omitempty"`
}

// ==================== Stream Callbacks ====================

// KiroStreamCallback stream response callbacks
type KiroStreamCallback struct {
	OnText         func(text string, isThinking bool)
	OnToolUse      func(toolUse KiroToolUse)
	OnComplete     func(inputTokens, outputTokens int)
	OnError        func(err error)
	OnCredits      func(credits float64)
	OnContextUsage func(percentage float64)
}

type KiroAPIError struct {
	StatusCode int
	Endpoint   string
	Body       string
}

func (e *KiroAPIError) Error() string {
	if e == nil {
		return ""
	}
	if strings.TrimSpace(e.Body) != "" {
		return fmt.Sprintf("HTTP %d from %s: %s", e.StatusCode, e.Endpoint, e.Body)
	}
	return fmt.Sprintf("HTTP %d from %s", e.StatusCode, e.Endpoint)
}

func IsKiroRateLimitError(err error) bool {
	if apiErr, ok := err.(*KiroAPIError); ok && apiErr.StatusCode == http.StatusTooManyRequests {
		return true
	}
	if err == nil {
		return false
	}
	errMsg := strings.ToLower(err.Error())
	return strings.Contains(errMsg, "http 429") || strings.Contains(errMsg, "too many requests") || strings.Contains(errMsg, "quota") || strings.Contains(errMsg, "temporary limits") || strings.Contains(errMsg, "suspicious activity")
}

func IsKiroRetryableAccountError(err error) bool {
	if apiErr, ok := err.(*KiroAPIError); ok {
		return apiErr.StatusCode == http.StatusTooManyRequests || apiErr.StatusCode >= 500
	}
	if err == nil {
		return false
	}
	errMsg := strings.ToLower(err.Error())
	return strings.Contains(errMsg, "http 429") || strings.Contains(errMsg, "http 500") || strings.Contains(errMsg, "http 502") || strings.Contains(errMsg, "http 503") || strings.Contains(errMsg, "http 504") || strings.Contains(errMsg, "temporary limits") || strings.Contains(errMsg, "suspicious activity")
}

// ==================== API Call ====================

func setPayloadProfileArnForAccount(payload *KiroPayload, account *config.Account) {
	if payload == nil {
		return
	}

	payload.ProfileArn = strings.TrimSpace(payload.ProfileArn)
	if account != nil {
		if profileArn := strings.TrimSpace(account.ProfileArn); profileArn != "" {
			payload.ProfileArn = profileArn
		}
	}
}

// getSortedEndpoints returns endpoints ordered by user preference, with optional fallback.
func getSortedEndpoints(preferred string) []kiroEndpoint {
	fallback := config.GetEndpointFallback()

	var primary int
	switch preferred {
	case "kiro":
		primary = 0
	case "codewhisperer":
		primary = 1
	case "amazonq":
		primary = 2
	default:
		primary = 0 // "auto": Kiro endpoint remains the primary choice
	}

	if !fallback {
		// No fallback: only use the selected endpoint (or Kiro primary when preferred=auto)
		return []kiroEndpoint{kiroEndpoints[primary]}
	}

	// With fallback: selected first, then others in order
	result := []kiroEndpoint{kiroEndpoints[primary]}
	for i, ep := range kiroEndpoints {
		if i != primary {
			result = append(result, ep)
		}
	}
	return result
}

// CallKiroAPI calls the Kiro streaming API, trying each configured endpoint with automatic fallback.
func CallKiroAPI(ctx context.Context, account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	if ctx == nil {
		ctx = context.Background()
	}
	originalProfileArn := ""
	if payload != nil {
		originalProfileArn = payload.ProfileArn
		defer func() {
			payload.ProfileArn = originalProfileArn
		}()
	}
	setPayloadProfileArnForAccount(payload, account)

	// Debug: dump full payload for troubleshooting upstream rejections.
	// Guard on the level so we don't marshal the (potentially large) payload
	// when debug logging is off. The per-endpoint marshal below also surfaces
	// any marshal error, so we no longer pre-marshal just to validate.
	if logger.GetLevel() <= logger.LevelDebug {
		if payloadJSON, err := json.Marshal(payload); err == nil {
			logger.Debugf("[KiroAPI] Request payload: %s", string(payloadJSON))
		}
	}

	// Wrap OnToolUse to normalize Kiro-generated tool inputs and restore
	// original tool names for strict clients such as Claude Code.
	if callback != nil && callback.OnToolUse != nil && payload != nil && (len(payload.ToolNameMap) > 0 || len(payload.ToolSchemas) > 0) {
		originalOnToolUse := callback.OnToolUse
		nameMap := payload.ToolNameMap
		schemaMap := payload.ToolSchemas
		wrapped := *callback
		wrapped.OnToolUse = func(tu KiroToolUse) {
			if schema, ok := schemaMap[tu.Name]; ok {
				tu.Input = sanitizeToolInput(tu.Input, schema)
			} else {
				tu.Input = normalizeCommonToolInputScalars(tu.Input)
			}
			if original, ok := nameMap[tu.Name]; ok {
				tu.Name = original
			}
			originalOnToolUse(tu)
		}
		callback = &wrapped
	}

	if payload != nil && strings.TrimSpace(payload.ProfileArn) == "" {
		if profileArn, err := ResolveProfileArn(account); err == nil {
			payload.ProfileArn = profileArn
		} else if isProfileArnResolutionSoftError(err) {
			logger.Debugf("[ProfileArn] Skipped profile ARN resolution for %s: %v", accountEmailForLog(account), err)
		} else {
			logger.Warnf("[ProfileArn] Failed to resolve profile ARN for %s: %v", accountEmailForLog(account), err)
		}
	}

	// Build endpoint list ordered by configuration.
	endpoints := getSortedEndpoints(config.GetPreferredEndpoint())

	var lastErr error
	for _, ep := range endpoints {
		// Update the origin field for the selected endpoint.
		payload.ConversationState.CurrentMessage.UserInputMessage.Origin = ep.Origin

		// Target the profile's data-plane region; endpoint URLs are declared for us-east-1.
		epURL := regionalizeURLForProfile(ep.URL, account, payload.ProfileArn)

		reqBody, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, "POST", epURL, bytes.NewReader(reqBody))
		if err != nil {
			lastErr = err
			continue
		}

		host := ""
		if parsedURL, parseErr := url.Parse(epURL); parseErr == nil {
			host = parsedURL.Host
		}
		headerValues := buildStreamingHeaderValues(account, host)

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "*/*")
		if ep.AmzTarget != "" {
			req.Header.Set("X-Amz-Target", ep.AmzTarget)
		}
		applyKiroBaseHeaders(req, account, headerValues)
		req.Header.Set("x-amzn-kiro-agent-mode", "vibe")
		req.Header.Set("x-amzn-codewhisperer-optout", "true")
		req.Header.Set("Amz-Sdk-Request", "attempt=1; max=3")
		req.Header.Set("Amz-Sdk-Invocation-Id", uuid.New().String())

		resp, err := GetClientForProxy(ResolveAccountProxyURL(account)).Do(req)
		if err != nil {
			lastErr = err
			logger.Warnf("[KiroAPI] Endpoint %s failed: %v", ep.Name, err)
			// Skip client-initiated cancellation/timeout: that is not an upstream
			// fault and would otherwise flood the probe store with client noise.
			if !isClientDisconnectError(ctx, err) {
				recordUpstreamErrorProbe(ep.Name, "connect", 0, currentMessageModelID(payload), account, err.Error())
			}
			continue
		}

		if resp.StatusCode == 429 {
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			body := strings.TrimSpace(string(errBody))
			lastErr = &KiroAPIError{StatusCode: resp.StatusCode, Endpoint: ep.Name, Body: body}
			entry := recordKiro429ProbeLog(ep.Name, account, body)
			logger.Warnf("[Kiro429Probe] endpoint=%q class=%q accountID=%q email=%q tier=%q bodyBytes=%d retainedFor=%s", entry.Endpoint, entry.Class, entry.AccountID, entry.Email, entry.Tier, len(body), kiro429ProbeTTL)
			logger.Warnf("[KiroAPI] Endpoint %s quota exhausted (429), trying next endpoint", ep.Name)
			continue
		}

		if resp.StatusCode != 200 {
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			body := strings.TrimSpace(string(errBody))
			lastErr = &KiroAPIError{StatusCode: resp.StatusCode, Endpoint: ep.Name, Body: body}
			recordUpstreamErrorProbe(ep.Name, "response", resp.StatusCode, currentMessageModelID(payload), account, body)
			// Authentication errors and payment errors are not retried across endpoints.
			if resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 402 {
				return lastErr
			}
			logger.Warnf("[KiroAPI] Endpoint %s error: %v", ep.Name, lastErr)
			continue
		}

		err = parseEventStream(ctx, resp.Body, callback)
		resp.Body.Close()
		// A mid-stream failure (response began 200 then the event stream broke)
		// is otherwise invisible: it surfaces to clients only as a generic
		// "api_error" with no upstream detail. Persist it, but skip client
		// disconnects so the probe store reflects upstream faults only.
		if err != nil && !isClientDisconnectError(ctx, err) {
			recordUpstreamErrorProbe(ep.Name, "stream", 200, currentMessageModelID(payload), account, err.Error())
		}
		return err
	}

	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("all endpoints failed")
}

func accountEmailForLog(account *config.Account) string {
	if account == nil {
		return "<nil>"
	}
	return account.Email
}

// ==================== Event Stream Parsing ====================

// ==================== Tool Use Handling ====================
