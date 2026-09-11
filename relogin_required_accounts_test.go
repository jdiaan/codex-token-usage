package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestReloginRequiredAccountsManagementRoute(t *testing.T) {
	oldStore := globalStore
	oldConfig := globalAccountProtection.config()
	store := newTestStore(t)
	globalStore = store
	globalAccountProtection.configure(defaultPluginConfig())
	t.Cleanup(func() {
		store.close()
		globalStore = oldStore
		globalAccountProtection.configure(oldConfig)
	})

	db, _, err := store.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(2000000000, 0)
	states := []authLifecycleState{
		{AuthIndex: "z-invalid", AuthID: "z.json", Name: "z.json", State: authInvalid, Disabled: true, DisabledByPlugin: true, SyncStatus: "synced", Reason: "refresh_token_invalidated", DisabledAt: now.Unix(), Fingerprint: "credential-fingerprint", ContentHash: "content-hash", LastErrorMessage: "raw-secret-error"},
		{AuthIndex: "a-billing", AuthID: "a.json", Name: "a.json", State: authBillingBlocked, Disabled: true, DisabledByPlugin: true, SyncStatus: "synced", Reason: "billing_blocked", DisabledAt: now.Add(time.Second).Unix()},
		{AuthIndex: "m-permission", AuthID: "m.json", Name: "m.json", State: authPermissionBlocked, Disabled: true, DisabledByPlugin: true, SyncStatus: "synced", Reason: "workspace_deactivated", DisabledAt: now.Add(2 * time.Second).Unix()},
		{AuthIndex: "quota", State: authQuotaCooldown, Disabled: true, DisabledByPlugin: true, SyncStatus: "synced", RecoverAt: now.Add(time.Hour).Unix()},
		{AuthIndex: "healthy", State: authHealthy, SyncStatus: "synced"},
		{AuthIndex: "manual", State: authInvalid, Disabled: true, DisabledByPlugin: true, ManualDisabled: true, SyncStatus: "synced"},
		{AuthIndex: "pending", State: authInvalid, Disabled: true, DisabledByPlugin: true, SyncStatus: "synced", PendingAction: "disable"},
		{AuthIndex: "sync-error", State: authInvalid, Disabled: true, DisabledByPlugin: true, SyncStatus: "failed", SyncError: "write failed"},
		{AuthIndex: "unconfirmed", State: authInvalid, Disabled: false, DisabledByPlugin: true, SyncStatus: "synced"},
	}
	for i := range states {
		if err := saveLifecycleState(context.Background(), db, &states[i], now, "test"); err != nil {
			t.Fatal(err)
		}
	}

	response := handleManagement(managementRequest{Method: http.MethodGet, Path: "/v0/management/plugins/" + pluginID + "/relogin-required-accounts"})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.StatusCode, response.Body)
	}
	var result reloginRequiredAccountsResponse
	if err := json.Unmarshal(response.Body, &result); err != nil {
		t.Fatal(err)
	}
	if result.GeneratedAt == "" || result.Count != 3 || len(result.Accounts) != 3 {
		t.Fatalf("result=%+v", result)
	}
	want := []struct {
		index  string
		status int
		state  string
	}{
		{"a-billing", http.StatusPaymentRequired, authBillingBlocked},
		{"m-permission", http.StatusForbidden, authPermissionBlocked},
		{"z-invalid", http.StatusUnauthorized, authInvalid},
	}
	for i, expected := range want {
		got := result.Accounts[i]
		if got.AuthIndex != expected.index || got.HTTPStatus != expected.status || got.State != expected.state || got.Version < 1 {
			t.Fatalf("account[%d]=%+v, want index=%q status=%d state=%q", i, got, expected.index, expected.status, expected.state)
		}
	}
	if result.Accounts[2].AuthID != "z.json" || result.Accounts[2].Name != "z.json" || result.Accounts[2].Reason != "refresh_token_invalidated" || result.Accounts[2].DisabledAt != now.Unix() {
		t.Fatalf("invalid account=%+v", result.Accounts[2])
	}
	for _, forbidden := range []string{"credential-fingerprint", "content-hash", "raw-secret-error", "last_error_message", "fingerprint", "content_hash"} {
		if strings.Contains(string(response.Body), forbidden) {
			t.Fatalf("response leaked lifecycle internals %q: %s", forbidden, response.Body)
		}
	}
}

func TestReloginRequiredAccountsRouteErrorsAndRegistration(t *testing.T) {
	oldStore := globalStore
	oldConfig := globalAccountProtection.config()
	t.Cleanup(func() {
		globalStore = oldStore
		globalAccountProtection.configure(oldConfig)
	})

	globalAccountProtection.configure(defaultPluginConfig())
	path := "/v0/management/plugins/" + pluginID + "/relogin-required-accounts"
	if response := handleManagement(managementRequest{Method: http.MethodPost, Path: path}); response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("method status=%d body=%s", response.StatusCode, response.Body)
	}

	legacy := defaultPluginConfig()
	legacy.SchedulingMode = "legacy"
	globalAccountProtection.configure(legacy)
	if response := handleManagement(managementRequest{Method: http.MethodGet, Path: path}); response.StatusCode != http.StatusConflict || !strings.Contains(string(response.Body), "unsupported_scheduling_mode") {
		t.Fatalf("legacy response=%d %s", response.StatusCode, response.Body)
	}

	globalAccountProtection.configure(defaultPluginConfig())
	db, err := openSQLiteDB(t.TempDir() + "/closed.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	globalStore = &store{db: db}
	if response := handleManagement(managementRequest{Method: http.MethodGet, Path: path}); response.StatusCode != http.StatusInternalServerError || !strings.Contains(string(response.Body), "relogin_accounts_failed") {
		t.Fatalf("database response=%d %s", response.StatusCode, response.Body)
	}

	raw, err := handleMethod("management.register", nil)
	if err != nil {
		t.Fatal(err)
	}
	var envelopeValue envelope
	if err := json.Unmarshal(raw, &envelopeValue); err != nil {
		t.Fatal(err)
	}
	var registration managementRegistrationResponse
	if !envelopeValue.OK || json.Unmarshal(envelopeValue.Result, &registration) != nil {
		t.Fatalf("bad registration envelope: %s", raw)
	}
	for _, route := range registration.Routes {
		if route.Method == http.MethodGet && route.Path == "/plugins/"+pluginID+"/relogin-required-accounts" {
			return
		}
	}
	t.Fatalf("relogin route missing from registration: %+v", registration.Routes)
}
