package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"
)

const lifecyclePolicyVersion = 2

func temporaryAuthState(state string) bool {
	return state == authQuotaCooldown || state == authRateLimited
}
func loginAuthState(state string) bool {
	return state == authInvalid || state == authBillingBlocked || state == authPermissionBlocked
}

func runtimeFailureSignature(entry hostAuthFileEntry) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(entry.StatusMessage)))
}

func snapshotChanged(s authLifecycleState, current lifecycleSnapshot) bool {
	return s.AuthID != current.Entry.ID || s.Name != current.Entry.Name || s.Identity != current.Identity
}

func snapshotCredentialTime(s lifecycleSnapshot) int64 {
	value := lifecycleString(s.Data, "last_refresh")
	if t, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return t.UnixNano()
	}
	return 0
}

func stateFromSnapshot(s lifecycleSnapshot, now time.Time) authLifecycleState {
	state := authLifecycleState{PolicyVersion: lifecyclePolicyVersion, AuthIndex: s.Entry.AuthIndex, AuthID: s.Entry.ID, Name: s.Entry.Name, Identity: s.Identity, Fingerprint: s.Fingerprint, ContentHash: s.ContentHash, RuntimeUpdated: s.Entry.UpdatedAt, State: authHealthy, Disabled: s.Entry.Disabled && s.FileDisabled, ManualDisabled: s.FileDisabled, SyncStatus: "synced", CreatedAt: now.Unix(), CredentialSince: snapshotCredentialTime(s)}
	if state.ManualDisabled {
		state.State = authManualDisabled
		state.Reason = "manual_disabled"
	}
	if s.Entry.Disabled != s.FileDisabled {
		state.SyncStatus = "pending"
		state.SyncError = "physical and runtime status are synchronizing"
	}
	return state
}

func clearLifecycleFailure(s *authLifecycleState) {
	s.State, s.BlockedState, s.BlockedFingerprint, s.Reason = authHealthy, "", "", ""
	s.RecoverAt, s.DisabledAt = 0, 0
	s.CheckOK, s.Paused = false, false
}

func eventIsStale(e lifecycleEvent, s authLifecycleState) bool {
	ns := e.Decision.RequestedNS
	if ns == 0 {
		ns = e.RequestedAt * int64(time.Second)
	}
	return (s.CredentialSince > 0 && ns < s.CredentialSince) || e.RequestedAt < s.IgnoreBefore || (e.Decision.Fingerprint != "" && e.Decision.Fingerprint != s.Fingerprint)
}

// Observe credential changes before consuming failures or applying status writes.
// Content hashes and runtime timestamps are observations, not ownership locks.
func (c *authLifecycleController) observeSnapshot(ctx context.Context, db *sql.DB, s *authLifecycleState, current lifecycleSnapshot) error {
	if snapshotChanged(*s, current) {
		c.retry(ctx, db, s, "account identity changed; waiting for matching CPA auth")
		return errLifecycleConflict
	}
	before, _ := json.Marshal(s)
	if s.PolicyVersion < lifecyclePolicyVersion {
		if err := c.migrateAccountPolicy(ctx, db, s, current); err != nil {
			return err
		}
	}
	credentialChanged := s.Fingerprint != current.Fingerprint
	if _, terminal := classifyRuntimeAuthFailure(current.Entry); !terminal {
		s.IgnoredRuntimeFailure = ""
	}
	if credentialChanged {
		if _, terminal := classifyRuntimeAuthFailure(current.Entry); terminal {
			s.IgnoredRuntimeFailure = runtimeFailureSignature(current.Entry)
		}
		boundary := snapshotCredentialTime(current)
		if boundary <= s.CredentialSince || boundary > c.clock.Now().UnixNano() {
			boundary = c.clock.Now().UnixNano()
		}
		s.CredentialSince = boundary
		if loginAuthState(s.State) && !s.ManualDisabled && s.BlockedFingerprint != "" && s.BlockedFingerprint != current.Fingerprint {
			if current.FileDisabled || current.Entry.Disabled {
				s.PendingAction = "enable"
				s.PendingOwner = false
			} else {
				clearLifecycleFailure(s)
				s.PendingAction = ""
				s.DisabledByPlugin = false
			}
			s.RetryAt, s.RetryCount = 0, 0
		}
	}
	if s.PendingAction == "" {
		if current.FileDisabled && !s.Disabled && !s.DisabledByPlugin {
			s.ManualDisabled = true
			s.State, s.Reason = authManualDisabled, "manual_disabled"
		} else if !current.FileDisabled && !current.Entry.Disabled && (s.Disabled || s.ManualDisabled) {
			// An operator's enable is effective immediately, without a permanent pause.
			clearLifecycleFailure(s)
			s.ManualDisabled, s.DisabledByPlugin = false, false
			s.IgnoreBefore = c.clock.Now().Unix()
		}
	}
	s.Disabled = current.FileDisabled && current.Entry.Disabled
	s.Fingerprint, s.ContentHash, s.RuntimeUpdated = current.Fingerprint, current.ContentHash, current.Entry.UpdatedAt
	s.Paused = false
	if current.FileDisabled != current.Entry.Disabled && s.PendingAction == "" {
		s.SyncStatus, s.SyncError = "pending", "physical and runtime status are synchronizing"
	} else if s.PendingAction == "" && (s.SyncStatus == "conflict" || s.SyncStatus == "pending") {
		s.SyncStatus, s.SyncError, s.RetryAt = "synced", "", 0
	}
	after, _ := json.Marshal(s)
	if string(before) != string(after) {
		return saveLifecycleState(ctx, db, s, c.clock.Now(), "auth_observed")
	}
	return nil
}

func (c *authLifecycleController) reconcile(ctx context.Context) {
	if err := lockMutexWithContext(ctx, &c.opMu); err != nil {
		return
	}
	defer c.opMu.Unlock()
	if c.host == nil || !tryAcquireQuotaProbeGate() {
		return
	}
	defer releaseQuotaProbeGate()
	db, _, err := c.store.open(ctx)
	if err != nil {
		c.setError(errors.New("lifecycle database unavailable"))
		return
	}
	entries, err := c.host.List(ctx)
	if err != nil {
		c.setError(err)
		return
	}
	c.setError(nil)
	states, err := listLifecycleStates(ctx, db)
	if err != nil {
		c.setError(err)
		return
	}
	known := map[string]bool{}
	for _, s := range states {
		known[s.AuthIndex] = true
	}
	if nativeScheduling() {
		for _, entry := range entries {
			if known[entry.AuthIndex] || entry.AuthIndex == "" || entry.RuntimeOnly || !stringsEqualCodex(firstNonEmptyString(entry.Provider, entry.Type)) {
				continue
			}
			snapshot, readErr := readLifecycleInventoryEntry(ctx, c.host, entries, entry.AuthIndex)
			if readErr != nil {
				continue
			}
			s := stateFromSnapshot(snapshot, c.clock.Now())
			if err = saveLifecycleState(ctx, db, &s, c.clock.Now(), "discovered"); err != nil {
				c.setError(err)
				return
			}
			states = append(states, s)
			known[s.AuthIndex] = true
		}
	}
	for _, s := range states {
		if !nativeScheduling() && !s.DisabledByPlugin && s.PendingAction == "" {
			continue
		}
		snapshot, readErr := readLifecycleInventoryEntry(ctx, c.host, entries, s.AuthIndex)
		if readErr != nil {
			if s.RetryAt <= c.clock.Now().Unix() {
				c.retry(ctx, db, &s, "auth snapshot unavailable; retrying")
			}
			continue
		}
		if err = c.observeSnapshot(ctx, db, &s, snapshot); err != nil {
			c.setError(err)
		}
	}
	if nativeScheduling() {
		if err = c.processEvents(ctx, db, entries); err != nil {
			c.setError(err)
			return
		}
	}
	states, err = listLifecycleStates(ctx, db)
	if err != nil {
		c.setError(err)
		return
	}
	for _, s := range states {
		if !nativeScheduling() && !s.DisabledByPlugin && s.PendingAction == "" {
			continue
		}
		if s.RetryAt > c.clock.Now().Unix() {
			continue
		}
		snapshot, readErr := readLifecycleInventoryEntry(ctx, c.host, entries, s.AuthIndex)
		if readErr != nil {
			c.retry(ctx, db, &s, "auth snapshot unavailable; retrying")
			continue
		}
		if err = c.syncAccount(ctx, db, &s, snapshot); err != nil {
			c.setError(err)
		}
	}
}

func (c *authLifecycleController) syncAccount(ctx context.Context, db *sql.DB, s *authLifecycleState, snapshot lifecycleSnapshot) error {
	if err := c.observeSnapshot(ctx, db, s, snapshot); err != nil {
		return err
	}
	if !nativeScheduling() && s.PendingAction == "disable" && !s.DisabledByPlugin {
		clearLifecycleFailure(s)
		s.PendingAction, s.PendingOwner, s.RetryAt = "", false, 0
		return saveLifecycleState(ctx, db, s, c.clock.Now(), "legacy_pending_canceled")
	}
	if s.ManualDisabled {
		if s.PendingAction == "disable" {
			return c.transition(ctx, db, s, snapshot, true, false)
		}
		return nil
	}
	if s.RetryAt > c.clock.Now().Unix() {
		return nil
	}
	if !s.Disabled && s.PendingAction == "" && !loginAuthState(s.State) && nativeScheduling() {
		if d, ok := classifyRuntimeAuthFailure(snapshot.Entry); ok {
			// A new credential may coexist briefly with the old runtime error.
			if s.IgnoredRuntimeFailure != runtimeFailureSignature(snapshot.Entry) {
				s.State, s.BlockedState, s.Reason, s.LastHTTPStatus = d.State, d.State, d.Reason, d.Status
				s.LastErrorCode, s.LastErrorMessage, s.BlockedFingerprint = d.ErrorCode, d.Reason, s.Fingerprint
				s.RecoverAt = 0
			}
		}
	}
	if temporaryAuthState(s.State) && s.RecoverAt == 0 {
		s.RecoverAt = s.UpdatedAt + 60
	}
	if temporaryAuthState(s.State) && s.RecoverAt <= c.clock.Now().Unix() {
		pending, err := pendingLifecycleEvents(ctx, db, *s)
		if err != nil {
			return err
		}
		if pending {
			return nil
		}
		if snapshot.FileDisabled || snapshot.Entry.Disabled {
			s.PendingAction = "enable"
			s.PendingOwner = false
		} else {
			clearLifecycleFailure(s)
			s.PendingAction = ""
			s.DisabledByPlugin = false
		}
		if err = saveLifecycleState(ctx, db, s, c.clock.Now(), "cooldown_expired"); err != nil {
			return err
		}
	}
	if s.PendingAction != "enable" && (loginAuthState(s.State) || (temporaryAuthState(s.State) && s.RecoverAt > c.clock.Now().Unix())) {
		if s.BlockedFingerprint == "" {
			s.BlockedFingerprint = s.Fingerprint
		}
		if !s.Disabled || s.PendingAction == "enable" {
			s.PendingAction = "disable"
			s.PendingOwner = true
		}
	}
	if s.PendingAction != "" {
		if s.PendingAction == "enable" {
			pending, err := pendingLifecycleEvents(ctx, db, *s)
			if err != nil {
				return err
			}
			if pending {
				return nil
			}
		}
		return c.transition(ctx, db, s, snapshot, s.PendingAction == "disable", s.PendingOwner)
	}
	return nil
}

func (c *authLifecycleController) transition(ctx context.Context, db *sql.DB, s *authLifecycleState, before lifecycleSnapshot, disabled, owner bool) error {
	s.PendingAction = "enable"
	if disabled {
		s.PendingAction = "disable"
	}
	s.PendingOwner = owner
	if !c.host.Ready() {
		c.retry(ctx, db, s, "management API is not configured; retrying status sync")
		return errors.New(s.SyncError)
	}
	current, err := c.host.Read(ctx, s.AuthIndex)
	if err != nil {
		c.retry(ctx, db, s, "auth read before status sync failed")
		return err
	}
	if snapshotChanged(*s, current) || current.Fingerprint != before.Fingerprint {
		c.retry(ctx, db, s, "auth changed before status sync; retrying with current credentials")
		return errLifecycleConflict
	}
	s.PendingContentHash, s.PendingAt, s.SyncStatus, s.SyncError = current.ContentHash, c.clock.Now().Unix(), "pending", ""
	if err = saveLifecycleState(ctx, db, s, c.clock.Now(), "status_intent"); err != nil {
		return err
	}
	if current.FileDisabled != disabled || current.Entry.Disabled != disabled {
		_ = c.host.SetDisabled(ctx, current, disabled)
	}
	after, err := c.host.Read(ctx, s.AuthIndex)
	if err != nil {
		c.retry(ctx, db, s, "status read-back unavailable; retrying")
		return err
	}
	if snapshotChanged(*s, after) || current.Fingerprint != after.Fingerprint {
		c.retry(ctx, db, s, "credentials changed during status sync; retrying")
		return errLifecycleConflict
	}
	if after.FileDisabled != disabled || after.Entry.Disabled != disabled {
		s.Disabled = after.FileDisabled && after.Entry.Disabled
		c.retry(ctx, db, s, "status not yet confirmed in file and runtime; retrying")
		return errors.New(s.SyncError)
	}
	s.Disabled, s.DisabledByPlugin = disabled, disabled && owner
	s.ContentHash, s.RuntimeUpdated, s.Fingerprint = after.ContentHash, after.Entry.UpdatedAt, after.Fingerprint
	s.PendingAction, s.PendingOwner, s.SyncStatus, s.SyncError = "", false, "synced", ""
	s.RetryAt, s.RetryCount, s.Paused = 0, 0, false
	if disabled {
		if s.DisabledAt == 0 {
			s.DisabledAt = c.clock.Now().Unix()
		}
	} else {
		clearLifecycleFailure(s)
		s.ManualDisabled = false
		s.IgnoreBefore = c.clock.Now().Unix()
	}
	if err = saveLifecycleState(ctx, db, s, c.clock.Now(), "status_confirmed"); err != nil {
		return err
	}
	globalCodexAuthSource.invalidate()
	log.Printf("auth lifecycle status confirmed: auth_index=%s disabled=%t state=%s", s.AuthIndex, s.Disabled, s.State)
	return nil
}
