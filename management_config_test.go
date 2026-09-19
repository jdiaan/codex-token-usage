package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

const testManagementYAML = "management_url: 'http://127.0.0.1:8317/'\nmanagement_key: 'test-private-secret'\n"

func writeManagementConfig(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestManagementConfigPaths(t *testing.T) {
	home := t.TempDir()
	withUserHomeDir(t, home, nil)
	t.Setenv("CPA_TOKEN_USAGE_MANAGEMENT_CONFIG_FILE", "")
	t.Setenv("CPA_TOKEN_USAGE_DIR", filepath.Join(home, "ordinary-data"))
	path, err := managementConfigPath()
	if err != nil || path != filepath.Join(home, ".cli-proxy-api", "secrets", managementConfigFileName) {
		t.Fatalf("unexpected default private path: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("resolving private configuration must not create directories")
	}
	override := filepath.Join(home, "private.yaml")
	t.Setenv("CPA_TOKEN_USAGE_MANAGEMENT_CONFIG_FILE", override)
	if got, err := managementConfigPath(); err != nil || got != override {
		t.Fatal("absolute override was not honored")
	}
	t.Setenv("CPA_TOKEN_USAGE_MANAGEMENT_CONFIG_FILE", "relative.yaml")
	if got := loadManagementConfig(); got.errorCode != "invalid_path" {
		t.Fatalf("relative path error = %s", got.errorCode)
	}
	t.Setenv("CPA_TOKEN_USAGE_MANAGEMENT_CONFIG_FILE", "")
	withUserHomeDir(t, "", errors.New("sensitive home error"))
	if got := loadManagementConfig(); got.errorCode != "invalid_path" {
		t.Fatalf("home resolution error = %s", got.errorCode)
	}
}

func TestManagementConfigStrictYAML(t *testing.T) {
	for _, tc := range []struct{ name, raw, code string }{
		{"valid", testManagementYAML, ""},
		{"quoted special characters", "management_url: https://example.test\nmanagement_key: 'a:# b'", ""},
		{"empty", "", "invalid_yaml"},
		{"malformed", "management_key: [", "invalid_yaml"},
		{"sequence", "- management_key", "invalid_yaml"},
		{"multiple documents", testManagementYAML + "---\nmanagement_key: other", "invalid_yaml"},
		{"duplicate", testManagementYAML + "management_key: other", "duplicate_field"},
		{"unknown", testManagementYAML + "other: value", "unknown_field"},
		{"merge", testManagementYAML + "<<: {management_key: other}", "unknown_field"},
		{"number", "management_url: http://localhost\nmanagement_key: 12345", "invalid_type"},
		{"bool", "management_url: http://localhost\nmanagement_key: true", "invalid_type"},
		{"null", "management_url: http://localhost\nmanagement_key: null", "invalid_type"},
		{"nested", "management_url: http://localhost\nmanagement_key: {key: secret}", "invalid_type"},
		{"alias", "management_url: &url http://localhost\nmanagement_key: *url", "invalid_type"},
		{"missing key", "management_url: http://localhost", "missing_fields"},
		{"empty key", "management_url: http://localhost\nmanagement_key: '  '", "missing_fields"},
		{"empty map", "{}", "missing_fields"},
		{"scheme", "management_url: ftp://localhost\nmanagement_key: secret", "invalid_url"},
		{"host missing", "management_url: http:///\nmanagement_key: secret", "invalid_url"},
		{"userinfo", "management_url: http://user:password@localhost\nmanagement_key: secret", "invalid_url"},
		{"query", "management_url: http://localhost?key=secret\nmanagement_key: secret", "invalid_url"},
		{"fragment", "management_url: http://localhost/#secret\nmanagement_key: secret", "invalid_url"},
		{"header injection", "management_url: http://localhost\nmanagement_key: \"first\\nsecond\"", "invalid_key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := parseManagementConfig([]byte(tc.raw))
			if config.errorCode != tc.code || config.ready() != (tc.code == "") {
				t.Fatalf("error = %q, ready = %v; want error %q", config.errorCode, config.ready(), tc.code)
			}
			client := newManagementAuthClient(config)
			if client.Ready() != config.ready() {
				t.Fatal("client readiness disagrees with private configuration")
			}
			if !config.ready() {
				transport := &managementCountingTransport{}
				client.client.Transport = transport
				if err := client.SetDisabled(context.Background(), lifecycleSnapshot{}, true); err == nil || transport.calls != 0 {
					t.Fatal("invalid private configuration attempted an HTTP request")
				}
			}
		})
	}
}

type managementCountingTransport struct{ calls int }

func (t *managementCountingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls++
	return nil, errors.New("unexpected transport access")
}

func TestManagementConfigFilesAndPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), managementConfigFileName)
	t.Setenv("CPA_TOKEN_USAGE_MANAGEMENT_CONFIG_FILE", path)
	if got := loadManagementConfig(); got.errorCode != "file_missing" {
		t.Fatalf("missing file error = %s", got.errorCode)
	}
	writeManagementConfig(t, path, testManagementYAML)
	if got := loadManagementConfig(); !got.ready() || got.baseURL != "http://127.0.0.1:8317" {
		t.Fatalf("valid file rejected: %s", got.errorCode)
	}
	writeManagementConfig(t, path, strings.Repeat("x", managementConfigMaxBytes+1))
	if got := loadManagementConfig(); got.errorCode != "invalid_file" {
		t.Fatalf("oversized file error = %s", got.errorCode)
	}
	t.Setenv("CPA_TOKEN_USAGE_MANAGEMENT_CONFIG_FILE", filepath.Dir(path))
	if got := loadManagementConfig(); got.errorCode != "invalid_file" {
		t.Fatalf("directory error = %s", got.errorCode)
	}
	for _, goos := range []string{"linux", "darwin"} {
		for _, mode := range []os.FileMode{0400, 0600, 0440, 0640} {
			if !safeManagementConfigMode(mode, goos) {
				t.Fatalf("%s rejected mode %o", goos, mode)
			}
		}
		for _, mode := range []os.FileMode{0644, 0601, 0602, 0660, 0650, 0777} {
			if safeManagementConfigMode(mode, goos) {
				t.Fatalf("%s accepted unsafe mode %o", goos, mode)
			}
		}
	}
	if !safeManagementConfigMode(0666, "windows") {
		t.Fatal("Windows ACL permissions cannot be inferred from POSIX mode bits")
	}
	if runtime.GOOS != "windows" {
		t.Setenv("CPA_TOKEN_USAGE_MANAGEMENT_CONFIG_FILE", path)
		writeManagementConfig(t, path, testManagementYAML)
		if err := os.Chmod(path, 0644); err != nil {
			t.Fatal(err)
		}
		if got := loadManagementConfig(); got.errorCode != "unsafe_permissions" {
			t.Fatalf("unsafe file permissions error = %s", got.errorCode)
		}
		if err := os.Chmod(path, 0000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, 0600) })
		if os.Geteuid() != 0 {
			if got := loadManagementConfig(); got.errorCode != "file_unreadable" {
				t.Fatalf("unreadable file error = %s", got.errorCode)
			}
		}
	}
}

func TestManagementConfigCacheKeepsSuccessAndFailureUntilRestart(t *testing.T) {
	for _, initial := range []string{"valid", "missing", "invalid"} {
		t.Run(initial, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), managementConfigFileName)
			t.Setenv("CPA_TOKEN_USAGE_MANAGEMENT_CONFIG_FILE", path)
			if initial == "valid" {
				writeManagementConfig(t, path, testManagementYAML)
			} else if initial == "invalid" {
				writeManagementConfig(t, path, "management_key: [")
			}
			cache := &managementConfigCache{}
			cache.initialize()
			before := cache.current()
			writeManagementConfig(t, path, strings.ReplaceAll(testManagementYAML, "test-private-secret", "replacement-secret"))
			var wg sync.WaitGroup
			for i := 0; i < 16; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					cache.initialize()
					if !reflect.DeepEqual(before, cache.current()) {
						t.Error("cached configuration changed without restarting")
					}
				}()
			}
			wg.Wait()
			freshProcessCache := &managementConfigCache{}
			freshProcessCache.initialize()
			if got := freshProcessCache.current(); !got.ready() || got.key != "replacement-secret" {
				t.Fatal("fresh process cache did not read replacement configuration")
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before.status(), cache.current().status()) {
				t.Fatal("status polling reread the removed file")
			}
		})
	}
}

func TestManagementConfigOldSourcesIgnoredAndStatusPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), managementConfigFileName)
	t.Setenv("CPA_TOKEN_USAGE_MANAGEMENT_CONFIG_FILE", path)
	t.Setenv("CPA_TOKEN_USAGE_MANAGEMENT_URL", "http://127.0.0.1:9999")
	t.Setenv("CPA_TOKEN_USAGE_MANAGEMENT_KEY", "old-environment-secret")
	if got := loadManagementConfig(); got.ready() || got.errorCode != "file_missing" {
		t.Fatal("legacy environment unexpectedly supplied missing private configuration")
	}
	writeManagementConfig(t, path, testManagementYAML)
	previous := globalManagementConfig
	globalManagementConfig = &managementConfigCache{}
	t.Cleanup(func() { globalManagementConfig = previous })
	globalManagementConfig.initialize()
	config := globalManagementConfig.current()
	if config.baseURL != "http://127.0.0.1:8317" || config.key != "test-private-secret" {
		t.Fatal("environment overrode private file configuration")
	}
	controller := &authLifecycleController{}
	before := controller.status()
	writeManagementConfig(t, path, "management_key: [")
	parsePluginConfigYAML([]byte("management_url: http://attacker.test\nmanagement_key: old-plugin-secret\nmanagement_config_file: other.yaml"), defaultPluginConfig())
	t.Setenv("CPA_TOKEN_USAGE_MANAGEMENT_CONFIG_FILE", filepath.Join(t.TempDir(), "changed.yaml"))
	globalManagementConfig.initialize()
	if after := controller.status(); !reflect.DeepEqual(before, after) {
		t.Fatal("status changed after ordinary configuration or environment update")
	}
	status := config.status()
	if status["url_source"] != "local_file" || status["key_source"] != "local_file" || status["error_code"] != "" {
		t.Fatal("status does not describe local configuration")
	}
	var logs bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousOutput) })
	for _, raw := range []string{testManagementYAML, testManagementYAML + "test-private-secret: [", "management_url: 'http://test-private-secret@localhost'\nmanagement_key: test-private-secret"} {
		value := parseManagementConfig([]byte(raw))
		encoded, err := json.Marshal(value.status())
		if err != nil {
			t.Fatal(err)
		}
		err = newManagementAuthClient(managementConfigFailure(value.errorCode)).SetDisabled(context.Background(), lifecycleSnapshot{}, true)
		output := string(encoded) + err.Error() + logs.String()
		for _, secret := range []string{"test-private-secret", "old-environment-secret", path, "http://"} {
			if strings.Contains(output, secret) {
				t.Fatal("private configuration leaked through public output")
			}
		}
	}
	encoded, _ := json.Marshal(before)
	if strings.Contains(string(encoded), "test-private-secret") || strings.Contains(string(encoded), path) {
		t.Fatal("controller status leaked private configuration")
	}
}

func TestManagementConfigNewProcessReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), managementConfigFileName)
	t.Setenv("CPA_TOKEN_USAGE_MANAGEMENT_CONFIG_FILE", path)
	t.Setenv("CPA_TOKEN_USAGE_DIR", t.TempDir())
	for _, key := range []string{"first-key", "second-key"} {
		writeManagementConfig(t, path, strings.ReplaceAll(testManagementYAML, "test-private-secret", key))
		cmd := exec.Command(os.Args[0], "-test.run=^TestManagementConfigProcessHelper$")
		cmd.Env = append(os.Environ(), "CPA_PRIVATE_CONFIG_TEST_HELPER=1", "CPA_PRIVATE_CONFIG_EXPECTED="+key)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fresh process failed: %v\n%s", err, output)
		}
	}
}

func TestManagementConfigProcessHelper(t *testing.T) {
	if os.Getenv("CPA_PRIVATE_CONFIG_TEST_HELPER") != "1" {
		return
	}
	configure := func(extra string) {
		t.Helper()
		raw, err := json.Marshal(map[string]string{"config_yaml": "model_price_auto_update_enabled: false\nquota_trigger_enabled: false\nsummary_precompute_enabled: false\n" + extra})
		if err != nil {
			t.Fatal(err)
		}
		if err := configurePlugin(raw); err != nil {
			t.Fatal(err)
		}
	}
	configure("")
	defer globalAuthLifecycle.stop()
	if value := globalManagementConfig.current(); !value.ready() || value.key != os.Getenv("CPA_PRIVATE_CONFIG_EXPECTED") {
		t.Fatal("new process did not load the current private file")
	}
	before := globalManagementConfig.current()
	writeManagementConfig(t, os.Getenv("CPA_TOKEN_USAGE_MANAGEMENT_CONFIG_FILE"), "management_key: [")
	t.Setenv("CPA_TOKEN_USAGE_MANAGEMENT_CONFIG_FILE", filepath.Join(t.TempDir(), "replacement.yaml"))
	t.Setenv("CPA_TOKEN_USAGE_MANAGEMENT_URL", "http://attacker.test")
	t.Setenv("CPA_TOKEN_USAGE_MANAGEMENT_KEY", "legacy-secret")
	configure("management_url: http://attacker.test\nmanagement_key: legacy-secret\naccount_protection_free_token_limit: 12345\n")
	if globalAccountProtection.config().AccountProtectionFreeTokenLimit != 12345 {
		t.Fatal("ordinary plugin reconfiguration did not run")
	}
	globalAuthLifecycle.opMu.Lock()
	client, ok := globalAuthLifecycle.host.(*managementAuthClient)
	unchanged := ok && client.baseURL == before.baseURL && client.key == before.key
	globalAuthLifecycle.opMu.Unlock()
	if !unchanged || !reflect.DeepEqual(before, globalManagementConfig.current()) || globalAuthLifecycle.status()["management_configured"] != true {
		t.Fatal("plugin reconfiguration changed the active private client or status")
	}
}

func TestManagementConfigClientWritesAndRejectsRedirects(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusUnauthorized, http.StatusTemporaryRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var requests, redirects atomic.Int32
			destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				redirects.Add(1)
				w.WriteHeader(http.StatusOK)
			}))
			defer destination.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestNumber := requests.Add(1)
				if r.Method != http.MethodPatch || r.URL.Path != "/v0/management/auth-files/status" || r.Header.Get("Authorization") != "Bearer test-private-secret" {
					t.Error("wrong management request contract")
				}
				var body struct {
					Name     string `json:"name"`
					Index    string `json:"auth_index"`
					Disabled bool   `json:"disabled"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name != "a.json" || body.Index != "a" || body.Disabled != (requestNumber == 1) {
					t.Error("wrong account status payload")
				}
				w.Header().Set("Location", destination.URL)
				w.WriteHeader(status)
				_, _ = w.Write([]byte("test-private-secret must not appear in errors"))
			}))
			defer server.Close()
			path := filepath.Join(t.TempDir(), managementConfigFileName)
			t.Setenv("CPA_TOKEN_USAGE_MANAGEMENT_CONFIG_FILE", path)
			writeManagementConfig(t, path, strings.ReplaceAll(testManagementYAML, "http://127.0.0.1:8317/", server.URL))
			client := newManagementAuthClient(loadManagementConfig())
			for _, disabled := range []bool{true, false} {
				err := client.SetDisabled(context.Background(), lifecycleSnapshot{Entry: hostAuthFileEntry{Name: "a.json", AuthIndex: "a"}}, disabled)
				if (err == nil) != (status == http.StatusOK) {
					t.Fatalf("status %d: %v", status, err)
				}
				if err != nil && strings.Contains(err.Error(), "test-private-secret") {
					t.Fatal("management error leaked response body or key")
				}
			}
			if requests.Load() != 2 || redirects.Load() != 0 {
				t.Fatalf("requests=%d redirects=%d", requests.Load(), redirects.Load())
			}
			server.Close()
			if err := client.SetDisabled(context.Background(), lifecycleSnapshot{}, false); err == nil || !strings.Contains(err.Error(), "request failed") || strings.Contains(err.Error(), "test-private-secret") {
				t.Fatal("network failure was not safely reported")
			}
		})
	}
}
