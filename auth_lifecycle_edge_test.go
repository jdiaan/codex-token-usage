package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestNativeExternalChangeRequiresRecheckWithoutPriorFailure(t *testing.T) {
	c, host, _ := nativeTestController(t)
	snapshot := host.accounts["a"]
	snapshot.ContentHash = "externally-replaced"
	host.accounts["a"] = snapshot
	c.reconcile(context.Background())
	s := lifecycleStateForTest(t, c, "a")
	if s.SyncStatus != "conflict" || s.LastHTTPStatus != 0 {
		t.Fatalf("expected conflict without HTTP failure: %+v", s)
	}
	if _, err := c.action(context.Background(), lifecycleActionRequest{AuthIndex: "a", Version: s.Version, Action: "enable"}); err == nil {
		t.Fatal("external replacement enabled without recheck")
	}
	if len(host.writes) != 0 {
		t.Fatal("rejected action wrote auth")
	}
}

func TestLifecycleProbeModelExclusions(t *testing.T) {
	for _, key := range []string{"excluded_models", "excluded-models"} {
		data := map[string]json.RawMessage{key: json.RawMessage(`["gpt-5*","blocked-model"]`)}
		if lifecycleProbeModelAllowed(data, "gpt-5.5") || lifecycleProbeModelAllowed(data, "blocked-model") {
			t.Fatal("excluded model was allowed")
		}
		if !lifecycleProbeModelAllowed(data, "allowed-model") {
			t.Fatal("unexcluded model was rejected")
		}
	}
}

func TestNativeLateFailureAfterExternalEnable(t *testing.T) {
	c, host, clock := nativeTestController(t)
	lifecycleFailure(t, c, "a", 401, `{}`)
	started := clock.Now().Unix()
	clock.now = clock.now.Add(time.Minute)
	snapshot := host.accounts["a"]
	snapshot.Entry.Disabled = false
	snapshot.FileDisabled = false
	host.accounts["a"] = snapshot
	c.reconcile(context.Background())
	db, _, err := c.store.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(authDecision{State: authInvalid, Status: 401, Disable: true})
	if _, err = db.Exec(`INSERT INTO auth_lifecycle_events(auth_index,auth_id,requested_at,payload) VALUES(?,?,?,?)`, "a", "a.json", started, string(raw)); err != nil {
		t.Fatal(err)
	}
	c.reconcile(context.Background())
	s := lifecycleStateForTest(t, c, "a")
	if !s.Paused || s.DisabledByPlugin || host.accounts["a"].Entry.Disabled || len(host.writes) != 1 {
		t.Fatalf("late failure reclaimed external enable: %+v", s)
	}
}

func TestNativeRuntimeRefreshInvalidationIsolatesWithoutUsageEvent(t *testing.T) {
	c, host, _ := nativeTestController(t)
	snapshot := host.accounts["a"]
	snapshot.Entry.Status = "error"
	snapshot.Entry.StatusMessage = `Token refresh attempt 3 failed: {"error":{"message":"Your session has ended. Please log in again.","code":"refresh_token_invalidated"}}`
	snapshot.Entry.Unavailable = true
	host.accounts["a"] = snapshot

	c.reconcile(context.Background())
	s := lifecycleStateForTest(t, c, "a")
	if !host.accounts["a"].Entry.Disabled || !s.DisabledByPlugin || s.State != authInvalid || s.LastHTTPStatus != http.StatusUnauthorized || s.LastErrorCode != "refresh_token_invalidated" {
		t.Fatalf("runtime refresh failure was not isolated: %+v", s)
	}
	db, _, err := c.store.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var events int
	if err = db.QueryRow(`SELECT count(*) FROM auth_lifecycle_events WHERE auth_index='a'`).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 0 {
		t.Fatalf("runtime-only failure unexpectedly required a Usage event: %d", events)
	}
	var raw string
	if err = db.QueryRow(`SELECT payload FROM auth_lifecycle_states WHERE auth_index='a'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "session has ended") || strings.Contains(raw, "Token refresh attempt") {
		t.Fatal("raw runtime error was persisted")
	}
}

func TestNativeRuntimeRefreshClassifierIgnoresTransientErrors(t *testing.T) {
	for _, entry := range []hostAuthFileEntry{
		{Status: "error", StatusMessage: "token refresh temporarily unavailable", Unavailable: true},
		{Status: "active", StatusMessage: "", Unavailable: false},
	} {
		if decision, terminal := classifyRuntimeAuthFailure(entry); terminal || decision.Disable {
			t.Fatalf("transient runtime error classified as terminal: %+v", decision)
		}
	}
}

func TestNativeExternalEnableClearsManualDisabledState(t *testing.T) {
	c, host, _ := nativeTestController(t)
	s := lifecycleStateForTest(t, c, "a")
	if _, err := c.action(context.Background(), lifecycleActionRequest{AuthIndex: "a", Version: s.Version, Action: "disable"}); err != nil {
		t.Fatal(err)
	}
	snapshot := host.accounts["a"]
	snapshot.Entry.Disabled = false
	snapshot.FileDisabled = false
	host.accounts["a"] = snapshot

	c.reconcile(context.Background())
	s = lifecycleStateForTest(t, c, "a")
	if s.State != authHealthy || s.Disabled || !s.Paused || s.SyncStatus != "conflict" || s.Reason != "external_enable" {
		t.Fatalf("external enable left a stale manual-disabled state: %+v", s)
	}
}

func TestNativeRepairsPersistedManualDisabledAfterExternalEnable(t *testing.T) {
	c, _, _ := nativeTestController(t)
	db, _, err := c.store.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s := lifecycleStateForTest(t, c, "a")
	s.State = authManualDisabled
	s.Disabled = false
	s.Paused = true
	s.Reason = "manual_disabled"
	s.SyncStatus = "conflict"
	if err = saveLifecycleState(context.Background(), db, &s, c.clock.Now(), "legacy_stale_state"); err != nil {
		t.Fatal(err)
	}

	c.reconcile(context.Background())
	s = lifecycleStateForTest(t, c, "a")
	if s.State != authHealthy || s.Reason != "external_enable" || !s.Paused {
		t.Fatalf("persisted stale manual-disabled state was not repaired: %+v", s)
	}
}

func TestLifecycleEventRetryColumnMigration(t *testing.T) {
	c, _, _ := nativeTestController(t)
	ctx := context.Background()
	db, _, err := c.store.open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`ALTER TABLE auth_lifecycle_events DROP COLUMN retry_at`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err = migrateAuthLifecycle(ctx, db); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.Exec(`SELECT retry_at FROM auth_lifecycle_events`); err != nil {
		t.Fatal(err)
	}
	states, err := listLifecycleStates(ctx, db)
	if err != nil || len(states) != 4 {
		t.Fatalf("migration lost state: %d %v", len(states), err)
	}
}

func TestNativeMissingConfigurationAndLegacyPendingCancellation(t *testing.T) {
	c, host, clock := nativeTestController(t)
	host.ready = false
	lifecycleFailure(t, c, "a", 401, `{}`)
	s := lifecycleStateForTest(t, c, "a")
	if s.PendingAction != "disable" || s.DisabledByPlugin || len(host.writes) != 0 {
		t.Fatalf("state=%+v", s)
	}
	globalAccountProtection.configure(legacyPluginConfig())
	host.ready = true
	clock.now = clock.now.Add(time.Minute)
	c.reconcile(context.Background())
	if len(host.writes) != 0 {
		t.Fatal("legacy executed pending native disable")
	}
	if s = lifecycleStateForTest(t, c, "a"); s.PendingAction != "" || !s.Paused {
		t.Fatalf("pending action not canceled: %+v", s)
	}
}

func TestNativeRecheckNeverEnablesAndRejectsChangedProbe(t *testing.T) {
	c, host, _ := nativeTestController(t)
	lifecycleFailure(t, c, "a", 401, `{}`)
	oldURL := codexQuotaURLOverrideForTest
	t.Cleanup(func() { codexQuotaURLOverrideForTest = oldURL })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":10,"reset_at":2000000000},"secondary_window":null}}`))
	}))
	defer server.Close()
	codexQuotaURLOverrideForTest = server.URL
	s := lifecycleStateForTest(t, c, "a")
	checked, err := c.action(context.Background(), lifecycleActionRequest{AuthIndex: "a", Version: s.Version, Action: "recheck"})
	if err != nil {
		t.Fatal(err)
	}
	if !checked.CheckOK || !host.accounts["a"].Entry.Disabled || checked.DisabledByPlugin {
		t.Fatal("recheck implicitly enabled or claimed ownership")
	}
	if _, err = c.action(context.Background(), lifecycleActionRequest{AuthIndex: "a", Version: checked.Version, Action: "enable"}); err != nil {
		t.Fatal(err)
	}
	if host.accounts["a"].Entry.Disabled {
		t.Fatal("explicit enable failed")
	}
}

func TestNativeBillingProbeRequiresExplicitAcknowledgement(t *testing.T) {
	c, host, clock := nativeTestController(t)
	lifecycleFailure(t, c, "a", 402, `{}`)
	clock.now = clock.now.Add(24 * time.Hour)
	c.reconcile(context.Background())
	s := lifecycleStateForTest(t, c, "a")
	if _, err := c.action(context.Background(), lifecycleActionRequest{AuthIndex: "a", Version: s.Version, Action: "recheck"}); err == nil {
		t.Fatal("billing probe lacked acknowledgement")
	}
	if !host.accounts["a"].Entry.Disabled {
		t.Fatal("billing timed recovery")
	}
}

func TestNativeUnknownQuotaResetRequiresAuthoritativeRead(t *testing.T) {
	c, host, clock := nativeTestController(t)
	lifecycleFailure(t, c, "a", 429, `{"error":{"type":"usage_limit_reached"}}`)
	if lifecycleStateForTest(t, c, "a").RecoverAt != 0 {
		t.Fatal("invented reset")
	}
	oldURL := codexQuotaURLOverrideForTest
	t.Cleanup(func() { codexQuotaURLOverrideForTest = oldURL })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":100,"reset_at":1893502800},"secondary_window":null}}`))
	}))
	defer server.Close()
	codexQuotaURLOverrideForTest = server.URL
	c.reconcile(context.Background())
	s := lifecycleStateForTest(t, c, "a")
	if s.RecoverAt != 0 {
		t.Fatal("background reconciliation queried quota")
	}
	s, err := c.action(context.Background(), lifecycleActionRequest{AuthIndex: "a", Version: s.Version, Action: "check_and_recover"})
	if err != nil {
		t.Fatal(err)
	}
	if s.RecoverAt != 1893502800 || !host.accounts["a"].Entry.Disabled {
		t.Fatalf("quota reset read=%+v", s)
	}
	clock.now = time.Unix(s.RecoverAt+1, 0)
	c.reconcile(context.Background())
	if host.accounts["a"].Entry.Disabled {
		t.Fatal("discovered reset did not recover")
	}
}

func TestNativeOldDatabaseAndMaintenanceIsolation(t *testing.T) {
	c, _, clock := nativeTestController(t)
	db, _, _ := c.store.open(context.Background())
	_, err := db.Exec(`INSERT INTO autoban_bans(auth_id,auth_index,provider,window,reason,banned_at,reset_at,active,last_status_code) VALUES('legacy.json','legacy','codex','5h','historical',?,?,1,429)`, clock.now.Unix(), clock.now.Add(time.Hour).Unix())
	if err != nil {
		t.Fatal(err)
	}
	if err = migrateAuthLifecycle(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err = migrateAuthLifecycle(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	var evidence int
	_ = db.QueryRow(`SELECT count(*) FROM auth_lifecycle_legacy_evidence WHERE auth_id='legacy.json'`).Scan(&evidence)
	if evidence != 1 {
		t.Fatal("migration not idempotent")
	}
	var ownership int
	_ = db.QueryRow(`SELECT count(*) FROM auth_lifecycle_states WHERE auth_index='legacy'`).Scan(&ownership)
	if ownership != 0 {
		t.Fatal("historical ban became ownership")
	}
	if err = expireAutobans(context.Background(), db, clock.now.Add(24*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	var active int
	_ = db.QueryRow(`SELECT active FROM autoban_bans WHERE auth_id='legacy.json'`).Scan(&active)
	if active != 1 {
		t.Fatal("native mutated legacy state")
	}
	before, _ := c.store.currentRevision(context.Background())
	s := lifecycleStateForTest(t, c, "a")
	s.Reason = "test"
	if err = saveLifecycleState(context.Background(), db, &s, clock.Now(), "test"); err != nil {
		t.Fatal(err)
	}
	after, _ := c.store.currentRevision(context.Background())
	if before.Revision == after.Revision {
		t.Fatal("lifecycle failed to invalidate summary revision")
	}
}

func TestNativeDefaultAndInvalidMode(t *testing.T) {
	if defaultPluginConfig().SchedulingMode != "native" {
		t.Fatal("native is not default")
	}
	if mode := parsePluginConfigYAML([]byte("scheduling_mode: legacy"), defaultPluginConfig()).SchedulingMode; mode != "legacy" {
		t.Fatal(mode)
	}
	raw, _ := json.Marshal(lifecycleRequest{ConfigYAML: json.RawMessage(`"scheduling_mode: invalid"`)})
	if err := configurePlugin(raw); err == nil || !strings.Contains(err.Error(), "scheduling_mode") {
		t.Fatal("invalid mode accepted")
	}
}

func TestManagementConfigFieldsAndEnvironmentPrecedence(t *testing.T) {
	fields := pluginConfigFields()
	seen := map[string]bool{}
	for _, field := range fields {
		seen[field.Name] = true
	}
	if !seen["management_url"] || !seen["management_key"] {
		t.Fatalf("management fields missing from plugin registration: %+v", seen)
	}

	cfg := parsePluginConfigYAML([]byte("management_url: http://127.0.0.1:8317\nmanagement_key: plugin-secret\nFree 5 分钟 Token 上限: 12345\n"), defaultPluginConfig())
	if cfg.ManagementURL != "http://127.0.0.1:8317" || cfg.ManagementKey != "plugin-secret" {
		t.Fatalf("management plugin config was not parsed: url=%q key=%q", cfg.ManagementURL, cfg.ManagementKey)
	}
	if cfg.AccountProtectionFreeTokenLimit != 12345 {
		t.Fatalf("config key containing spaces was ignored: %d", cfg.AccountProtectionFreeTokenLimit)
	}

	t.Setenv("CPA_TOKEN_USAGE_MANAGEMENT_URL", "")
	t.Setenv("CPA_TOKEN_USAGE_MANAGEMENT_KEY", "")
	client := newManagementAuthClient(cfg)
	if !client.Ready() || client.baseURL != cfg.ManagementURL || client.key != cfg.ManagementKey {
		t.Fatalf("plugin config did not configure management client: ready=%v url=%q", client.Ready(), client.baseURL)
	}
	t.Setenv("CPA_TOKEN_USAGE_MANAGEMENT_URL", "http://127.0.0.1:9999/")
	t.Setenv("CPA_TOKEN_USAGE_MANAGEMENT_KEY", "environment-secret")
	client = newManagementAuthClient(cfg)
	if client.baseURL != "http://127.0.0.1:9999" || client.key != "environment-secret" {
		t.Fatalf("environment did not override plugin config: url=%q", client.baseURL)
	}
	status := managementAuthConfigStatus(cfg)
	if status["url_source"] != "environment" || status["key_source"] != "environment" {
		t.Fatalf("management source status = %+v", status)
	}
	encoded, _ := json.Marshal(status)
	if strings.Contains(string(encoded), "secret") {
		t.Fatalf("management status leaked a key: %s", encoded)
	}
}

func TestCodexLifecycleAcceptsExecutorWhenProviderIsEmpty(t *testing.T) {
	if !isCodexUsage(usageRecord{ExecutorType: "codex", AuthType: "oauth"}) {
		t.Fatal("Codex executor usage was ignored when Provider was empty")
	}
	if !isCodexUsage(usageRecord{ExecutorType: "openai-codex-executor", AuthType: "oauth"}) {
		t.Fatal("Codex executor variant was ignored")
	}
	if isCodexUsage(usageRecord{ExecutorType: "openai", AuthType: "oauth"}) {
		t.Fatal("non-Codex executor was accepted")
	}
}

func TestNativeDashboardJavaScript(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node unavailable; CI syntax/render check requires Node")
	}
	// Parse the entire shipped script, then execute the new status rendering with
	// hostile labels. This catches broken JS and attribute-injection regressions.
	source := `new Function(` + jsQuoted(dashboardScripts) + `);`
	start := strings.Index(dashboardScripts, "function lifecycleStatus(s){")
	end := strings.Index(dashboardScripts[start:], "\ndocument.addEventListener")
	escStart := strings.Index(dashboardScripts, "function esc(v){")
	escEnd := strings.Index(dashboardScripts[escStart:], "\n")
	source += dashboardScripts[escStart:escStart+escEnd] + "\n" + dashboardScripts[start:start+end] + "\n"
	source += `const html=lifecycleStatus({auth_index:'\" onclick=\"alert(1)',version:7,state:'QUOTA_COOLDOWN',disabled:true,disabled_by_plugin:true,recover_at:2000000000});if(html.includes('data-auth-index="" onclick='))throw Error('unsafe attribute');if(!html.includes('data-version="7"')||!html.includes('Recheck')||!html.includes('Recover at'))throw Error('missing lifecycle controls');`
	command := exec.Command(node, "-")
	command.Stdin = strings.NewReader(source)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("dashboard JS: %s %v", output, err)
	}
}

func jsQuoted(v string) string { raw, _ := json.Marshal(v); return string(raw) }
