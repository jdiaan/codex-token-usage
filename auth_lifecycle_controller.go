package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"sync"
	"time"
)

type lifecycleClock interface {
	Now() time.Time
	After(time.Duration) <-chan time.Time
}
type realLifecycleClock struct{}

func (realLifecycleClock) Now() time.Time                         { return time.Now() }
func (realLifecycleClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

type authLifecycleController struct {
	store     *store
	host      lifecycleHost
	clock     lifecycleClock
	opMu      sync.Mutex
	lifeMu    sync.Mutex
	cancel    context.CancelFunc
	done      chan struct{}
	wake      chan struct{}
	statusMu  sync.Mutex
	lastError string
}

var globalAuthLifecycle = &authLifecycleController{store: globalStore, clock: realLifecycleClock{}, wake: make(chan struct{}, 1)}

func nativeScheduling() bool { return globalAccountProtection.config().SchedulingMode != "legacy" }

func (c *authLifecycleController) stop() {
	c.lifeMu.Lock()
	defer c.lifeMu.Unlock()
	if c.cancel != nil {
		c.cancel()
		<-c.done
		c.cancel = nil
	}
	c.opMu.Lock()
	c.opMu.Unlock()
}

func (c *authLifecycleController) configure(cfg pluginConfig) {
	c.stop()
	c.lifeMu.Lock()
	defer c.lifeMu.Unlock()
	c.opMu.Lock()
	c.host = newManagementAuthClient(cfg)
	c.opMu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	c.done = make(chan struct{})
	go func() {
		defer close(c.done)
		for {
			c.reconcile(ctx)
			select {
			case <-ctx.Done():
				return
			case <-c.wake:
			case <-c.clock.After(30 * time.Second):
			}
		}
	}()
}

func (c *authLifecycleController) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}
func (c *authLifecycleController) setError(err error) {
	c.statusMu.Lock()
	defer c.statusMu.Unlock()
	c.lastError = ""
	if err != nil {
		c.lastError = err.Error()
	}
}

func (c *authLifecycleController) status() map[string]any {
	c.statusMu.Lock()
	last := c.lastError
	c.statusMu.Unlock()
	cfg := globalAccountProtection.config()
	client := newManagementAuthClient(cfg)
	mode := "native"
	if !nativeScheduling() {
		mode = "legacy"
	}
	return map[string]any{"scheduling_mode": mode, "management_configured": client.Ready(), "management_config": managementAuthConfigStatus(cfg), "last_error": last, "concurrency_enforced": mode == "legacy" && globalAccountProtection.enabled(), "token_demotion_enforced": mode == "legacy" && globalAccountProtection.enabled(), "reconcile_interval_seconds": 30}
}

func observeAuthLifecycle(ctx context.Context, db *sql.DB, rec usageRecord) error {
	if !nativeScheduling() || !isCodexUsage(rec) {
		return nil
	}
	// Successful requests must not clear a plugin-owned ban: another request may
	// have succeeded concurrently with a quota or permission failure.
	if !rec.Failed {
		exhausted := false
		for _, prefix := range []string{"primary", "secondary"} {
			if pct := headerFloat(rec.ResponseHeaders, "x-codex-"+prefix+"-used-percent"); pct != nil && *pct >= 100 {
				exhausted = true
			}
		}
		if !exhausted {
			return nil
		}
		// A successful final request or quota read can exhaust a window too.
		rec.Failed = true
		rec.Failure = usageFailure{StatusCode: 429, Body: `{"error":{"type":"usage_limit_reached"}}`}
	}
	d := classifyAuthFailure(rec, time.Now())
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO auth_lifecycle_events(auth_index,auth_id,requested_at,payload) VALUES(?,?,?,?)`, rec.AuthIndex, rec.AuthID, rec.RequestedAt.Unix(), string(raw))
	if err == nil {
		globalAuthLifecycle.signal()
	}
	return err
}

func isCodexUsage(rec usageRecord) bool {
	// CPA's Usage scope treats a Codex executor as Codex even when Provider is
	// empty. Keep lifecycle classification aligned with the stored dashboard
	// scope, otherwise those failures are visible as 401 rows but never reach
	// the account-state controller.
	isCodex := stringsEqualCodex(rec.Provider) || strings.Contains(strings.ToLower(trim(rec.ExecutorType)), "codex")
	return isCodex && !isCodexAPIKeyUsageRecord(rec)
}
func stringsEqualCodex(provider string) bool { return strings.EqualFold(trim(provider), "codex") }

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
		c.setError(errors.New("lifecycle state read failed"))
		return
	}
	known := map[string]bool{}
	for _, state := range states {
		known[state.AuthIndex] = true
	}
	if nativeScheduling() {
		for _, entry := range entries {
			if known[entry.AuthIndex] || entry.AuthIndex == "" || !stringsEqualCodex(firstNonEmptyString(entry.Provider, entry.Type)) || entry.RuntimeOnly {
				continue
			}
			snapshot, readErr := readLifecycleInventoryEntry(ctx, c.host, entries, entry.AuthIndex)
			if readErr != nil {
				continue
			}
			state := stateFromSnapshot(snapshot, c.clock.Now())
			if err = saveLifecycleState(ctx, db, &state, c.clock.Now(), "discovered"); err != nil {
				c.setError(errors.New("lifecycle discovery write failed"))
				return
			}
			states = append(states, state)
			known[state.AuthIndex] = true
		}
	}
	for _, state := range states {
		if ctx.Err() != nil {
			return
		}
		if !nativeScheduling() && !state.DisabledByPlugin && state.PendingAction == "" {
			continue
		}
		if state.RetryAt > c.clock.Now().Unix() {
			continue
		}
		snapshot, readErr := readLifecycleInventoryEntry(ctx, c.host, entries, state.AuthIndex)
		if readErr != nil {
			c.retry(ctx, db, &state, "auth identity unavailable; no state write attempted")
			continue
		}
		if state.PendingAction != "" {
			if !nativeScheduling() && state.PendingAction == "disable" && !state.DisabledByPlugin {
				c.conflict(ctx, db, &state, snapshot, "legacy mode canceled unconfirmed native isolation")
				continue
			}
			// Crash between PATCH and confirmation cannot establish ownership.
			desired := state.PendingAction == "disable"
			if snapshot.Entry.Disabled == desired || snapshot.FileDisabled == desired {
				c.conflict(ctx, db, &state, snapshot, "unconfirmed prior operation; manual recheck required")
				continue
			}
			if snapshot.ContentHash != state.PendingContentHash || snapshot.Entry.UpdatedAt != state.RuntimeUpdated {
				c.conflict(ctx, db, &state, snapshot, "auth changed during pending operation")
				continue
			}
			if !c.host.Ready() {
				c.retry(ctx, db, &state, "management API environment is not configured")
				continue
			}
			c.transition(ctx, db, &state, snapshot, desired, state.PendingOwner)
			continue
		}
		if snapshotChanged(state, snapshot) {
			c.conflict(ctx, db, &state, snapshot, "external auth change; manual recheck required")
			continue
		}
		// Repair states written by older versions after an operator enabled a
		// manually disabled auth outside the plugin. The account remains paused
		// for review, but its business state must no longer claim it is disabled.
		if reconcileExternallyEnabledState(&state, snapshot) {
			if err = saveLifecycleState(ctx, db, &state, c.clock.Now(), "external_enable_observed"); err != nil {
				c.setError(errors.New("external enable state write failed"))
				return
			}
		}
		// Refresh-token failure happens before a provider request exists, so CPA
		// may expose it only through the runtime auth status rather than Usage.
		// Polling the exact runtime identity closes that event gap without log
		// parsing or treating transient refresh failures as permanent.
		if !state.Paused && !state.Disabled && state.PendingAction == "" {
			if d, terminal := classifyRuntimeAuthFailure(snapshot.Entry); terminal {
				state.State = d.State
				state.BlockedState = d.State
				state.Reason = d.Reason
				state.RecoverAt = 0
				state.LastHTTPStatus = d.Status
				state.LastErrorType = d.ErrorType
				state.LastErrorCode = d.ErrorCode
				state.LastErrorMessage = d.Reason
				state.CheckOK = false
				state.PendingAction = "disable"
				state.PendingOwner = true
				state.PendingContentHash = state.ContentHash
				if err = saveLifecycleState(ctx, db, &state, c.clock.Now(), "runtime_auth_failure_observed"); err != nil {
					c.setError(errors.New("runtime auth failure state write failed"))
					return
				}
				_ = c.transition(ctx, db, &state, snapshot, true, true)
				continue
			}
		}
		if state.State == authRateLimited && !state.Disabled && state.RecoverAt <= c.clock.Now().Unix() && !state.Paused {
			state.State = authHealthy
			state.BlockedState = ""
			state.RecoverAt = 0
			_ = saveLifecycleState(ctx, db, &state, c.clock.Now(), "rate_limit_observation_expired")
		}
		// Adopt still-current rate-limit observations from versions that only
		// displayed 429 without disabling the exact auth.
		if nativeScheduling() && state.State == authRateLimited && !state.Disabled && !state.Paused && state.RecoverAt > c.clock.Now().Unix() {
			state.BlockedState = authRateLimited
			_ = c.transition(ctx, db, &state, snapshot, true, true)
			continue
		}
		if (state.State == authQuotaCooldown || state.State == authRateLimited) && state.DisabledByPlugin && !state.Paused && state.RecoverAt > 0 && state.RecoverAt <= c.clock.Now().Unix() {
			c.transition(ctx, db, &state, snapshot, false, false)
		}
		// Missing resets are never invented. Read-only recheck may discover one.
		if state.State == authQuotaCooldown && state.DisabledByPlugin && !state.Paused && state.RecoverAt == 0 {
			c.discoverQuotaReset(ctx, db, &state, snapshot)
		}
	}
	if nativeScheduling() {
		if err = c.processEvents(ctx, db, entries); err != nil {
			c.setError(errors.New("lifecycle event processing failed"))
			return
		}
	}
}

func stateFromSnapshot(s lifecycleSnapshot, now time.Time) authLifecycleState {
	state := authLifecycleState{AuthIndex: s.Entry.AuthIndex, AuthID: s.Entry.ID, Name: s.Entry.Name, Identity: s.Identity, Fingerprint: s.Fingerprint, ContentHash: s.ContentHash, RuntimeUpdated: s.Entry.UpdatedAt, State: authHealthy, Disabled: s.Entry.Disabled, SyncStatus: "synced", CreatedAt: now.Unix()}
	if s.Entry.Disabled || s.FileDisabled {
		state.State = authManualDisabled
		state.Paused = true
	}
	if s.Entry.Disabled != s.FileDisabled {
		state.SyncStatus = "conflict"
		state.Paused = true
		state.SyncError = "physical and runtime state differ"
	}
	if updated, err := time.Parse(time.RFC3339Nano, s.Entry.UpdatedAt); err == nil {
		state.IgnoreBefore = updated.Unix()
	}
	return state
}

func snapshotChanged(s authLifecycleState, current lifecycleSnapshot) bool {
	return s.AuthID != current.Entry.ID || s.Name != current.Entry.Name || s.ContentHash != current.ContentHash || s.Identity != current.Identity || s.Fingerprint != current.Fingerprint || s.Disabled != current.Entry.Disabled || current.Entry.Disabled != current.FileDisabled || (s.DisabledByPlugin && s.RuntimeUpdated != current.Entry.UpdatedAt)
}

func (c *authLifecycleController) conflict(ctx context.Context, db *sql.DB, s *authLifecycleState, current lifecycleSnapshot, reason string) {
	s.Paused = true
	s.DisabledByPlugin = false
	s.Disabled = current.Entry.Disabled
	s.PendingAction = ""
	s.PendingOwner = false
	s.SyncStatus = "conflict"
	s.SyncError = reason
	s.Fingerprint = current.Fingerprint
	s.Identity = current.Identity
	s.ContentHash = current.ContentHash
	s.RuntimeUpdated = current.Entry.UpdatedAt
	s.IgnoreBefore = c.clock.Now().Unix()
	s.CheckOK = false
	s.RetryAt = 0
	if current.Entry.Disabled || current.FileDisabled {
		s.State = authManualDisabled
	} else {
		reconcileExternallyEnabledState(s, current)
	}
	if err := saveLifecycleState(ctx, db, s, c.clock.Now(), "ownership_conflict"); err != nil {
		c.setError(errors.New("lifecycle conflict write failed"))
	}
}

func reconcileExternallyEnabledState(s *authLifecycleState, current lifecycleSnapshot) bool {
	if s.State != authManualDisabled || current.Entry.Disabled || current.FileDisabled {
		return false
	}
	s.Disabled = false
	s.DisabledAt = 0
	s.RecoverAt = 0
	s.CheckOK = false
	if s.BlockedState != "" {
		s.State = s.BlockedState
		s.Reason = "external_enable_requires_recheck"
	} else {
		s.State = authHealthy
		s.BlockedState = ""
		s.Reason = "external_enable"
	}
	return true
}

func (c *authLifecycleController) retry(ctx context.Context, db *sql.DB, s *authLifecycleState, reason string) {
	s.RetryCount++
	delay := time.Second * 5
	for i := 1; i < s.RetryCount && delay < 5*time.Minute; i++ {
		delay *= 2
	}
	if delay > 5*time.Minute {
		delay = 5 * time.Minute
	}
	s.RetryAt = c.clock.Now().Add(delay).Unix()
	s.SyncStatus = "pending"
	s.SyncError = reason
	if err := saveLifecycleState(ctx, db, s, c.clock.Now(), "retry_pending"); err != nil {
		c.setError(errors.New("lifecycle retry write failed"))
	}
	c.setError(errors.New(reason))
}

func (c *authLifecycleController) transition(ctx context.Context, db *sql.DB, s *authLifecycleState, before lifecycleSnapshot, disabled, owner bool) error {
	if !c.host.Ready() {
		c.retry(ctx, db, s, "management API environment is not configured")
		return errors.New("management API environment is not configured")
	}
	// Re-read directly before the mutation, including metadata and runtime revision.
	current, err := c.host.Read(ctx, s.AuthIndex)
	if err != nil {
		c.retry(ctx, db, s, "auth read before status change failed")
		return err
	}
	if current.ContentHash != before.ContentHash || current.Entry.ID != before.Entry.ID || current.Entry.Name != before.Entry.Name || current.Entry.Disabled != before.Entry.Disabled || current.Entry.UpdatedAt != before.Entry.UpdatedAt || current.FileDisabled != before.FileDisabled {
		c.conflict(ctx, db, s, current, "auth changed before status update")
		return errLifecycleConflict
	}
	s.PendingAction = "enable"
	if disabled {
		s.PendingAction = "disable"
	}
	s.PendingOwner = owner
	s.PendingContentHash = current.ContentHash
	s.PendingAt = c.clock.Now().Unix()
	s.SyncStatus = "pending"
	s.SyncError = ""
	if err = saveLifecycleState(ctx, db, s, c.clock.Now(), "status_intent"); err != nil {
		return err
	}
	patchErr := c.host.SetDisabled(ctx, current, disabled)
	after, readErr := c.host.Read(ctx, s.AuthIndex)
	if readErr != nil {
		c.retry(ctx, db, s, "status read-back unavailable")
		return errors.New("status read-back unavailable")
	}
	if after.ContentHash != current.ContentHash || after.Entry.ID != current.Entry.ID || after.Entry.Name != current.Entry.Name {
		c.conflict(ctx, db, s, after, "auth fields changed during status write; not overwriting credentials")
		return errLifecycleConflict
	}
	if after.Entry.Disabled != disabled || after.FileDisabled != disabled {
		c.retry(ctx, db, s, "status not confirmed in both physical and runtime auth")
		return errors.New("status not confirmed in both physical and runtime auth")
	}
	// A timeout can follow a successful PATCH. Read-back is authoritative.
	_ = patchErr
	s.Disabled = disabled
	s.DisabledByPlugin = disabled && owner
	s.RuntimeUpdated = after.Entry.UpdatedAt
	s.PendingAction = ""
	s.PendingOwner = false
	s.SyncStatus = "synced"
	s.SyncError = ""
	s.RetryAt = 0
	s.RetryCount = 0
	if disabled {
		s.DisabledAt = c.clock.Now().Unix()
	} else {
		s.State = authHealthy
		s.BlockedState = ""
		s.RecoverAt = 0
		s.DisabledAt = 0
		s.Reason = ""
		s.Paused = false
		s.IgnoreBefore = c.clock.Now().Unix()
	}
	if err = saveLifecycleState(ctx, db, s, c.clock.Now(), "status_confirmed"); err != nil {
		return err
	}
	globalCodexAuthSource.invalidate()
	log.Printf("auth lifecycle status confirmed: auth_index=%s disabled=%t plugin_owned=%t state=%s", s.AuthIndex, s.Disabled, s.DisabledByPlugin, s.State)
	c.setError(nil)
	return nil
}

type lifecycleEvent struct {
	ID            int64
	Index, AuthID string
	RequestedAt   int64
	Decision      authDecision
}

func (c *authLifecycleController) processEvents(ctx context.Context, db *sql.DB, entries []hostAuthFileEntry) error {
	rows, err := db.QueryContext(ctx, `SELECT id,auth_index,auth_id,requested_at,payload FROM auth_lifecycle_events WHERE processed=0 AND retry_at<=? ORDER BY id LIMIT 100`, c.clock.Now().Unix())
	if err != nil {
		return err
	}
	events := []lifecycleEvent{}
	for rows.Next() {
		var event lifecycleEvent
		var raw string
		if err = rows.Scan(&event.ID, &event.Index, &event.AuthID, &event.RequestedAt, &raw); err != nil {
			rows.Close()
			return err
		}
		if err = json.Unmarshal([]byte(raw), &event.Decision); err != nil {
			rows.Close()
			return err
		}
		events = append(events, event)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, event := range events {
		index := ""
		count := 0
		for _, entry := range entries {
			if (event.Index != "" && entry.AuthIndex == event.Index && (event.AuthID == "" || event.AuthID == entry.ID)) || (event.Index == "" && event.AuthID != "" && entry.ID == event.AuthID) {
				index = entry.AuthIndex
				count++
			}
		}
		if count == 1 && index != "" {
			s, loadErr := loadLifecycleState(ctx, db, index)
			if loadErr != nil {
				if !errors.Is(loadErr, sql.ErrNoRows) {
					return loadErr
				}
				// A temporarily unavailable physical auth must not discard the
				// failure that would isolate it. Defer without blocking later events.
				if _, err = db.ExecContext(ctx, `UPDATE auth_lifecycle_events SET retry_at=? WHERE id=?`, c.clock.Now().Add(30*time.Second).Unix(), event.ID); err != nil {
					return err
				}
				c.setError(errors.New("account snapshot unavailable; failure deferred"))
				continue
			}
			if loadErr == nil && event.ID > s.LastEventID {
				d := event.Decision
				s.LastEventID = event.ID
				s.LastHTTPStatus = d.Status
				s.LastErrorType = d.ErrorType
				s.LastErrorCode = d.ErrorCode
				s.LastErrorMessage = d.Reason
				if d.ModelIssue {
					if _, err = db.ExecContext(ctx, `INSERT INTO auth_model_issues(auth_index,model,observed_at,status,error_code) VALUES(?,?,?,?,?) ON CONFLICT(auth_index,model) DO UPDATE SET observed_at=excluded.observed_at,status=excluded.status,error_code=excluded.error_code`, index, d.Model, c.clock.Now().Unix(), d.Status, d.Reason); err != nil {
						return err
					}
				}
				eligible := !s.Paused && s.PendingAction == "" && event.RequestedAt >= s.IgnoreBefore && (!s.Disabled || (s.DisabledByPlugin && d.Disable && lifecycleFailurePriority(d.State) >= lifecycleFailurePriority(s.State)))
				if eligible && d.State != authHealthy {
					if s.Disabled && d.State == s.State {
						if d.RecoverAt == 0 || s.RecoverAt == 0 {
							d.RecoverAt = 0
						} else if s.RecoverAt > d.RecoverAt {
							d.RecoverAt = s.RecoverAt
						}
					}
					s.CheckOK = false
					s.State = d.State
					s.Reason = d.Reason
					s.RecoverAt = d.RecoverAt
					if d.Disable {
						s.BlockedState = d.State
					}
				}
				if eligible && d.Disable && !s.Disabled {
					s.PendingAction = "disable"
					s.PendingOwner = true
					s.PendingContentHash = s.ContentHash
				}
				if err = saveLifecycleState(ctx, db, &s, c.clock.Now(), "failure_observed"); err != nil {
					return err
				}
				if eligible && d.Disable && !s.Disabled {
					snapshot, readErr := c.host.Read(ctx, index)
					if readErr != nil {
						c.retry(ctx, db, &s, "auth read failed; isolation not applied")
					} else if snapshotChanged(s, snapshot) {
						c.conflict(ctx, db, &s, snapshot, "auth changed before failure isolation")
					} else {
						_ = c.transition(ctx, db, &s, snapshot, true, true)
					}
				}
			}
		} else {
			c.setError(errors.New("usage auth identity missing or ambiguous; automatic isolation skipped"))
		}
		if _, err = db.ExecContext(ctx, `UPDATE auth_lifecycle_events SET processed=1 WHERE id=?`, event.ID); err != nil {
			return err
		}
	}
	if len(events) == 100 {
		c.signal()
	}
	return nil
}

func lifecycleFailurePriority(state string) int {
	switch state {
	case authInvalid, authBillingBlocked, authPermissionBlocked:
		return 3
	case authQuotaCooldown:
		return 2
	case authRateLimited:
		return 1
	}
	return 0
}

func (c *authLifecycleController) discoverQuotaReset(ctx context.Context, db *sql.DB, s *authLifecycleState, snapshot lifecycleSnapshot) {
	account := triggerAuthAccount{configuredAccount: configuredAccount{AuthID: s.AuthID, AuthIndex: s.AuthIndex, Provider: "codex"}, AccessToken: lifecycleString(snapshot.Data, "access_token"), ChatGPTAccountID: lifecycleString(snapshot.Data, "account_id")}
	run := executeQuotaUsageRequest(ctx, db, account, defaultPluginConfig())
	after, err := c.host.Read(ctx, s.AuthIndex)
	if err != nil {
		c.retry(ctx, db, s, "quota recheck auth read-back failed")
		return
	}
	if snapshotChanged(*s, after) {
		c.conflict(ctx, db, s, after, "auth changed during quota recheck")
		return
	}
	s.RetryAt = c.clock.Now().Add(5 * time.Minute).Unix()
	if run.HTTPStatus >= 200 && run.HTTPStatus < 300 {
		decision := classifyAuthFailure(usageRecord{Failed: true, Failure: usageFailure{StatusCode: 429, Body: `{"error":{"type":"usage_limit_reached"}}`}, ResponseHeaders: run.ResponseHeaders}, c.clock.Now())
		s.RecoverAt = decision.RecoverAt
		if s.RecoverAt == 0 && lifecycleQuotaAvailable(run, c.clock.Now()) {
			s.RecoverAt = c.clock.Now().Unix()
		}
		if s.RecoverAt > 0 {
			s.RetryAt = 0
		}
	}
	if err = saveLifecycleState(ctx, db, s, c.clock.Now(), "quota_reset_recheck"); err != nil {
		c.setError(errors.New("quota recheck state write failed"))
	}
}

func lifecycleQuotaAvailable(run quotaTriggerRun, now time.Time) bool {
	quota := quotaActivationQuotaFromRun(run)
	observed := false
	for _, window := range []quotaActivationWindow{quota.Primary, quota.Secondary} {
		if window.Presence == quotaWindowAbsent {
			continue
		}
		if !activationWindowMetadataValid(window, now.Unix()) || window.UsedPercent == nil || *window.UsedPercent >= 100 {
			return false
		}
		observed = true
	}
	return observed
}

func readLifecycleInventoryEntry(ctx context.Context, host lifecycleHost, entries []hostAuthFileEntry, index string) (lifecycleSnapshot, error) {
	if client, ok := host.(*managementAuthClient); ok {
		var match hostAuthFileEntry
		count := 0
		for _, entry := range entries {
			if entry.AuthIndex == index {
				match = entry
				count++
			}
		}
		if count != 1 {
			return lifecycleSnapshot{}, errors.New("auth identity missing or ambiguous")
		}
		return client.readEntry(ctx, match)
	}
	return host.Read(ctx, index)
}
