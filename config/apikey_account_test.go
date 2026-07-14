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

	t.Run("secret only in accessToken backfills kiroApiKey", func(t *testing.T) {
		a := Account{AccessToken: "ksk_in_at", AuthMethod: "api_key"}
		NormalizeApiKeyCredential(&a)
		if a.KiroApiKey != "ksk_in_at" {
			t.Fatalf("KiroApiKey = %q, want ksk_in_at (backfill from AccessToken)", a.KiroApiKey)
		}
		if a.AccessToken != "ksk_in_at" {
			t.Fatalf("AccessToken = %q, want ksk_in_at", a.AccessToken)
		}
	})

	t.Run("kiroApiKey wins over divergent accessToken", func(t *testing.T) {
		a := Account{KiroApiKey: "ksk_truth", AccessToken: "stale_token", AuthMethod: "api_key"}
		NormalizeApiKeyCredential(&a)
		if a.AccessToken != "ksk_truth" {
			t.Fatalf("AccessToken = %q, want ksk_truth (KiroApiKey is source of truth)", a.AccessToken)
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

func TestAddAccountsSupportsApiKeyAccounts(t *testing.T) {
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
	if added != 2 || skipped != 4 {
		t.Fatalf("AddAccounts counts = (%d added, %d skipped), want (2, 4)", added, skipped)
	}
	got := findAccount(t, "api-new")
	if got.AuthMethod != "api_key" || got.KiroApiKey != "ksk_new" || got.AccessToken != "ksk_new" {
		t.Fatalf("API-key Account not normalized after bulk add: %+v", got)
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

// TestUpdateAccountTokenDoesNotWipeBearerWhenKiroApiKeyEmpty guards the edge where
// on-disk state is diverged: AuthMethod=api_key with the secret only in AccessToken
// and an empty KiroApiKey (hand-edit or partial write). The guard must adopt the
// AccessToken as the source of truth, never zero the bearer.
func TestUpdateAccountTokenDoesNotWipeBearerWhenKiroApiKeyEmpty(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	// Bypass the AddAccount normalizer to construct the diverged state directly.
	cfgLock.Lock()
	cfg.Accounts = append(cfg.Accounts, Account{ID: "a1", AccessToken: "ksk_real", AuthMethod: "api_key", Enabled: true})
	cfgLock.Unlock()

	if err := UpdateAccountToken("a1", "oauth_access", "oauth_refresh", 9999999999); err != nil {
		t.Fatalf("UpdateAccountToken: %v", err)
	}
	got := findAccount(t, "a1")
	if got.AccessToken != "ksk_real" || got.KiroApiKey != "ksk_real" {
		t.Fatalf("bearer wiped or not backfilled: AccessToken=%q KiroApiKey=%q", got.AccessToken, got.KiroApiKey)
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
