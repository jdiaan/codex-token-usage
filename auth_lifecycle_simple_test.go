package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func rotateLifecycleCredential(t *testing.T, c *authLifecycleController, host *fakeLifecycleHost, index string) {
	t.Helper()
	snapshot := host.accounts[index]
	snapshot.Fingerprint += "-new"
	snapshot.ContentHash += "-new"
	snapshot.Entry.UpdatedAt = c.clock.Now().Format(time.RFC3339Nano)
	snapshot.Data["last_refresh"], _ = json.Marshal(c.clock.Now().Format(time.RFC3339Nano))
	host.accounts[index] = snapshot
}

func TestSimpleLoginRecoveryAndLateOldFailure(t *testing.T) {
	for _, status := range []int{401, 402} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			c, host, clock := nativeTestController(t)
			calls := quotaServerForTest(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected upstream probe") })
			oldTime := clock.Now()
			lifecycleFailure(t, c, "a", status, `{}`)
			oldFingerprint := lifecycleStateForTest(t, c, "a").Fingerprint
			clock.now = clock.now.Add(time.Second)
			rotateLifecycleCredential(t, c, host, "a")
			c.reconcile(context.Background())
			s := lifecycleStateForTest(t, c, "a")
			if s.Disabled || s.Paused || s.State != authHealthy || host.accounts["a"].FileDisabled {
				t.Fatalf("login did not recover: %+v", s)
			}
			db, _, _ := c.store.open(context.Background())
			d := classifyAuthFailure(usageRecord{Failed: true, Failure: usageFailure{StatusCode: status}}, clock.Now())
			d.Fingerprint, d.RequestedNS = oldFingerprint, oldTime.UnixNano()
			raw, _ := json.Marshal(d)
			if _, err := db.Exec(`INSERT INTO auth_lifecycle_events(auth_index,auth_id,requested_at,payload) VALUES('a','a.json',?,?)`, oldTime.Unix(), string(raw)); err != nil {
				t.Fatal(err)
			}
			c.reconcile(context.Background())
			if host.accounts["a"].FileDisabled || calls.Load() != 0 {
				t.Fatal("old failure disabled new credentials or probed")
			}
			clock.now = clock.now.Add(time.Second)
			lifecycleFailure(t, c, "a", status, `{}`)
			if !host.accounts["a"].FileDisabled {
				t.Fatal("new failure after login was ignored")
			}
		})
	}
}

func TestSimpleManualDisableSurvivesCredentialChange(t *testing.T) {
	c, host, clock := nativeTestController(t)
	snapshot := host.accounts["a"]
	snapshot.FileDisabled, snapshot.Entry.Disabled = true, true
	host.accounts["a"] = snapshot
	c.reconcile(context.Background())
	clock.now = clock.now.Add(time.Hour)
	rotateLifecycleCredential(t, c, host, "a")
	c.reconcile(context.Background())
	lifecycleFailure(t, c, "a", 401, `{}`)
	s := lifecycleStateForTest(t, c, "a")
	if !s.ManualDisabled || s.DisabledByPlugin || !host.accounts["a"].FileDisabled || len(host.writes) != 0 {
		t.Fatalf("manual disable changed: %+v", s)
	}
}

func TestSimpleUnknown429AndAliasesNeverProbe(t *testing.T) {
	c, host, clock := nativeTestController(t)
	calls := quotaServerForTest(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected upstream probe") })
	lifecycleFailure(t, c, "a", 429, `{"error":{"type":"usage_limit_reached"}}`)
	s := lifecycleStateForTest(t, c, "a")
	if !s.Disabled || s.RecoverAt != clock.Now().Add(time.Minute).Unix() {
		t.Fatalf("missing default cooldown: %+v", s)
	}
	for _, action := range []string{"retry_sync", "recheck", "check_and_recover"} {
		s = lifecycleStateForTest(t, c, "a")
		if _, err := c.action(context.Background(), lifecycleActionRequest{AuthIndex: "a", Version: s.Version, Action: action}); err != nil {
			t.Fatal(err)
		}
	}
	clock.now = clock.now.Add(61 * time.Second)
	c.reconcile(context.Background())
	if host.accounts["a"].FileDisabled || calls.Load() != 0 {
		t.Fatal("timer failed or made a quota request")
	}
}

func TestSimplePartialWriteRetriesToConfirmation(t *testing.T) {
	c, host, clock := nativeTestController(t)
	host.partial = true
	lifecycleFailure(t, c, "a", 401, `{}`)
	s := lifecycleStateForTest(t, c, "a")
	if s.Paused || s.PendingAction != "disable" || s.DisabledByPlugin {
		t.Fatalf("partial sync=%+v", s)
	}
	host.partial = false
	clock.now = clock.now.Add(time.Minute)
	c.reconcile(context.Background())
	s = lifecycleStateForTest(t, c, "a")
	if !s.Disabled || !s.DisabledByPlugin || s.PendingAction != "" || !host.accounts["a"].FileDisabled {
		t.Fatalf("retry failed=%+v", s)
	}
}

func TestSimpleLatestQuotaResetSurvivesMissingWindow(t *testing.T) {
	now := time.Unix(2000000000, 0)
	for _, status := range []int{429, 503} {
		d := classifyAuthFailure(usageRecord{Failed: true, Failure: usageFailure{StatusCode: status, Body: `{"error":{"type":"usage_limit_reached"}}`}, ResponseHeaders: map[string][]string{"x-codex-primary-used-percent": {"100"}, "x-codex-secondary-used-percent": {"100"}, "x-codex-secondary-reset-at": {"2000003600"}}}, now)
		if !d.Disable || d.RecoverAt != 2000003600 {
			t.Fatalf("lost reset=%+v", d)
		}
	}
}

func TestSimpleMigrationReplaysScreenshot401AndCleansHealthyRows(t *testing.T) {
	c, host, clock := nativeTestController(t)
	db, _, _ := c.store.open(context.Background())
	for _, index := range []string{"a", "b"} {
		s := lifecycleStateForTest(t, c, index)
		s.PolicyVersion = 0
		s.Paused = true
		s.SyncStatus = "conflict"
		s.IgnoreBefore = clock.Now().Add(time.Minute).Unix()
		s.Reason = "external_enable"
		if err := saveLifecycleState(context.Background(), db, &s, clock.Now(), "old_conflict"); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		id := enqueueFailureForTest(t, c, "a", "a.json", 401, clock.Now().Unix())
		if _, err := db.Exec(`UPDATE auth_lifecycle_events SET processed=1,outcome='identity_conflict' WHERE id=?`, id); err != nil {
			t.Fatal(err)
		}
	}
	c.reconcile(context.Background())
	bans, err := queryLifecycleAutobans(context.Background(), db, clock.Now().Unix())
	if err != nil || len(bans) != 1 || bans[0].AuthIndex != "a" || !host.accounts["a"].FileDisabled {
		t.Fatalf("screenshot regression: %+v %v", bans, err)
	}
	if s := lifecycleStateForTest(t, c, "b"); s.Paused || s.State != authHealthy {
		t.Fatalf("healthy conflict not cleaned: %+v", s)
	}
	writes := len(host.writes)
	c.reconcile(context.Background())
	if len(host.writes) != writes {
		t.Fatal("migration repeated writes")
	}
}

func TestSimpleMigrationIgnoresExpired429AndNewerSuccess(t *testing.T) {
	c, host, clock := nativeTestController(t)
	db, _, _ := c.store.open(context.Background())
	for _, index := range []string{"a", "b"} {
		s := lifecycleStateForTest(t, c, index)
		s.PolicyVersion = 0
		s.Paused = true
		s.SyncStatus = "conflict"
		if err := saveLifecycleState(context.Background(), db, &s, clock.Now(), "old_conflict"); err != nil {
			t.Fatal(err)
		}
		status := 401
		if index == "b" {
			status = 429
		}
		id := enqueueFailureForTest(t, c, index, index+".json", status, clock.Now().Add(-time.Hour).Unix())
		if _, err := db.Exec(`UPDATE auth_lifecycle_events SET processed=1,outcome='identity_conflict' WHERE id=?`, id); err != nil {
			t.Fatal(err)
		}
		if index == "b" {
			if _, err := db.Exec(`UPDATE auth_lifecycle_events SET payload=json_set(payload,'$.recover_at',?) WHERE id=?`, clock.Now().Add(-time.Minute).Unix(), id); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := c.store.recordUsage(context.Background(), usageRecord{Provider: "codex", AuthID: "a.json", AuthIndex: "a", RequestedAt: clock.Now()}); err != nil {
		t.Fatal(err)
	}
	c.reconcile(context.Background())
	if host.accounts["a"].FileDisabled || host.accounts["b"].FileDisabled {
		t.Fatal("historical failure incorrectly reactivated")
	}
}

func TestSimpleRuntimeOldErrorDoesNotDisableRelogin(t *testing.T) {
	c, host, clock := nativeTestController(t)
	snapshot := host.accounts["a"]
	snapshot.Entry.StatusMessage = "refresh_token_invalidated"
	host.accounts["a"] = snapshot
	c.reconcile(context.Background())
	if !host.accounts["a"].FileDisabled {
		t.Fatal("runtime invalidation not isolated")
	}
	clock.now = clock.now.Add(time.Second)
	rotateLifecycleCredential(t, c, host, "a")
	c.reconcile(context.Background())
	c.reconcile(context.Background())
	if host.accounts["a"].FileDisabled {
		t.Fatal("old runtime failure blocked new login")
	}
}
