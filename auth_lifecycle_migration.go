package main

import (
	"context"
	"database/sql"
	"encoding/json"
)

// Upgrade the former review-gated policy once per account. No quota probe or
// new failure is fabricated; only existing local failure evidence is replayed.
func (c *authLifecycleController) migrateAccountPolicy(ctx context.Context, db *sql.DB, s *authLifecycleState, current lifecycleSnapshot) error {
	if s.PolicyVersion >= 2 {
		s.PolicyVersion = lifecyclePolicyVersion
		return nil
	}
	explicitManual := s.Reason == "manual_disabled" || s.Reason == "manual_clear"
	s.ManualDisabled = explicitManual && (current.FileDisabled || s.PendingAction == "disable")
	s.PolicyVersion, s.Paused = lifecyclePolicyVersion, false
	s.IgnoreBefore = 0 // Old metadata-conflict timestamps are not login boundaries.
	s.CredentialSince = 0
	s.SyncStatus, s.SyncError, s.RetryAt, s.RetryCount = "synced", "", 0, 0
	if s.ManualDisabled {
		s.State, s.Reason, s.DisabledByPlugin = authManualDisabled, "manual_disabled", false
		return nil
	}
	if s.BlockedState != "" {
		s.State = s.BlockedState
	}
	if s.State == authManualDisabled {
		clearLifecycleFailure(s)
		s.DisabledByPlugin = false
		if current.FileDisabled || current.Entry.Disabled {
			s.State, s.Reason = authDisabledUnknown, "disabled_origin_unknown"
		}
	}
	if loginAuthState(s.State) || temporaryAuthState(s.State) {
		s.BlockedFingerprint = s.Fingerprint
		if s.DisabledAt > 0 && current.FileDisabled {
			s.DisabledByPlugin = true
		}
		if temporaryAuthState(s.State) && s.RecoverAt == 0 {
			s.RecoverAt = s.UpdatedAt + 60
		}
	}
	if loginAuthState(s.State) {
		// The old conflict handler overwrote the observed fingerprint. Recover
		// the failure generation from the latest event when that evidence exists.
		var raw string
		err := db.QueryRowContext(ctx, `SELECT payload FROM auth_lifecycle_events WHERE (auth_index=? OR (auth_index='' AND auth_id=?)) AND json_extract(payload,'$.state') IN ('AUTH_INVALID','BILLING_BLOCKED','PERMISSION_BLOCKED') ORDER BY requested_at DESC,id DESC LIMIT 1`, s.AuthIndex, s.AuthID).Scan(&raw)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if err == nil {
			var d authDecision
			if err = json.Unmarshal([]byte(raw), &d); err != nil {
				return err
			}
			if d.Fingerprint != "" && d.Fingerprint != "previous-generation" {
				s.BlockedFingerprint = d.Fingerprint
			}
		}
	}
	if s.Fingerprint != current.Fingerprint || (loginAuthState(s.State) && s.BlockedFingerprint != "" && s.BlockedFingerprint != current.Fingerprint) {
		s.CredentialSince = snapshotCredentialTime(current)
		if s.CredentialSince <= 0 || s.CredentialSince > c.clock.Now().UnixNano() {
			s.CredentialSince = c.clock.Now().UnixNano()
		}
	}
	var success int64
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(requested_at),0) FROM usage_events WHERE failed=0 AND (auth_index=? OR (auth_index='' AND auth_id=?))`, s.AuthIndex, s.AuthID).Scan(&success); err != nil {
		return err
	}
	rows, err := db.QueryContext(ctx, `SELECT id,requested_at,payload FROM auth_lifecycle_events WHERE processed=1 AND outcome IN ('identity_conflict','ignored_stale') AND (auth_index=? OR (auth_index='' AND auth_id=?)) ORDER BY id`, s.AuthIndex, s.AuthID)
	if err != nil {
		return err
	}
	events := []lifecycleEvent{}
	for rows.Next() {
		var e lifecycleEvent
		var raw string
		if err = rows.Scan(&e.ID, &e.RequestedAt, &raw); err != nil {
			rows.Close()
			return err
		}
		if err = json.Unmarshal([]byte(raw), &e.Decision); err != nil {
			rows.Close()
			return err
		}
		events = append(events, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, e := range events {
		if !e.Decision.Disable {
			continue
		}
		outcome, detail, processed := "pending", "replaying failure after policy upgrade", 0
		if e.RequestedAt < success || (s.CredentialSince > 0 && e.RequestedAt < s.CredentialSince/1e9) || (e.Decision.Fingerprint != "" && e.Decision.Fingerprint != current.Fingerprint) || explicitManual {
			outcome, detail, processed = "ignored_stale", "newer login, successful usage or manual action superseded failure", 1
		}
		if temporaryAuthState(e.Decision.State) {
			reset := e.Decision.RecoverAt
			if reset == 0 {
				reset = e.RequestedAt + 60
			}
			if reset <= c.clock.Now().Unix() {
				outcome, detail, processed = "ignored_expired", "old cooldown already expired", 1
			}
		}
		if _, err = db.ExecContext(ctx, `UPDATE auth_lifecycle_events SET processed=?,outcome=?,detail=?,retry_at=0 WHERE id=?`, processed, outcome, detail, e.ID); err != nil {
			return err
		}
	}
	return nil
}
