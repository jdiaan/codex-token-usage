package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func enqueueTimedLifecycleFailure(t *testing.T, c *authLifecycleController, requested time.Time) {
	t.Helper()
	db, _, err := c.store.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s := lifecycleStateForTest(t, c, "a")
	d := classifyAuthFailure(usageRecord{Failed: true, Failure: usageFailure{StatusCode: 401}}, requested)
	d.Fingerprint = s.Fingerprint
	d.RequestedNS = requested.UnixNano()
	raw, _ := json.Marshal(d)
	if _, err = db.Exec(`INSERT INTO auth_lifecycle_events(auth_index,auth_id,requested_at,payload) VALUES('a','a.json',?,?)`, requested.Unix(), string(raw)); err != nil {
		t.Fatal(err)
	}
}

func TestManualEnableSupersedesSameSecondFailuresAcrossRestart(t *testing.T) {
	c, host, clock := nativeTestController(t)
	probes := quotaServerForTest(t, func(w http.ResponseWriter, r *http.Request) { t.Error("enable sent an upstream probe") })
	snapshot := host.accounts["a"]
	snapshot.Entry.StatusMessage = "refresh_token_invalidated"
	host.accounts["a"] = snapshot
	c.reconcile(context.Background())
	oldTime := clock.Now().Add(100 * time.Millisecond)
	clock.now = clock.now.Add(500 * time.Millisecond)
	enqueueTimedLifecycleFailure(t, c, oldTime)
	s := lifecycleStateForTest(t, c, "a")
	got, err := c.action(context.Background(), lifecycleActionRequest{AuthIndex: "a", Version: s.Version, Action: "enable"})
	if err != nil || got.Disabled || got.State != authHealthy || got.ControlSince != clock.Now().UnixNano() {
		t.Fatalf("enable failed: %+v %v", got, err)
	}
	// Force all state through SQLite, keeping the same stale CPA runtime error.
	c.store.close()
	c.reconcile(context.Background())
	enqueueTimedLifecycleFailure(t, c, oldTime) // A late callback after restart.
	c.reconcile(context.Background())
	if host.accounts["a"].FileDisabled {
		t.Fatal("old same-second failure undid enable")
	}
	db, _, _ := c.store.open(context.Background())
	accounts, err := queryReloginRequiredAccounts(context.Background(), db)
	if err != nil || len(accounts) != 0 {
		t.Fatalf("recovered account remains in relogin list: %v %v", accounts, err)
	}
	clock.now = clock.now.Add(100 * time.Millisecond)
	enqueueTimedLifecycleFailure(t, c, clock.Now())
	c.reconcile(context.Background())
	if !host.accounts["a"].FileDisabled {
		t.Fatal("new failure in same second incorrectly ignored")
	}
	if probes.Load() != 0 {
		t.Fatal("unexpected quota probe")
	}
}

func TestReloginRetryDoesNotMoveCredentialBoundary(t *testing.T) {
	c, host, clock := nativeTestController(t)
	lifecycleFailure(t, c, "a", 401, `{}`)
	clock.now = clock.now.Add(time.Second)
	rotateLifecycleCredential(t, c, host, "a")
	host.partial = true
	c.reconcile(context.Background())
	s := lifecycleStateForTest(t, c, "a")
	boundary := s.CredentialSince
	retryAt := s.RetryAt
	clock.now = clock.now.Add(time.Millisecond)
	c.reconcile(context.Background())
	s = lifecycleStateForTest(t, c, "a")
	if s.CredentialSince != boundary || s.RetryAt != retryAt {
		t.Fatalf("retry moved login boundary: %+v", s)
	}
	enqueueTimedLifecycleFailure(t, c, clock.Now())
	clock.now = clock.now.Add(time.Minute)
	host.partial = false
	c.reconcile(context.Background())
	if !host.accounts["a"].FileDisabled || lifecycleStateForTest(t, c, "a").State != authInvalid {
		t.Fatal("new failure lost while enabling new credentials")
	}
}

func TestManualEnablePartialSyncRetainsBoundaryAndRetries(t *testing.T) {
	c, host, clock := nativeTestController(t)
	lifecycleFailure(t, c, "a", 401, `{}`)
	clock.now = clock.now.Add(time.Second)
	host.partial = true
	s := lifecycleStateForTest(t, c, "a")
	_, err := c.action(context.Background(), lifecycleActionRequest{AuthIndex: "a", Version: s.Version, Action: "enable"})
	if err == nil {
		t.Fatal("partial enable reported success")
	}
	s = lifecycleStateForTest(t, c, "a")
	if s.PendingAction != "enable" || s.SyncStatus != "pending" || s.ControlSince == 0 {
		t.Fatalf("lost retry intent: %+v", s)
	}
	boundary := s.ControlSince
	enqueueTimedLifecycleFailure(t, c, clock.Now().Add(-time.Millisecond))
	c.store.close()
	host.partial = false
	clock.now = clock.now.Add(time.Minute)
	c.reconcile(context.Background())
	s = lifecycleStateForTest(t, c, "a")
	if host.accounts["a"].FileDisabled || s.PendingAction != "" || s.SyncStatus != "synced" || s.ControlSince != boundary {
		t.Fatalf("retry failed: %+v", s)
	}
}

func TestScreenshotConflictMigrationRecoversOfflineLogin(t *testing.T) {
	for _, overwritten := range []bool{false, true} {
		t.Run(map[bool]string{false: "old_fingerprint", true: "old_conflict_overwrote_fingerprint"}[overwritten], func(t *testing.T) {
			c, host, clock := nativeTestController(t)
			lifecycleFailure(t, c, "a", 401, `{}`)
			s := lifecycleStateForTest(t, c, "a")
			oldFingerprint := s.Fingerprint
			clock.now = clock.now.Add(time.Second)
			rotateLifecycleCredential(t, c, host, "a")
			snapshot := host.accounts["a"]
			snapshot.Entry.StatusMessage = "refresh_token_invalidated"
			host.accounts["a"] = snapshot
			s.PolicyVersion = 0
			s.State, s.BlockedState = authManualDisabled, authInvalid
			s.SyncStatus, s.SyncError, s.Reason = "conflict", "external auth change; manual recheck required", "auth_invalid"
			s.Paused, s.DisabledByPlugin = true, false
			if overwritten {
				s.Fingerprint = snapshot.Fingerprint
			}
			db, _, _ := c.store.open(context.Background())
			if _, err := db.Exec(`UPDATE auth_lifecycle_events SET payload=json_set(payload,'$.credential_fingerprint',?),processed=1,outcome='identity_conflict'`, oldFingerprint); err != nil {
				t.Fatal(err)
			}
			if err := saveLifecycleState(context.Background(), db, &s, clock.Now(), "old_conflict"); err != nil {
				t.Fatal(err)
			}
			c.store.close()
			c.reconcile(context.Background())
			c.reconcile(context.Background())
			s = lifecycleStateForTest(t, c, "a")
			if s.State != authHealthy || s.Paused || s.Disabled || host.accounts["a"].FileDisabled {
				t.Fatalf("offline login stuck: %+v", s)
			}
		})
	}
}

func TestOldConflictMetadataChangeDoesNotPretendToBeLogin(t *testing.T) {
	c, host, clock := nativeTestController(t)
	lifecycleFailure(t, c, "a", 401, `{}`)
	s := lifecycleStateForTest(t, c, "a")
	s.PolicyVersion = 0
	s.State, s.BlockedState = authManualDisabled, authInvalid
	s.Paused, s.DisabledByPlugin = true, false
	s.SyncStatus, s.Reason = "conflict", "auth_invalid"
	db, _, _ := c.store.open(context.Background())
	if err := saveLifecycleState(context.Background(), db, &s, clock.Now(), "old_conflict"); err != nil {
		t.Fatal(err)
	}
	clock.now = clock.now.Add(time.Hour)
	snapshot := host.accounts["a"]
	snapshot.ContentHash = "metadata-only"
	snapshot.Entry.UpdatedAt = clock.Now().Format(time.RFC3339Nano)
	snapshot.Data["last_refresh"], _ = json.Marshal(clock.Now().Format(time.RFC3339Nano))
	host.accounts["a"] = snapshot
	c.reconcile(context.Background())
	s = lifecycleStateForTest(t, c, "a")
	if !s.Disabled || s.State != authInvalid || s.ManualDisabled || s.Paused {
		t.Fatalf("metadata treated as login or manual disable: %+v", s)
	}
}

func TestUnknownDisabledOriginNeedsExplicitEnable(t *testing.T) {
	c, host, clock := nativeTestController(t)
	db, _, _ := c.store.open(context.Background())
	if _, err := db.Exec(`DELETE FROM auth_lifecycle_states WHERE auth_index='a'`); err != nil {
		t.Fatal(err)
	}
	snapshot := host.accounts["a"]
	snapshot.FileDisabled, snapshot.Entry.Disabled = true, true
	host.accounts["a"] = snapshot
	c.reconcile(context.Background())
	clock.now = clock.now.Add(time.Hour)
	rotateLifecycleCredential(t, c, host, "a")
	c.reconcile(context.Background())
	s := lifecycleStateForTest(t, c, "a")
	if s.State != authDisabledUnknown || s.ManualDisabled || s.DisabledByPlugin || !s.Disabled || len(host.writes) != 0 {
		t.Fatalf("invented disable ownership: %+v", s)
	}
	if _, err := c.action(context.Background(), lifecycleActionRequest{AuthIndex: "a", Version: s.Version, Action: "enable"}); err != nil {
		t.Fatal(err)
	}
	if host.accounts["a"].FileDisabled {
		t.Fatal("unknown account could not be enabled")
	}
}

func TestOldExplicitManualDisableSurvivesOfflineLogin(t *testing.T) {
	c, host, clock := nativeTestController(t)
	s := lifecycleStateForTest(t, c, "a")
	if _, err := c.action(context.Background(), lifecycleActionRequest{AuthIndex: "a", Version: s.Version, Action: "disable"}); err != nil {
		t.Fatal(err)
	}
	s = lifecycleStateForTest(t, c, "a")
	s.PolicyVersion = 0
	db, _, _ := c.store.open(context.Background())
	if err := saveLifecycleState(context.Background(), db, &s, clock.Now(), "old_manual"); err != nil {
		t.Fatal(err)
	}
	clock.now = clock.now.Add(time.Hour)
	rotateLifecycleCredential(t, c, host, "a")
	c.reconcile(context.Background())
	s = lifecycleStateForTest(t, c, "a")
	if !s.ManualDisabled || !s.Disabled || s.DisabledByPlugin {
		t.Fatalf("manual disable lost: %+v", s)
	}
}

func TestEnableRejectsStaleVersionWithoutWrite(t *testing.T) {
	c, host, _ := nativeTestController(t)
	s := lifecycleStateForTest(t, c, "a")
	lifecycleFailure(t, c, "a", 401, `{}`)
	writes := len(host.writes)
	_, err := c.action(context.Background(), lifecycleActionRequest{AuthIndex: "a", Version: s.Version, Action: "enable"})
	if !errors.Is(err, errLifecycleConflict) || len(host.writes) != writes {
		t.Fatalf("stale action changed CPA: %v", err)
	}
}

func TestManagement401IsReportedAsManagementAuthentication(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer server.Close()
	client := &managementAuthClient{baseURL: server.URL, key: "wrong-key", client: server.Client()}
	err := client.SetDisabled(context.Background(), lifecycleSnapshot{Entry: hostAuthFileEntry{Name: "a.json", AuthIndex: "a"}}, false)
	if err == nil || !strings.Contains(err.Error(), "management authentication failed") || !strings.Contains(err.Error(), "management_key") || strings.Contains(err.Error(), "wrong-key") {
		t.Fatalf("unhelpful management 401: %v", err)
	}
}

type rejectedLifecycleWriteHost struct{ *fakeLifecycleHost }

func (h *rejectedLifecycleWriteHost) SetDisabled(context.Context, lifecycleSnapshot, bool) error {
	return errors.New("CPA management authentication failed (HTTP 401); check management_key")
}

func TestEnablePreservesManagementWriteFailureForDashboard(t *testing.T) {
	c, host, _ := nativeTestController(t)
	lifecycleFailure(t, c, "a", 401, `{}`)
	c.host = &rejectedLifecycleWriteHost{host}
	s := lifecycleStateForTest(t, c, "a")
	_, err := c.action(context.Background(), lifecycleActionRequest{AuthIndex: "a", Version: s.Version, Action: "enable"})
	if err == nil || !strings.Contains(err.Error(), "management authentication failed") {
		t.Fatalf("write failure hidden: %v", err)
	}
	s = lifecycleStateForTest(t, c, "a")
	if !s.Disabled || s.PendingAction != "enable" || !strings.Contains(s.SyncError, "management_key") {
		t.Fatalf("failed enable not retryable: %+v", s)
	}
}
