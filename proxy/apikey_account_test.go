package proxy

import (
	"encoding/json"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestApiKeyAccountSendsApiKeyTokenType verifies outbound auth for an API-key
// Account announces TokenType: API_KEY and sends the static ksk_… as the bearer
// (ADR-0002).
func TestApiKeyAccountSendsApiKeyTokenType(t *testing.T) {
	account := &config.Account{
		AccessToken: "ksk_static_bearer",
		KiroApiKey:  "ksk_static_bearer",
		AuthMethod:  "api_key",
		MachineId:   "machine-apikey",
	}
	req, err := http.NewRequest("POST", "https://codewhisperer.us-east-1.amazonaws.com/generateAssistantResponse", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	setKiroHeaders(req, account)

	if got := req.Header.Get("TokenType"); got != "API_KEY" {
		t.Fatalf("expected TokenType=API_KEY for api_key account, got %q", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer ksk_static_bearer" {
		t.Fatalf("expected bearer with the static key, got %q", got)
	}
}

// TestNonApiKeyAccountOmitsApiKeyTokenType guards that OAuth accounts are
// unaffected by the API_KEY token-type header.
func TestNonApiKeyAccountOmitsApiKeyTokenType(t *testing.T) {
	for _, method := range []string{"idc", "social", ""} {
		account := &config.Account{AccessToken: "oauth_tok", AuthMethod: method, MachineId: "m"}
		req, err := http.NewRequest("POST", "https://codewhisperer.us-east-1.amazonaws.com/generateAssistantResponse", nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		setKiroHeaders(req, account)
		if got := req.Header.Get("TokenType"); got != "" {
			t.Fatalf("auth method %q should not set API_KEY TokenType, got %q", method, got)
		}
	}
}

// TestEnsureValidTokenSkipsApiKeyAccount verifies the OAuth refresh path is
// skipped for API-key Accounts: no refresh material exists, and an expired
// ExpiresAt must not trigger a (failing) refresh that could disable the account.
func TestEnsureValidTokenSkipsApiKeyAccount(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	h := &Handler{pool: accountpool.GetPool()}
	account := &config.Account{
		ID:          "apikey-1",
		KiroApiKey:  "ksk_x",
		AccessToken: "ksk_x",
		AuthMethod:  "api_key",
		// Deliberately in the past so the OAuth path would have attempted a refresh.
		ExpiresAt: time.Now().Add(-time.Hour).Unix(),
	}
	if err := h.ensureValidToken(account); err != nil {
		t.Fatalf("ensureValidToken should be a no-op for API-key account, got %v", err)
	}
	if account.AccessToken != "ksk_x" {
		t.Fatalf("AccessToken changed by refresh path: %q", account.AccessToken)
	}
}

func TestAdminRefreshTokenHelperSkipsApiKeyAccountWithStaleRefreshToken(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	h := &Handler{pool: accountpool.GetPool()}
	account := &config.Account{
		ID:           "api-key",
		AccessToken:  "ksk_static",
		KiroApiKey:   "ksk_static",
		AuthMethod:   "api_key",
		RefreshToken: "stale-refresh-token",
		ExpiresAt:    time.Now().Add(-time.Hour).Unix(),
	}

	if err := h.refreshAccountTokenIfNeeded(account); err != nil {
		t.Fatalf("API-key Account refresh helper returned error: %v", err)
	}
	if account.AccessToken != "ksk_static" || account.RefreshToken != "stale-refresh-token" {
		t.Fatalf("API-key Account credentials changed: access=%q refresh=%q", account.AccessToken, account.RefreshToken)
	}
}

// TestResolveProfileArnSkipsApiKeyAccount verifies profile-ARN resolution is a
// soft skip for API-key Accounts (no IDC/OAuth profile discovery), so data-plane
// calls proceed on the default region.
func TestResolveProfileArnSkipsApiKeyAccount(t *testing.T) {
	account := &config.Account{
		ID:         "apikey-1",
		KiroApiKey: "ksk_x",
		AuthMethod: "api_key",
	}
	_, err := ResolveProfileArn(account)
	if err == nil {
		t.Fatalf("expected a skip error, got nil")
	}
	if !isProfileArnResolutionSoftError(err) {
		t.Fatalf("expected a soft (skip) error, got %v", err)
	}
}

// TestResolveProfileArnApiKeyIgnoresCachedArn verifies stale OAuth profile
// metadata is never used by an API-key Account.
func TestResolveProfileArnApiKeyIgnoresCachedArn(t *testing.T) {
	const arn = "arn:aws:codewhisperer:us-east-1:123456789012:profile/ABCDEF"
	account := &config.Account{
		ID:         "apikey-1",
		KiroApiKey: "ksk_x",
		AuthMethod: "api_key",
		ProfileArn: arn,
	}
	got, err := ResolveProfileArn(account)
	if err == nil || !isProfileArnResolutionSoftError(err) {
		t.Fatalf("expected soft API-key profile skip, got value=%q error=%v", got, err)
	}
	if got != "" {
		t.Fatalf("expected stale profile ARN to be ignored, got %q", got)
	}
}

func TestRefreshAccountInfoSkipsApiKeyUsageLookup(t *testing.T) {
	account := &config.Account{
		ID:                "api-key",
		Email:             "ops@example.com",
		AccessToken:       "ksk_static",
		KiroApiKey:        "ksk_static",
		AuthMethod:        "api_key",
		Enabled:           true,
		SubscriptionType:  "PRO",
		SubscriptionTitle: "Kiro Pro",
		UsageCurrent:      12,
		UsageLimit:        100,
	}

	info, err := RefreshAccountInfo(account)
	if err != nil {
		t.Fatalf("RefreshAccountInfo returned error for API-key Account: %v", err)
	}
	if info.Email != account.Email || info.SubscriptionType != account.SubscriptionType {
		t.Fatalf("API-key Account identity/subscription metadata was not preserved: %+v", info)
	}
	if info.UsageCurrent != 0 || info.UsageLimit != 0 || info.UsagePercent != 0 || info.NextResetDate != "" || info.LastRefresh != 0 || info.TrialUsageCurrent != 0 || info.TrialUsageLimit != 0 || info.TrialUsagePercent != 0 || info.TrialStatus != "" || info.TrialExpiresAt != 0 {
		t.Fatalf("API-key Account refresh returned unsupported usage metadata: %+v", info)
	}
	if !account.Enabled || account.BanStatus != "" {
		t.Fatalf("API-key Account was disabled during metadata refresh: %+v", account)
	}
}

// TestTryRefreshApiKeyAccountFails verifies an auth-looking failure on an
// API-key Account is not treated as a refreshable transient — the caller must be
// allowed to disable a bad static key.
func TestTryRefreshApiKeyAccountFails(t *testing.T) {
	h := &Handler{pool: accountpool.GetPool()}
	account := &config.Account{ID: "apikey-1", KiroApiKey: "ksk_x", AccessToken: "ksk_x", AuthMethod: "api_key"}
	if got := h.tryRefreshAccountAfterAuthError(account); got != authRefreshFailed {
		t.Fatalf("expected authRefreshFailed for API-key account, got %v", got)
	}
}

// TestApiAddAccountApiKeyDualWrite verifies the add-one admin path persists an
// API-key Account with the dual-write invariant and canonical AuthMethod.
func TestApiAddAccountApiKeyDualWrite(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	h := &Handler{pool: accountpool.GetPool()}

	body := `{"kiroApiKey":"ksk_added","authMethod":"apikey","email":"ops@example.com","enabled":true}`
	req := httptest.NewRequest("POST", "/admin/api/accounts", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.apiAddAccount(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("apiAddAccount status = %d, body=%s", rec.Code, rec.Body.String())
	}
	acc := findProxyAccount(t, "ops@example.com")
	if !acc.IsApiKeyCredential() || acc.AuthMethod != "api_key" {
		t.Fatalf("expected canonical api_key account, got %+v", acc)
	}
	if acc.AccessToken != "ksk_added" || acc.KiroApiKey != "ksk_added" {
		t.Fatalf("dual-write violated: AccessToken=%q KiroApiKey=%q", acc.AccessToken, acc.KiroApiKey)
	}
	if acc.MachineId == "" {
		t.Fatalf("expected MachineId to be generated for API-key Account add path")
	}
	if acc.Region != "us-east-1" {
		t.Fatalf("Region = %q, want us-east-1 default", acc.Region)
	}
}

func TestApiAddAccountApiKeyRequiresSecret(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	h := &Handler{pool: accountpool.GetPool()}

	req := httptest.NewRequest(http.MethodPost, "/admin/api/accounts", strings.NewReader(`{"authMethod":"api_key","email":"missing@example.com","enabled":true}`))
	rec := httptest.NewRecorder()
	h.apiAddAccount(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing secret, got %d body=%s", rec.Code, rec.Body.String())
	}
	if len(config.GetAccounts()) != 0 {
		t.Fatalf("API-key Account without a secret was persisted: %+v", config.GetAccounts())
	}
}

func TestApiAddAccountRejectsMaskedApiKey(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	h := &Handler{pool: accountpool.GetPool()}

	req := httptest.NewRequest(http.MethodPost, "/admin/api/accounts", strings.NewReader(`{"authMethod":"api_key","kiroApiKey":"ksk_ve****7890","enabled":true}`))
	rec := httptest.NewRecorder()
	h.apiAddAccount(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a masked Kiro API Key, got %d body=%s", rec.Code, rec.Body.String())
	}
	if len(config.GetAccounts()) != 0 {
		t.Fatalf("masked credential was persisted: %+v", config.GetAccounts())
	}
}

func TestApiAddAccountDoesNotAcceptAccessTokenAlias(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	h := &Handler{pool: accountpool.GetPool()}

	req := httptest.NewRequest(http.MethodPost, "/admin/api/accounts", strings.NewReader(`{"authMethod":"api_key","accessToken":"ksk_alias","enabled":true}`))
	rec := httptest.NewRecorder()
	h.apiAddAccount(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when add-one omits KiroApiKey, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestApiGetAccountsHidesApiKeyOAuthMetadata(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:           "api-key",
		KiroApiKey:   "ksk_static",
		AuthMethod:   "api_key",
		RefreshToken: "stale-refresh",
		ExpiresAt:    time.Now().Add(-time.Hour).Unix(),
		Enabled:      true,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	h := &Handler{pool: accountpool.GetPool()}
	h.pool.Reload()
	req := httptest.NewRequest(http.MethodGet, "/admin/api/accounts", nil)
	rec := httptest.NewRecorder()
	h.apiGetAccounts(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var accounts []map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &accounts); err != nil {
		t.Fatalf("decode accounts: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("accounts count = %d, want 1", len(accounts))
	}
	if got, _ := accounts[0]["hasRefreshToken"].(bool); got {
		t.Fatal("API-key Account list entry advertised a refresh token")
	}
	if got, _ := accounts[0]["expiresAt"].(float64); got != 0 {
		t.Fatalf("API-key Account expiresAt = %v, want 0", got)
	}
}

// TestApiImportCredentialsApiKeyAccount verifies the import-one path creates an
// API-key Account without any OAuth refresh round-trip and honors the dual-write.
func TestApiImportCredentialsApiKeyAccount(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	h := &Handler{pool: accountpool.GetPool()}

	body := `{"kiroApiKey":"ksk_imported","authMethod":"api_key","email":"imp@example.com"}`
	req := httptest.NewRequest("POST", "/admin/api/credentials", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.apiImportCredentials(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("apiImportCredentials status = %d, body=%s", rec.Code, rec.Body.String())
	}
	acc := findProxyAccount(t, "imp@example.com")
	if !acc.IsApiKeyCredential() || acc.AuthMethod != "api_key" {
		t.Fatalf("expected canonical api_key account, got %+v", acc)
	}
	if acc.AccessToken != "ksk_imported" || acc.KiroApiKey != "ksk_imported" {
		t.Fatalf("dual-write violated: AccessToken=%q KiroApiKey=%q", acc.AccessToken, acc.KiroApiKey)
	}
	if acc.RefreshToken != "" {
		t.Fatalf("API-key import must not carry a refresh token, got %q", acc.RefreshToken)
	}
}

func TestApiImportCredentialsApiKeyAcceptsAccessTokenAlias(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	h := &Handler{pool: accountpool.GetPool()}

	body := `{"accessToken":"ksk_legacy","authMethod":"api_key","email":"legacy@example.com"}`
	req := httptest.NewRequest("POST", "/admin/api/credentials", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.apiImportCredentials(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("apiImportCredentials status = %d, body=%s", rec.Code, rec.Body.String())
	}
	acc := findProxyAccount(t, "legacy@example.com")
	if acc.KiroApiKey != "ksk_legacy" || acc.AccessToken != "ksk_legacy" {
		t.Fatalf("legacy accessToken alias was not normalized: %+v", acc)
	}
}

func TestApiImportCredentialsRejectsMaskedApiKey(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	h := &Handler{pool: accountpool.GetPool()}

	body := `{"accessToken":"ksk_ve****7890","authMethod":"api_key","email":"masked@example.com"}`
	req := httptest.NewRequest(http.MethodPost, "/admin/api/credentials", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.apiImportCredentials(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a masked Kiro API Key, got %d body=%s", rec.Code, rec.Body.String())
	}
	if len(config.GetAccounts()) != 0 {
		t.Fatalf("masked credential was persisted: %+v", config.GetAccounts())
	}
}

// TestApiImportCredentialsApiKeyRequiresSecret verifies the import path rejects an
// api_key request that carries neither kiroApiKey nor accessToken.
func TestApiImportCredentialsApiKeyRequiresSecret(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	h := &Handler{pool: accountpool.GetPool()}

	body := `{"authMethod":"api_key","email":"x@example.com"}`
	req := httptest.NewRequest("POST", "/admin/api/credentials", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.apiImportCredentials(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing secret, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestApiGetAccountFullMasksApiKey verifies the detail endpoint never returns the
// raw ksk_… secret — accessToken and kiroApiKey are masked (ADR-0002).
func TestApiGetAccountFullMasksApiKey(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	const secret = "ksk_verylongsecretvalue1234567890"
	if err := config.AddAccount(config.Account{
		ID:                "a1",
		KiroApiKey:        secret,
		AuthMethod:        "api_key",
		RefreshToken:      "stale-refresh",
		ClientID:          "stale-client",
		ClientSecret:      "stale-secret",
		ExpiresAt:         time.Now().Add(-time.Hour).Unix(),
		OverageStatus:     "ENABLED",
		OverageCapability: "OVERAGE_CAPABLE",
		OverageCap:        1000,
		OverageRate:       2,
		CurrentOverages:   50,
		OverageCheckedAt:  time.Now().Unix(),
		UsageCurrent:      120,
		UsageLimit:        100,
		UsagePercent:      1.2,
		NextResetDate:     "2099-01-01",
		LastRefresh:       time.Now().Unix(),
		TrialUsageCurrent: 5,
		TrialUsageLimit:   10,
		TrialUsagePercent: 0.5,
		TrialStatus:       "ACTIVE",
		TrialExpiresAt:    time.Now().Add(time.Hour).Unix(),
		Enabled:           true,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	h := &Handler{pool: accountpool.GetPool()}
	req := httptest.NewRequest("GET", "/admin/api/accounts/a1/full", nil)
	rec := httptest.NewRecorder()
	h.apiGetAccountFull(rec, req, "a1")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, _ := resp["accessToken"].(string); got == secret || strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("raw secret leaked in detail payload: %s", rec.Body.String())
	}
	if got, _ := resp["accessToken"].(string); got != config.MaskKiroApiKey(secret) {
		t.Fatalf("accessToken not masked: got %q want %q", got, config.MaskKiroApiKey(secret))
	}
	if got, _ := resp["kiroApiKey"].(string); got != config.MaskKiroApiKey(secret) {
		t.Fatalf("kiroApiKey not masked: got %q want %q", got, config.MaskKiroApiKey(secret))
	}
	if got, _ := resp["refreshToken"].(string); got != "" {
		t.Fatalf("API-key Account refreshToken leaked in detail payload: %q", got)
	}
	if got, _ := resp["clientId"].(string); got != "" {
		t.Fatalf("API-key Account clientId leaked in detail payload: %q", got)
	}
	if got, _ := resp["clientSecret"].(string); got != "" {
		t.Fatalf("API-key Account clientSecret leaked in detail payload: %q", got)
	}
	if isKey, _ := resp["isApiKeyAccount"].(bool); !isKey {
		t.Fatalf("expected isApiKeyAccount=true in detail payload")
	}
	for _, field := range []string{"expiresAt", "overageCap", "overageRate", "currentOverages", "overageCheckedAt", "usageCurrent", "usageLimit", "usagePercent", "lastRefresh", "trialUsageCurrent", "trialUsageLimit", "trialUsagePercent", "trialExpiresAt"} {
		if got, _ := resp[field].(float64); got != 0 {
			t.Fatalf("API-key Account detail %s = %v, want 0", field, got)
		}
	}
	for _, field := range []string{"overageStatus", "overageCapability", "nextResetDate", "trialStatus"} {
		if got, _ := resp[field].(string); got != "" {
			t.Fatalf("API-key Account detail %s = %q, want empty", field, got)
		}
	}
	if got, _ := resp["overageEffective"].(bool); got {
		t.Fatal("API-key Account detail advertised effective overage")
	}
}

func TestApiExportAccountsMasksApiKeyAccountSecret(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	const apiKeySecret = "ksk_verylongsecretvalue1234567890"
	const oauthToken = "oauth_portable_backup_token"
	if err := config.AddAccount(config.Account{ID: "api-key", KiroApiKey: apiKeySecret, AuthMethod: "api_key", RefreshToken: "stale-refresh", Enabled: true}); err != nil {
		t.Fatalf("AddAccount API-key Account: %v", err)
	}
	if err := config.AddAccount(config.Account{ID: "oauth", AccessToken: oauthToken, AuthMethod: "social", Enabled: true}); err != nil {
		t.Fatalf("AddAccount OAuth Account: %v", err)
	}

	h := &Handler{pool: accountpool.GetPool()}
	req := httptest.NewRequest(http.MethodPost, "/admin/api/export", strings.NewReader(`{"ids":["api-key","oauth"]}`))
	rec := httptest.NewRecorder()
	h.apiExportAccounts(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), apiKeySecret) {
		t.Fatalf("raw Kiro API Key leaked in export payload: %s", rec.Body.String())
	}
	var response struct {
		Accounts []struct {
			ID          string `json:"id"`
			Credentials struct {
				AccessToken  string `json:"accessToken"`
				RefreshToken string `json:"refreshToken"`
			} `json:"credentials"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode export: %v", err)
	}
	accessTokens := make(map[string]string, len(response.Accounts))
	for _, account := range response.Accounts {
		accessTokens[account.ID] = account.Credentials.AccessToken
	}
	if got := accessTokens["api-key"]; got != config.MaskKiroApiKey(apiKeySecret) {
		t.Fatalf("API-key Account accessToken = %q, want masked value %q", got, config.MaskKiroApiKey(apiKeySecret))
	}
	if got := accessTokens["oauth"]; got != oauthToken {
		t.Fatalf("OAuth Account accessToken = %q, want portable token %q", got, oauthToken)
	}
	for _, account := range response.Accounts {
		if account.ID == "api-key" && account.Credentials.RefreshToken != "" {
			t.Fatalf("API-key Account refreshToken leaked in export: %q", account.Credentials.RefreshToken)
		}
	}
}

func TestApiSetAccountOverageRejectsApiKeyAccount(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{ID: "api-key", KiroApiKey: "ksk_static", AuthMethod: "api_key", Enabled: true}); err != nil {
		t.Fatalf("AddAccount API-key Account: %v", err)
	}

	h := &Handler{pool: accountpool.GetPool()}
	req := httptest.NewRequest(http.MethodPost, "/admin/api/accounts/api-key/overage", strings.NewReader(`{"enabled":true}`))
	rec := httptest.NewRecorder()
	h.apiSetAccountOverage(rec, req, "api-key")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Overages cannot be toggled for an API-key Account") {
		t.Fatalf("expected clear unsupported-operation error, got %s", rec.Body.String())
	}
}

func TestApiGetAccountOverageRejectsApiKeyAccount(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{ID: "api-key", KiroApiKey: "ksk_static", AuthMethod: "api_key", Enabled: true}); err != nil {
		t.Fatalf("AddAccount API-key Account: %v", err)
	}

	h := &Handler{pool: accountpool.GetPool()}
	req := httptest.NewRequest(http.MethodGet, "/admin/api/accounts/api-key/overage", nil)
	rec := httptest.NewRecorder()
	h.apiGetAccountOverage(rec, req, "api-key")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Overages are not supported for an API-key Account") {
		t.Fatalf("expected clear unsupported-operation error, got %s", rec.Body.String())
	}
}

func TestApiBatchEnableReportsQuarantinedApiKeyAccountFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	raw := `{"password":"x","port":8089,"accounts":[{"id":"quarantined","authMethod":"api_key","accessToken":"ksk_ab****7890","enabled":true}]}`
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	if err := config.Init(path); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	h := &Handler{pool: accountpool.GetPool()}
	req := httptest.NewRequest(http.MethodPost, "/admin/api/accounts/batch", strings.NewReader(`{"action":"enable","ids":["quarantined"]}`))
	rec := httptest.NewRecorder()
	h.apiBatchAccounts(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
	var resp struct {
		Success bool `json:"success"`
		Count   int  `json:"count"`
		Failed  int  `json:"failed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Success || resp.Count != 0 || resp.Failed != 1 {
		t.Fatalf("unexpected batch response: %+v", resp)
	}
	account := config.GetAccounts()[0]
	if account.Enabled || account.KiroApiKey != "" || account.AccessToken != "" {
		t.Fatalf("quarantined account was enabled or restored: %+v", account)
	}
}

func TestFetchOverageStatusRejectsApiKeyAccountBeforeNetwork(t *testing.T) {
	account := &config.Account{ID: "api-key", KiroApiKey: "ksk_static", AuthMethod: "api_key"}

	if _, err := FetchOverageStatus(account); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("expected API-key Account overage rejection, got %v", err)
	}
}

func findProxyAccount(t *testing.T, email string) config.Account {
	t.Helper()
	for _, a := range config.GetAccounts() {
		if a.Email == email {
			return a
		}
	}
	t.Fatalf("account with email %q not found", email)
	return config.Account{}
}
