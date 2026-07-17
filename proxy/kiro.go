// Package proxy is the core proxy layer for the Kiro API.
// It handles streaming API calls to the Kiro backend and parses AWS Event Stream responses.
package proxy

import (
	"fmt"
	"kiro-go/config"
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
// Per-account streaming clients also use Timeout=0 so long event streams are
// not cut by a fixed wall clock (see InitKiroHttpClient).
func GetClientForProxy(proxyURL string) *http.Client {
	if proxyURL == "" {
		return kiroHttpStore.Load()
	}
	if cached, ok := proxyClientCache.Load(proxyURL); ok {
		return cached.(*http.Client)
	}
	client := &http.Client{
		Timeout:   0,
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
// ResponseHeaderTimeout bounds time-to-first-byte only; body streaming is unlimited
// because generateAssistantResponse event streams can run far longer than a fixed
// client Timeout (multi-tool agent turns regularly exceed 5 minutes).
func buildKiroTransport(proxyURL string) *http.Transport {
	t := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		// Upstream binary event-stream bodies are not compressed; leave the default
		// Accept-Encoding negotiation in place for management REST calls that share
		// the same transport builder via InitKiroHttpClient rest client.
		DisableCompression: false,
		ForceAttemptHTTP2:  true,
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
	// Streaming client: Timeout must be 0. A non-zero Client.Timeout covers the
	// entire response body and aborts long generateAssistantResponse streams
	// mid-flight (observed as unexpected EOF after tens of events). Header wait
	// is bounded by ResponseHeaderTimeout on the transport instead.
	client := &http.Client{
		Timeout:   0,
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

func accountEmailForLog(account *config.Account) string {
	if account == nil {
		return "<nil>"
	}
	return account.Email
}

// setKiroStreamingRequestHeaders applies the shared generateAssistantResponse
// header set used by both the primary attempt and the profile-less 403 retry.
// OAuth/CodeWhisperer paths include x-amzn-kiro-agent-mode: vibe; the API-key
// kiro.dev path deliberately omits it (parity with the working fork).
func setKiroStreamingRequestHeaders(req *http.Request, account *config.Account, amzTarget, epURL string) {
	if req == nil {
		return
	}
	host := ""
	if parsedURL, parseErr := url.Parse(epURL); parseErr == nil {
		host = parsedURL.Host
	} else if req.URL != nil {
		host = req.URL.Host
	}
	headerValues := buildStreamingHeaderValues(account, host)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	if amzTarget != "" {
		req.Header.Set("X-Amz-Target", amzTarget)
	}
	applyKiroBaseHeaders(req, account, headerValues)
	// Prefer KiroApiKey as the bearer for API-key Accounts when dual-write is stale.
	if account != nil && account.IsApiKeyCredential() {
		if bearer := apiKeyBearer(account); bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
	}
	req.Header.Set("x-amzn-kiro-agent-mode", "vibe")
	req.Header.Set("x-amzn-codewhisperer-optout", "true")
	req.Header.Set("Amz-Sdk-Request", "attempt=1; max=3")
	req.Header.Set("Amz-Sdk-Invocation-Id", uuid.New().String())
}

// setKiroDevAPIKeyStreamingHeaders applies the leaner header set used by static
// ksk_… generateAssistantResponse calls against runtime.{region}.kiro.dev.
// Matches the working fork: base auth headers + SDK attempt headers, without
// x-amzn-kiro-agent-mode (that header is for OAuth IDE endpoints only).
func setKiroDevAPIKeyStreamingHeaders(req *http.Request, account *config.Account, epURL string) {
	if req == nil {
		return
	}
	host := ""
	if parsedURL, parseErr := url.Parse(epURL); parseErr == nil {
		host = parsedURL.Host
	} else if req.URL != nil {
		host = req.URL.Host
	}
	headerValues := buildStreamingHeaderValues(account, host)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	applyKiroBaseHeaders(req, account, headerValues)
	if bearer := apiKeyBearer(account); bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("Amz-Sdk-Request", "attempt=1; max=3")
	req.Header.Set("Amz-Sdk-Invocation-Id", uuid.New().String())
}

// ==================== Event Stream Parsing ====================

// ==================== Tool Use Handling ====================
