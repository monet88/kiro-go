package config

import "runtime"

// SetPassword updates the admin password.
// Primarily used for environment variable override in containerized deployments.
func SetPassword(password string) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.Password = password
}

func GetPassword() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.Password
}

func GetPort() int {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.Port == 0 {
		return 8089
	}
	return cfg.Port
}

func GetHost() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.Host == "" {
		return "127.0.0.1"
	}
	return cfg.Host
}

func ApplyConservativeRoutingProfile() error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.BalanceMode = "health"
	cfg.PreferredEndpoint = "kiro"
	fallback := false
	cfg.EndpointFallback = &fallback
	return Save()
}

func GetApiKey() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.ApiKey
}

func IsApiKeyRequired() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.RequireApiKey
}

func UpdateSettings(apiKey string, requireApiKey bool, password string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.ApiKey = apiKey
	cfg.RequireApiKey = requireApiKey
	if password != "" {
		cfg.Password = password
	}
	return Save()
}

func UpdateSettingsPatch(apiKey *string, requireApiKey *bool, password string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if apiKey != nil {
		cfg.ApiKey = *apiKey
	}
	if requireApiKey != nil {
		cfg.RequireApiKey = *requireApiKey
	}
	if password != "" {
		cfg.Password = password
	}
	return Save()
}

func UpdateStats(totalReq, successReq, failedReq, totalTokens int, totalCredits float64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.TotalRequests = totalReq
	cfg.SuccessRequests = successReq
	cfg.FailedRequests = failedReq
	cfg.TotalTokens = totalTokens
	cfg.TotalCredits = totalCredits
	return Save()
}

func GetStats() (int, int, int, int, float64) {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.TotalRequests, cfg.SuccessRequests, cfg.FailedRequests, cfg.TotalTokens, cfg.TotalCredits
}

// GetFilterClaudeCode returns whether Claude Code system prompt detection is enabled.
// Also checks the legacy SanitizeClaudeCodePrompt flag for backward compatibility.
func GetFilterClaudeCode() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.FilterClaudeCode || cfg.SanitizeClaudeCodePrompt
}

// GetFilterEnvNoise returns whether environment noise line stripping is enabled.
func GetFilterEnvNoise() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.FilterEnvNoise
}

// GetFilterStripBoundaries returns whether boundary marker stripping is enabled.
func GetFilterStripBoundaries() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.FilterStripBoundaries
}

// GetPromptFilterConfig returns all prompt filter settings.
func GetPromptFilterConfig() PromptFilterConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return PromptFilterConfig{Rules: []PromptFilterRule{}}
	}
	rules := make([]PromptFilterRule, len(cfg.PromptFilterRules))
	copy(rules, cfg.PromptFilterRules)
	return PromptFilterConfig{
		FilterClaudeCode:      cfg.FilterClaudeCode || cfg.SanitizeClaudeCodePrompt,
		FilterEnvNoise:        cfg.FilterEnvNoise,
		FilterStripBoundaries: cfg.FilterStripBoundaries,
		Rules:                 rules,
	}
}

// UpdatePromptFilterConfig saves all prompt filter settings atomically.
func UpdatePromptFilterConfig(filterClaudeCode, filterEnvNoise, filterStripBoundaries bool, rules []PromptFilterRule) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.FilterClaudeCode = filterClaudeCode
	cfg.FilterEnvNoise = filterEnvNoise
	cfg.FilterStripBoundaries = filterStripBoundaries
	// Clear legacy flag to avoid double-applying after first save
	cfg.SanitizeClaudeCodePrompt = false
	if rules != nil {
		cfg.PromptFilterRules = rules
	}
	return Save()
}

// GetPromptFilterRules returns the current prompt filter rules.
func GetPromptFilterRules() []PromptFilterRule {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	rules := make([]PromptFilterRule, len(cfg.PromptFilterRules))
	copy(rules, cfg.PromptFilterRules)
	return rules
}

// GetThinkingConfig 获取 thinking 配置
func GetThinkingConfig() ThinkingConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()

	suffix := cfg.ThinkingSuffix
	if suffix == "" {
		suffix = "-thinking"
	}
	openaiFormat := cfg.OpenAIThinkingFormat
	if openaiFormat == "" {
		openaiFormat = "reasoning_content"
	}
	claudeFormat := cfg.ClaudeThinkingFormat
	if claudeFormat == "" {
		claudeFormat = "thinking"
	}

	return ThinkingConfig{
		Suffix:       suffix,
		OpenAIFormat: openaiFormat,
		ClaudeFormat: claudeFormat,
	}
}

// UpdateThinkingConfig 更新 thinking 配置
func UpdateThinkingConfig(suffix, openaiFormat, claudeFormat string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.ThinkingSuffix = suffix
	cfg.OpenAIThinkingFormat = openaiFormat
	cfg.ClaudeThinkingFormat = claudeFormat
	return Save()
}

// GetPreferredEndpoint 获取首选端点配置
func GetPreferredEndpoint() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.PreferredEndpoint == "" {
		return "auto"
	}
	return cfg.PreferredEndpoint
}

// UpdatePreferredEndpoint 更新首选端点配置
func UpdatePreferredEndpoint(endpoint string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.PreferredEndpoint = endpoint
	return Save()
}

// GetEndpointFallback returns whether endpoint fallback is enabled. Defaults to true.
func GetEndpointFallback() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.EndpointFallback == nil {
		return true
	}
	return *cfg.EndpointFallback
}

// UpdateEndpointFallback sets the endpoint fallback switch and persists the change.
func UpdateEndpointFallback(enabled bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.EndpointFallback = &enabled
	return Save()
}

// GetProxyURL 获取出站代理地址
func GetProxyURL() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.ProxyURL
}

// UpdateProxySettings 更新出站代理配置
func UpdateProxySettings(proxyURL string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.ProxyURL = proxyURL
	return Save()
}

// GetAllowOverUsage returns whether over-usage is allowed when account quota is exhausted.
func GetAllowOverUsage() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.AllowOverUsage
}

// UpdateAllowOverUsage sets the over-usage setting and persists the change.
func UpdateAllowOverUsage(allow bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.AllowOverUsage = allow
	return Save()
}

// GetBalanceMode returns the configured routing mode. Defaults to "health".
func GetBalanceMode() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.BalanceMode == "" {
		return "health"
	}
	switch cfg.BalanceMode {
	case "health", "managed", "aggressive":
		return cfg.BalanceMode
	default:
		return "health"
	}
}

// UpdateBalanceMode sets the routing mode and persists it.
func UpdateBalanceMode(mode string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	switch mode {
	case "health", "managed", "aggressive":
		cfg.BalanceMode = mode
	default:
		cfg.BalanceMode = "health"
	}
	return Save()
}

func GetRoutingConcurrencyConfig() RoutingConcurrencyConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return defaultRoutingConcurrencyConfig()
	}
	return normalizeRoutingConcurrencyConfig(cfg.RoutingConcurrency)
}

// defaultMaxRequestBodyBytes bounds inbound request bodies on the public
// inference endpoints. 32 MiB mirrors Anthropic's documented request limit:
// large enough for long multimodal contexts, small enough to stop a single
// oversized/malicious body from ballooning process memory via io.ReadAll.
const defaultMaxRequestBodyBytes int64 = 32 << 20

// GetMaxRequestBodyBytes returns the configured inbound body cap in bytes,
// falling back to defaultMaxRequestBodyBytes when unset or non-positive.
func GetMaxRequestBodyBytes() int64 {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.MaxRequestBodyMB <= 0 {
		return defaultMaxRequestBodyBytes
	}
	return int64(cfg.MaxRequestBodyMB) << 20
}

func UpdateRoutingConcurrencyConfig(rc RoutingConcurrencyConfig) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.RoutingConcurrency = normalizeRoutingConcurrencyConfig(rc)
	return Save()
}

// GetLogLevel returns the configured log level (debug/info/warn/error). Defaults to "info".
func GetLogLevel() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.LogLevel == "" {
		return "info"
	}
	return cfg.LogLevel
}

// UpdateLogLevel updates the log level setting and persists the change.
func UpdateLogLevel(level string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.LogLevel = level
	return Save()
}

// GetServerReadTimeout returns the configured ReadTimeout in seconds (default 120).
func GetServerReadTimeout() int {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.ServerReadTimeoutSeconds <= 0 {
		return 120
	}
	return cfg.ServerReadTimeoutSeconds
}

// GetServerIdleTimeout returns the configured IdleTimeout in seconds (default 120).
func GetServerIdleTimeout() int {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.ServerIdleTimeoutSeconds <= 0 {
		return 120
	}
	return cfg.ServerIdleTimeoutSeconds
}

// UpdateServerReadTimeout updates server ReadTimeout (requires restart to take effect).
func UpdateServerReadTimeout(seconds int) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if seconds <= 0 {
		seconds = 120
	}
	cfg.ServerReadTimeoutSeconds = seconds
	return Save()
}

// UpdateServerIdleTimeout updates server IdleTimeout (requires restart to take effect).
func UpdateServerIdleTimeout(seconds int) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if seconds <= 0 {
		seconds = 120
	}
	cfg.ServerIdleTimeoutSeconds = seconds
	return Save()
}

// DefaultPromptCacheMaxEntries is the default in-memory Cross-account Prompt
// Cache LRU bound (ADR-0001): the maximum number of distinct Cache Fingerprints
// held across all Accounts before the least-recently-used entry is evicted.
const DefaultPromptCacheMaxEntries = 131072

// minPromptCacheMaxEntries is the floor applied to a misconfigured (too small)
// PromptCacheMaxEntries so multi-turn prefixes are not evicted immediately.
const minPromptCacheMaxEntries = 1024

// DefaultPromptCacheMaxRatio caps reported cache-read tokens at 85% of total
// input tokens so the newest turn is never reported as fully cache-served.
const DefaultPromptCacheMaxRatio = 0.85

// GetPromptCacheMaxEntries returns the configured in-memory Prompt Cache LRU
// bound, falling back to DefaultPromptCacheMaxEntries when unset and raising
// too-small values to the minimum floor.
func GetPromptCacheMaxEntries() int {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.PromptCacheMaxEntries <= 0 {
		return DefaultPromptCacheMaxEntries
	}
	if cfg.PromptCacheMaxEntries < minPromptCacheMaxEntries {
		return minPromptCacheMaxEntries
	}
	return cfg.PromptCacheMaxEntries
}

// GetPromptCacheMaxRatio returns the configured cache-read ratio cap, falling
// back to DefaultPromptCacheMaxRatio when unset or out of the (0,1] range. The
// negated-range test also rejects NaN (every comparison with NaN is false).
func GetPromptCacheMaxRatio() float64 {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || !(cfg.PromptCacheMaxRatio > 0 && cfg.PromptCacheMaxRatio <= 1) {
		return DefaultPromptCacheMaxRatio
	}
	return cfg.PromptCacheMaxRatio
}

// UpdatePromptCacheMaxEntries updates the in-memory Prompt Cache LRU bound and
// persists the change. Non-positive values reset to the default; values below
// the minimum floor are raised to the floor.
func UpdatePromptCacheMaxEntries(entries int) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	switch {
	case entries <= 0:
		cfg.PromptCacheMaxEntries = DefaultPromptCacheMaxEntries
	case entries < minPromptCacheMaxEntries:
		cfg.PromptCacheMaxEntries = minPromptCacheMaxEntries
	default:
		cfg.PromptCacheMaxEntries = entries
	}
	return Save()
}

// UpdatePromptCacheMaxRatio updates the cache-read ratio cap and persists the
// change. Out-of-range values (including NaN) reset to the default; the
// negated-range test rejects NaN since every NaN comparison is false.
func UpdatePromptCacheMaxRatio(ratio float64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if ratio > 0 && ratio <= 1 {
		cfg.PromptCacheMaxRatio = ratio
	} else {
		cfg.PromptCacheMaxRatio = DefaultPromptCacheMaxRatio
	}
	return Save()
}

// DefaultMaxPayloadBytes is the default upper bound for the serialized Kiro
// request body. It replaces the old hard ~900KiB cap: operators can raise it via
// MaxPayloadBytes so large multimodal/long-context requests are not truncated
// before reaching upstream. Kiro rejects oversized requests with HTTP 400
// (CONTENT_LENGTH_EXCEEDS_THRESHOLD), so the effective ceiling is still bounded
// by upstream; this knob only controls the local truncation trigger.
const DefaultMaxPayloadBytes = 2_000_000

// minMaxPayloadBytes is a sanity floor so a misconfigured tiny value cannot
// truncate essentially every request down to the fallback placeholder.
const minMaxPayloadBytes = 64 * 1024

// GetMaxPayloadBytes returns the configured serialized request-body cap used by
// the local truncation pass, falling back to DefaultMaxPayloadBytes when unset
// and raising too-small values to the minimum floor.
func GetMaxPayloadBytes() int {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.MaxPayloadBytes <= 0 {
		return DefaultMaxPayloadBytes
	}
	if cfg.MaxPayloadBytes < minMaxPayloadBytes {
		return minMaxPayloadBytes
	}
	return cfg.MaxPayloadBytes
}

// UpdateMaxPayloadBytes updates the serialized request-body cap and persists the
// change. Non-positive values reset to the default; values below the minimum
// floor are raised to the floor.
func UpdateMaxPayloadBytes(bytes int) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	switch {
	case bytes <= 0:
		cfg.MaxPayloadBytes = DefaultMaxPayloadBytes
	case bytes < minMaxPayloadBytes:
		cfg.MaxPayloadBytes = minMaxPayloadBytes
	default:
		cfg.MaxPayloadBytes = bytes
	}
	return Save()
}

func GetKiroClientConfig() KiroClientConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()

	kiroVersion := "0.11.107"
	if cfg != nil && cfg.KiroVersion != "" {
		kiroVersion = cfg.KiroVersion
	}

	systemVersion := ""
	if cfg != nil {
		systemVersion = cfg.SystemVersion
	}
	if systemVersion == "" {
		systemVersion = defaultSystemVersion()
	}

	nodeVersion := "22.22.0"
	if cfg != nil && cfg.NodeVersion != "" {
		nodeVersion = cfg.NodeVersion
	}

	return KiroClientConfig{
		KiroVersion:   kiroVersion,
		SystemVersion: systemVersion,
		NodeVersion:   nodeVersion,
	}
}

func defaultSystemVersion() string {
	switch runtime.GOOS {
	case "windows":
		return "win32#10.0.22631"
	case "darwin":
		return "darwin#24.6.0"
	default:
		return "linux#6.6.87"
	}
}
