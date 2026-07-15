package config

// Account represents a Kiro API account with authentication credentials and usage statistics.
type Account struct {
	// Basic identification
	ID       string `json:"id"`                 // Unique account identifier (UUID)
	Email    string `json:"email,omitempty"`    // User email address
	UserId   string `json:"userId,omitempty"`   // Kiro user ID
	Nickname string `json:"nickname,omitempty"` // Display name for admin panel

	// Authentication credentials
	AccessToken  string `json:"accessToken"`            // OAuth access token for API calls
	RefreshToken string `json:"refreshToken"`           // OAuth refresh token for token renewal
	ClientID     string `json:"clientId,omitempty"`     // OIDC client ID (for IdC auth)
	ClientSecret string `json:"clientSecret,omitempty"` // OIDC client secret (for IdC auth)
	AuthMethod   string `json:"authMethod"`             // Authentication method: "idc" (AWS IdC), "social" (GitHub/Google), "external_idp" (enterprise SSO, e.g. Azure AD), or "api_key" (static Kiro API Key)

	// KiroApiKey is the source of truth for an API-key Account (AuthMethod=api_key).
	// It holds a static Kiro API Key bearer (ksk_…) and is dual-written into
	// AccessToken on every write path so existing bearer call sites keep working
	// (ADR-0002). API-key Accounts skip OAuth refresh and profile-ARN resolution.
	// This is NOT a Gateway API Key (client→proxy sk-…, see config.ApiKeys).
	KiroApiKey string `json:"kiroApiKey,omitempty"`
	Provider   string `json:"provider,omitempty"`   // Identity provider name (e.g., "BuilderId", "GitHub", "AzureAD")
	Region     string `json:"region"`               // AWS region for OIDC endpoints
	StartUrl   string `json:"startUrl,omitempty"`   // AWS SSO start URL
	ExpiresAt  int64  `json:"expiresAt,omitempty"`  // Token expiration timestamp (Unix seconds)
	MachineId  string `json:"machineId,omitempty"`  // UUID machine identifier for request tracking
	ProfileArn string `json:"profileArn,omitempty"` // CodeWhisperer/Kiro profile ARN for generation requests

	// External IdP (enterprise SSO, e.g. Microsoft 365 / Entra ID / Azure AD) refresh material.
	// When AuthMethod == "external_idp" the credential is an IdP-issued OAuth token refreshed
	// against TokenEndpoint using ClientID and Scopes (refresh_token grant), NOT the AWS SSO
	// OIDC endpoint. IssuerURL is the OIDC issuer the endpoints were discovered from.
	TokenEndpoint string `json:"tokenEndpoint,omitempty"` // External IdP OAuth2 token endpoint (refresh)
	IssuerURL     string `json:"issuerUrl,omitempty"`     // External IdP OIDC issuer URL
	Scopes        string `json:"scopes,omitempty"`        // Space-separated scopes granted by the external IdP

	// Per-account outbound proxy (falls back to global ProxyURL if empty)
	ProxyURL string `json:"proxyURL,omitempty"`

	// Priority weight for load balancing (higher = more requests)
	Weight int `json:"weight,omitempty"` // 0 or 1 = normal, 2+ = higher priority

	// Upstream Overages state (mirrored from AWS Q `setUserPreference` / `getUsageLimits`).
	// OverageStatus is the upstream switch state; usageCurrent > usageLimit is also treated as effective overage.
	// Allowed values: "ENABLED", "DISABLED", "UNKNOWN" (or empty when not yet fetched).
	OverageStatus     string  `json:"overageStatus,omitempty"`
	OverageCapability string  `json:"overageCapability,omitempty"` // "OVERAGE_CAPABLE" / "NOT_OVERAGE_CAPABLE"
	OverageCap        float64 `json:"overageCap,omitempty"`        // Hard upper bound (points)
	OverageRate       float64 `json:"overageRate,omitempty"`       // Per-invocation points
	CurrentOverages   float64 `json:"currentOverages,omitempty"`   // Cumulative overage points
	OverageCheckedAt  int64   `json:"overageCheckedAt,omitempty"`  // Last successful upstream sync (Unix seconds)

	// LegacyAllowOverage is kept for backward-compatible JSON loading only.
	// Pre-Overages-switch deployments persisted `allowOverage: true` to mean
	// "keep dispatching when quota is exhausted". On first load we migrate it
	// into OverageStatus="ENABLED" and zero this field so it does not get
	// re-emitted on future saves. Do not read this field elsewhere.
	LegacyAllowOverage bool `json:"allowOverage,omitempty"`

	// Account status
	Enabled   bool   `json:"enabled"`             // Whether account is active in the pool
	BanStatus string `json:"banStatus,omitempty"` // Ban status: "ACTIVE", "BANNED", "SUSPENDED"
	BanReason string `json:"banReason,omitempty"` // Reason for ban/suspension
	BanTime   int64  `json:"banTime,omitempty"`   // Timestamp when ban was detected

	// Subscription information
	SubscriptionType  string `json:"subscriptionType,omitempty"`  // Tier: FREE, PRO, PRO_PLUS, or POWER
	SubscriptionTitle string `json:"subscriptionTitle,omitempty"` // Human-readable subscription name
	DaysRemaining     int    `json:"daysRemaining,omitempty"`     // Days until subscription expires

	// Usage tracking
	UsageCurrent  float64 `json:"usageCurrent,omitempty"`  // Current period usage (credits)
	UsageLimit    float64 `json:"usageLimit,omitempty"`    // Maximum allowed usage per period
	UsagePercent  float64 `json:"usagePercent,omitempty"`  // Usage percentage (0.0-1.0)
	NextResetDate string  `json:"nextResetDate,omitempty"` // Date when usage resets (YYYY-MM-DD)
	LastRefresh   int64   `json:"lastRefresh,omitempty"`   // Last info refresh timestamp

	// Trial usage tracking
	TrialUsageCurrent float64 `json:"trialUsageCurrent,omitempty"` // Trial quota current usage
	TrialUsageLimit   float64 `json:"trialUsageLimit,omitempty"`   // Trial quota total limit
	TrialUsagePercent float64 `json:"trialUsagePercent,omitempty"` // Trial quota usage percentage (0.0-1.0)
	TrialStatus       string  `json:"trialStatus,omitempty"`       // Trial status: ACTIVE, EXPIRED, NONE
	TrialExpiresAt    int64   `json:"trialExpiresAt,omitempty"`    // Trial expiration timestamp (Unix seconds)

	// Runtime statistics (updated during operation)
	RequestCount int     `json:"requestCount,omitempty"` // Total requests processed
	ErrorCount   int     `json:"errorCount,omitempty"`   // Total errors encountered
	LastUsed     int64   `json:"lastUsed,omitempty"`     // Last request timestamp
	TotalTokens  int     `json:"totalTokens,omitempty"`  // Cumulative tokens processed
	TotalCredits float64 `json:"totalCredits,omitempty"` // Cumulative credits consumed
}

// PromptFilterRule defines a single custom prompt sanitization rule.
// Type can be: "regex" (regexp find/replace within prompt) or
// "lines-containing" (remove lines containing the match substring).
type PromptFilterRule struct {
	ID      string `json:"id"`                // Unique rule identifier
	Name    string `json:"name"`              // Human-readable rule name
	Type    string `json:"type"`              // "regex" or "lines-containing"
	Match   string `json:"match"`             // Pattern to match (regex pattern or substring)
	Replace string `json:"replace,omitempty"` // Replacement string (only for regex; empty = delete match)
	Enabled bool   `json:"enabled"`           // Whether this rule is active
}

// ApiKeyEntry represents a single API key with optional usage limits and counters.
// Limits with value 0 are treated as "no limit". Counters are cumulative and never reset
// automatically; operators can use the admin endpoint to manually reset them.
type ApiKeyEntry struct {
	ID         string `json:"id"`                 // Unique identifier (UUID)
	Name       string `json:"name,omitempty"`     // Human-readable label
	Key        string `json:"key"`                // The actual key value clients send
	Enabled    bool   `json:"enabled"`            // Whether this key may authenticate
	Migrated   bool   `json:"migrated,omitempty"` // True if migrated from legacy single ApiKey field
	CreatedAt  int64  `json:"createdAt"`          // Creation timestamp (Unix seconds)
	LastUsedAt int64  `json:"lastUsedAt,omitempty"`

	// StreamOnly, when true, rejects non-streaming (synchronous) requests made
	// with this key. Synchronous requests buffer the entire upstream response in
	// memory (via strings.Builder) before replying, so under load they are the
	// dominant memory pressure; restricting selected keys to streaming-only keeps
	// per-request memory bounded. Streaming requests are unaffected.
	StreamOnly bool `json:"streamOnly,omitempty"`

	// Limits (0 = unlimited)
	TokenLimit  int64   `json:"tokenLimit,omitempty"`
	CreditLimit float64 `json:"creditLimit,omitempty"`

	// Cumulative usage (never auto-reset)
	TokensUsed    int64   `json:"tokensUsed,omitempty"`
	CreditsUsed   float64 `json:"creditsUsed,omitempty"`
	RequestsCount int64   `json:"requestsCount,omitempty"`
}

// RoutingConcurrencyConfig controls request routing concurrency, queueing,
// per-account concurrency and sticky-account overflow behavior.
type RoutingConcurrencyConfig struct {
	Enabled                 bool `json:"enabled"`
	GlobalMaxConcurrent     int  `json:"globalMaxConcurrent,omitempty"`
	GlobalQueueSize         int  `json:"globalQueueSize,omitempty"`
	GlobalQueueTimeoutMs    int  `json:"globalQueueTimeoutMs,omitempty"`
	PerAccountMaxConcurrent int  `json:"perAccountMaxConcurrent,omitempty"`
	PerAccountMinIntervalMs int  `json:"perAccountMinIntervalMs,omitempty"`
	StickyAccount           bool `json:"stickyAccount"`
	OverflowToOtherAccounts bool `json:"overflowToOtherAccounts"`

	// AccountRetryAttempts is the maximum number of accounts the proxy tries
	// before returning an error. Default 4.
	AccountRetryAttempts int `json:"accountRetryAttempts,omitempty"`

	// Transient429CooldownMs is the cooldown in milliseconds applied after a
	// transient (retryable) upstream 429. Default 5000 (5s).
	Transient429CooldownMs int `json:"transient429CooldownMs,omitempty"`
}

// Config represents the global application configuration.
type Config struct {
	// Server settings
	Password      string        `json:"password"`          // Admin panel password
	Port          int           `json:"port"`              // HTTP server port (default: 8089)
	Host          string        `json:"host"`              // HTTP server bind address (default: 0.0.0.0)
	ApiKey        string        `json:"apiKey,omitempty"`  // [Deprecated] Legacy single API key, migrated into ApiKeys on first load
	RequireApiKey bool          `json:"requireApiKey"`     // [Deprecated] Whether to enforce API key validation; with multi-key support, len(ApiKeys)>0 implicitly enforces auth
	ApiKeys       []ApiKeyEntry `json:"apiKeys,omitempty"` // Multiple API keys, each with independent quota
	KiroVersion   string        `json:"kiroVersion,omitempty"`
	SystemVersion string        `json:"systemVersion,omitempty"`
	NodeVersion   string        `json:"nodeVersion,omitempty"`
	Accounts      []Account     `json:"accounts"` // Registered Kiro accounts

	// ServerReadTimeoutSeconds controls the maximum duration for reading the
	// entire HTTP request (header + body). Default 120. Requires restart.
	ServerReadTimeoutSeconds int `json:"serverReadTimeoutSeconds,omitempty"`
	// ServerIdleTimeoutSeconds controls the maximum idle time between requests
	// on a keep-alive connection. Default 120. Requires restart.
	ServerIdleTimeoutSeconds int `json:"serverIdleTimeoutSeconds,omitempty"`

	// Thinking mode configuration for extended reasoning output
	ThinkingSuffix       string `json:"thinkingSuffix,omitempty"`       // Model suffix to trigger thinking mode (default: "-thinking")
	OpenAIThinkingFormat string `json:"openaiThinkingFormat,omitempty"` // OpenAI output format: "reasoning_content", "thinking", or "think"
	ClaudeThinkingFormat string `json:"claudeThinkingFormat,omitempty"` // Claude output format: "reasoning_content", "thinking", or "think"

	// Endpoint configuration: "auto", "kiro", "codewhisperer", or "amazonq"
	PreferredEndpoint string `json:"preferredEndpoint,omitempty"`

	// EndpointFallback controls whether to try other endpoints when the preferred one fails.
	// Defaults to true. Set to false to only use the preferred endpoint.
	EndpointFallback *bool `json:"endpointFallback,omitempty"`

	// AllowOverUsage allows accounts to continue serving requests even when their
	// usage quota has been exhausted. When enabled, the pool will not skip accounts
	// solely because usageCurrent >= usageLimit.
	AllowOverUsage bool `json:"allowOverUsage,omitempty"`

	// BalanceMode controls how the account pool picks the next account.
	// Supported values: "health", "managed", "aggressive".
	BalanceMode string `json:"balanceMode,omitempty"`

	// RoutingConcurrency controls sticky routing, per-account limits and global queueing.
	RoutingConcurrency RoutingConcurrencyConfig `json:"routingConcurrency,omitempty"`

	// PromptCacheMaxEntries bounds the in-memory Cross-account Prompt Cache LRU
	// (ADR-0001): the maximum number of distinct Cache Fingerprints held in
	// memory across all Accounts. 0 or negative selects the built-in default
	// (see GetPromptCacheMaxEntries); values below the minimum floor are raised
	// to the floor to avoid a pathologically tiny cache under misconfiguration.
	PromptCacheMaxEntries int `json:"promptCacheMaxEntries,omitempty"`

	// PromptCacheMaxRatio caps reported cache-read tokens as a fraction of a
	// request's total input tokens, keeping cache-hit estimates realistic (the
	// newest content is never fully served from cache on the current turn).
	// 0/negative or >1 selects the built-in default (see GetPromptCacheMaxRatio).
	PromptCacheMaxRatio float64 `json:"promptCacheMaxRatio,omitempty"`

	// MaxRequestBodyMB caps inbound request body size (in MiB) on the public
	// inference endpoints (messages / count_tokens / chat completions / responses).
	// Bodies are read with io.ReadAll, so an unbounded size lets a single oversized
	// or malicious request balloon memory; exceeding the cap returns HTTP 413.
	// 0 or negative means use the built-in default (see GetMaxRequestBodyBytes).
	MaxRequestBodyMB int `json:"maxRequestBodyMB,omitempty"`

	// MaxPayloadBytes is the upper bound (in bytes) for the serialized Kiro
	// request body after translation. When a converted payload exceeds this
	// size, older conversation history is dropped (with a placeholder note) so
	// the request fits under Kiro's upstream input limit. Operators can raise it
	// above the conservative built-in default when the upstream tolerates larger
	// bodies. 0 or negative selects the built-in default (see GetMaxPayloadBytes).
	MaxPayloadBytes int `json:"maxPayloadBytes,omitempty"`

	// Proxy configuration: optional outbound proxy for Kiro API requests
	// Format: "socks5://host:port", "socks5://user:pass@host:port",
	//         "http://host:port",  "http://user:pass@host:port"
	// Leave empty to connect directly.
	ProxyURL string `json:"proxyURL,omitempty"`

	// SanitizeClaudeCodePrompt is kept for backward-compatible JSON loading only.
	// Migrated to FilterClaudeCode on first load. Do not use directly.
	SanitizeClaudeCodePrompt bool `json:"sanitizeClaudeCodePrompt,omitempty"`

	// FilterClaudeCode detects the Claude Code CLI built-in system prompt and replaces it
	// with a compact backend-only prompt, reducing token usage significantly.
	FilterClaudeCode bool `json:"filterClaudeCode,omitempty"`

	// FilterEnvNoise strips environment metadata lines from system prompts:
	// git status, recent commits, environment sections, fast_mode_info tags, etc.
	FilterEnvNoise bool `json:"filterEnvNoise,omitempty"`

	// FilterStripBoundaries removes --- SYSTEM PROMPT --- / --- END SYSTEM PROMPT --- markers.
	FilterStripBoundaries bool `json:"filterStripBoundaries,omitempty"`

	// PromptFilterRules is a list of user-defined prompt sanitization rules (regex or line-filter).
	PromptFilterRules []PromptFilterRule `json:"promptFilterRules,omitempty"`

	// LogLevel controls verbosity of application logs.
	// Accepted values: "debug", "info", "warn", "error". Defaults to "info".
	// Can be overridden by the LOG_LEVEL environment variable.
	LogLevel string `json:"logLevel,omitempty"`

	// Global statistics (persisted across restarts)
	TotalRequests   int     `json:"totalRequests,omitempty"`   // Total API requests received
	SuccessRequests int     `json:"successRequests,omitempty"` // Successful requests count
	FailedRequests  int     `json:"failedRequests,omitempty"`  // Failed requests count
	TotalTokens     int     `json:"totalTokens,omitempty"`     // Total tokens processed
	TotalCredits    float64 `json:"totalCredits,omitempty"`    // Total credits consumed
}

// AccountInfo contains account metadata retrieved from Kiro API.
// Used for updating subscription and usage information.
type AccountInfo struct {
	Email             string
	UserId            string
	SubscriptionType  string
	SubscriptionTitle string
	DaysRemaining     int
	UsageCurrent      float64
	UsageLimit        float64
	UsagePercent      float64
	NextResetDate     string
	LastRefresh       int64
	TrialUsageCurrent float64
	TrialUsageLimit   float64
	TrialUsagePercent float64
	TrialStatus       string
	TrialExpiresAt    int64
}

// Version current version. Declared as var (not const) so release builds can
// override it via -ldflags "-X kiro-go/config.Version=<tag>".
var Version = "1.1.3"

// PromptFilterConfig holds all prompt filter settings for API responses.
type PromptFilterConfig struct {
	FilterClaudeCode      bool               `json:"filterClaudeCode"`
	FilterEnvNoise        bool               `json:"filterEnvNoise"`
	FilterStripBoundaries bool               `json:"filterStripBoundaries"`
	Rules                 []PromptFilterRule `json:"rules"`
}

// ThinkingConfig holds settings for AI thinking/reasoning mode.
// When enabled, models output their reasoning process alongside the response.
type ThinkingConfig struct {
	Suffix       string `json:"suffix"`       // Model name suffix that triggers thinking mode
	OpenAIFormat string `json:"openaiFormat"` // Output format for OpenAI-compatible responses
	ClaudeFormat string `json:"claudeFormat"` // Output format for Claude-compatible responses
}

type KiroClientConfig struct {
	KiroVersion   string
	SystemVersion string
	NodeVersion   string
}
