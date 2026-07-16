package proxy

import (
	"encoding/json"
	"kiro-go/auth"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"strings"
	"time"
)

func (h *Handler) apiStartIamSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		StartUrl string `json:"startUrl"`
		Region   string `json:"region"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if req.StartUrl == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "startUrl is required"})
		return
	}

	sessionID, authorizeUrl, expiresIn, err := auth.StartIamSsoLogin(req.StartUrl, req.Region)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId":    sessionID,
		"authorizeUrl": authorizeUrl,
		"expiresIn":    expiresIn,
	})
}

func (h *Handler) apiCompleteIamSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID   string `json:"sessionId"`
		CallbackUrl string `json:"callbackUrl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	accessToken, refreshToken, clientID, clientSecret, region, expiresIn, err := auth.CompleteIamSsoLogin(req.SessionID, req.CallbackUrl)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// 获取用户信息
	email, _, _ := auth.GetUserInfo(accessToken)

	// 创建账号
	account := config.Account{
		ID:           auth.GenerateAccountID(),
		Email:        email,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		AuthMethod:   "idc",
		Region:       region,
		ExpiresAt:    time.Now().Unix() + int64(expiresIn),
		Enabled:      true,
		MachineId:    config.GenerateMachineId(),
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}

func (h *Handler) apiStartBuilderIdLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Region string `json:"region"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	session, err := auth.StartBuilderIdLogin(req.Region)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId":       session.ID,
		"userCode":        session.UserCode,
		"verificationUri": session.VerificationUri,
		"interval":        session.Interval,
	})
}

func (h *Handler) apiPollBuilderIdAuth(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	accessToken, refreshToken, clientID, clientSecret, region, expiresIn, status, err := auth.PollBuilderIdAuth(req.SessionID)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	if status == "pending" || status == "slow_down" {
		// 获取当前间隔
		interval := 5
		if session := auth.GetBuilderIdSession(req.SessionID); session != nil {
			interval = session.Interval
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":   true,
			"completed": false,
			"status":    status,
			"interval":  interval,
		})
		return
	}

	// 授权完成，获取用户信息
	email, _, _ := auth.GetUserInfo(accessToken)

	// 创建账号
	account := config.Account{
		ID:           auth.GenerateAccountID(),
		Email:        email,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		AuthMethod:   "idc",
		Provider:     "BuilderId",
		Region:       region,
		ExpiresAt:    time.Now().Unix() + int64(expiresIn),
		Enabled:      true,
		MachineId:    config.GenerateMachineId(),
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"completed": true,
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}

func (h *Handler) apiStartKiroSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Region string `json:"region"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	session, signInURL, err := auth.StartKiroSsoLogin(req.Region)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId": session.ID,
		"signInUrl": signInURL,
		"interval":  2,
	})
}

func (h *Handler) apiCancelKiroSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.SessionID != "" {
		auth.CancelKiroSsoLogin(req.SessionID)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// apiCompleteKiroSso lets the operator paste a callback URL the browser could not
// deliver (e.g. remote deployment where localhost isn't the proxy host).
func (h *Handler) apiCompleteKiroSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID   string `json:"sessionId"`
		CallbackURL string `json:"callbackUrl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if req.SessionID == "" || req.CallbackURL == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "sessionId and callbackUrl are required"})
		return
	}
	redirectURL, err := auth.FeedCallbackURL(req.SessionID, req.CallbackURL)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	resp := map[string]interface{}{"success": true}
	if redirectURL != "" {
		resp["redirectUrl"] = redirectURL
		resp["message"] = "Open this URL to continue Microsoft 365 login, then paste the final callback URL"
	} else {
		resp["message"] = "Callback processed – check the login status"
	}
	json.NewEncoder(w).Encode(resp)
}

func (h *Handler) apiPollKiroSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	result, status, err := auth.PollKiroSsoAuth(req.SessionID)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	if status == "pending" {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":   true,
			"completed": false,
			"status":    "pending",
		})
		return
	}

	account := config.Account{
		ID:            auth.GenerateAccountID(),
		Email:         result.Email,
		AccessToken:   result.AccessToken,
		RefreshToken:  result.RefreshToken,
		ClientID:      result.ClientID,
		AuthMethod:    result.AuthMethod,
		Provider:      result.Provider,
		Region:        result.Region,
		ProfileArn:    result.ProfileArn,
		TokenEndpoint: result.TokenEndpoint,
		IssuerURL:     result.IssuerURL,
		Scopes:        result.Scopes,
		ExpiresAt:     time.Now().Unix() + int64(result.ExpiresIn),
		Enabled:       true,
		MachineId:     config.GenerateMachineId(),
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"completed": true,
		"account": map[string]interface{}{
			"id":         account.ID,
			"email":      account.Email,
			"authMethod": account.AuthMethod,
		},
	})
}

func (h *Handler) apiImportSsoToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BearerToken string `json:"bearerToken"`
		Region      string `json:"region"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if req.BearerToken == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "bearerToken is required"})
		return
	}

	// 支持批量导入，按行分割
	tokens := strings.Split(strings.TrimSpace(req.BearerToken), "\n")
	var imported []map[string]interface{}
	var errors []string

	for _, token := range tokens {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}

		accessToken, refreshToken, clientID, clientSecret, expiresIn, err := auth.ImportFromSsoToken(token, req.Region)
		if err != nil {
			errors = append(errors, err.Error())
			continue
		}

		// 获取用户信息
		email, _, _ := auth.GetUserInfo(accessToken)

		// 创建账号
		account := config.Account{
			ID:           auth.GenerateAccountID(),
			Email:        email,
			AccessToken:  accessToken,
			RefreshToken: refreshToken,
			ClientID:     clientID,
			ClientSecret: clientSecret,
			AuthMethod:   "idc",
			Region:       req.Region,
			ExpiresAt:    time.Now().Unix() + int64(expiresIn),
			Enabled:      true,
			MachineId:    config.GenerateMachineId(),
		}

		if err := config.AddAccount(account); err != nil {
			errors = append(errors, err.Error())
			continue
		}

		imported = append(imported, map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		})
	}

	h.pool.Reload()

	if len(imported) == 0 && len(errors) > 0 {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   strings.Join(errors, "; "),
		})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"accounts": imported,
		"errors":   errors,
	})
}

func (h *Handler) apiImportCredentials(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
		AuthMethod   string `json:"authMethod"`
		Provider     string `json:"provider"`
		Region       string `json:"region"`
		// external_idp (enterprise SSO / Azure AD) refresh material.
		TokenEndpoint string `json:"tokenEndpoint"`
		IssuerURL     string `json:"issuerUrl"`
		Scopes        string `json:"scopes"`
		// Optional identity preservation when pasting a full account record.
		ID         string `json:"id"`
		Email      string `json:"email"`
		ProfileArn string `json:"profileArn"`
		// userId (account-level in Kiro Account Manager exports) embeds the Azure
		// tenant, from which tokenEndpoint/issuerUrl/scopes are derived when missing.
		UserID string `json:"userId"`
		// kiroApiKey imports an API-key Account (ksk_…). It is the source of truth
		// for the static bearer and needs no refreshToken/OAuth material (ADR-0002).
		KiroApiKey string `json:"kiroApiKey"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// API-key Account import (ADR-0002): a static Kiro API Key is the source of
	// truth and never refreshes, so it takes neither a refreshToken nor OAuth
	// profile-ARN resolution. Detect it by an explicit kiroApiKey OR an
	// authMethod of api_key/apikey (secret then supplied in accessToken) and
	// short-circuit the OAuth import path entirely. config.AddAccount enforces the
	// dual-write (AccessToken == KiroApiKey) and canonical AuthMethod.
	if kiroKey := strings.TrimSpace(req.KiroApiKey); kiroKey != "" || config.IsApiKeyAuthMethod(req.AuthMethod) {
		if kiroKey == "" {
			kiroKey = strings.TrimSpace(req.AccessToken)
		}
		if kiroKey == "" {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "kiroApiKey (or accessToken) is required for an API-key account"})
			return
		}
		if strings.Contains(kiroKey, "*") {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "masked Kiro API Keys cannot be imported"})
			return
		}
		// Refuse before the live probe so a double-bound Add click or re-paste
		// of the same ksk_ does not create a second pool entry / spend quota.
		if config.KiroApiKeyExists(kiroKey) {
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]string{"error": "this Kiro API Key is already registered"})
			return
		}
		if req.Region == "" {
			req.Region = "us-east-1"
		}
		id := req.ID
		if id == "" || config.AccountIDExists(id) {
			id = auth.GenerateAccountID()
		}
		account := config.Account{
			ID:          id,
			Email:       req.Email,
			KiroApiKey:  kiroKey,
			AccessToken: kiroKey,
			AuthMethod:  "api_key",
			Provider:    req.Provider,
			Region:      req.Region,
			Enabled:     true,
			MachineId:   config.GenerateMachineId(),
		}
		// Validate against management.{region}.kiro.dev and capture live quota /
		// subscription (same as the dedicated ApiKey fork). Hard-fail if the key
		// is rejected in every probed region so we never store a dead key with 0/0.
		info, region, err := probeApiKeyServingRegion(&account)
		if err != nil || info == nil {
			msg := "API key validation failed"
			if err != nil {
				msg = "API key validation failed: " + err.Error()
			}
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": msg})
			return
		}
		applyApiKeyProbeResult(&account, info, region)
		if strings.TrimSpace(account.Provider) == "" {
			account.Provider = "API Key"
		}
		if err := config.AddAccount(account); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		h.pool.Reload()
		// Warm the models cache with the validated regional key.
		if account.Enabled && account.AccessToken != "" {
			go func(acc config.Account) {
				if err := h.fetchAndCacheAccountModels(&acc); err != nil {
					logger.Warnf("[ModelsCache] Auto-refresh failed for new API-key Account %s: %v", acc.Email, err)
				}
			}(account)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"account": map[string]interface{}{
				"id":             account.ID,
				"email":          account.Email,
				"region":         account.Region,
				"subscription":   account.SubscriptionTitle,
				"usageCurrent":   account.UsageCurrent,
				"usageLimit":     account.UsageLimit,
				"usagePercent":   account.UsagePercent,
				"nextResetDate":  account.NextResetDate,
			},
		})
		return
	}

	if req.RefreshToken == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "refreshToken is required"})
		return
	}

	// 设置默认值
	if req.Region == "" {
		req.Region = "us-east-1"
	}
	// 标准化 authMethod。external_idp 必须先于 clientId+clientSecret→idc 的推断被识别
	//（external_idp 带 clientId 但没有 clientSecret），否则会被误判成 social 而 refresh 到错误端点。
	req.AuthMethod = normalizeImportAuthMethod(req.AuthMethod, req.ClientID, req.ClientSecret, req.TokenEndpoint)

	// Resolve Azure endpoints from userId (Kiro export, account level) or the
	// accessToken JWT issuer (bare blobs: clientId + token only). A derivation that
	// also clears the allow-list is itself proof the credential is external_idp —
	// IdC/social access tokens are not microsoftonline JWTs, so a bare IdC blob (its
	// iss is an AWS host) won't clear the list and won't be misclassified.
	derivedTE, derivedIss, derivedSc := auth.DeriveExternalIdpEndpoints(req.UserID, req.ClientID, req.AccessToken)
	if derivedTE != "" && auth.ValidateExternalIdpEndpoint(derivedTE) == nil && req.AuthMethod != "external_idp" {
		req.AuthMethod = "external_idp"
	}

	// external_idp 的 tokenEndpoint 是用户可填的新信任边界：必须经 allow-list 校验，
	// 否则一份不信任的 credential JSON 可指向内网/攻击者主机，导致 refresh token 被外泄。
	if req.AuthMethod == "external_idp" {
		// Kiro Account Manager exports and bare blobs omit tokenEndpoint/issuerUrl/
		// scopes; fill them from the derived (userId or accessToken-JWT) tenant.
		if req.TokenEndpoint == "" {
			req.TokenEndpoint = derivedTE
		}
		if req.IssuerURL == "" {
			req.IssuerURL = derivedIss
		}
		if req.Scopes == "" {
			req.Scopes = derivedSc
		}
		if req.ClientID == "" || req.TokenEndpoint == "" {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "external_idp requires clientId and tokenEndpoint (or userId/accessToken to derive it)"})
			return
		}
		if err := auth.ValidateExternalIdpEndpoint(req.TokenEndpoint); err != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "external IdP endpoint rejected: " + err.Error()})
			return
		}
		if req.IssuerURL != "" {
			if err := auth.ValidateExternalIdpEndpoint(req.IssuerURL); err != nil {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]string{"error": "external IdP issuer rejected: " + err.Error()})
				return
			}
		}
	}

	// Resolve the access token to persist. For external_idp we prefer TRUST-ON-IMPORT:
	// when the pasted JSON carries an Azure AD access token (a JWT with a real exp),
	// persist it directly WITHOUT a live refresh round-trip. The JSON can then be
	// imported repeatedly / into multiple instances without each import consuming
	// (rotating) the refresh token, and without requiring egress to Microsoft at
	// import time. The runtime background refresh (backgroundRefresh /
	// ensureValidToken) renews it later when the account is actually used. Falls
	// back to refresh-at-import for idc/social and for external_idp credentials
	// carrying only a refreshToken (so the regression gate — reject when refresh
	// fails — still holds there).
	var (
		accessToken string
		expiresAt   int64
		profileArn  string
	)
	email := req.Email
	if req.AuthMethod == "external_idp" && req.AccessToken != "" {
		if exp := auth.ExpFromAccessTokenJWT(req.AccessToken); exp > 0 {
			accessToken = req.AccessToken
			expiresAt = exp
			profileArn = req.ProfileArn
		}
	}
	if accessToken == "" {
		tempAccount := &config.Account{
			RefreshToken:  req.RefreshToken,
			ClientID:      req.ClientID,
			ClientSecret:  req.ClientSecret,
			AuthMethod:    req.AuthMethod,
			Region:        req.Region,
			TokenEndpoint: req.TokenEndpoint,
			Scopes:        req.Scopes,
		}
		a, newRT, ea, newPA, err := auth.RefreshToken(tempAccount)
		if err != nil {
			// Refresh failed: reject outright instead of falling back to the
			// supplied accessToken. A fallback account is persisted with
			// ExpiresAt = now+300, which the pool's Pick filter skips (now >
			// ExpiresAt-120) within ~3 minutes, and the on-demand refresh path
			// never repairs it (Pick filters it out before ensureValidToken runs).
			// The caller must provide a refreshToken that actually works.
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "Token refresh failed: " + err.Error()})
			return
		}
		accessToken = a
		expiresAt = ea
		profileArn = newPA
		if newRT != "" {
			req.RefreshToken = newRT
		}
		if fetchedEmail, _, _ := auth.GetUserInfo(accessToken); fetchedEmail != "" {
			email = fetchedEmail
		}
	}
	if profileArn == "" {
		profileArn = req.ProfileArn // external_idp refresh returns no profileArn
	}

	// 创建账号
	provider := req.Provider
	if provider == "" && req.AuthMethod == "external_idp" {
		provider = "AzureAD"
	}
	// Reuse a pasted record's id when it does not collide; otherwise mint a fresh
	// one so re-importing a backup never creates a duplicate entry.
	id := req.ID
	if id == "" || config.AccountIDExists(id) {
		id = auth.GenerateAccountID()
	}
	account := config.Account{
		ID:            id,
		Email:         email,
		AccessToken:   accessToken,
		RefreshToken:  req.RefreshToken,
		ClientID:      req.ClientID,
		ClientSecret:  req.ClientSecret,
		AuthMethod:    req.AuthMethod,
		Provider:      provider,
		Region:        req.Region,
		ExpiresAt:     expiresAt,
		Enabled:       true,
		MachineId:     config.GenerateMachineId(),
		ProfileArn:    profileArn,
		TokenEndpoint: req.TokenEndpoint,
		IssuerURL:     req.IssuerURL,
		Scopes:        req.Scopes,
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}

// externalIdpAuthMethodAliases are lower-cased authMethod values (or Kiro Account
// Manager provider labels) that mean "external IdP / enterprise SSO" and must
// normalize to "external_idp".
var externalIdpAuthMethodAliases = map[string]bool{
	"external_idp": true,
	"azuread":      true,
	"azure":        true,
	"entra":        true,
	"entra-id":     true,
	"entra_id":     true,
	"microsoft":    true,
	"m365":         true,
	"office365":    true,
	"external":     true,
}

// normalizeImportAuthMethod maps a pasted credential JSON's authMethod (plus its
// clientId/clientSecret/tokenEndpoint) onto one of the three canonical methods
// ("external_idp" | "idc" | "social"). external_idp MUST be detected before the
// clientId+clientSecret→idc inference, because external_idp accounts carry clientId
// but NO clientSecret, so the old default branch misclassified them as "social" and
// refresh hit the wrong endpoint.
//
// It preserves the pre-existing idc/social heuristics:
//   - empty authMethod + clientId present             -> idc
//   - empty authMethod, no clientId                   -> social
//   - "enterprise" (Kiro Account Manager IdC label)   -> idc
//   - unrecognized non-empty + clientId+clientSecret  -> idc, else social
func normalizeImportAuthMethod(authMethod, clientID, clientSecret, tokenEndpoint string) string {
	am := strings.ToLower(strings.TrimSpace(authMethod))
	switch {
	case externalIdpAuthMethodAliases[am]:
		return "external_idp"
	case tokenEndpoint != "": // infer when not declared explicitly
		return "external_idp"
	case am == "social" || am == "google" || am == "github":
		return "social"
	case am == "idc" || am == "builderid" || am == "enterprise":
		return "idc"
	}
	if am == "" {
		if clientID != "" {
			return "idc"
		}
		return "social"
	}
	if clientID != "" && clientSecret != "" {
		return "idc"
	}
	return "social"
}
