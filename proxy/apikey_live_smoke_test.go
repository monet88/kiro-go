package proxy

import (
	"context"
	"fmt"
	"kiro-go/config"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLiveApiKeyKiroDevSmoke(t *testing.T) {
	key := strings.TrimSpace(os.Getenv("KIRO_SMOKE_API_KEY"))
	if key == "" {
		t.Skip("set KIRO_SMOKE_API_KEY to run live smoke")
	}
	region := strings.TrimSpace(os.Getenv("KIRO_SMOKE_REGION"))
	if region == "" {
		region = "eu-central-1"
	}
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	InitKiroHttpClient("")
	account := &config.Account{
		ID:          "smoke-apikey",
		KiroApiKey:  key,
		AccessToken: key,
		AuthMethod:  "api_key",
		Region:      region,
		Enabled:     true,
		MachineId:   "smoke-machine-id",
	}
	// Direct management call first with the same client stack.
	url := kiroDevManagementBase(account) + "/getUsageLimits?origin=AI_EDITOR&resourceType=AGENTIC_REQUEST&isEmailRequired=true"
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("TokenType", "API_KEY")
	req.Header.Set("Accept", "application/json")
	client := GetRestClientForProxy("")
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("direct rest client: %v after %s proxyURL=%q", err, time.Since(start), ResolveAccountProxyURL(account))
	}
	resp.Body.Close()
	t.Logf("direct rest status=%d in %s", resp.StatusCode, time.Since(start))

	info, err := RefreshAccountInfo(account)
	if err != nil {
		t.Fatalf("RefreshAccountInfo: %v", err)
	}
	t.Logf("email=%s sub=%s usage=%.1f/%.1f", info.Email, info.SubscriptionType, info.UsageCurrent, info.UsageLimit)

	models, err := ListAvailableModels(account)
	if err != nil {
		t.Fatalf("ListAvailableModels: %v", err)
	}
	t.Logf("models=%d first=%s", len(models), models[0].ModelId)

	openaiReq := &OpenAIRequest{
		Model:     "auto",
		Messages:  []OpenAIMessage{{Role: "user", Content: "say only hi"}},
		MaxTokens: 8,
		Stream:    false,
	}
	payload := OpenAIToKiro(openaiReq, false)
	var content string
	cb := &KiroStreamCallback{OnText: func(text string, isThinking bool) { content += text }}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err := CallKiroAPI(ctx, account, payload, cb); err != nil {
		t.Fatalf("CallKiroAPI: %v", err)
	}
	if strings.TrimSpace(content) == "" {
		t.Fatalf("empty stream content")
	}
	t.Logf("reply=%q", content)
	_ = fmt.Sprintf
}
