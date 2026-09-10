package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

type fakeLifecycleClock struct{ now time.Time }

func (f *fakeLifecycleClock) Now() time.Time                       { return f.now }
func (f *fakeLifecycleClock) After(time.Duration) <-chan time.Time { return make(chan time.Time) }

type fakeLifecycleHost struct {
	accounts       map[string]lifecycleSnapshot
	writes         []bool
	ready          bool
	partial        bool
	failAfterWrite bool
	mutateField    bool
}

func (f *fakeLifecycleHost) Ready() bool { return f.ready }
func (f *fakeLifecycleHost) List(context.Context) ([]hostAuthFileEntry, error) {
	var list []hostAuthFileEntry
	for _, s := range f.accounts {
		list = append(list, s.Entry)
	}
	return list, nil
}
func (f *fakeLifecycleHost) Read(_ context.Context, index string) (lifecycleSnapshot, error) {
	s, ok := f.accounts[index]
	if !ok {
		return s, errors.New("missing")
	}
	return s, nil
}
func (f *fakeLifecycleHost) SetDisabled(_ context.Context, s lifecycleSnapshot, disabled bool) error {
	f.writes = append(f.writes, disabled)
	s.Entry.Disabled = disabled
	if !f.partial {
		s.FileDisabled = disabled
	}
	s.Entry.UpdatedAt = s.Entry.UpdatedAt + ".updated"
	if f.mutateField {
		s.ContentHash = "changed"
	}
	f.accounts[s.Entry.AuthIndex] = s
	if f.failAfterWrite {
		return errors.New("timeout")
	}
	return nil
}

func nativeTestController(t *testing.T) (*authLifecycleController, *fakeLifecycleHost, *fakeLifecycleClock) {
	t.Helper()
	old := globalAccountProtection.config()
	globalAccountProtection.configure(defaultPluginConfig())
	t.Cleanup(func() { globalAccountProtection.configure(old) })
	s := newTestStore(t)
	clock := &fakeLifecycleClock{now: time.Date(2030, 1, 1, 10, 0, 0, 0, time.UTC)}
	host := &fakeLifecycleHost{ready: true, accounts: map[string]lifecycleSnapshot{}}
	for _, index := range []string{"a", "b", "c", "d"} {
		host.accounts[index] = lifecycleSnapshot{Entry: hostAuthFileEntry{AuthIndex: index, ID: index + ".json", Name: index + ".json", Path: "/auth/" + index + ".json", Provider: "codex", Email: "same@example.test"}, Fingerprint: index + "-credential", Identity: index + "-account", ContentHash: index + "-content", Data: map[string]json.RawMessage{"access_token": json.RawMessage(`"fake-token"`)}}
	}
	c := &authLifecycleController{store: s, host: host, clock: clock, wake: make(chan struct{}, 1)}
	c.reconcile(context.Background())
	return c, host, clock
}

func lifecycleStateForTest(t *testing.T, c *authLifecycleController, index string) authLifecycleState {
	t.Helper()
	db, _, err := c.store.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s, err := loadLifecycleState(context.Background(), db, index)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func lifecycleFailure(t *testing.T, c *authLifecycleController, index string, status int, body string) {
	t.Helper()
	db, _, err := c.store.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rec := usageRecord{Provider: "codex", AuthID: index + ".json", AuthIndex: index, RequestedAt: c.clock.Now(), Failed: true, Failure: usageFailure{StatusCode: status, Body: body}, Model: "test-model"}
	// Classify against injected time, just as an observed response would be.
	d := classifyAuthFailure(rec, c.clock.Now())
	raw, _ := json.Marshal(d)
	if _, err = db.Exec(`INSERT INTO auth_lifecycle_events(auth_index,auth_id,requested_at,payload) VALUES(?,?,?,?)`, index, rec.AuthID, rec.RequestedAt.Unix(), string(raw)); err != nil {
		t.Fatal(err)
	}
	c.reconcile(context.Background())
}

func TestNativeClassifier(t *testing.T) {
	now := time.Unix(2000000000, 0)
	for _, tc := range []struct {
		name            string
		status          int
		body, state     string
		disabled, model bool
		reset           int64
	}{
		{"401", 401, `{}`, authInvalid, true, false, 0},
		{"402", 402, `{}`, authBillingBlocked, true, false, 0},
		{"quota-relative", 429, `{"error":{"type":"usage_limit_reached","resets_in_seconds":3600}}`, authQuotaCooldown, true, false, now.Unix() + 3600},
		{"quota-absolute", 429, `{"error":{"type":"usage_limit_reached","resets_at":2000000100}}`, authQuotaCooldown, true, false, 2000000100},
		{"quota-unknown", 429, `{"error":{"type":"usage_limit_reached"}}`, authQuotaCooldown, true, false, 0},
		{"rate", 429, `{"error":{"type":"rate_limit_exceeded","retry_after":30}}`, authRateLimited, true, false, now.Unix() + 30},
		{"unknown429", 429, `{}`, authRateLimited, true, false, now.Unix() + 60},
		{"model403", 403, `{"error":{"code":"model_not_allowed"}}`, authHealthy, false, true, 0},
		{"unknown403", 403, `{"error":{"message":"not allowed"}}`, authHealthy, false, true, 0},
		{"workspace403", 403, `{"error":{"code":"deactivated_workspace"}}`, authPermissionBlocked, true, false, 0},
		{"server", 503, `{}`, authHealthy, false, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := classifyAuthFailure(usageRecord{Failed: true, Failure: usageFailure{StatusCode: tc.status, Body: tc.body}}, now)
			if d.State != tc.state || d.Disable != tc.disabled || d.ModelIssue != tc.model || d.RecoverAt != tc.reset {
				t.Fatalf("decision=%+v", d)
			}
		})
	}
	d := classifyAuthFailure(usageRecord{Failed: true, Failure: usageFailure{StatusCode: 429}, ResponseHeaders: map[string][]string{"x-codex-primary-used-percent": {"100"}, "x-codex-primary-reset-at": {"2000000100"}, "x-codex-secondary-used-percent": {"100"}, "x-codex-secondary-reset-at": {"2000000300"}}}, now)
	if d.RecoverAt != 2000000300 {
		t.Fatalf("both windows: %+v", d)
	}
}

func TestNativeIsolationAndRecoveryAcrossRestart(t *testing.T) {
	c, host, clock := nativeTestController(t)
	lifecycleFailure(t, c, "a", 429, `{"error":{"type":"usage_limit_reached","resets_in_seconds":3600}}`)
	lifecycleFailure(t, c, "b", 429, `{"error":{"type":"usage_limit_reached","resets_in_seconds":3600}}`)
	if !host.accounts["a"].Entry.Disabled || !host.accounts["b"].Entry.Disabled || host.accounts["c"].Entry.Disabled || host.accounts["d"].Entry.Disabled {
		t.Fatal("incorrect isolation")
	}
	resp, err := c.store.pickAuth(context.Background(), schedulerPickRequest{Provider: "codex", Candidates: []schedulerAuthCandidate{{ID: "a.json", Provider: "codex"}}})
	if err != nil || resp.Handled || resp.AuthID != "" || resp.DelegateBuiltin != "" {
		t.Fatalf("native interfered: %+v %v", resp, err)
	}
	c.store.close() // reopen durable state with a new controller
	restarted := &authLifecycleController{store: c.store, host: host, clock: clock, wake: make(chan struct{}, 1)}
	clock.now = clock.now.Add(61 * time.Minute)
	restarted.reconcile(context.Background())
	if host.accounts["a"].Entry.Disabled || host.accounts["b"].Entry.Disabled {
		t.Fatal("quota did not recover")
	}
	if s := lifecycleStateForTest(t, restarted, "a"); s.State != authHealthy || s.DisabledByPlugin {
		t.Fatalf("state=%+v", s)
	}
}

func TestNativeManualOwnershipAndReloginConflict(t *testing.T) {
	c, host, clock := nativeTestController(t)
	lifecycleFailure(t, c, "a", 401, `{}`)
	clock.now = clock.now.Add(24 * time.Hour)
	c.reconcile(context.Background())
	if !host.accounts["a"].Entry.Disabled {
		t.Fatal("401 timed recovery")
	}
	snapshot := host.accounts["a"]
	snapshot.Fingerprint = "new-login"
	snapshot.ContentHash = "new-login-content"
	host.accounts["a"] = snapshot
	c.reconcile(context.Background())
	s := lifecycleStateForTest(t, c, "a")
	if !s.Paused || s.DisabledByPlugin || !host.accounts["a"].Entry.Disabled {
		t.Fatalf("relogin took ownership: %+v", s)
	}
	snapshot = host.accounts["b"]
	snapshot.Entry.Disabled = true
	snapshot.FileDisabled = true
	host.accounts["b"] = snapshot
	c.reconcile(context.Background())
	clock.now = clock.now.Add(30 * 24 * time.Hour)
	c.reconcile(context.Background())
	if s = lifecycleStateForTest(t, c, "b"); s.State != authManualDisabled || s.DisabledByPlugin || !host.accounts["b"].Entry.Disabled {
		t.Fatalf("manual disabled recovered: %+v", s)
	}
}

func TestNativePartialWriteAndTimeout(t *testing.T) {
	for _, partial := range []bool{true, false} {
		t.Run(map[bool]string{true: "partial", false: "timeout-after-commit"}[partial], func(t *testing.T) {
			c, host, clock := nativeTestController(t)
			host.partial = partial
			host.failAfterWrite = true
			lifecycleFailure(t, c, "a", 401, `{}`)
			s := lifecycleStateForTest(t, c, "a")
			if partial {
				if s.DisabledByPlugin || s.SyncStatus != "pending" {
					t.Fatalf("partial claimed: %+v", s)
				}
				clock.now = clock.now.Add(time.Minute)
				c.reconcile(context.Background())
				s = lifecycleStateForTest(t, c, "a")
				if !s.Paused || s.DisabledByPlugin {
					t.Fatal("restart ambiguity claimed ownership")
				}
			} else if !s.DisabledByPlugin || s.SyncStatus != "synced" {
				t.Fatalf("readback failed: %+v", s)
			}
		})
	}
}

func TestNativeFieldMutationDoesNotOverwriteNewAuth(t *testing.T) {
	c, host, _ := nativeTestController(t)
	host.mutateField = true
	lifecycleFailure(t, c, "a", 401, `{}`)
	if len(host.writes) != 1 || lifecycleStateForTest(t, c, "a").DisabledByPlugin {
		t.Fatal("unsafe restoration or ownership after field mutation")
	}
}

func TestNativeDisablesRateButNotModel403Or5xx(t *testing.T) {
	c, host, _ := nativeTestController(t)
	lifecycleFailure(t, c, "a", 403, `{"error":{"code":"model_not_allowed"}}`)
	lifecycleFailure(t, c, "b", 429, `{"error":{"type":"rate_limit_exceeded","retry_after":30}}`)
	lifecycleFailure(t, c, "c", 503, `{"error":{"message":"secret-token-not-for-storage"}}`)
	if len(host.writes) != 1 || !host.accounts["b"].Entry.Disabled || host.accounts["a"].Entry.Disabled || host.accounts["c"].Entry.Disabled {
		t.Fatal("only rate-limited auth should be disabled")
	}
	db, _, _ := c.store.open(context.Background())
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM auth_model_issues`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("model issue: %d %v", count, err)
	}
	var raw string
	_ = db.QueryRow(`SELECT payload FROM auth_lifecycle_events WHERE auth_index='c'`).Scan(&raw)
	if strings.Contains(raw, "secret-token") {
		t.Fatal("raw error persisted")
	}
}

func TestNativeVersionedActionsAndLegacyRecovery(t *testing.T) {
	c, host, clock := nativeTestController(t)
	s := lifecycleStateForTest(t, c, "a")
	if _, err := c.action(context.Background(), lifecycleActionRequest{AuthIndex: "a", Version: s.Version + 1, Action: "disable"}); !errors.Is(err, errLifecycleConflict) {
		t.Fatal("missing optimistic concurrency")
	}
	if _, err := c.action(context.Background(), lifecycleActionRequest{AuthIndex: "a", Version: s.Version, Action: "disable"}); err != nil {
		t.Fatal(err)
	}
	s = lifecycleStateForTest(t, c, "a")
	if s.DisabledByPlugin || !host.accounts["a"].Entry.Disabled {
		t.Fatal("manual ownership incorrect")
	}
	if _, err := c.action(context.Background(), lifecycleActionRequest{AuthIndex: "a", Version: s.Version, Action: "clear"}); err != nil {
		t.Fatal(err)
	}
	if !host.accounts["a"].Entry.Disabled {
		t.Fatal("clear enabled account")
	}
	lifecycleFailure(t, c, "b", 429, `{"error":{"type":"usage_limit_reached","resets_in_seconds":30}}`)
	globalAccountProtection.configure(legacyPluginConfig())
	clock.now = clock.now.Add(time.Minute)
	c.reconcile(context.Background())
	if host.accounts["b"].Entry.Disabled || !host.accounts["a"].Entry.Disabled {
		t.Fatal("legacy lost existing recovery obligation")
	}
}

func TestNativeManagementClientPreservesJSONAndExactIdentity(t *testing.T) {
	physical := `{"access_token":"test-access-secret","refresh_token":"test-refresh-secret","type":"codex","disabled":false,"priority":10,"weight":3,"excluded-models":["premium"],"unknown":{"number":9007199254740993}}`
	entry := hostAuthFileEntry{AuthIndex: "index", ID: "auth.json", Name: "auth.json", Path: "/auth/auth.json", Provider: "codex"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PATCH" || r.URL.Path != "/v0/management/auth-files/status" || r.Header.Get("Authorization") != "Bearer test-management-secret" {
			t.Error("wrong API contract")
		}
		var fields map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&fields)
		if len(fields) != 3 || string(fields["name"]) != `"auth.json"` || string(fields["auth_index"]) != `"index"` {
			t.Error("unexpected overwrite or identity")
		}
		entry.Disabled = true
		physical = strings.Replace(physical, `"disabled":false`, `"disabled":true`, 1)
		w.WriteHeader(200)
	}))
	defer server.Close()
	client := &managementAuthClient{baseURL: server.URL, key: "test-management-secret", client: server.Client(), call: func(method string, payload any) (json.RawMessage, error) {
		switch method {
		case "host.auth.list":
			return json.Marshal(hostAuthListResponse{Files: []hostAuthFileEntry{entry}})
		case "host.auth.get_runtime":
			return json.Marshal(hostAuthRuntimeResponse{Auth: entry})
		case "host.auth.get":
			return json.Marshal(hostAuthGetResponse{AuthIndex: "index", Name: entry.Name, Path: entry.Path, JSON: json.RawMessage(physical)})
		default:
			t.Fatal("unexpected host write")
			return nil, nil
		}
	}}
	before, err := client.Read(context.Background(), "index")
	if err != nil {
		t.Fatal(err)
	}
	if err = client.SetDisabled(context.Background(), before, true); err != nil {
		t.Fatal(err)
	}
	after, err := client.Read(context.Background(), "index")
	if err != nil {
		t.Fatal(err)
	}
	if before.ContentHash != after.ContentHash || !after.FileDisabled || !after.Entry.Disabled {
		t.Fatal("metadata preservation failed")
	}
	if _, err = client.Read(context.Background(), "other"); err == nil {
		t.Fatal("ambiguous identity accepted")
	}
}

func TestNativeProtocolFixture(t *testing.T) {
	c, _, _ := nativeTestController(t)
	response, err := c.store.pickAuth(context.Background(), schedulerPickRequest{Provider: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := okJSON(response)
	if err != nil {
		t.Fatal(err)
	}
	var envelopeValue envelope
	if err = json.Unmarshal(raw, &envelopeValue); err != nil || !envelopeValue.OK {
		t.Fatal("bad envelope")
	}
	if string(envelopeValue.Result) != `{"AuthID":"","DelegateBuiltin":"","Handled":false}` {
		t.Fatalf("native wire response=%s", raw)
	}
	if path := os.Getenv("CPA_NATIVE_RESPONSE_FIXTURE"); path != "" {
		if err = os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
