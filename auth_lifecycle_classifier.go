package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type authDecision struct {
	State      string `json:"state"`
	Disable    bool   `json:"disable"`
	Reason     string `json:"reason"`
	RecoverAt  int64  `json:"recover_at"`
	Status     int    `json:"status"`
	ErrorType  string `json:"error_type"`
	ErrorCode  string `json:"error_code"`
	Model      string `json:"model"`
	ModelIssue bool   `json:"model_issue"`
}

// Whitelist machine-readable classifications; never persist provider message/body.
func classifyAuthFailure(rec usageRecord, now time.Time) authDecision {
	d := authDecision{State: authHealthy, Status: rec.Failure.StatusCode, Model: rec.Model}
	if !rec.Failed {
		return d
	}
	var body map[string]any
	decoder := json.NewDecoder(strings.NewReader(rec.Failure.Body))
	decoder.UseNumber()
	_ = decoder.Decode(&body)
	e := mapFromAny(body["error"])
	if e == nil {
		e = body
	}
	typ, _ := e["type"].(string)
	code, _ := e["code"].(string)
	known := func(v string) string {
		switch v {
		case "usage_limit_reached", "rate_limit_exceeded", "model_not_allowed", "model_not_found", "model_access_denied", "permission_denied", "deactivated_workspace", "workspace_deactivated", "account_deactivated", "account_disabled", "workspace_disabled", "invalid_api_key", "invalid_token", "refresh_token_invalidated":
			return v
		}
		return ""
	}
	d.ErrorType, d.ErrorCode = known(typ), known(code)
	signal := strings.ToLower(typ + " " + code)
	message, _ := e["message"].(string)
	switch d.Status {
	case 401:
		d.State, d.Disable, d.Reason = authInvalid, true, "auth_invalid"
	case 402:
		d.State, d.Disable, d.Reason = authBillingBlocked, true, "billing_blocked"
	case 403:
		text := strings.ToLower(signal + " " + message)
		if strings.Contains(text, "model_not_allowed") || strings.Contains(text, "model_access_denied") || strings.Contains(text, "model permission") || strings.Contains(text, "model access") {
			d.ModelIssue, d.Reason = true, "model_permission_rejected"
		} else if strings.Contains(signal, "deactivated_workspace") || strings.Contains(signal, "workspace_deactivated") || strings.Contains(signal, "account_deactivated") || strings.Contains(signal, "account_disabled") || strings.Contains(signal, "workspace_disabled") {
			d.State, d.Disable, d.Reason = authPermissionBlocked, true, "account_permission_blocked"
		} else {
			d.ModelIssue, d.Reason = true, "unclassified_forbidden"
		}
	case 429:
		quota := strings.Contains(signal, "usage_limit_reached")
		unknownReset := false
		for _, prefix := range []string{"primary", "secondary"} {
			pct := headerFloat(rec.ResponseHeaders, "x-codex-"+prefix+"-used-percent")
			if pct == nil || *pct < 100 {
				continue
			}
			quota = true
			reset := headerInt(rec.ResponseHeaders, "x-codex-"+prefix+"-reset-at")
			if reset == nil || normalizeUnixSeconds(*reset) <= now.Unix() {
				unknownReset = true
				continue
			}
			if t := normalizeUnixSeconds(*reset); t > d.RecoverAt {
				d.RecoverAt = t
			}
		}
		if quota {
			d.State, d.Disable, d.Reason = authQuotaCooldown, true, "usage_limit_reached"
			if reset, ok := lifecycleReset(e, now); ok && reset > d.RecoverAt {
				d.RecoverAt = reset
			}
			if unknownReset {
				d.RecoverAt = 0
			}
		} else {
			d.State, d.Disable, d.Reason = authRateLimited, true, "rate_limited"
			v := headerValue(rec.ResponseHeaders, "Retry-After")
			if seconds, err := strconv.ParseInt(v, 10, 64); err == nil && seconds >= 0 && seconds < 31536000 {
				d.RecoverAt = now.Unix() + seconds
			} else if t, err := http.ParseTime(v); err == nil && t.After(now) {
				d.RecoverAt = t.Unix()
			}
			if d.RecoverAt == 0 {
				if seconds, ok := exactInt64FromAny(e["retry_after"]); ok && seconds >= 0 && seconds < 31536000 {
					d.RecoverAt = now.Unix() + seconds
				}
			}
			if reset, ok := lifecycleReset(e, now); ok && reset > d.RecoverAt {
				d.RecoverAt = reset
			}
			if d.RecoverAt == 0 {
				// A short local retry policy, not a claimed quota reset time.
				d.RecoverAt = now.Add(time.Minute).Unix()
				d.Reason = "rate_limit_backoff"
			}
		}
	default:
		d.Reason = "transient_failure"
	}
	return d
}

// CPA can reject an expired Codex credential while refreshing it before an
// upstream request is dispatched. That path does not always produce a Usage
// event, but the terminal error is exposed on the exact runtime auth entry.
// Only consume the machine-readable terminal code; never infer a permanent
// credential failure from generic status text or persist the provider message.
func classifyRuntimeAuthFailure(entry hostAuthFileEntry) (authDecision, bool) {
	signal := strings.ToLower(entry.StatusMessage)
	if !strings.Contains(signal, "refresh_token_invalidated") {
		return authDecision{}, false
	}
	return authDecision{
		State:     authInvalid,
		Disable:   true,
		Reason:    "auth_invalid",
		Status:    http.StatusUnauthorized,
		ErrorCode: "refresh_token_invalidated",
	}, true
}

func lifecycleReset(window map[string]any, now time.Time) (int64, bool) {
	for _, key := range []string{"resets_at", "reset_at", "resetAt"} {
		if raw, exists := window[key]; exists {
			n, ok := exactInt64FromAny(raw)
			if !ok {
				return 0, false
			}
			n = normalizeUnixSeconds(n)
			return n, n > now.Unix()
		}
	}
	for _, key := range []string{"resets_in_seconds", "reset_after_seconds", "resetAfterSeconds", "reset_in", "resetIn"} {
		if raw, exists := window[key]; exists {
			n, ok := exactInt64FromAny(raw)
			if !ok || n < 0 || n > 31536000 {
				return 0, false
			}
			return now.Unix() + n, true
		}
	}
	return 0, false
}
