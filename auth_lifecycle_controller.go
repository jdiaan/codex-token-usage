package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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

func nativeScheduling() bool { return true }

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
	c.host = newManagementAuthClient(globalManagementConfig.current())
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
	config := globalManagementConfig.current()
	return map[string]any{"management_configured": config.ready(), "management_config": config.status(), "last_error": last, "concurrency_enforced": false, "token_demotion_enforced": false, "obsolete_config_keys": obsoleteConfigKeys(), "reconcile_interval_seconds": 30}
}

type lifecycleExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func observeAuthLifecycle(ctx context.Context, db lifecycleExecer, rec usageRecord) error {
	inserted, err := persistAuthLifecycleEvent(ctx, db, rec)
	if err == nil && inserted {
		globalAuthLifecycle.signal()
	}
	return err
}

func persistAuthLifecycleEvent(ctx context.Context, db lifecycleExecer, rec usageRecord) (bool, error) {
	if !nativeScheduling() || !isCodexUsage(rec) {
		return false, nil
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
			return false, nil
		}
		// A successful final request or quota read can exhaust a window too.
		rec.Failed = true
		rec.Failure = usageFailure{StatusCode: 429, Body: `{"error":{"type":"usage_limit_reached"}}`}
	}
	d := classifyAuthFailure(rec, time.Now())
	d.RequestedNS = rec.RequestedAt.UnixNano()
	var stateJSON string
	// Bind to the observed credential generation without reading tokens or making
	// host calls in the usage transaction. The request time guards late arrivals.
	if err := db.QueryRowContext(ctx, `SELECT payload FROM auth_lifecycle_states WHERE auth_index=?`, rec.AuthIndex).Scan(&stateJSON); err == nil {
		var state authLifecycleState
		if json.Unmarshal([]byte(stateJSON), &state) == nil && (rec.AuthID == "" || rec.AuthID == state.AuthID) {
			d.Fingerprint = state.Fingerprint
			if state.CredentialSince > 0 && d.RequestedNS < state.CredentialSince {
				d.Fingerprint = "previous-generation"
			}
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return false, err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO auth_lifecycle_events(auth_index,auth_id,requested_at,payload) VALUES(?,?,?,?)`, rec.AuthIndex, rec.AuthID, rec.RequestedAt.Unix(), string(raw))
	return err == nil, err
}

func isCodexUsage(rec usageRecord) bool {
	// CPA's Usage scope treats a Codex executor as Codex even when Provider is
	// empty. Keep lifecycle classification aligned with the stored dashboard
	// scope, otherwise those failures are visible as 401 rows but never reach
	// the account-state controller.
	isCodex := stringsEqualCodex(rec.Provider) || strings.Contains(strings.ToLower(trim(rec.ExecutorType)), "codex")
	return isCodex && !isAPIKeyAuthType(rec.AuthType) && !isCodexAPIKeyUsageRecord(rec)
}
func stringsEqualCodex(provider string) bool { return strings.EqualFold(trim(provider), "codex") }

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
		if !event.Decision.Disable && !event.Decision.ModelIssue {
			if err = c.recordEventOutcome(ctx, db, event, true, "observed", "non-isolating failure"); err != nil {
				return err
			}
			continue
		}
		index, count := "", 0
		for _, entry := range entries {
			if (event.Index != "" && entry.AuthIndex == event.Index && (event.AuthID == "" || event.AuthID == entry.ID)) || (event.Index == "" && event.AuthID != "" && entry.ID == event.AuthID) {
				index = entry.AuthIndex
				count++
			}
		}
		if count != 1 || index == "" {
			if err = c.recordEventOutcome(ctx, db, event, false, "pending_identity", "auth identity missing or ambiguous"); err != nil {
				return err
			}
			continue
		}
		s, loadErr := loadLifecycleState(ctx, db, index)
		if loadErr != nil {
			if !errors.Is(loadErr, sql.ErrNoRows) {
				return loadErr
			}
			if err = c.recordEventOutcome(ctx, db, event, false, "pending_snapshot", "account snapshot unavailable or unsupported"); err != nil {
				return err
			}
			continue
		}
		d := event.Decision
		if eventIsStale(event, s) {
			if err = c.recordEventOutcome(ctx, db, event, true, "ignored_stale", "request predates current auth control boundary"); err != nil {
				return err
			}
			continue
		}
		if s.ManualDisabled {
			if err = c.recordEventOutcome(ctx, db, event, true, "manual_disabled", "account was manually disabled"); err != nil {
				return err
			}
			continue
		}
		snapshot, readErr := c.host.Read(ctx, index)
		if readErr != nil {
			if err = c.recordEventOutcome(ctx, db, event, false, "pending_snapshot", "auth snapshot temporarily unavailable"); err != nil {
				return err
			}
			continue
		}
		if err = c.observeSnapshot(ctx, db, &s, snapshot); err != nil {
			if err = c.recordEventOutcome(ctx, db, event, false, "pending_snapshot", "account identity or snapshot not ready"); err != nil {
				return err
			}
			continue
		}
		if eventIsStale(event, s) {
			if err = c.recordEventOutcome(ctx, db, event, true, "ignored_stale", "failure belongs to previous credential generation"); err != nil {
				return err
			}
			continue
		}
		if s.ManualDisabled {
			if err = c.recordEventOutcome(ctx, db, event, true, "manual_disabled", "account was manually disabled"); err != nil {
				return err
			}
			continue
		}
		if d.ModelIssue {
			if _, err = db.ExecContext(ctx, `INSERT INTO auth_model_issues(auth_index,model,observed_at,status,error_code) VALUES(?,?,?,?,?) ON CONFLICT(auth_index,model) DO UPDATE SET observed_at=excluded.observed_at,status=excluded.status,error_code=excluded.error_code`, index, d.Model, c.clock.Now().Unix(), d.Status, d.Reason); err != nil {
				return err
			}
		}
		if !d.Disable || ((s.Disabled || s.PendingAction == "disable") && lifecycleFailurePriority(d.State) < lifecycleFailurePriority(s.State)) {
			if err = c.recordEventOutcome(ctx, db, event, true, "observed", "non-isolating or superseded failure"); err != nil {
				return err
			}
			continue
		}
		if temporaryAuthState(d.State) {
			if d.RecoverAt == 0 {
				d.RecoverAt = event.RequestedAt + 60
			}
			if d.RecoverAt <= c.clock.Now().Unix() {
				if err = c.recordEventOutcome(ctx, db, event, true, "ignored_expired", "temporary limit already expired"); err != nil {
					return err
				}
				continue
			}
			if temporaryAuthState(s.State) && s.RecoverAt > d.RecoverAt {
				d.RecoverAt = s.RecoverAt
			}
		}
		// Pending disable is executable intent, not a reason to lose later failures.
		// Upgrade 429 to 401 even while Management is unavailable.
		s.State, s.BlockedState, s.Reason, s.RecoverAt = d.State, d.State, d.Reason, d.RecoverAt
		s.LastHTTPStatus, s.LastErrorType, s.LastErrorCode, s.LastErrorMessage = d.Status, d.ErrorType, d.ErrorCode, d.Reason
		s.CheckOK = false
		s.CheckResult = ""
		s.BlockedFingerprint = s.Fingerprint
		s.Paused = false
		if event.ID > s.LastEventID {
			s.LastEventID = event.ID
		}
		if !s.Disabled || s.PendingAction == "enable" {
			s.PendingAction, s.PendingOwner, s.PendingContentHash = "disable", true, s.ContentHash
		}
		if err = saveLifecycleState(ctx, db, &s, c.clock.Now(), "failure_observed"); err != nil {
			return err
		}
		outcome := "disabled"
		if !s.Disabled {
			outcome = "pending_disable"
		}
		if err = c.recordEventOutcome(ctx, db, event, true, outcome, "failure merged into account isolation"); err != nil {
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
