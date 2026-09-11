package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

const (
	authHealthy           = "HEALTHY"
	authInvalid           = "AUTH_INVALID"
	authQuotaCooldown     = "QUOTA_COOLDOWN"
	authRateLimited       = "RATE_LIMITED"
	authBillingBlocked    = "BILLING_BLOCKED"
	authPermissionBlocked = "PERMISSION_BLOCKED"
	authManualDisabled    = "MANUAL_DISABLED"
	authDisabledUnknown   = "DISABLED_UNKNOWN"
)

// Only observations and hashes are durable. Never put auth JSON or raw errors here.
type authLifecycleState struct {
	PolicyVersion         int    `json:"policy_version"`
	ManualDisabled        bool   `json:"manual_disabled"`
	CredentialSince       int64  `json:"credential_since_ns"`
	ControlSince          int64  `json:"control_since_ns,omitempty"`
	BlockedFingerprint    string `json:"blocked_fingerprint,omitempty"`
	IgnoredRuntimeFailure string `json:"ignored_runtime_failure,omitempty"`
	AuthIndex             string `json:"auth_index"`
	AuthID                string `json:"auth_id"`
	Name                  string `json:"name"`
	Identity              string `json:"auth_identity"`
	Fingerprint           string `json:"auth_fingerprint"`
	ContentHash           string `json:"content_hash"`
	RuntimeUpdated        string `json:"runtime_updated"`
	State                 string `json:"state"`
	BlockedState          string `json:"blocked_state,omitempty"`
	Disabled              bool   `json:"disabled"`
	DisabledByPlugin      bool   `json:"disabled_by_plugin"`
	Paused                bool   `json:"paused"`
	Reason                string `json:"disable_reason"`
	DisabledAt            int64  `json:"disabled_at"`
	RecoverAt             int64  `json:"recover_at"`
	LastHTTPStatus        int    `json:"last_http_status"`
	LastErrorType         string `json:"last_error_type"`
	LastErrorCode         string `json:"last_error_code"`
	LastErrorMessage      string `json:"last_error_message"`
	SyncStatus            string `json:"sync_status"`
	SyncError             string `json:"sync_error,omitempty"`
	PendingAction         string `json:"pending_action,omitempty"`
	PendingOwner          bool   `json:"pending_owner"`
	PendingContentHash    string `json:"pending_content_hash,omitempty"`
	PendingAt             int64  `json:"pending_at,omitempty"`
	RetryAt               int64  `json:"retry_at,omitempty"`
	RetryCount            int    `json:"retry_count"`
	LastEventID           int64  `json:"last_event_id"`
	IgnoreBefore          int64  `json:"ignore_before"`
	CheckedAt             int64  `json:"checked_at"`
	CheckOK               bool   `json:"check_ok"`
	CheckResult           string `json:"check_result,omitempty"`
	CreatedAt             int64  `json:"created_at"`
	UpdatedAt             int64  `json:"updated_at"`
	Version               int64  `json:"version"`
}

const authLifecycleSchema = `
CREATE TABLE IF NOT EXISTS auth_lifecycle_states (
 auth_index TEXT PRIMARY KEY, version INTEGER NOT NULL, updated_at INTEGER NOT NULL,
 recover_at INTEGER NOT NULL DEFAULT 0, payload TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS auth_lifecycle_operations (
 id INTEGER PRIMARY KEY AUTOINCREMENT, auth_index TEXT NOT NULL, at INTEGER NOT NULL,
 action TEXT NOT NULL, outcome TEXT NOT NULL, detail TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS auth_lifecycle_events (
 id INTEGER PRIMARY KEY AUTOINCREMENT, auth_index TEXT NOT NULL, auth_id TEXT NOT NULL,
 requested_at INTEGER NOT NULL, payload TEXT NOT NULL, processed INTEGER NOT NULL DEFAULT 0,
 retry_at INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS idx_auth_lifecycle_events_pending ON auth_lifecycle_events(processed,id);
CREATE INDEX IF NOT EXISTS idx_auth_lifecycle_recovery ON auth_lifecycle_states(recover_at);
CREATE TABLE IF NOT EXISTS auth_model_issues (
 auth_index TEXT NOT NULL, model TEXT NOT NULL, observed_at INTEGER NOT NULL,
 status INTEGER NOT NULL, error_code TEXT NOT NULL, PRIMARY KEY(auth_index,model));
CREATE TABLE IF NOT EXISTS auth_lifecycle_legacy_evidence (
 source TEXT NOT NULL, auth_id TEXT NOT NULL, observed_at INTEGER NOT NULL,
 status INTEGER NOT NULL, recover_at INTEGER NOT NULL, PRIMARY KEY(source,auth_id));
`

func migrateAuthLifecycle(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, authLifecycleSchema); err != nil {
		return err
	}
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(auth_lifecycle_events)`)
	if err != nil {
		return err
	}
	columns := map[string]bool{}
	for rows.Next() {
		var cid, notNull, primary int
		var name, kind string
		var defaultValue sql.NullString
		if err = rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primary); err != nil {
			rows.Close()
			return err
		}
		columns[name] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, column := range []struct{ name, definition string }{
		{"retry_at", "INTEGER NOT NULL DEFAULT 0"},
		{"outcome", "TEXT NOT NULL DEFAULT 'pending'"},
		{"detail", "TEXT NOT NULL DEFAULT ''"},
	} {
		if columns[column.name] {
			continue
		}
		if _, err = db.ExecContext(ctx, `ALTER TABLE auth_lifecycle_events ADD COLUMN `+column.name+` `+column.definition); err != nil {
			return err
		}
	}
	if !columns["outcome"] {
		if _, err = db.ExecContext(ctx, `UPDATE auth_lifecycle_events SET outcome='observed',detail='historical disposition unavailable' WHERE processed=1`); err != nil {
			return err
		}
	}
	if _, err = db.ExecContext(ctx, `
CREATE INDEX IF NOT EXISTS idx_lifecycle_event_attention ON auth_lifecycle_events(id) WHERE processed=0 OR outcome IN ('pending_disable','identity_conflict');
CREATE INDEX IF NOT EXISTS idx_lifecycle_event_outcome_auth ON auth_lifecycle_events(outcome,auth_index,auth_id);`); err != nil {
		return err
	}
	// Evidence is not ownership and is never executable work.
	_, err = db.ExecContext(ctx, `
INSERT OR IGNORE INTO auth_lifecycle_legacy_evidence
 SELECT 'autoban',auth_id,banned_at,last_status_code,reset_at FROM autoban_bans;
INSERT OR IGNORE INTO auth_lifecycle_legacy_evidence
 SELECT 'invalid',auth_id,invalidated_at,last_status_code,0 FROM invalid_auths;`)
	return err
}

func loadLifecycleState(ctx context.Context, db *sql.DB, index string) (authLifecycleState, error) {
	var raw string
	var state authLifecycleState
	err := db.QueryRowContext(ctx, `SELECT payload FROM auth_lifecycle_states WHERE auth_index=?`, index).Scan(&raw)
	if err != nil {
		return state, err
	}
	err = json.Unmarshal([]byte(raw), &state)
	return state, err
}

func listLifecycleStates(ctx context.Context, db *sql.DB) ([]authLifecycleState, error) {
	rows, err := db.QueryContext(ctx, `SELECT payload FROM auth_lifecycle_states ORDER BY auth_index`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []authLifecycleState{}
	for rows.Next() {
		var raw string
		var s authLifecycleState
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &s); err != nil {
			return nil, err
		}
		result = append(result, s)
	}
	return result, rows.Err()
}

var errLifecycleConflict = errors.New("account state changed; refresh and retry")

func saveLifecycleState(ctx context.Context, db *sql.DB, s *authLifecycleState, now time.Time, action string) error {
	previous := s.Version
	s.Version++
	s.UpdatedAt = now.Unix()
	if s.CreatedAt == 0 {
		s.CreatedAt = s.UpdatedAt
	}
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var result sql.Result
	if previous == 0 {
		result, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO auth_lifecycle_states(auth_index,version,updated_at,recover_at,payload) VALUES(?,?,?,?,?)`, s.AuthIndex, s.Version, s.UpdatedAt, s.RecoverAt, string(raw))
	} else {
		result, err = tx.ExecContext(ctx, `UPDATE auth_lifecycle_states SET version=?,updated_at=?,recover_at=?,payload=? WHERE auth_index=? AND version=?`, s.Version, s.UpdatedAt, s.RecoverAt, string(raw), s.AuthIndex, previous)
	}
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errLifecycleConflict
	}
	if (s.SyncStatus == "synced" && s.PendingAction == "") || s.Paused {
		outcome := "observed"
		if s.Paused {
			outcome = "identity_conflict"
		} else if s.Disabled {
			outcome = "disabled"
		}
		if _, err = tx.ExecContext(ctx, `UPDATE auth_lifecycle_events SET outcome=?,detail=? WHERE outcome='pending_disable' AND (auth_index=? OR (auth_index='' AND auth_id=?))`, outcome, s.SyncError, s.AuthIndex, s.AuthID); err != nil {
			return err
		}
	}
	if action == "manual_recheck" || action == "manual_clear" || (action == "status_confirmed" && !s.Disabled) {
		if _, err = tx.ExecContext(ctx, `UPDATE auth_lifecycle_events SET outcome='observed',detail='resolved by explicit account control or confirmed recovery' WHERE outcome='identity_conflict' AND (auth_index=? OR (auth_index='' AND auth_id=?))`, s.AuthIndex, s.AuthID); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO auth_lifecycle_operations(auth_index,at,action,outcome,detail) VALUES(?,?,?,?,?)`, s.AuthIndex, s.UpdatedAt, action, s.SyncStatus, s.SyncError); err != nil {
		return err
	}
	return tx.Commit()
}
