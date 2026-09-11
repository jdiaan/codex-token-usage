package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"path"
	"sort"
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

// reloginRequiredAccount is the deliberately small public representation of a
// plugin-owned Codex credential that cannot recover until it is signed in
// again. Do not add lifecycle internals here: this endpoint is intended as a
// stable integration contract.
type reloginRequiredAccount struct {
	AuthIndex  string `json:"auth_index"`
	AuthID     string `json:"auth_id"`
	Name       string `json:"name"`
	State      string `json:"state"`
	HTTPStatus int    `json:"http_status"`
	Reason     string `json:"reason"`
	DisabledAt int64  `json:"disabled_at"`
	Version    int64  `json:"version"`
}

type reloginRequiredAccountsResponse struct {
	GeneratedAt string                   `json:"generated_at"`
	Count       int                      `json:"count"`
	Accounts    []reloginRequiredAccount `json:"accounts"`
}

func lifecycleDisplayState(s authLifecycleState) string {
	if s.State == authManualDisabled && s.BlockedState != "" {
		return s.BlockedState
	}
	return s.State
}

// lifecycleRequiresRelogin defines the same confirmed, non-timed lifecycle
// states the dashboard presents as "等待重新登录". Pending or unhealthy sync
// state is intentionally excluded: an external caller must not act on a
// disable that CPA has not confirmed.
func lifecycleRequiresRelogin(s authLifecycleState) bool {
	if !s.Disabled || !s.DisabledByPlugin || s.ManualDisabled || s.Paused ||
		s.PendingAction != "" || s.SyncError != "" || s.SyncStatus != "synced" || s.RecoverAt != 0 {
		return false
	}
	switch lifecycleDisplayState(s) {
	case authInvalid, authBillingBlocked, authPermissionBlocked:
		return true
	default:
		return false
	}
}

func lifecycleHTTPStatus(s authLifecycleState) int {
	switch lifecycleDisplayState(s) {
	case authInvalid:
		return http.StatusUnauthorized
	case authBillingBlocked:
		return http.StatusPaymentRequired
	case authPermissionBlocked:
		return http.StatusForbidden
	case authQuotaCooldown, authRateLimited:
		return http.StatusTooManyRequests
	default:
		return s.LastHTTPStatus
	}
}

func queryReloginRequiredAccounts(ctx context.Context, db *sql.DB) ([]reloginRequiredAccount, error) {
	states, err := listLifecycleStates(ctx, db)
	if err != nil {
		return nil, err
	}
	accounts := make([]reloginRequiredAccount, 0, len(states))
	for _, s := range states {
		if !lifecycleRequiresRelogin(s) {
			continue
		}
		accounts = append(accounts, reloginRequiredAccount{
			AuthIndex:  s.AuthIndex,
			AuthID:     s.AuthID,
			Name:       s.Name,
			State:      lifecycleDisplayState(s),
			HTTPStatus: lifecycleHTTPStatus(s),
			Reason:     s.Reason,
			DisabledAt: s.DisabledAt,
			Version:    s.Version,
		})
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].AuthIndex < accounts[j].AuthIndex })
	return accounts, nil
}

func handleReloginRequiredAccounts(req managementRequest) managementResponse {
	if req.Method != http.MethodGet {
		return jsonResponse(http.StatusMethodNotAllowed, map[string]any{"error": "method_not_allowed"})
	}
	if !nativeScheduling() {
		return jsonResponse(http.StatusConflict, map[string]any{"error": "unsupported_scheduling_mode"})
	}
	db, _, err := globalStore.open(context.Background())
	if err != nil {
		return jsonResponse(http.StatusInternalServerError, map[string]any{"error": "relogin_accounts_failed", "message": err.Error()})
	}
	accounts, err := queryReloginRequiredAccounts(context.Background(), db)
	if err != nil {
		return jsonResponse(http.StatusInternalServerError, map[string]any{"error": "relogin_accounts_failed", "message": err.Error()})
	}
	return jsonResponse(http.StatusOK, reloginRequiredAccountsResponse{
		GeneratedAt: time.Now().Format(time.RFC3339),
		Count:       len(accounts),
		Accounts:    accounts,
	})
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

var errLifecycleAction = errors.New("invalid lifecycle action")

func (c *authLifecycleController) action(ctx context.Context, req lifecycleActionRequest) (authLifecycleState, error) {
	var s authLifecycleState
	if err := lockMutexWithContext(ctx, &c.opMu); err != nil {
		return s, err
	}
	defer c.opMu.Unlock()
	if !tryAcquireQuotaProbeGate() {
		return s, errors.New("account synchronization in progress; retry shortly")
	}
	defer releaseQuotaProbeGate()
	if c.host == nil {
		return s, errors.New("lifecycle controller is not initialized")
	}
	db, _, err := c.store.open(ctx)
	if err != nil {
		return s, err
	}
	s, err = loadLifecycleState(ctx, db, req.AuthIndex)
	if err != nil {
		return s, err
	}
	if s.Version != req.Version {
		return s, errLifecycleConflict
	}
	snapshot, err := c.host.Read(ctx, s.AuthIndex)
	if err != nil {
		return s, err
	}
	if err = c.observeSnapshot(ctx, db, &s, snapshot); err != nil {
		return s, err
	}
	switch req.Action {
	case "retry_sync", "recheck", "check_and_recover":
		// Compatibility aliases now synchronize local CPA state; never probe upstream.
		entries, listErr := c.host.List(ctx)
		if listErr != nil {
			return s, listErr
		}
		if nativeScheduling() {
			if err = c.processEvents(ctx, db, entries); err != nil {
				return s, err
			}
		}
		s, err = loadLifecycleState(ctx, db, s.AuthIndex)
		if err != nil {
			return s, err
		}
		s.RetryAt = 0
		err = c.syncAccount(ctx, db, &s, snapshot)
		return s, err
	case "enable", "disable":
		clearLifecycleFailure(&s)
		s.ManualDisabled = req.Action == "disable"
		if s.ManualDisabled {
			s.State, s.Reason = authManualDisabled, "manual_disabled"
		}
		returnStateErr := c.transition(ctx, db, &s, snapshot, s.ManualDisabled, false)
		return s, returnStateErr
	case "clear":
		clearLifecycleFailure(&s)
		s.DisabledByPlugin, s.PendingOwner, s.PendingAction = false, false, ""
		s.ManualDisabled = snapshot.FileDisabled
		if s.ManualDisabled {
			s.State, s.Reason = authManualDisabled, "manual_disabled"
		}
		s.IgnoreBefore = c.clock.Now().Unix()
		err = saveLifecycleState(ctx, db, &s, c.clock.Now(), "manual_clear")
		return s, err
	default:
		return s, errLifecycleAction
	}
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
	return queryLifecycleRows(ctx, db, now, false)
}

func queryLifecycleRows(ctx context.Context, db *sql.DB, now int64, pending bool) ([]autobanRow, error) {
	states, err := listLifecycleStates(ctx, db)
	if err != nil {
		return nil, err
	}
	rows := []autobanRow{}
	for _, s := range states {
		confirmed := s.Disabled && s.DisabledByPlugin && s.PendingAction != "disable"
		if s.ManualDisabled {
			continue
		}
		if pending {
			if confirmed || (s.PendingAction == "" && s.SyncError == "") {
				continue
			}
		} else if !confirmed {
			continue
		}
		copy := s
		r := autobanRow{Lifecycle: &copy, AuthID: s.AuthID, AuthIndex: s.AuthIndex, AuthFile: s.Name, Source: s.Name, Provider: "codex", Active: true, Window: s.State, Reason: s.Reason, BannedAt: s.DisabledAt, ResetAt: s.RecoverAt, SecondsRemaining: -1}
		switch lifecycleDisplayState(s) {
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
