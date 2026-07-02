package config

import "time"

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
	cfg.Accounts = append(cfg.Accounts, account)
	return Save()
}

// AddAccounts appends multiple accounts in a single locked pass and persists
// with exactly one Save(), avoiding the O(n²) write amplification that calling
// AddAccount in a loop would cause (each AddAccount re-serializes the entire
// config.json). Accounts whose RefreshToken already exists (against the current
// config or earlier entries in the same batch) are skipped to keep bulk imports
// idempotent across retries/re-pastes. Entries with an empty RefreshToken are
// also skipped — there is no stable identity to dedup on and they cannot be
// activated later. Returns how many were added and how many were skipped.
//
// Save() is only invoked when at least one account is actually added, so a
// fully-duplicate batch does not churn the config file.
func AddAccounts(accounts []Account) (added int, skipped int, err error) {
	cfgLock.Lock()
	defer cfgLock.Unlock()

	// Seed the seen-set with refresh tokens already persisted so the batch
	// dedups against existing accounts, not just within itself.
	seen := make(map[string]struct{}, len(cfg.Accounts)+len(accounts))
	for i := range cfg.Accounts {
		if rt := cfg.Accounts[i].RefreshToken; rt != "" {
			seen[rt] = struct{}{}
		}
	}

	for _, a := range accounts {
		if a.RefreshToken == "" {
			skipped++
			continue
		}
		if _, dup := seen[a.RefreshToken]; dup {
			skipped++
			continue
		}
		seen[a.RefreshToken] = struct{}{}
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
			return Save()
		}
	}
	return nil
}
