package main

import (
	"encoding/json"
	"os"
	"testing"
)

func TestXAIStateResolveManagementRouteIsIdempotent(t *testing.T) {
	oldStore := globalStore
	oldSource := globalXAIAuthSource
	oldCaller := hostAuthCaller
	globalStore = &store{}
	globalXAIAuthSource = &xaiAuthSourceManager{}
	hostAuthCaller = func(method string, _ any) (json.RawMessage, error) {
		if method != "host.auth.list" {
			return nil, os.ErrNotExist
		}
		return json.Marshal(hostAuthListResponse{})
	}
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	t.Cleanup(func() {
		globalStore.close()
		globalStore = oldStore
		globalXAIAuthSource = oldSource
		hostAuthCaller = oldCaller
	})

	body, err := json.Marshal(xaiStateResolveRequest{Items: []xaiStateResolveRequestItem{{
		StateKey:      "__safe_missing_xai_state__",
		ExpectedState: xaiStateUnauthorized,
		Action:        "file_absent",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	response := handleManagement(managementRequest{
		Method: "POST",
		Path:   "/v0/management/plugins/" + pluginID + "/xai-states/resolve",
		Body:   body,
	})
	if response.StatusCode != 200 {
		t.Fatalf("status=%d body=%s", response.StatusCode, response.Body)
	}
	var result xaiStateResolveResponse
	if err := json.Unmarshal(response.Body, &result); err != nil {
		t.Fatal(err)
	}
	if result.AlreadyResolved != 1 || len(result.Items) != 1 || result.Items[0].Status != "already_resolved" {
		t.Fatalf("result=%+v", result)
	}
}
