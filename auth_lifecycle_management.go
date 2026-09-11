package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"path"
	"strings"
	"time"
)

type lifecycleActionRequest struct {
	AuthIndex         string `json:"auth_index"`
	Version           int64  `json:"version"`
	Action            string `json:"action"`
	ConfirmModelProbe bool   `json:"confirm_model_probe"`
	ProbeModel        string `json:"probe_model,omitempty"`
}

func handleLifecycleAction(req managementRequest) managementResponse {
	if req.Method != http.MethodPost {
		return jsonResponse(http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
	}
	var action lifecycleActionRequest
	if json.Unmarshal(req.Body, &action) != nil || action.AuthIndex == "" || action.Version < 1 {
		return jsonResponse(http.StatusBadRequest, map[string]any{"error": "auth_index and current version are required"})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	s, err := globalAuthLifecycle.action(ctx, action)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, errLifecycleConflict) {
			status = http.StatusConflict
		}
		if errors.Is(err, errLifecycleAction) {
			status = http.StatusBadRequest
		}
		return jsonResponse(status, map[string]any{"error": err.Error()})
	}
	return jsonResponse(http.StatusOK, map[string]any{"account": s})
}

var errLifecycleAction = errors.New("invalid lifecycle action or required probe acknowledgement missing")

func (c *authLifecycleController) action(ctx context.Context, req lifecycleActionRequest) (authLifecycleState, error) {
	var s authLifecycleState
	if err := lockMutexWithContext(ctx, &c.opMu); err != nil {
		return s, err
	}
	defer c.opMu.Unlock()
	if !tryAcquireQuotaProbeGate() {
		return s, errors.New("quota operation in progress; retry after completion")
	}
	defer releaseQuotaProbeGate()
	if c.host == nil {
		return s, errors.New("lifecycle controller is not initialized")
	}
	db, _, err := c.store.open(ctx)
	if err != nil {
		return s, errors.New("lifecycle database unavailable")
	}
	s, err = loadLifecycleState(ctx, db, req.AuthIndex)
	if err != nil {
		return s, errors.New("account state unavailable")
	}
	if s.Version != req.Version {
		return s, errLifecycleConflict
	}
	if req.Action == "clear" {
		if s.PendingAction != "" {
			return s, errors.New("pending status write must be reconciled before clearing ownership")
		}
		s.DisabledByPlugin = false
		s.Paused = true
		s.RecoverAt = 0
		s.CheckOK = false
		s.SyncStatus = "cleared"
		s.SyncError = ""
		s.IgnoreBefore = c.clock.Now().Unix()
		if s.Disabled {
			s.State = authManualDisabled
		} else {
			s.State = authHealthy
		}
		s.Reason = "manual_clear"
		err = saveLifecycleState(ctx, db, &s, c.clock.Now(), "manual_clear")
		return s, err
	}
	snapshot, err := c.host.Read(ctx, req.AuthIndex)
	if err != nil {
		return s, err
	}
	if req.Action == "check_and_recover" {
		return c.checkAndRecover(ctx, db, s, snapshot)
	}
	if req.Action == "recheck" {
		// Explicit recheck adopts the *current* exact auth, but never enables it.
		s.AuthID = snapshot.Entry.ID
		s.Name = snapshot.Entry.Name
		s.Identity = snapshot.Identity
		s.Fingerprint = snapshot.Fingerprint
		s.ContentHash = snapshot.ContentHash
		s.RuntimeUpdated = snapshot.Entry.UpdatedAt
		s.Disabled = snapshot.Entry.Disabled
		modelProbe := s.BlockedState == authBillingBlocked || s.BlockedState == authPermissionBlocked
		if modelProbe && !req.ConfirmModelProbe {
			return s, errLifecycleAction
		}
		probeModel := firstNonEmptyString(req.ProbeModel, codexProbeModel)
		if modelProbe && !lifecycleProbeModelAllowed(snapshot.Data, probeModel) {
			return s, errors.New("probe model is excluded by this auth; select an allowed probe_model")
		}
		account := triggerAuthAccount{configuredAccount: configuredAccount{AuthID: s.AuthID, AuthIndex: s.AuthIndex, AuthFile: s.Name, Provider: "codex"}, AccessToken: lifecycleString(snapshot.Data, "access_token"), ChatGPTAccountID: lifecycleString(snapshot.Data, "account_id")}
		var run quotaTriggerRun
		if modelProbe {
			run = executeQuotaProbeRequestWithModel(ctx, db, account, defaultPluginConfig(), probeModel)
		} else {
			run = executeQuotaUsageRequest(ctx, db, account, defaultPluginConfig())
		}
		after, readErr := c.host.Read(ctx, req.AuthIndex)
		if readErr != nil {
			return s, readErr
		}
		if snapshot.ContentHash != after.ContentHash || snapshot.Entry.UpdatedAt != after.Entry.UpdatedAt || snapshot.Entry.Disabled != after.Entry.Disabled || after.Entry.Disabled != after.FileDisabled {
			c.conflict(ctx, db, &s, after, "auth changed during recheck")
			return s, errLifecycleConflict
		}
		s.CheckedAt = c.clock.Now().Unix()
		s.CheckOK = successfulStatusCode(run.HTTPStatus)
		if s.State == authQuotaCooldown || s.BlockedState == authQuotaCooldown || s.State == authRateLimited || s.BlockedState == authRateLimited {
			s.CheckOK = s.CheckOK && lifecycleQuotaAvailable(run, c.clock.Now())
		}
		s.LastHTTPStatus = run.HTTPStatus
		s.PendingAction = ""
		s.PendingOwner = false
		s.RetryAt = 0
		s.SyncStatus = "rechecked"
		s.SyncError = ""
		if s.CheckOK {
			s.CheckResult = "passed"
			s.LastErrorMessage = "probe succeeded; explicit enable required"
		} else {
			s.CheckResult = "not_available"
			s.LastErrorMessage = "probe failed; account remains unchanged"
			d := classifyAuthFailure(usageRecord{Failed: true, Failure: usageFailure{StatusCode: run.HTTPStatus, Body: run.FailureBody}, ResponseHeaders: run.ResponseHeaders}, c.clock.Now())
			if d.Disable {
				s.BlockedState = d.State
			}
		}
		// No automatic ownership is granted by a probe.
		s.DisabledByPlugin = false
		s.Paused = true
		if err = saveLifecycleState(ctx, db, &s, c.clock.Now(), "manual_recheck"); err != nil {
			return s, err
		}
		return s, nil
	}
	if req.Action != "enable" && req.Action != "disable" {
		return s, errLifecycleAction
	}
	if snapshotChanged(s, snapshot) {
		c.conflict(ctx, db, &s, snapshot, "auth changed before manual action")
		return s, errLifecycleConflict
	}
	needsRecheck := s.LastHTTPStatus >= 400 || s.BlockedState != "" || s.SyncStatus == "conflict"
	if req.Action == "enable" && needsRecheck && (!s.CheckOK || c.clock.Now().Unix()-s.CheckedAt > 300) {
		return s, errors.New("successful fresh Recheck is required before enabling a blocked account")
	}
	if req.Action == "disable" {
		s.State = authManualDisabled
		s.Reason = "manual_disabled"
		s.Paused = true
		s.DisabledByPlugin = false
		s.RecoverAt = 0
	}
	err = c.transition(ctx, db, &s, snapshot, req.Action == "disable", false)
	return s, err
}

// Only an explicit user action can query quota for recovery. No background
// caller, cached dashboard percentage, or successful in-flight usage can enable.
func (c *authLifecycleController) checkAndRecover(ctx context.Context, db *sql.DB, s authLifecycleState, snapshot lifecycleSnapshot) (authLifecycleState, error) {
	if !nativeScheduling() || !s.Disabled || !s.DisabledByPlugin || s.Paused || s.PendingAction != "" || (s.State != authQuotaCooldown && s.State != authRateLimited) {
		return s, errLifecycleAction
	}
	if snapshotChanged(s, snapshot) {
		c.conflict(ctx, db, &s, snapshot, "auth changed before quota recovery")
		return s, errLifecycleConflict
	}
	if !c.host.Ready() {
		return s, errors.New("management API environment is not configured")
	}
	pending, err := pendingLifecycleEvents(ctx, db, s)
	if err != nil {
		return s, err
	}
	if pending {
		return s, errors.New("account failure events pending; retry after reconciliation")
	}
	// Suspend an old timer before the network call. A timeout, cancellation or
	// failed read-back must not leave a due timer able to enable the account.
	s.CheckOK, s.CheckResult, s.RecoverAt = false, "incomplete", 0
	if err = saveLifecycleState(ctx, db, &s, c.clock.Now(), "manual_quota_check_started"); err != nil {
		return s, err
	}
	account := triggerAuthAccount{configuredAccount: configuredAccount{AuthID: s.AuthID, AuthIndex: s.AuthIndex, AuthFile: s.Name, Provider: "codex"}, AccessToken: lifecycleString(snapshot.Data, "access_token"), ChatGPTAccountID: lifecycleString(snapshot.Data, "account_id")}
	run := executeQuotaUsageRequest(ctx, db, account, defaultPluginConfig())
	after, err := c.host.Read(ctx, s.AuthIndex)
	if err != nil {
		s.CheckResult = "request_failed"
		_ = saveLifecycleState(ctx, db, &s, c.clock.Now(), "manual_quota_check_failed")
		return s, errors.New("quota check auth read-back failed")
	}
	if snapshotChanged(s, after) {
		c.conflict(ctx, db, &s, after, "auth changed during quota recovery")
		return s, errLifecycleConflict
	}
	s.CheckedAt, s.CheckOK = c.clock.Now().Unix(), successfulStatusCode(run.HTTPStatus) && lifecycleQuotaAvailable(run, c.clock.Now())
	s.CheckResult = "not_available"
	s.RetryAt, s.RecoverAt = 0, 0
	if !successfulStatusCode(run.HTTPStatus) {
		s.CheckResult = "request_failed"
		d := classifyAuthFailure(usageRecord{Failed: true, Failure: usageFailure{StatusCode: run.HTTPStatus, Body: run.FailureBody}, ResponseHeaders: run.ResponseHeaders}, c.clock.Now())
		if d.Disable && (lifecycleFailurePriority(d.State) >= lifecycleFailurePriority(s.State)) {
			s.State, s.BlockedState, s.Reason, s.RecoverAt = d.State, d.State, d.Reason, d.RecoverAt
			s.LastHTTPStatus, s.LastErrorCode, s.LastErrorType, s.LastErrorMessage = d.Status, d.ErrorCode, d.ErrorType, d.Reason
		}
	} else if !s.CheckOK {
		// Only complete, valid window evidence can supply a new recovery time.
		quota := quotaActivationQuotaFromRun(run)
		valid, exhausted := true, false
		for _, window := range []quotaActivationWindow{quota.Primary, quota.Secondary} {
			if window.Presence == quotaWindowAbsent {
				continue
			}
			if !activationWindowMetadataValid(window, c.clock.Now().Unix()) || window.UsedPercent == nil {
				valid = false
				continue
			}
			exhausted = exhausted || *window.UsedPercent >= 100
		}
		if valid && exhausted {
			d := classifyAuthFailure(usageRecord{Failed: true, Failure: usageFailure{StatusCode: 429, Body: `{"error":{"type":"usage_limit_reached"}}`}, ResponseHeaders: run.ResponseHeaders}, c.clock.Now())
			s.State, s.BlockedState, s.Reason, s.RecoverAt = d.State, d.State, d.Reason, d.RecoverAt
			s.CheckResult = "quota_exhausted"
		}
	}
	pending, err = pendingLifecycleEvents(ctx, db, s)
	if err != nil {
		return s, err
	}
	if pending {
		s.CheckOK, s.CheckResult = false, "pending_failure"
	}
	if s.CheckOK {
		s.CheckResult = "passed"
	}
	if err = saveLifecycleState(ctx, db, &s, c.clock.Now(), "manual_quota_recovery_check"); err != nil {
		return s, err
	}
	if s.CheckOK {
		err = c.transition(ctx, db, &s, after, false, false)
	}
	return s, err
}

func lifecycleProbeModelAllowed(data map[string]json.RawMessage, model string) bool {
	if strings.TrimSpace(model) == "" || len(model) > 256 {
		return false
	}
	for _, key := range []string{"excluded_models", "excluded-models"} {
		raw, exists := data[key]
		if !exists {
			continue
		}
		var patterns []string
		if json.Unmarshal(raw, &patterns) != nil {
			return false
		}
		for _, pattern := range patterns {
			match, err := path.Match(pattern, model)
			if err != nil || match {
				return false
			}
		}
	}
	return true
}

// Old routes remain callable with explicit current preconditions. Unversioned
// release-all must not acquire the new and stronger power of enabling CPA auths.
func handleLifecycleCompatibility(req managementRequest) managementResponse {
	var action lifecycleActionRequest
	if json.Unmarshal(req.Body, &action) != nil || action.AuthIndex == "" || action.Version < 1 {
		return jsonResponse(http.StatusConflict, map[string]any{"error": "refresh_required", "message": "Native mode requires auth_index and current lifecycle version; use auth-states/action. No auth or plugin state was changed."})
	}
	if strings.HasSuffix(req.Path, "/autobans/release") {
		action.Action = "enable"
	} else if action.Action == "" {
		action.Action = "recheck"
	}
	req.Body, _ = json.Marshal(action)
	return handleLifecycleAction(req)
}

func applyLifecycleSummary(ctx context.Context, db *sql.DB, accounts []accountRow) error {
	states, err := listLifecycleStates(ctx, db)
	if err != nil {
		return err
	}
	byIndex := map[string]authLifecycleState{}
	for _, s := range states {
		byIndex[s.AuthIndex] = s
	}
	for i := range accounts {
		r := &accounts[i]
		if state, ok := byIndex[r.AuthIndex]; ok && state.AuthID == r.AuthID {
			copy := state
			r.Lifecycle = &copy
		}
	}
	return nil
}

// This list is independent of usage-window pagination: disabled credentials
// must remain visible even if they have no requests in the selected window.
func queryLifecycleAutobans(ctx context.Context, db *sql.DB, now int64) ([]autobanRow, error) {
	states, err := listLifecycleStates(ctx, db)
	if err != nil {
		return nil, err
	}
	rows := []autobanRow{}
	for _, s := range states {
		if !s.Disabled && s.PendingAction == "" && s.SyncStatus != "conflict" && (s.State == authHealthy || s.State == authManualDisabled) {
			continue
		}
		if !s.Disabled && s.PendingAction == "" && s.State == authRateLimited && s.RecoverAt > 0 && s.RecoverAt <= now {
			continue
		}
		copy := s
		r := autobanRow{Lifecycle: &copy, AuthID: s.AuthID, AuthIndex: s.AuthIndex, AuthFile: s.Name, Source: s.Name, Provider: "codex", Active: true, Window: s.State, Reason: s.Reason, BannedAt: s.DisabledAt, ResetAt: s.RecoverAt, SecondsRemaining: -1}
		state := s.State
		if state == authManualDisabled && s.BlockedState != "" {
			state = s.BlockedState
		}
		switch state {
		case authInvalid:
			r.Window, r.LastStatusCode = "401", 401
		case authBillingBlocked:
			r.Window, r.LastStatusCode = "402", 402
		case authPermissionBlocked:
			r.Window, r.LastStatusCode = "403", 403
		case authQuotaCooldown:
			r.Window, r.LastStatusCode = "quota", 429
		case authRateLimited:
			r.Window, r.LastStatusCode = "429", 429
		}
		if r.BannedAt > 0 {
			r.BannedAtText = unixTime(r.BannedAt)
		}
		if r.ResetAt > 0 {
			r.ResetAtText = unixTime(r.ResetAt)
			r.SecondsRemaining = r.ResetAt - now
			if r.SecondsRemaining < 0 {
				r.SecondsRemaining = 0
			}
		}
		rows = append(rows, r)
	}
	return rows, nil
}
