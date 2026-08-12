package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	xaiStateUnauthorized  = "unauthorized"
	xaiStateForbidden     = "forbidden"
	xaiStateFreeExhausted = "free_usage_exhausted"
	xaiStateRateLimited   = "rate_limited"
)

type xaiAccountStateRow struct {
	StateKey         string `json:"state_key"`
	AuthID           string `json:"auth_id"`
	AuthIndex        string `json:"auth_index"`
	Source           string `json:"source"`
	Provider         string `json:"provider"`
	State            string `json:"state"`
	Reason           string `json:"reason"`
	ObservedAt       int64  `json:"observed_at"`
	ObservedAtText   string `json:"observed_at_text"`
	ResetAt          int64  `json:"reset_at"`
	ResetAtText      string `json:"reset_at_text"`
	SecondsRemaining int64  `json:"seconds_remaining"`
	Active           bool   `json:"active"`
	LastStatusCode   int    `json:"last_status_code"`
	AuthFile         string `json:"auth_file,omitempty"`
	AuthFileMTime    int64  `json:"auth_file_mtime,omitempty"`
}

type xaiStateResolveRequest struct {
	Items []xaiStateResolveRequestItem `json:"items"`
}

type xaiStateResolveRequestItem struct {
	StateKey      string `json:"state_key"`
	ExpectedState string `json:"expected_state,omitempty"`
	Action        string `json:"action"`
}

type xaiStateResolveResponse struct {
	Items           []xaiStateResolveResult `json:"items"`
	Resolved        int                     `json:"resolved"`
	AlreadyResolved int                     `json:"already_resolved"`
	ReplacementKept int                     `json:"replacement_kept"`
	Failed          int                     `json:"failed"`
}

type xaiStateResolveResult struct {
	StateKey string `json:"state_key"`
	Status   string `json:"status"`
	Message  string `json:"message,omitempty"`
}

func isXAISchedulerRequest(req schedulerPickRequest) bool {
	if strings.EqualFold(strings.TrimSpace(req.Provider), "xai") {
		return true
	}
	return len(req.Providers) == 1 && strings.EqualFold(strings.TrimSpace(req.Providers[0]), "xai")
}

func xaiStateForRecord(rec usageRecord, status int, now int64) (state, reason string, resetAt int64) {
	if !rec.Failed {
		return "", "", 0
	}
	parsed := parseXAIError(status, rec.Failure.Body)
	switch parsed.Kind {
	case xaiErrorUnauthorized, xaiErrorTokenExpired, xaiErrorTokenRevoked:
		return xaiStateUnauthorized, xaiErrorReason(status, parsed, "xAI credential is invalid"), 0
	case xaiErrorPermissionDenied, xaiErrorAccountUnavailable:
		return xaiStateForbidden, xaiErrorReason(status, parsed, "xAI access is denied"), 0
	case xaiErrorFreeUsageExhausted:
		return xaiStateFreeExhausted, xaiErrorReason(status, parsed, "xAI free usage is exhausted"), now + int64((24 * time.Hour).Seconds())
	case xaiErrorRateLimited:
		return xaiStateRateLimited, xaiErrorReason(status, parsed, "temporary xAI throttling"), xaiRetryAfterUnix(rec.ResponseHeaders, now)
	default:
		return "", "", 0
	}
}

func xaiFreeUsageExhaustedBody(body string) bool {
	return parseXAIError(0, body).Kind == xaiErrorFreeUsageExhausted
}

func xaiErrorReason(status int, parsed xaiParsedError, fallback string) string {
	evidence := firstNonEmptyString(parsed.Code, parsed.Message)
	if parsed.Code != "" && parsed.Message != "" && !strings.EqualFold(parsed.Code, parsed.Message) {
		evidence = parsed.Code + ": " + parsed.Message
	}
	evidence = sanitizeTriggerError(evidence)
	if evidence == "" {
		evidence = fallback
	}
	if status > 0 {
		return fmt.Sprintf("%d %s: %s", status, parsed.Kind, evidence)
	}
	return fmt.Sprintf("%s: %s", parsed.Kind, evidence)
}

func xaiRetryAfterUnix(headers map[string][]string, now int64) int64 {
	for key, values := range headers {
		if !strings.EqualFold(strings.TrimSpace(key), "retry-after") {
			continue
		}
		for _, value := range values {
			value = strings.TrimSpace(value)
			if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 {
				if seconds > int64((15 * time.Minute).Seconds()) {
					seconds = int64((15 * time.Minute).Seconds())
				}
				return now + seconds
			}
			if parsed, err := http.ParseTime(value); err == nil && parsed.Unix() > now {
				resetAt := parsed.Unix()
				maxReset := now + int64((15 * time.Minute).Seconds())
				if resetAt > maxReset {
					resetAt = maxReset
				}
				return resetAt
			}
		}
	}
	return now + int64(time.Minute.Seconds())
}

func xaiAuthFileStateForRecord(rec usageRecord) (string, int64) {
	if authFile := fileNameIfJSON(rec.AuthFile); authFile != "" {
		return authFileStateForName(authFile)
	}
	if authFile := firstNonEmptyString(fileNameIfJSON(rec.AuthIndex), fileNameIfJSON(rec.Source), fileNameIfJSON(rec.AuthID)); authFile != "" {
		return authFileStateForName(authFile)
	}
	configured := readConfiguredXAIAccounts()
	emailCounts := configuredEmailCounts(configured)
	for _, cfg := range configured {
		if aliasesOverlap(normalizeAccountAliases(rec.AuthIndex, rec.AuthID, rec.Source), configuredAccountMatchAliases(cfg, emailCounts)) {
			return cfg.AuthFile, cfg.AuthFileMTime
		}
	}
	return "", 0
}

func xaiStateKeyForRecord(rec usageRecord, authFile string) string {
	if authFile != "" {
		return normalizeAccountAlias(authFile)
	}
	return normalizeAccountAlias(firstNonEmptyString(rec.AuthID, rec.AuthIndex, rec.Source))
}

func recordXAIStateIfNeeded(ctx context.Context, db *sql.DB, rec usageRecord, status int) error {
	if !strings.EqualFold(trim(rec.Provider), "xai") {
		return nil
	}
	now := rec.RequestedAt.Unix()
	if now <= 0 {
		now = time.Now().Unix()
	}
	if !rec.Failed && successfulStatusCode(status) {
		changed, err := clearRecoveredXAIState(ctx, db, rec)
		if err != nil {
			return err
		}
		if changed {
			globalSchedulerState.invalidate()
		}
		return nil
	}
	state, reason, resetAt := xaiStateForRecord(rec, status, now)
	if state == "" {
		return nil
	}
	authFile, authFileMTime := xaiAuthFileStateForRecord(rec)
	stateKey := xaiStateKeyForRecord(rec, authFile)
	if stateKey == "" {
		return nil
	}
	globalSchedulerState.beginRestrictionWrite("xai")
	_, err := db.ExecContext(ctx, `
INSERT INTO xai_account_states (
  state_key, auth_id, auth_index, source, provider, state, reason, observed_at,
  reset_at, active, last_status_code, auth_file, auth_file_mtime
) VALUES (?, ?, ?, ?, 'xai', ?, ?, ?, ?, 1, ?, ?, ?)
ON CONFLICT(state_key) DO UPDATE SET
  auth_id=excluded.auth_id,
  auth_index=excluded.auth_index,
  source=excluded.source,
  provider='xai',
  state=excluded.state,
  reason=excluded.reason,
  observed_at=excluded.observed_at,
  reset_at=excluded.reset_at,
  active=1,
  last_status_code=excluded.last_status_code,
  auth_file=excluded.auth_file,
  auth_file_mtime=excluded.auth_file_mtime`,
		stateKey, trim(rec.AuthID), trim(rec.AuthIndex), trim(rec.Source), state, reason, now, resetAt, status, authFile, authFileMTime)
	globalSchedulerState.finishRestrictionWrite("xai")
	return err
}

func clearRecoveredXAIState(ctx context.Context, db *sql.DB, rec usageRecord) (bool, error) {
	authFile, _ := xaiAuthFileStateForRecord(rec)
	aliases := normalizeAccountAliases(authFile, rec.AuthID, rec.AuthIndex, rec.Source)
	changed := false
	for _, alias := range aliases {
		result, err := db.ExecContext(ctx, `
UPDATE xai_account_states SET active=0
WHERE active=1
AND state IN (?, ?, ?)
AND (lower(state_key)=? OR lower(auth_id)=? OR lower(auth_index)=? OR lower(source)=? OR lower(auth_file)=?)`,
			xaiStateUnauthorized, xaiStateForbidden, xaiStateRateLimited,
			alias, alias, alias, alias, alias)
		if err != nil {
			return false, err
		}
		if affected, err := result.RowsAffected(); err == nil && affected > 0 {
			changed = true
		}
	}
	return changed, nil
}

func expireXAIStates(ctx context.Context, db *sql.DB, now int64) error {
	_, err := db.ExecContext(ctx, `UPDATE xai_account_states SET active=0 WHERE active=1 AND reset_at>0 AND reset_at<=?`, now)
	return err
}

func queryActiveXAIStates(ctx context.Context, db *sql.DB, now int64) ([]xaiAccountStateRow, error) {
	rows, err := db.QueryContext(ctx, `
SELECT state_key, auth_id, auth_index, source, provider, state, reason, observed_at, reset_at,
active, last_status_code, auth_file, auth_file_mtime
FROM xai_account_states
WHERE active=1 AND (reset_at=0 OR reset_at>?)
ORDER BY CASE WHEN reset_at=0 THEN 1 ELSE 0 END, reset_at, observed_at DESC`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]xaiAccountStateRow, 0)
	for rows.Next() {
		var row xaiAccountStateRow
		var active int
		if err := rows.Scan(&row.StateKey, &row.AuthID, &row.AuthIndex, &row.Source, &row.Provider, &row.State, &row.Reason,
			&row.ObservedAt, &row.ResetAt, &active, &row.LastStatusCode, &row.AuthFile, &row.AuthFileMTime); err != nil {
			return nil, err
		}
		row.Active = active != 0
		row.ObservedAtText = unixTime(row.ObservedAt)
		if row.ResetAt > 0 {
			row.ResetAtText = unixTime(row.ResetAt)
			row.SecondsRemaining = maxInt64(0, row.ResetAt-now)
		} else {
			row.ResetAtText = "认证文件恢复后解除"
			row.SecondsRemaining = -1
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func xaiStateAliases(row xaiAccountStateRow) []string {
	return normalizeAccountAliases(row.StateKey, row.AuthID, row.AuthIndex, row.Source, row.AuthFile)
}

func filterMissingXAIStateRows(rows []xaiAccountStateRow, configured []configuredAccount, authDirReadable bool) []xaiAccountStateRow {
	if !authDirReadable || len(rows) == 0 {
		return rows
	}
	aliases := configuredAliasSet(configured)
	strict := configuredStrictAliasSet(configured)
	out := rows[:0]
	for _, row := range rows {
		fileAliases := fileBackedCleanupAliases(row.StateKey, row.AuthIndex, row.Source, row.AuthFile)
		if len(fileAliases) > 0 {
			if aliasesContainAny(strict, fileAliases...) {
				out = append(out, row)
			}
			continue
		}
		if aliasesContainAny(aliases, row.StateKey, row.AuthID, row.AuthIndex, row.Source, row.AuthFile) {
			out = append(out, row)
		}
	}
	return out
}

func applyXAIStates(accounts []accountRow, states []xaiAccountStateRow) {
	for i := range accounts {
		for _, state := range states {
			if !aliasesOverlap(accountAliases(accounts[i]), xaiStateAliases(state)) {
				continue
			}
			accounts[i].XAIState = state.State
			accounts[i].XAIStateReason = state.Reason
			accounts[i].XAIStateObservedAt = state.ObservedAtText
			accounts[i].XAIStateResetAt = state.ResetAt
			accounts[i].XAIStateResetAtText = state.ResetAtText
			accounts[i].XAIStateSecondsRemaining = state.SecondsRemaining
			accounts[i].XAILastStatusCode = state.LastStatusCode
			break
		}
	}
}

func clearReplacedOrMissingXAIStates(ctx context.Context, db *sql.DB) error {
	configured := readConfiguredXAIAccounts()
	return clearReplacedOrMissingXAIStatesForConfigured(ctx, db, configured, globalXAIAuthSource.authoritative(), time.Now().Unix())
}

func clearReplacedOrMissingXAIStatesForConfigured(ctx context.Context, db *sql.DB, configured []configuredAccount, authoritative bool, now int64) error {
	states, err := queryActiveXAIStates(ctx, db, now)
	if err != nil {
		return err
	}
	if authoritative {
		visible := filterMissingXAIStateRows(states, configured, true)
		keep := make(map[string]struct{}, len(visible))
		for _, state := range visible {
			keep[state.StateKey] = struct{}{}
		}
		for _, state := range states {
			if _, ok := keep[state.StateKey]; ok {
				continue
			}
			if _, err := db.ExecContext(ctx, `UPDATE xai_account_states SET active=0 WHERE state_key=?`, state.StateKey); err != nil {
				return err
			}
		}
	}
	for _, state := range states {
		if state.AuthFile == "" {
			continue
		}
		baseline := state.AuthFileMTime
		if baseline <= 0 {
			baseline = state.ObservedAt
		}
		for _, cfg := range configured {
			if !aliasesOverlap(normalizeAccountAliases(cfg.AuthFile), normalizeAccountAliases(state.AuthFile)) || cfg.AuthFileMTime <= baseline {
				continue
			}
			if _, err := db.ExecContext(ctx, `UPDATE xai_account_states SET active=0 WHERE active=1 AND state_key=?`, state.StateKey); err != nil {
				return err
			}
			break
		}
	}
	return nil
}

func resolveXAIStates(ctx context.Context, db *sql.DB, req xaiStateResolveRequest) (xaiStateResolveResponse, error) {
	globalXAIAuthSource.invalidate()
	inventory, inventoryErr := globalXAIAuthSource.hostAccounts()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return xaiStateResolveResponse{}, err
	}
	response := xaiStateResolveResponse{Items: make([]xaiStateResolveResult, 0, len(req.Items))}
	changed := false
	for _, item := range req.Items {
		result, itemChanged, err := resolveXAIStateItem(ctx, tx, item, inventory, inventoryErr)
		if err != nil {
			_ = tx.Rollback()
			return xaiStateResolveResponse{}, err
		}
		changed = changed || itemChanged
		response.add(result)
	}
	if err := tx.Commit(); err != nil {
		return xaiStateResolveResponse{}, err
	}
	if changed {
		globalSchedulerState.invalidate()
		if err := globalSchedulerState.refresh(ctx, db); err != nil {
			globalSchedulerState.invalidate()
		}
	}
	return response, nil
}

func (r *xaiStateResolveResponse) add(result xaiStateResolveResult) {
	r.Items = append(r.Items, result)
	switch result.Status {
	case "resolved":
		r.Resolved++
	case "already_resolved":
		r.AlreadyResolved++
	case "replacement_kept":
		r.ReplacementKept++
	default:
		r.Failed++
	}
}

func resolveXAIStateItem(ctx context.Context, tx *sql.Tx, item xaiStateResolveRequestItem, inventory []configuredAccount, inventoryErr error) (xaiStateResolveResult, bool, error) {
	stateKey := normalizeAccountAlias(item.StateKey)
	result := xaiStateResolveResult{StateKey: stateKey}
	if stateKey == "" {
		result.Status = "failed"
		result.Message = "state_key is required"
		return result, false, nil
	}
	state, err := queryActiveXAIStateByKey(ctx, tx, stateKey)
	if errors.Is(err, sql.ErrNoRows) {
		result.Status = "already_resolved"
		return result, false, nil
	}
	if err != nil {
		return xaiStateResolveResult{}, false, err
	}
	if expected := strings.TrimSpace(item.ExpectedState); expected != "" && !strings.EqualFold(expected, state.State) {
		result.Status = "failed"
		result.Message = "xAI state changed while the management action was running"
		return result, false, nil
	}
	action := strings.ToLower(strings.TrimSpace(item.Action))
	switch action {
	case "file_deleted", "file_absent":
		present, currentMTime, stateErr := xaiStateAuthFileOnDisk(state)
		if stateErr != nil {
			result.Status = "failed"
			result.Message = stateErr.Error()
			return result, false, nil
		}
		if present {
			baseline := normalizeUnixSeconds(state.AuthFileMTime)
			if baseline <= 0 {
				baseline = state.ObservedAt
			}
			replacedByIdentity := inventoryErr == nil && xaiStateFileIdentityChanged(state, inventory)
			if !replacedByIdentity && (currentMTime <= 0 || currentMTime <= baseline) {
				result.Status = "still_present"
				result.Message = "the original xAI auth file is still present"
				return result, false, nil
			}
			changed, err := deactivateXAIState(ctx, tx, state.StateKey)
			if err != nil {
				return xaiStateResolveResult{}, false, err
			}
			if !changed {
				result.Status = "already_resolved"
				return result, false, nil
			}
			result.Status = "replacement_kept"
			result.Message = "a newer xAI auth file was preserved and the old state was cleared"
			return result, true, nil
		}
		changed, err := deactivateXAIState(ctx, tx, state.StateKey)
		if err != nil {
			return xaiStateResolveResult{}, false, err
		}
		if !changed {
			result.Status = "already_resolved"
			return result, false, nil
		}
		result.Status = "resolved"
		return result, true, nil
	case "runtime_disabled":
		if inventoryErr != nil {
			result.Status = "failed"
			result.Message = "fresh xAI host auth inventory is unavailable: " + sanitizeTriggerError(inventoryErr)
			return result, false, nil
		}
		current, present, ambiguous := currentXAIStateInventoryEntry(state, inventory)
		if ambiguous {
			result.Status = "failed"
			result.Message = "xAI runtime credential identity is ambiguous"
			return result, false, nil
		}
		if present {
			if normalizeAuthSourceKind(current.AuthSourceKind) != authSourceKindRuntimeOnly {
				result.Status = "failed"
				result.Message = "xAI state is not backed by a runtime-only credential"
				return result, false, nil
			}
			if !runtimeAuthEntryDisabled(current) {
				result.Status = "still_present"
				result.Message = "xAI runtime credential is still enabled"
				return result, false, nil
			}
		}
		changed, err := deactivateXAIState(ctx, tx, state.StateKey)
		if err != nil {
			return xaiStateResolveResult{}, false, err
		}
		if !changed {
			result.Status = "already_resolved"
			return result, false, nil
		}
		result.Status = "resolved"
		if !present {
			result.Message = "runtime credential was already absent"
		}
		return result, true, nil
	case "manual_release":
		if state.State != xaiStateFreeExhausted && state.State != xaiStateRateLimited {
			result.Status = "failed"
			result.Message = "only xAI 429 states can be released without changing credentials"
			return result, false, nil
		}
		changed, err := deactivateXAIState(ctx, tx, state.StateKey)
		if err != nil {
			return xaiStateResolveResult{}, false, err
		}
		if !changed {
			result.Status = "already_resolved"
			return result, false, nil
		}
		result.Status = "resolved"
		return result, true, nil
	default:
		result.Status = "failed"
		result.Message = "unsupported action"
		return result, false, nil
	}
}

func queryActiveXAIStateByKey(ctx context.Context, tx *sql.Tx, stateKey string) (xaiAccountStateRow, error) {
	var row xaiAccountStateRow
	var active int
	err := tx.QueryRowContext(ctx, `
SELECT state_key, auth_id, auth_index, source, provider, state, reason, observed_at, reset_at,
active, last_status_code, auth_file, auth_file_mtime
FROM xai_account_states
WHERE active=1 AND state_key=?`, stateKey).Scan(
		&row.StateKey, &row.AuthID, &row.AuthIndex, &row.Source, &row.Provider, &row.State,
		&row.Reason, &row.ObservedAt, &row.ResetAt, &active, &row.LastStatusCode,
		&row.AuthFile, &row.AuthFileMTime,
	)
	row.Active = active != 0
	return row, err
}

func deactivateXAIState(ctx context.Context, tx *sql.Tx, stateKey string) (bool, error) {
	result, err := tx.ExecContext(ctx, `UPDATE xai_account_states SET active=0 WHERE active=1 AND state_key=?`, stateKey)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected > 0, err
}

func xaiStateAuthFileOnDisk(state xaiAccountStateRow) (bool, int64, error) {
	authFile := fileNameIfJSON(state.AuthFile)
	if authFile == "" {
		return false, 0, fmt.Errorf("xAI state has no safe physical JSON file name")
	}
	authDir := configuredAuthDir()
	if authDir == "" {
		return false, 0, fmt.Errorf("CPA auth directory is unavailable")
	}
	info, err := os.Stat(filepath.Join(authDir, authFile))
	if err == nil {
		return true, info.ModTime().Unix(), nil
	}
	if os.IsNotExist(err) {
		return false, 0, nil
	}
	return false, 0, err
}

func currentXAIStateInventoryEntry(state xaiAccountStateRow, inventory []configuredAccount) (configuredAccount, bool, bool) {
	matches := make([]configuredAccount, 0, 2)
	for _, candidate := range inventory {
		if !strings.EqualFold(strings.TrimSpace(candidate.Provider), "xai") {
			continue
		}
		matched := false
		for _, pair := range [][2]string{
			{state.AuthID, candidate.AuthID},
			{state.AuthIndex, candidate.AuthIndex},
			{state.AuthFile, candidate.AuthFile},
			{state.StateKey, candidate.AuthIndex},
			{state.StateKey, candidate.AuthFile},
		} {
			left, right := normalizeAccountAlias(pair[0]), normalizeAccountAlias(pair[1])
			if left != "" && left == right {
				matched = true
				break
			}
		}
		if matched {
			matches = append(matches, candidate)
		}
	}
	if len(matches) == 1 {
		return matches[0], true, false
	}
	return configuredAccount{}, false, len(matches) > 1
}

func xaiStateFileIdentityChanged(state xaiAccountStateRow, inventory []configuredAccount) bool {
	authFile := normalizeAccountAlias(fileNameIfJSON(state.AuthFile))
	if authFile == "" {
		return false
	}
	matches := make([]configuredAccount, 0, 2)
	for _, candidate := range inventory {
		if !strings.EqualFold(strings.TrimSpace(candidate.Provider), "xai") || normalizeAccountAlias(fileNameIfJSON(candidate.AuthFile)) != authFile {
			continue
		}
		matches = append(matches, candidate)
	}
	if len(matches) != 1 {
		return false
	}
	current := matches[0]
	for _, pair := range [][2]string{{state.AuthID, current.AuthID}, {state.AuthIndex, current.AuthIndex}} {
		left, right := stableXAIAuthIdentity(pair[0]), stableXAIAuthIdentity(pair[1])
		if left != "" && right != "" && left != right {
			return true
		}
	}
	return false
}

func stableXAIAuthIdentity(value string) string {
	if fileNameIfJSON(value) != "" {
		return ""
	}
	return normalizeAccountAlias(value)
}

func candidateMatchesXAIState(candidate schedulerAuthCandidate, states []xaiAccountStateRow) bool {
	aliases := schedulerCandidateAliases(candidate)
	for _, state := range states {
		if aliasesOverlap(aliases, xaiStateAliases(state)) {
			return true
		}
	}
	return false
}

func earliestXAIStateReset(states []xaiAccountStateRow, now int64) int64 {
	var earliest int64
	for _, state := range states {
		if state.ResetAt <= now {
			continue
		}
		if earliest == 0 || state.ResetAt < earliest {
			earliest = state.ResetAt
		}
	}
	return earliest
}
