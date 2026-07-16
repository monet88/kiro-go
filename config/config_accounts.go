package config

import (
	"errors"
	"strings"
	"time"
)

// apiKeyAuthMethod is the canonical AuthMethod value for an API-key Account.
const apiKeyAuthMethod = "api_key"

// IsApiKeyAuthMethod reports whether an AuthMethod identifies an API-key
// Account. Keep this classifier in config so import and account logic share the
// same api_key/apikey semantics.
func IsApiKeyAuthMethod(authMethod string) bool {
	switch strings.ToLower(strings.TrimSpace(authMethod)) {
	case "api_key", "apikey":
		return true
	default:
		return false
	}
}

// IsApiKeyCredential reports whether this Account authenticates with a static
// Kiro API Key (ksk_…) rather than an OAuth-style credential. It is true when
// KiroApiKey is set OR AuthMethod is api_key/apikey (case-insensitive). API-key
// Accounts skip OAuth refresh and profile-ARN resolution and send their bearer
// with the API_KEY token type (ADR-0002).
func (a Account) IsApiKeyCredential() bool {
	if strings.TrimSpace(a.KiroApiKey) != "" {
		return true
	}
	return IsApiKeyAuthMethod(a.AuthMethod)
}

// NormalizeApiKeyCredential enforces the API-key Account invariants on write
// (ADR-0002): AuthMethod is canonicalized to "api_key", AccessToken is mirrored
// from the KiroApiKey source of truth, and OAuth-only metadata is removed.
// Legacy accessToken-only records are repaired explicitly during Load; new
// AddAccount and UpdateAccount calls must provide KiroApiKey.
func NormalizeApiKeyCredential(a *Account) {
	if a == nil || !a.IsApiKeyCredential() {
		return
	}
	a.AuthMethod = apiKeyAuthMethod
	a.AccessToken = a.KiroApiKey
	a.RefreshToken = ""
	a.ClientID = ""
	a.ClientSecret = ""
	a.StartUrl = ""
	a.ExpiresAt = 0
	a.ProfileArn = ""
	a.TokenEndpoint = ""
	a.IssuerURL = ""
	a.Scopes = ""
	a.OverageStatus = ""
	a.OverageCapability = ""
	a.OverageCap = 0
	a.OverageRate = 0
	a.CurrentOverages = 0
	a.OverageCheckedAt = 0
	// Usage/subscription fields are kept: API-key Accounts refresh them via
	// management.{region}.kiro.dev (same source as OAuth getUsageLimits).
}

func ValidateApiKeyCredential(account Account) error {
	if !account.IsApiKeyCredential() {
		return nil
	}
	apiKey := strings.TrimSpace(account.KiroApiKey)
	if apiKey == "" {
		return errors.New("Kiro API Key is required for API-key Accounts")
	}
	if strings.Contains(apiKey, "*") {
		return errors.New("masked Kiro API Key cannot be used as a credential")
	}
	return nil
}

// MaskKiroApiKey masks static account credentials even when they are short.
func MaskKiroApiKey(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 20 {
		return strings.Repeat("*", len(key))
	}
	return key[:6] + "****" + key[len(key)-4:]
}

func AutoQuarantineSuspicious429Reason() string {
	return autoQuarantineSuspicious429Reason
}

// OperatorDisabledReason is the BanReason stamped when a human operator disables
// an account. It marks the account as DISABLED (not SUSPENDED), which keeps the
// auto-restore sweep from ever re-enabling it.
func OperatorDisabledReason() string {
	return operatorDisabledReason
}

func shouldAutoRestoreSuspendedAccount(a Account, now time.Time) bool {
	return a.BanStatus == "SUSPENDED" && a.BanReason == autoQuarantineSuspicious429Reason && a.BanTime > 0 && now.Unix()-a.BanTime >= int64(autoQuarantineDuration/time.Second)
}

func applyAutoRestoreLocked() bool {
	if cfg == nil {
		return false
	}
	now := time.Now()
	changed := false
	for i := range cfg.Accounts {
		if shouldAutoRestoreSuspendedAccount(cfg.Accounts[i], now) {
			cfg.Accounts[i].Enabled = true
			cfg.Accounts[i].BanStatus = "ACTIVE"
			cfg.Accounts[i].BanReason = ""
			cfg.Accounts[i].BanTime = 0
			changed = true
		}
	}
	if changed {
		_ = Save()
	}
	return changed
}

func GetAccounts() []Account {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	applyAutoRestoreLocked()
	accounts := make([]Account, len(cfg.Accounts))
	copy(accounts, cfg.Accounts)
	return accounts
}

func GetEnabledAccounts() []Account {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	applyAutoRestoreLocked()
	var accounts []Account
	for _, a := range cfg.Accounts {
		if a.Enabled {
			accounts = append(accounts, a)
		}
	}
	return accounts
}

func AddAccount(account Account) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	NormalizeApiKeyCredential(&account)
	if err := ValidateApiKeyCredential(account); err != nil {
		return err
	}
	// API-key Accounts are keyed by the static ksk_ secret. Reject a second
	// write of the same key so a double-click / double-bound UI handler cannot
	// create two pool entries for one credential.
	if account.IsApiKeyCredential() {
		key := strings.TrimSpace(account.KiroApiKey)
		for i := range cfg.Accounts {
			existing := strings.TrimSpace(cfg.Accounts[i].KiroApiKey)
			if existing == "" {
				existing = strings.TrimSpace(cfg.Accounts[i].AccessToken)
			}
			if existing != "" && existing == key {
				return errors.New("this Kiro API Key is already registered")
			}
		}
	}
	cfg.Accounts = append(cfg.Accounts, account)
	return Save()
}

// KiroApiKeyExists reports whether any account already holds the given static
// Kiro API Key (ksk_…). Used by credential import to refuse a duplicate before
// spending an upstream probe, and as a read-side companion to AddAccount's
// write-side guard.
func KiroApiKeyExists(kiroApiKey string) bool {
	key := strings.TrimSpace(kiroApiKey)
	if key == "" {
		return false
	}
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	for i := range cfg.Accounts {
		if !cfg.Accounts[i].IsApiKeyCredential() {
			continue
		}
		existing := strings.TrimSpace(cfg.Accounts[i].KiroApiKey)
		if existing == "" {
			existing = strings.TrimSpace(cfg.Accounts[i].AccessToken)
		}
		if existing == key {
			return true
		}
	}
	return false
}

// AddAccounts appends multiple accounts in a single locked pass and persists
// with exactly one Save(), avoiding the O(n²) write amplification that calling
// AddAccount in a loop would cause (each AddAccount re-serializes the entire
// config.json). Accounts whose RefreshToken already exists (against the current
// config or earlier entries in the same batch) are skipped to keep OAuth bulk
// imports idempotent across retries/re-pastes. API-key Accounts are intentionally
// skipped because bulk API-key import is outside the MVP; use AddAccount for the
// legitimate single-account write path. Entries without a RefreshToken are also
// skipped. Returns how many were added and how many were skipped.
//
// Save() is only invoked when at least one account is actually added, so a
// fully-duplicate batch does not churn the config file.
func AddAccounts(accounts []Account) (added int, skipped int, err error) {
	cfgLock.Lock()
	defer cfgLock.Unlock()

	dedupKey := func(account Account) string {
		if account.IsApiKeyCredential() || strings.TrimSpace(account.RefreshToken) == "" {
			return ""
		}
		return "oauth\x00" + account.RefreshToken
	}

	// Seed the seen-set with credentials already persisted so the batch dedups
	// against existing accounts, not just within itself.
	seen := make(map[string]struct{}, len(cfg.Accounts)+len(accounts))
	for i := range cfg.Accounts {
		if key := dedupKey(cfg.Accounts[i]); key != "" {
			seen[key] = struct{}{}
		}
	}

	for _, a := range accounts {
		NormalizeApiKeyCredential(&a)
		key := dedupKey(a)
		if key == "" {
			skipped++
			continue
		}
		if _, dup := seen[key]; dup {
			skipped++
			continue
		}
		seen[key] = struct{}{}
		cfg.Accounts = append(cfg.Accounts, a)
		added++
	}

	if added == 0 {
		return 0, skipped, nil
	}
	if err := Save(); err != nil {
		// Roll back the in-memory appends so a failed persist does not leave
		// the running pool out of sync with what is on disk.
		cfg.Accounts = cfg.Accounts[:len(cfg.Accounts)-added]
		return 0, skipped, err
	}
	return added, skipped, nil
}

// RefreshTokenExists reports whether any account already holds the given refresh
// token. Used by bulk import to dedup candidates before spending an upstream
// token-exchange round-trip on a duplicate.
func RefreshTokenExists(refreshToken string) bool {
	if refreshToken == "" {
		return false
	}
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	for i := range cfg.Accounts {
		if cfg.Accounts[i].RefreshToken == refreshToken {
			return true
		}
	}
	return false
}

// AccountIDExists reports whether an account with the given id already exists.
// Used by credential import to decide whether a pasted record's id can be reused
// (re-importing a backup must not duplicate) or a fresh one must be minted.
func AccountIDExists(id string) bool {
	if id == "" {
		return false
	}
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	for i := range cfg.Accounts {
		if cfg.Accounts[i].ID == id {
			return true
		}
	}
	return false
}

func UpdateAccount(id string, account Account) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	NormalizeApiKeyCredential(&account)
	if err := ValidateApiKeyCredential(account); err != nil {
		return err
	}
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts[i] = account
			return Save()
		}
	}
	return nil
}

// UpdateAccountOverageStatus persists the cached upstream overage status fields.
// Called after a successful setUserPreference or getUsageLimits round-trip.
func UpdateAccountOverageStatus(id, status, capability string, cap, rate, current float64, checkedAt int64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			if status != "" {
				cfg.Accounts[i].OverageStatus = status
			}
			if capability != "" {
				cfg.Accounts[i].OverageCapability = capability
			}
			cfg.Accounts[i].OverageCap = cap
			cfg.Accounts[i].OverageRate = rate
			cfg.Accounts[i].CurrentOverages = current
			if checkedAt > 0 {
				cfg.Accounts[i].OverageCheckedAt = checkedAt
			}
			return Save()
		}
	}
	return nil
}

// ClearAccountCurrentOverages zeroes the cached CurrentOverages for an account
// while preserving the OverageStatus switch and the cap/rate billing config.
// Called when upstream usage has fallen back within the subscription quota
// (e.g. after a billing-period reset): overage points are zero by definition
// when usage is within quota, so stale points from a previous period must not
// linger in the UI/scheduler. Returns without writing if already zero, so the
// periodic refresh loop does not churn the config file every cycle.
func ClearAccountCurrentOverages(id string, checkedAt int64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			if cfg.Accounts[i].CurrentOverages == 0 {
				return nil
			}
			cfg.Accounts[i].CurrentOverages = 0
			if checkedAt > 0 {
				cfg.Accounts[i].OverageCheckedAt = checkedAt
			}
			return Save()
		}
	}
	return nil
}

// SetAccountEnabled toggles the enabled state of an account and persists the change.
// Used to disable accounts whose refresh token has been revoked (401 Bad credentials)
// so subsequent requests skip them automatically.
func SetAccountEnabled(id string, enabled bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts[i].Enabled = enabled
			if enabled {
				cfg.Accounts[i].BanStatus = "ACTIVE"
				cfg.Accounts[i].BanReason = ""
				cfg.Accounts[i].BanTime = 0
			} else {
				cfg.Accounts[i].BanStatus = "DISABLED"
				cfg.Accounts[i].BanTime = time.Now().Unix()
			}
			return Save()
		}
	}
	return nil
}

// SetAccountBanStatus marks an account as banned/disabled with a reason.
// Reason is recorded so operators can see why the account was auto-disabled.
func SetAccountBanStatus(id, status, reason string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts[i].BanStatus = status
			cfg.Accounts[i].BanReason = reason
			cfg.Accounts[i].BanTime = time.Now().Unix()
			if status == "BANNED" || status == "DISABLED" {
				cfg.Accounts[i].Enabled = false
			}
			return Save()
		}
	}
	return nil
}

func SuspendAccountTemporarily(id, reason string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	now := time.Now().Unix()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts[i].Enabled = false
			cfg.Accounts[i].BanStatus = "SUSPENDED"
			cfg.Accounts[i].BanReason = reason
			cfg.Accounts[i].BanTime = now
			return Save()
		}
	}
	return nil
}

func UpdateAccountProfileArn(id, profileArn string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts[i].ProfileArn = profileArn
			return Save()
		}
	}
	return nil
}

func DeleteAccount(id string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts = append(cfg.Accounts[:i], cfg.Accounts[i+1:]...)
			return Save()
		}
	}
	return nil
}

func UpdateAccountToken(id, accessToken, refreshToken string, expiresAt int64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			// An API-key Account never refreshes; KiroApiKey is the source of
			// truth. Guard against a stray OAuth-style token write clobbering the
			// static bearer (ADR-0002). A malformed legacy record with the secret
			// only in AccessToken is left untouched here; Load owns that migration.
			if cfg.Accounts[i].IsApiKeyCredential() {
				if strings.TrimSpace(cfg.Accounts[i].KiroApiKey) == "" {
					return errors.New("Kiro API Key is required for API-key Accounts")
				}
				NormalizeApiKeyCredential(&cfg.Accounts[i])
				return Save()
			}
			cfg.Accounts[i].AccessToken = accessToken
			if refreshToken != "" {
				cfg.Accounts[i].RefreshToken = refreshToken
			}
			cfg.Accounts[i].ExpiresAt = expiresAt
			return Save()
		}
	}
	return nil
}

func UpdateAccountStats(id string, requestCount, errorCount, totalTokens int, totalCredits float64, lastUsed int64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts[i].RequestCount = requestCount
			cfg.Accounts[i].ErrorCount = errorCount
			cfg.Accounts[i].TotalTokens = totalTokens
			cfg.Accounts[i].TotalCredits = totalCredits
			cfg.Accounts[i].LastUsed = lastUsed
			return Save()
		}
	}
	return nil
}

// UpdateAccountInfo updates an account's subscription and usage information.
// Called after refreshing account data from Kiro API.
func UpdateAccountInfo(id string, info AccountInfo) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			if info.Email != "" {
				cfg.Accounts[i].Email = info.Email
			}
			if info.UserId != "" {
				cfg.Accounts[i].UserId = info.UserId
			}
			cfg.Accounts[i].SubscriptionType = info.SubscriptionType
			cfg.Accounts[i].SubscriptionTitle = info.SubscriptionTitle
			cfg.Accounts[i].DaysRemaining = info.DaysRemaining
			cfg.Accounts[i].UsageCurrent = info.UsageCurrent
			cfg.Accounts[i].UsageLimit = info.UsageLimit
			cfg.Accounts[i].UsagePercent = info.UsagePercent
			cfg.Accounts[i].NextResetDate = info.NextResetDate
			cfg.Accounts[i].LastRefresh = info.LastRefresh
			cfg.Accounts[i].TrialUsageCurrent = info.TrialUsageCurrent
			cfg.Accounts[i].TrialUsageLimit = info.TrialUsageLimit
			cfg.Accounts[i].TrialUsagePercent = info.TrialUsagePercent
			cfg.Accounts[i].TrialStatus = info.TrialStatus
			cfg.Accounts[i].TrialExpiresAt = info.TrialExpiresAt
			NormalizeApiKeyCredential(&cfg.Accounts[i])
			return Save()
		}
	}
	return nil
}
