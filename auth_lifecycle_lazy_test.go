package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const availableQuota = `{"rate_limit":{"primary_window":{"used_percent":11},"secondary_window":null}}`

func quotaServerForTest(t *testing.T, handler http.HandlerFunc) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); handler(w, r) }))
	old := codexQuotaURLOverrideForTest
	codexQuotaURLOverrideForTest = server.URL
	t.Cleanup(func() { codexQuotaURLOverrideForTest = old; server.Close() })
	return &calls
}

func enqueueFailureForTest(t *testing.T, c *authLifecycleController, index, id string, status int, at int64) int64 {
	t.Helper()
	db, _, err := c.store.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	d := classifyAuthFailure(usageRecord{Failed: true, Failure: usageFailure{StatusCode: status}}, c.clock.Now())
	raw, _ := json.Marshal(d)
	result, err := db.Exec(`INSERT INTO auth_lifecycle_events(auth_index,auth_id,requested_at,payload) VALUES(?,?,?,?)`, index, id, at, string(raw))
	if err != nil {
		t.Fatal(err)
	}
	eventID, _ := result.LastInsertId()
	return eventID
}

func TestLazyQuotaNoBackgroundRequestsAndExplicitRecovery(t *testing.T) {
	c, host, clock := nativeTestController(t)
	calls := quotaServerForTest(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(availableQuota)) })
	lifecycleFailure(t, c, "a", 429, `{"error":{"type":"usage_limit_reached"}}`)
	for i := 0; i < 5; i++ {
		clock.now = clock.now.Add(10 * time.Minute)
		c.reconcile(context.Background())
	}
	if calls.Load() != 0 || !host.accounts["a"].FileDisabled {
		t.Fatal("background quota request or premature enable")
	}
	s := lifecycleStateForTest(t, c, "a")
	s, err := c.action(context.Background(), lifecycleActionRequest{AuthIndex: "a", Version: s.Version, Action: "check_and_recover"})
	if err != nil || s.Disabled || s.State != authHealthy || host.accounts["a"].FileDisabled || calls.Load() != 1 {
		t.Fatalf("explicit recovery=%+v err=%v calls=%d", s, err, calls.Load())
	}
	if _, err = c.action(context.Background(), lifecycleActionRequest{AuthIndex: "a", Version: s.Version - 1, Action: "check_and_recover"}); err == nil || calls.Load() != 1 {
		t.Fatal("stale action queried quota")
	}
}

func TestLazyQuotaUnavailableKeepsDisabled(t *testing.T) {
	for _, tc := range []struct {
		name, body, result string
		status             int
	}{
		{"exhausted", `{"rate_limit":{"primary_window":{"used_percent":100},"secondary_window":null}}`, "quota_exhausted", 200},
		{"secondary_exhausted", `{"rate_limit":{"primary_window":{"used_percent":11},"secondary_window":{"used_percent":100}}}`, "quota_exhausted", 200},
		{"missing_secondary", `{"rate_limit":{"primary_window":{"used_percent":11}}}`, "not_available", 200},
		{"empty", `{}`, "not_available", 200},
		{"absent", `{"rate_limit":{"primary_window":null,"secondary_window":null}}`, "not_available", 200},
		{"invalid_percent", `{"rate_limit":{"primary_window":{"used_percent":-1},"secondary_window":null}}`, "not_available", 200},
		{"contradictory", `{"rate_limit":{"primary_window":{"used_percent":11},"secondary_window":null},"primary_window":{"used_percent":100}}`, "not_available", 200},
		{"unavailable", `{}`, "request_failed", 503},
		{"auth_invalid", `{"error":{"code":"invalid_token"}}`, "request_failed", 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, host, clock := nativeTestController(t)
			calls := quotaServerForTest(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			lifecycleFailure(t, c, "a", 429, `{"error":{"type":"usage_limit_reached","resets_in_seconds":3600}}`)
			s := lifecycleStateForTest(t, c, "a")
			s, err := c.action(context.Background(), lifecycleActionRequest{AuthIndex: "a", Version: s.Version, Action: "check_and_recover"})
			if err != nil || !s.Disabled || s.CheckOK || s.CheckResult != tc.result || calls.Load() != 1 || s.RecoverAt != 0 {
				t.Fatalf("result=%+v err=%v calls=%d", s, err, calls.Load())
			}
			clock.now = clock.now.Add(2 * time.Hour)
			c.reconcile(context.Background())
			if !host.accounts["a"].FileDisabled || calls.Load() != 1 {
				t.Fatal("failed check recovered in background")
			}
			if tc.status == 401 && s.State != authInvalid {
				t.Fatal("quota check 401 did not supersede cooldown")
			}
		})
	}
}

func TestLazyRecheckDoesNotAcceptHTTP200Exhaustion(t *testing.T) {
	c, host, _ := nativeTestController(t)
	calls := quotaServerForTest(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":100},"secondary_window":null}}`))
	})
	lifecycleFailure(t, c, "a", 429, `{"error":{"type":"usage_limit_reached"}}`)
	s := lifecycleStateForTest(t, c, "a")
	s, err := c.action(context.Background(), lifecycleActionRequest{AuthIndex: "a", Version: s.Version, Action: "recheck"})
	if err != nil || s.CheckOK || !s.Disabled || calls.Load() != 1 {
		t.Fatalf("recheck=%+v %v", s, err)
	}
	if _, err = c.action(context.Background(), lifecycleActionRequest{AuthIndex: "a", Version: s.Version, Action: "enable"}); err == nil || !host.accounts["a"].FileDisabled {
		t.Fatal("exhausted quota accepted for enable")
	}
}

func TestLazyRecoveryProtectsManualAndChangedAuth(t *testing.T) {
	for _, kind := range []string{"manual", "401", "replacement", "pending", "partial"} {
		t.Run(kind, func(t *testing.T) {
			c, host, _ := nativeTestController(t)
			calls := quotaServerForTest(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(availableQuota)) })
			status, body := 429, `{"error":{"type":"usage_limit_reached"}}`
			if kind == "401" {
				status, body = 401, `{}`
			}
			lifecycleFailure(t, c, "a", status, body)
			s := lifecycleStateForTest(t, c, "a")
			if kind == "manual" {
				var err error
				s, err = c.action(context.Background(), lifecycleActionRequest{AuthIndex: "a", Version: s.Version, Action: "disable"})
				if err != nil {
					t.Fatal(err)
				}
			}
			if kind == "replacement" {
				snap := host.accounts["a"]
				snap.ContentHash = "changed"
				host.accounts["a"] = snap
			}
			if kind == "pending" {
				enqueueFailureForTest(t, c, "a", "a.json", 401, c.clock.Now().Unix())
			}
			if kind == "partial" {
				host.partial = true
			}
			_, err := c.action(context.Background(), lifecycleActionRequest{AuthIndex: "a", Version: s.Version, Action: "check_and_recover"})
			wantCalls := int32(0)
			if kind == "partial" {
				wantCalls = 1
			}
			if err == nil || calls.Load() != wantCalls || !host.accounts["a"].FileDisabled {
				t.Fatalf("unsafe recovery kind=%s calls=%d err=%v", kind, calls.Load(), err)
			}
		})
	}
}

func TestLazyPending401Supersedes429AndSurvivesRestart(t *testing.T) {
	c, host, clock := nativeTestController(t)
	host.ready = false
	lifecycleFailure(t, c, "a", 429, `{}`)
	lifecycleFailure(t, c, "a", 401, `{}`)
	s := lifecycleStateForTest(t, c, "a")
	if s.State != authInvalid || s.RecoverAt != 0 || s.PendingAction != "disable" {
		t.Fatalf("lost pending 401: %+v", s)
	}
	host.ready = true
	clock.now = clock.now.Add(2 * time.Minute)
	c = &authLifecycleController{store: c.store, host: host, clock: clock, wake: make(chan struct{}, 1)}
	c.reconcile(context.Background())
	s = lifecycleStateForTest(t, c, "a")
	if !host.accounts["a"].FileDisabled || s.State != authInvalid || s.RecoverAt != 0 {
		t.Fatalf("restart=%+v", s)
	}
	db, _, _ := c.store.open(context.Background())
	var outcome string
	if err := db.QueryRow(`SELECT outcome FROM auth_lifecycle_events ORDER BY id DESC LIMIT 1`).Scan(&outcome); err != nil || outcome != "disabled" {
		t.Fatalf("outcome=%s err=%v", outcome, err)
	}
}

func TestLazyQueued401PreventsDueRecoveryAcrossBatches(t *testing.T) {
	c, host, clock := nativeTestController(t)
	lifecycleFailure(t, c, "a", 429, `{}`)
	for i := 0; i < 100; i++ {
		enqueueFailureForTest(t, c, "b", "b.json", 599, clock.Now().Unix())
	}
	enqueueFailureForTest(t, c, "a", "a.json", 401, clock.Now().Unix())
	clock.now = clock.now.Add(2 * time.Minute)
	c.reconcile(context.Background())
	if !host.accounts["a"].FileDisabled {
		t.Fatal("timer ran ahead of queued failure")
	}
	c.reconcile(context.Background())
	if s := lifecycleStateForTest(t, c, "a"); s.State != authInvalid || !s.Disabled || s.RecoverAt != 0 {
		t.Fatalf("queued 401=%+v", s)
	}
}

func TestLazyMissingIdentityDeferredAndVisible(t *testing.T) {
	c, host, clock := nativeTestController(t)
	snapshot := host.accounts["a"]
	delete(host.accounts, "a")
	event := enqueueFailureForTest(t, c, "a", "a.json", 401, clock.Now().Unix())
	c.reconcile(context.Background())
	db, _, _ := c.store.open(context.Background())
	var processed int
	var outcome string
	if err := db.QueryRow(`SELECT processed,outcome FROM auth_lifecycle_events WHERE id=?`, event).Scan(&processed, &outcome); err != nil || processed != 0 || outcome != "pending_identity" {
		t.Fatalf("dropped event=%d %s %v", processed, outcome, err)
	}
	diag, err := queryLifecycleEventDiagnostics(context.Background(), db)
	if err != nil || len(diag["pending"].([]lifecycleEventDiagnostic)) != 1 {
		t.Fatalf("missing diagnostics=%+v %v", diag, err)
	}
	host.accounts["a"] = snapshot
	clock.now = clock.now.Add(time.Minute)
	c.reconcile(context.Background())
	if !host.accounts["a"].FileDisabled {
		t.Fatal("deferred failure never isolated")
	}
	if err = db.QueryRow(`SELECT processed,outcome FROM auth_lifecycle_events WHERE id=?`, event).Scan(&processed, &outcome); err != nil || processed != 1 || outcome != "disabled" {
		t.Fatalf("unconfirmed event=%d %s %v", processed, outcome, err)
	}
}

func TestLazyUsageAndFailureAtomicity(t *testing.T) {
	for _, kind := range []string{"event_failure", "auxiliary_failure"} {
		t.Run(kind, func(t *testing.T) {
			c, _, _ := nativeTestController(t)
			db, _, _ := c.store.open(context.Background())
			ddl := `CREATE TRIGGER fail_lifecycle BEFORE INSERT ON auth_lifecycle_events BEGIN SELECT RAISE(ABORT,'test event failure'); END`
			if kind == "auxiliary_failure" {
				ddl = `DROP TABLE account_protection_reservations`
			}
			if _, err := db.Exec(ddl); err != nil {
				t.Fatal(err)
			}
			err := c.store.recordUsage(context.Background(), usageRecord{Provider: "codex", AuthID: "a.json", AuthIndex: "a", Failed: true, Failure: usageFailure{StatusCode: 401}})
			if err == nil {
				t.Fatal("expected injected failure")
			}
			var usage, events int
			if err = db.QueryRow(`SELECT COUNT(*) FROM usage_events`).Scan(&usage); err != nil {
				t.Fatal(err)
			}
			if err = db.QueryRow(`SELECT COUNT(*) FROM auth_lifecycle_events`).Scan(&events); err != nil {
				t.Fatal(err)
			}
			want := 0
			if kind == "auxiliary_failure" {
				want = 1
			}
			if usage != want || events != want {
				t.Fatalf("non-atomic persistence usage=%d events=%d want=%d", usage, events, want)
			}
		})
	}
}

func TestLazyEventMigrationAndStaleDiagnostics(t *testing.T) {
	c, host, clock := nativeTestController(t)
	db, _, _ := c.store.open(context.Background())
	for _, index := range []string{"idx_lifecycle_event_attention", "idx_lifecycle_event_outcome_auth"} {
		if _, err := db.Exec(`DROP INDEX ` + index); err != nil {
			t.Fatal(err)
		}
	}
	for _, column := range []string{"outcome", "detail"} {
		if _, err := db.Exec(`ALTER TABLE auth_lifecycle_events DROP COLUMN ` + column); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := migrateAuthLifecycle(context.Background(), db); err != nil {
			t.Fatal(err)
		}
	}
	s := lifecycleStateForTest(t, c, "a")
	s.IgnoreBefore = clock.Now().Unix()
	if err := saveLifecycleState(context.Background(), db, &s, clock.Now(), "test_boundary"); err != nil {
		t.Fatal(err)
	}
	enqueueFailureForTest(t, c, "a", "a.json", 401, clock.Now().Add(-time.Hour).Unix())
	c.reconcile(context.Background())
	diag, err := queryLifecycleEventDiagnostics(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(diag)
	if !strings.Contains(string(raw), "ignored_stale") || host.accounts["a"].FileDisabled {
		t.Fatalf("stale event=%s", raw)
	}
}

type unreadableLifecycleHost struct{ *fakeLifecycleHost }

func (h unreadableLifecycleHost) Read(context.Context, string) (lifecycleSnapshot, error) {
	return lifecycleSnapshot{}, errors.New("temporary read failure")
}

func TestLazyUnreadableSnapshotRetryAndDuplicateEvent(t *testing.T) {
	c, host, clock := nativeTestController(t)
	c.host = unreadableLifecycleHost{host}
	event := enqueueFailureForTest(t, c, "a", "a.json", 401, clock.Now().Unix())
	c.reconcile(context.Background())
	db, _, _ := c.store.open(context.Background())
	var outcome string
	if err := db.QueryRow(`SELECT outcome FROM auth_lifecycle_events WHERE id=?`, event).Scan(&outcome); err != nil || outcome != "pending_snapshot" {
		t.Fatalf("outcome=%s %v", outcome, err)
	}
	c.host = host
	clock.now = clock.now.Add(time.Minute)
	c.reconcile(context.Background())
	if !host.accounts["a"].FileDisabled || len(host.writes) != 1 {
		t.Fatal("snapshot retry failed")
	}
	// Simulate restart after state commit but before the event acknowledgement.
	if _, err := db.Exec(`UPDATE auth_lifecycle_events SET processed=0 WHERE id=?`, event); err != nil {
		t.Fatal(err)
	}
	c.reconcile(context.Background())
	if len(host.writes) != 1 {
		t.Fatal("replayed event caused duplicate status write")
	}
}

func TestLazyNewFailureDuringQuotaCheckPreventsEnable(t *testing.T) {
	c, host, clock := nativeTestController(t)
	started, release := make(chan struct{}), make(chan struct{})
	calls := quotaServerForTest(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		_, _ = w.Write([]byte(availableQuota))
	})
	lifecycleFailure(t, c, "a", 429, `{"error":{"type":"usage_limit_reached"}}`)
	s := lifecycleStateForTest(t, c, "a")
	type result struct {
		s   authLifecycleState
		err error
	}
	done := make(chan result, 1)
	go func() {
		state, err := c.action(context.Background(), lifecycleActionRequest{AuthIndex: "a", Version: s.Version, Action: "check_and_recover"})
		done <- result{state, err}
	}()
	<-started
	enqueueFailureForTest(t, c, "a", "a.json", 401, clock.Now().Unix())
	close(release)
	r := <-done
	if r.err != nil || r.s.CheckOK || r.s.CheckResult != "pending_failure" || !host.accounts["a"].FileDisabled || calls.Load() != 1 {
		t.Fatalf("concurrent failure result=%+v err=%v", r.s, r.err)
	}
	c.reconcile(context.Background())
	if state := lifecycleStateForTest(t, c, "a"); state.State != authInvalid || state.RecoverAt != 0 {
		t.Fatalf("new failure lost: %+v", state)
	}
}

func TestLazyCanceledCheckCannotFallBackToOldTimer(t *testing.T) {
	c, host, clock := nativeTestController(t)
	started := make(chan struct{})
	calls := quotaServerForTest(t, func(w http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done() })
	lifecycleFailure(t, c, "a", 429, `{"error":{"type":"usage_limit_reached","resets_in_seconds":60}}`)
	s := lifecycleStateForTest(t, c, "a")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := c.action(ctx, lifecycleActionRequest{AuthIndex: "a", Version: s.Version, Action: "check_and_recover"})
		done <- err
	}()
	<-started
	cancel()
	<-done
	clock.now = clock.now.Add(time.Hour)
	c.reconcile(context.Background())
	s = lifecycleStateForTest(t, c, "a")
	if !s.Disabled || !host.accounts["a"].FileDisabled || s.RecoverAt != 0 || calls.Load() != 1 {
		t.Fatalf("canceled check recovered: %+v", s)
	}
}

func TestLazySuccessfulUsageDoesNotClearIsolation(t *testing.T) {
	c, host, _ := nativeTestController(t)
	lifecycleFailure(t, c, "a", 429, `{"error":{"type":"usage_limit_reached"}}`)
	err := c.store.recordUsage(context.Background(), usageRecord{Provider: "codex", AuthID: "a.json", AuthIndex: "a", ResponseHeaders: map[string][]string{"x-codex-primary-used-percent": {"11"}}})
	if err != nil {
		t.Fatal(err)
	}
	c.reconcile(context.Background())
	if !host.accounts["a"].FileDisabled {
		t.Fatal("successful in-flight usage cleared isolation")
	}
}
