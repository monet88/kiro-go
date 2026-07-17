package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"strings"
)

// streamAssistantFromKiro performs the upstream Kiro stream for one Account
// attempt, feeding semantic events through an attempt-local Assistant Normalizer.
// onEvent is invoked for each Assistant Event in order. Returning a non-nil
// error aborts the stream. Model Output Errors are delivered as Assistant Events
// and also returned as the function error so callers can classify routing.
func streamAssistantFromKiro(
	ctx context.Context,
	account *config.Account,
	payload *KiroPayload,
	tools *declaredToolSet,
	onEvent func(assistantEvent) error,
) error {
	if onEvent == nil {
		onEvent = func(assistantEvent) error { return nil }
	}
	normalizer := newAssistantNormalizer(tools)
	return callKiroAPIWithSemanticConsumer(ctx, account, payload, func(ev kiroSemanticEvent) error {
		for _, aev := range normalizer.handle(ev) {
			if err := onEvent(aev); err != nil {
				return err
			}
			if aev.kind == assistantKindModelOutputError {
				return aev.err
			}
		}
		return nil
	})
}

// callKiroAPIWithSemanticConsumer shares endpoint/auth transport with CallKiroAPI
// but delivers Kiro Semantic Events instead of the legacy callback surface.
func callKiroAPIWithSemanticConsumer(
	ctx context.Context,
	account *config.Account,
	payload *KiroPayload,
	onSemantic func(kiroSemanticEvent) error,
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if onSemantic == nil {
		onSemantic = func(kiroSemanticEvent) error { return nil }
	}

	originalProfileArn := ""
	if payload != nil {
		originalProfileArn = payload.ProfileArn
		defer func() { payload.ProfileArn = originalProfileArn }()
	}
	setPayloadProfileArnForAccount(payload, account)

	if logger.GetLevel() <= logger.LevelDebug {
		if payloadJSON, err := json.Marshal(payload); err == nil {
			logger.Debugf("[KiroAPI] Request payload: %s", string(payloadJSON))
		}
	}

	if account != nil && account.IsApiKeyCredential() {
		return callKiroDevWithSemanticConsumer(ctx, account, payload, onSemantic)
	}

	if payload != nil && strings.TrimSpace(payload.ProfileArn) == "" {
		if profileArn, err := ResolveProfileArn(account); err == nil {
			payload.ProfileArn = profileArn
		} else if isProfileArnResolutionSoftError(err) {
			logger.Debugf("[ProfileArn] Skipped profile ARN resolution for %s: %v", accountEmailForLog(account), err)
		} else {
			logger.Warnf("[ProfileArn] Failed to resolve profile ARN for %s: %v", accountEmailForLog(account), err)
		}
	}

	endpoints := getSortedEndpoints(config.GetPreferredEndpoint())
	var lastErr error
	for _, ep := range endpoints {
		payload.ConversationState.CurrentMessage.UserInputMessage.Origin = ep.Origin
		epURL := regionalizeURLForProfile(ep.URL, account, payload.ProfileArn)
		reqBody, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, "POST", epURL, bytes.NewReader(reqBody))
		if err != nil {
			lastErr = err
			continue
		}
		setKiroStreamingRequestHeaders(req, account, ep.AmzTarget, epURL)
		resp, err := GetClientForProxy(ResolveAccountProxyURL(account)).Do(req)
		if err != nil {
			lastErr = err
			logger.Warnf("[KiroAPI] Endpoint %s failed: %v", ep.Name, err)
			if !isClientDisconnectError(ctx, err) {
				recordUpstreamErrorProbe(ep.Name, "connect", 0, currentMessageModelID(payload), account, err.Error())
			}
			continue
		}
		if resp.StatusCode == 429 {
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			body := strings.TrimSpace(string(errBody))
			lastErr = &KiroAPIError{StatusCode: resp.StatusCode, Endpoint: ep.Name, Body: body}
			entry := recordKiro429ProbeLog(ep.Name, account, body)
			logger.Warnf("[Kiro429Probe] endpoint=%q class=%q accountID=%q email=%q tier=%q bodyBytes=%d retainedFor=%s", entry.Endpoint, entry.Class, entry.AccountID, entry.Email, entry.Tier, len(body), kiro429ProbeTTL)
			logger.Warnf("[KiroAPI] Endpoint %s quota exhausted (429), trying next endpoint", ep.Name)
			continue
		}
		if resp.StatusCode != 200 {
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			body := strings.TrimSpace(string(errBody))
			lastErr = &KiroAPIError{StatusCode: resp.StatusCode, Endpoint: ep.Name, Body: body}
			recordUpstreamErrorProbe(ep.Name, "response", resp.StatusCode, currentMessageModelID(payload), account, body)

			if resp.StatusCode == 403 && payload != nil && strings.TrimSpace(payload.ProfileArn) != "" && isInvalidBearerTokenBody(body) {
				logger.Warnf("[KiroAPI] Endpoint %s rejected profileArn for %s; retrying without profile on default data-plane", ep.Name, accountEmailForLog(account))
				clearAccountProfileArn(account)
				payload.ProfileArn = ""
				epURL = regionalizeURLForProfile(ep.URL, account, "")
				reqBody, marshalErr := json.Marshal(payload)
				if marshalErr != nil {
					return marshalErr
				}
				retryReq, retryErr := http.NewRequestWithContext(ctx, "POST", epURL, bytes.NewReader(reqBody))
				if retryErr != nil {
					lastErr = retryErr
					continue
				}
				setKiroStreamingRequestHeaders(retryReq, account, ep.AmzTarget, epURL)
				retryResp, retryDoErr := GetClientForProxy(ResolveAccountProxyURL(account)).Do(retryReq)
				if retryDoErr != nil {
					lastErr = retryDoErr
					if !isClientDisconnectError(ctx, retryDoErr) {
						recordUpstreamErrorProbe(ep.Name, "connect", 0, currentMessageModelID(payload), account, retryDoErr.Error())
					}
					continue
				}
				if retryResp.StatusCode == 200 {
					err = consumeEventStream(ctx, retryResp.Body, onSemantic)
					retryResp.Body.Close()
					if err != nil && !isClientDisconnectError(ctx, err) && !IsModelOutputError(err) {
						recordUpstreamErrorProbe(ep.Name, "stream", 200, currentMessageModelID(payload), account, err.Error())
					}
					return err
				}
				retryBody, _ := io.ReadAll(retryResp.Body)
				retryResp.Body.Close()
				body = strings.TrimSpace(string(retryBody))
				lastErr = &KiroAPIError{StatusCode: retryResp.StatusCode, Endpoint: ep.Name, Body: body}
				recordUpstreamErrorProbe(ep.Name, "response", retryResp.StatusCode, currentMessageModelID(payload), account, body)
				if retryResp.StatusCode == 401 || retryResp.StatusCode == 403 || retryResp.StatusCode == 402 {
					return lastErr
				}
				logger.Warnf("[KiroAPI] Endpoint %s error after profile-less retry: %v", ep.Name, lastErr)
				continue
			}
			if resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 402 {
				return lastErr
			}
			logger.Warnf("[KiroAPI] Endpoint %s error: %v", ep.Name, lastErr)
			continue
		}
		err = consumeEventStream(ctx, resp.Body, onSemantic)
		resp.Body.Close()
		if err != nil && !isClientDisconnectError(ctx, err) && !IsModelOutputError(err) {
			recordUpstreamErrorProbe(ep.Name, "stream", 200, currentMessageModelID(payload), account, err.Error())
		}
		return err
	}
	if lastErr != nil {
		return lastErr
	}
	return &KiroAPIError{StatusCode: 503, Endpoint: "none", Body: "no available endpoints"}
}

func callKiroDevWithSemanticConsumer(
	ctx context.Context,
	account *config.Account,
	payload *KiroPayload,
	onSemantic func(kiroSemanticEvent) error,
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if payload == nil {
		return fmt.Errorf("payload is nil")
	}
	payload.ProfileArn = ""
	payload.ConversationState.CurrentMessage.UserInputMessage.Origin = "AI_EDITOR"

	epURL := kiroDevRuntimeGenerateURL(account)
	reqBody, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", epURL, bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	setKiroDevAPIKeyStreamingHeaders(req, account, epURL)

	resp, err := GetClientForProxy(ResolveAccountProxyURL(account)).Do(req)
	if err != nil {
		if !isClientDisconnectError(ctx, err) {
			recordUpstreamErrorProbe("Kiro Runtime", "connect", 0, currentMessageModelID(payload), account, err.Error())
		}
		return err
	}
	if resp.StatusCode != 200 {
		errBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		body := strings.TrimSpace(string(errBody))
		recordUpstreamErrorProbe("Kiro Runtime", "response", resp.StatusCode, currentMessageModelID(payload), account, body)
		if resp.StatusCode == 429 {
			_ = recordKiro429ProbeLog("Kiro Runtime", account, body)
		}
		return &KiroAPIError{StatusCode: resp.StatusCode, Endpoint: "Kiro Runtime", Body: body}
	}
	err = consumeEventStream(ctx, resp.Body, onSemantic)
	resp.Body.Close()
	if err != nil && !isClientDisconnectError(ctx, err) && !IsModelOutputError(err) {
		recordUpstreamErrorProbe("Kiro Runtime", "stream", 200, currentMessageModelID(payload), account, err.Error())
	}
	return err
}
