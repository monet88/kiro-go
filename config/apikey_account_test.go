package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIsApiKeyCredential covers the ADR-0002 predicate: an Account is an API-key
// Account when KiroApiKey is set OR AuthMethod is api_key/apikey (case-insensitive).
func TestIsApiKeyCredential(t *testing.T) {
	cases := []struct {
		name string
		acc  Account
		want bool
	}{
		{"kiroApiKey set", Account{KiroApiKey: "ksk_abc"}, true},
		{"authMethod api_key", Account{AuthMethod: "api_key"}, true},
		{"authMethod apikey", Account{AuthMethod: "apikey"}, true},
		{"authMethod ApiKey mixed case", Account{AuthMethod: "ApiKey"}, true},
		{"kiroApiKey whitespace only", Account{KiroApiKey: "   "}, false},
		{"oauth idc", Account{AuthMethod: "idc", AccessToken: "at"}, false},
		{"oauth social", Account{AuthMethod: "social"}, false},
		{"external_idp", Account{AuthMethod: "external_idp"}, false},
		{"empty", Account{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.acc.IsApiKeyCredential(); got != tc.want {
				t.Fatalf("IsApiKeyCredential() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestNormalizeApiKeyCredentialDualWrite verifies the dual-write invariant: after
// normalization an API-key Account has AuthMethod canonicalized to "api_key" and
// AccessToken == KiroApiKey, whether the secret arrived in KiroApiKey or only in
// AccessToken (add-one with authMethod=api_key).
func TestNormalizeApiKeyCredentialDualWrite(t *testing.T) {
	t.Run("secret in kiroApiKey mirrors into accessToken", func(t *testing.T) {
		a := Account{KiroApiKey: "ksk_source", AuthMethod: "apikey"}
		NormalizeApiKeyCredential(&a)
		if a.AuthMethod != "api_key" {
			t.Fatalf("AuthMethod = %q, want api_key", a.AuthMethod)
		}
		if a.AccessToken != "ksk_source" {
			t.Fatalf("AccessToken = %q, want ksk_source (mirror of KiroApiKey)", a.AccessToken)
		}
		if a.KiroApiKey != "ksk_source" {
			t.Fatalf("KiroApiKey = %q, want ksk_source", a.KiroApiKey)
		}
	})

	t.Run("secret only in accessToken is not accepted as source of truth", func(t *testing.T) {
		a := Account{AccessToken: "ksk_in_at", AuthMethod: "api_key"}
		NormalizeApiKeyCredential(&a)
		if a.KiroApiKey != "" {
			t.Fatalf("KiroApiKey = %q, want empty outside legacy load/import migration", a.KiroApiKey)
		}
		if a.AccessToken != "" {
			t.Fatalf("AccessToken = %q, want mirrored empty KiroApiKey", a.AccessToken)
		}
	})

	t.Run("kiroApiKey wins over divergent accessToken", func(t *testing.T) {
		a := Account{
			KiroApiKey:        "ksk_truth",
			AccessToken:       "stale_token",
			RefreshToken:      "stale_refresh",
			ClientID:          "stale_client",
			ClientSecret:      "stale_secret",
			StartUrl:          "https://example.com/start",
			ExpiresAt:         123,
			ProfileArn:        "stale_arn",
			TokenEndpoint:     "https://example.com/token",
			IssuerURL:         "https://example.com",
			Scopes:            "openid",
			AuthMethod:        "api_key",
			OverageStatus:     "ENABLED",
			OverageCapability: "OVERAGE_CAPABLE",
			OverageCap:        100,
			OverageRate:       2,
			CurrentOverages:   10,
			OverageCheckedAt:  456,
			UsageCurrent:      75,
			UsageLimit:        100,
			UsagePercent:      0.75,
			NextResetDate:     "2099-01-01",
			LastRefresh:       789,
			TrialUsageCurrent: 1,
			TrialUsageLimit:   2,
			TrialUsagePercent: 0.5,
			TrialStatus:       "ACTIVE",
			TrialExpiresAt:    999,
		}
		NormalizeApiKeyCredential(&a)
		if a.AccessToken != "ksk_truth" {
			t.Fatalf("AccessToken = %q, want ksk_truth (KiroApiKey is source of truth)", a.AccessToken)
		}
		if a.RefreshToken != "" || a.ClientID != "" || a.ClientSecret != "" || a.StartUrl != "" || a.ExpiresAt != 0 || a.ProfileArn != "" || a.TokenEndpoint != "" || a.IssuerURL != "" || a.Scopes != "" {
			t.Fatalf("OAuth metadata was not scrubbed: %+v", a)
		}
		if a.OverageStatus != "" || a.OverageCapability != "" || a.OverageCap != 0 || a.OverageRate != 0 || a.CurrentOverages != 0 || a.OverageCheckedAt != 0 {
			t.Fatalf("overage metadata was not scrubbed: %+v", a)
		}
		// Usage/subscription metadata is retained so management.kiro.dev refresh can
		// persist limits for API-key Accounts.
		if a.UsageCurrent != 75 || a.UsageLimit != 100 || a.LastRefresh != 789 {
			t.Fatalf("usage metadata should be preserved for API-key Accounts: %+v", a)
		}
	})

	t.Run("oauth account untouched", func(t *testing.T) {
		a := Account{AuthMethod: "idc", AccessToken: "oauth_at", RefreshToken: "rt"}
		NormalizeApiKeyCredential(&a)
		if a.AuthMethod != "idc" || a.AccessToken != "oauth_at" || a.KiroApiKey != "" {
			t.Fatalf("OAuth account mutated: %+v", a)
		}
	})
}

// TestAddAccountEnforcesApiKeyDualWrite verifies the invariant holds through the
// persistence choke point AddAccount (add-one create path).
func TestAddAccountEnforcesApiKeyDualWrite(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := AddAccount(Account{ID: "a1", KiroApiKey: "ksk_add", AuthMethod: "apikey", Enabled: true}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	got := findAccount(t, "a1")
	if !got.IsApiKeyCredential() {
		t.Fatalf("expected API-key credential, got %+v", got)
	}
	if got.AuthMethod != "api_key" {
		t.Fatalf("AuthMethod = %q, want api_key", got.AuthMethod)
	}
	if got.AccessToken != got.KiroApiKey || got.AccessToken != "ksk_add" {
		t.Fatalf("dual-write violated: AccessToken=%q KiroApiKey=%q", got.AccessToken, got.KiroApiKey)
	}
}

func TestAddAccountRejectsApiKeyAccountWithoutSecret(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := AddAccount(Account{ID: "missing", AuthMethod: "api_key", Enabled: true}); err == nil {
		t.Fatal("expected AddAccount to reject an API-key Account without a secret")
	}
	if AccountIDExists("missing") {
		t.Fatal("invalid API-key Account was persisted")
	}
}

func TestAddAccountRejectsMaskedApiKey(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := AddAccount(Account{ID: "masked", KiroApiKey: "ksk_ve****7890", AuthMethod: "api_key", Enabled: true}); err == nil {
		t.Fatal("expected AddAccount to reject a masked Kiro API Key")
	}
	if AccountIDExists("masked") {
		t.Fatal("masked Kiro API Key was persisted")
	}
}

func TestAddAccountsSkipsApiKeyAccounts(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := AddAccount(Account{ID: "existing", KiroApiKey: "ksk_existing", AuthMethod: "api_key"}); err != nil {
		t.Fatalf("seed existing API-key Account: %v", err)
	}

	added, skipped, err := AddAccounts([]Account{
		{ID: "api-new", KiroApiKey: "ksk_new", AuthMethod: "apikey"},
		{ID: "api-existing-duplicate", KiroApiKey: "ksk_existing", AuthMethod: "api_key"},
		{ID: "api-batch-duplicate", AccessToken: "ksk_new", AuthMethod: "api_key"},
		{ID: "api-whitespace", AccessToken: "   ", AuthMethod: "api_key"},
		{ID: "oauth-new", RefreshToken: "oauth_refresh", AuthMethod: "social"},
		{ID: "oauth-empty", AuthMethod: "social"},
	})
	if err != nil {
		t.Fatalf("AddAccounts: %v", err)
	}
	if added != 1 || skipped != 5 {
		t.Fatalf("AddAccounts counts = (%d added, %d skipped), want (1, 5)", added, skipped)
	}
	if AccountIDExists("api-new") {
		t.Fatal("bulk AddAccounts must not create an API-key Account")
	}
	if AccountIDExists("api-batch-duplicate") {
		t.Fatal("bulk AddAccounts must not create an API-key Account from AccessToken")
	}
	if AccountIDExists("api-whitespace") {
		t.Fatal("bulk AddAccounts must not create an API-key Account with an empty key")
	}
	if got := findAccount(t, "oauth-new"); got.RefreshToken != "oauth_refresh" {
		t.Fatalf("OAuth Account changed during bulk add: %+v", got)
	}
}

// TestUpdateAccountEnforcesApiKeyDualWrite verifies the invariant holds through
// UpdateAccount (edit path).
func TestUpdateAccountEnforcesApiKeyDualWrite(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := AddAccount(Account{ID: "a1", KiroApiKey: "ksk_v1", AuthMethod: "api_key", Enabled: true}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	// Simulate an edit that rotates the key only in KiroApiKey and leaves a stale
	// AccessToken; the choke point must re-mirror.
	if err := UpdateAccount("a1", Account{ID: "a1", KiroApiKey: "ksk_v2", AccessToken: "stale", AuthMethod: "api_key", Enabled: true}); err != nil {
		t.Fatalf("UpdateAccount: %v", err)
	}
	got := findAccount(t, "a1")
	if got.AccessToken != "ksk_v2" || got.KiroApiKey != "ksk_v2" {
		t.Fatalf("dual-write violated after update: AccessToken=%q KiroApiKey=%q", got.AccessToken, got.KiroApiKey)
	}
}

func TestUpdateAccountRejectsApiKeyAccountWithoutSecret(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := AddAccount(Account{ID: "a1", KiroApiKey: "ksk_keep", AuthMethod: "api_key", Enabled: true}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if err := UpdateAccount("a1", Account{ID: "a1", AuthMethod: "api_key", Enabled: true}); err == nil {
		t.Fatal("expected UpdateAccount to reject an API-key Account without a secret")
	}
	got := findAccount(t, "a1")
	if got.KiroApiKey != "ksk_keep" || got.AccessToken != "ksk_keep" {
		t.Fatalf("invalid update changed persisted credentials: %+v", got)
	}
}

// TestUpdateAccountTokenDoesNotClobberApiKey verifies that a stray OAuth-style
// token write (e.g. from a shared refresh path) cannot overwrite the static
// bearer of an API-key Account.
func TestUpdateAccountTokenDoesNotClobberApiKey(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := AddAccount(Account{ID: "a1", KiroApiKey: "ksk_keep", AuthMethod: "api_key", Enabled: true}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if err := UpdateAccountToken("a1", "oauth_access", "oauth_refresh", 9999999999); err != nil {
		t.Fatalf("UpdateAccountToken: %v", err)
	}
	got := findAccount(t, "a1")
	if got.AccessToken != "ksk_keep" || got.KiroApiKey != "ksk_keep" {
		t.Fatalf("API key clobbered: AccessToken=%q KiroApiKey=%q", got.AccessToken, got.KiroApiKey)
	}
	if got.RefreshToken != "" {
		t.Fatalf("RefreshToken should stay empty for API-key Account, got %q", got.RefreshToken)
	}
}

// TestUpdateAccountTokenRejectsDivergedApiKeyState guards the edge where on-disk
// state bypassed Load migration. The write path must reject the malformed record
// without adopting a new OAuth token or erasing the existing bearer.
func TestUpdateAccountTokenDoesNotWipeBearerWhenKiroApiKeyEmpty(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	// Bypass the AddAccount normalizer to construct the diverged state directly.
	cfgLock.Lock()
	cfg.Accounts = append(cfg.Accounts, Account{ID: "a1", AccessToken: "ksk_real", AuthMethod: "api_key", Enabled: true})
	cfgLock.Unlock()

	if err := UpdateAccountToken("a1", "oauth_access", "oauth_refresh", 9999999999); err == nil {
		t.Fatal("expected UpdateAccountToken to reject diverged API-key state")
	}
	got := findAccount(t, "a1")
	if got.AccessToken != "ksk_real" || got.KiroApiKey != "" {
		t.Fatalf("diverged state changed: AccessToken=%q KiroApiKey=%q", got.AccessToken, got.KiroApiKey)
	}
}

// TestLoadRepairsApiKeyDualWrite verifies that a config.json carrying a diverged
// API-key Account (AuthMethod=api_key, secret only in accessToken, empty
// kiroApiKey) is repaired to the dual-write invariant on Load, and the repair is
// persisted so it survives the next restart.
func TestLoadRepairsApiKeyDualWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	raw := `{"password":"x","port":8089,"accounts":[{"id":"a1","authMethod":"api_key","accessToken":"ksk_ondisk","enabled":true}]}`
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	if err := Init(path); err != nil {
		t.Fatalf("init config: %v", err)
	}
	got := findAccount(t, "a1")
	if got.KiroApiKey != "ksk_ondisk" || got.AccessToken != "ksk_ondisk" {
		t.Fatalf("Load did not repair dual-write: AccessToken=%q KiroApiKey=%q", got.AccessToken, got.KiroApiKey)
	}
	// The repair must be persisted, not just in-memory.
	persisted, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back config: %v", err)
	}
	if !strings.Contains(string(persisted), `"kiroApiKey"`) || !strings.Contains(string(persisted), `ksk_ondisk`) {
		t.Fatalf("repaired kiroApiKey not persisted to disk: %s", persisted)
	}
}

func TestLoadQuarantinesMaskedApiKeyCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	raw := `{"password":"x","port":8089,"accounts":[` +
		`{"id":"legacy","authMethod":"api_key","accessToken":"ksk_ab****7890","enabled":true},` +
		`{"id":"current","authMethod":"api_key","kiroApiKey":"ksk_cd****1234","enabled":true}]}`
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	if err := Init(path); err != nil {
		t.Fatalf("init config: %v", err)
	}
	for _, id := range []string{"legacy", "current"} {
		got := findAccount(t, id)
		if got.Enabled || got.AccessToken != "" || got.KiroApiKey != "" {
			t.Fatalf("masked account %q was not quarantined: %+v", id, got)
		}
	}
	persisted, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back config: %v", err)
	}
	if strings.Contains(string(persisted), "****") {
		t.Fatalf("masked credentials remained persisted: %s", persisted)
	}
}

func findAccount(t *testing.T, id string) Account {
	t.Helper()
	for _, a := range GetAccounts() {
		if a.ID == id {
			return a
		}
	}
	t.Fatalf("account %q not found", id)
	return Account{}
}
