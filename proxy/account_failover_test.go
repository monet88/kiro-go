package proxy

import (
	"strings"
	"testing"
)

func TestAccountFailureClassifiers(t *testing.T) {
	tests := []struct {
		name string
		fn   func(string) bool
		msg  string
	}{
		{name: "quota", fn: isQuotaErrorMessage, msg: "HTTP 429: quota exhausted"},
		{name: "overage", fn: isOverageErrorMessage, msg: "HTTP 402 from Kiro IDE: OVERAGE limit exceeded"},
		{name: "suspension", fn: isSuspensionErrorMessage, msg: "Your User ID temporarily is suspended"},
		{name: "profile", fn: isProfileUnavailableErrorMessage, msg: "no available Kiro profile"},
		{name: "auth", fn: isAuthErrorMessage, msg: "Authentication failed - token invalid or expired"},
		{name: "bearer invalid", fn: isAuthErrorMessage, msg: `HTTP 403 from Kiro IDE: {"message":"The bearer token included in the request is invalid.","reason":null}`},
	}

	for _, tc := range tests {
		if !tc.fn(tc.msg) {
			t.Fatalf("%s classifier did not match %q", tc.name, tc.msg)
		}
	}
}

func TestIsInvalidBearerTokenBody(t *testing.T) {
	if !isInvalidBearerTokenBody(`{"message":"The bearer token included in the request is invalid.","reason":null}`) {
		t.Fatal("expected invalid bearer body to match")
	}
	if isInvalidBearerTokenBody(`{"message":"Your subscription does not support this application."}`) {
		t.Fatal("subscription rejection must not match invalid bearer helper")
	}
}

func TestProfileUnavailableIsSoftAndNotAuth(t *testing.T) {
	msg := "no available Kiro profile"
	if !isProfileUnavailableErrorMessage(msg) {
		t.Fatal("expected profile unavailable classifier")
	}
	// Soft path: empty profile must not hard-fail ensureRestProfileArn callers.
	if !isProfileArnResolutionSoftError(errString(msg)) {
		t.Fatal("expected empty/no profile to be soft for REST helpers")
	}
	// Case-insensitive soft match (upstream / wrapped errors vary casing).
	if !isProfileArnResolutionSoftError(errString("No Available Kiro Profile")) {
		t.Fatal("expected case-insensitive soft match for no available profile")
	}
	if !isProfileArnResolutionSoftError(errString("EMPTY PROFILE LIST from listAvailableProfiles")) {
		t.Fatal("expected case-insensitive soft match for empty profile list")
	}
}

func TestIsTransientCredentialRefreshError(t *testing.T) {
	transient := []string{
		`Post "https://oidc.us-east-1.amazonaws.com/token": dial tcp: lookup oidc.us-east-1.amazonaws.com: no such host`,
		"token refresh: i/o timeout",
		"connection reset by peer",
		"refresh failed: HTTP 503 service unavailable",
		"unexpected EOF",
	}
	for _, msg := range transient {
		if !isTransientCredentialRefreshError(errString(msg)) {
			t.Fatalf("expected transient: %q", msg)
		}
	}
	permanent := []string{
		"refresh failed: 400 invalid_grant",
		"token refresh failed: invalid_grant",
		"refresh failed: 401 unauthorized",
		"Social token refresh requires clientId",
		"IDC token endpoint is empty",
	}
	for _, msg := range permanent {
		if isTransientCredentialRefreshError(errString(msg)) {
			t.Fatalf("expected permanent auth failure, got transient: %q", msg)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestIsAuthErrorMessageDoesNotMatchDNSOnly(t *testing.T) {
	msg := `Post "https://q.eu-north-1.amazonaws.com/generateAssistantResponse": dial tcp: lookup q.eu-north-1.amazonaws.com on 127.0.0.11:53: no such host`
	if isAuthErrorMessage(msg) {
		t.Fatal("DNS/host errors must not be classified as auth")
	}
	if !strings.Contains(msg, "eu-north-1") {
		t.Fatal("sanity")
	}
}
