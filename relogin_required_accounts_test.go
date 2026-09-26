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
	store := newTestStore(t)
	globalStore = store
	t.Cleanup(func() {
		store.close()
		globalStore = oldStore
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
