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
