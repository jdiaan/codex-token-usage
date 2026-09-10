package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type lifecycleSnapshot struct {
	Entry                              hostAuthFileEntry
	Data                               map[string]json.RawMessage
	Fingerprint, Identity, ContentHash string
	FileDisabled                       bool
}

type lifecycleHost interface {
	List(context.Context) ([]hostAuthFileEntry, error)
	Read(context.Context, string) (lifecycleSnapshot, error)
	SetDisabled(context.Context, lifecycleSnapshot, bool) error
	Ready() bool
}

type managementAuthClient struct {
	call         hostCallFunc
	baseURL, key string
	client       *http.Client
}

func newManagementAuthClient() *managementAuthClient {
	return &managementAuthClient{call: hostAuthCaller, baseURL: strings.TrimRight(os.Getenv("CPA_TOKEN_USAGE_MANAGEMENT_URL"), "/"), key: os.Getenv("CPA_TOKEN_USAGE_MANAGEMENT_KEY"), client: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (c *managementAuthClient) Ready() bool {
	u, err := url.Parse(c.baseURL)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && c.key != ""
}

func (c *managementAuthClient) List(ctx context.Context) ([]hostAuthFileEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, err := c.call("host.auth.list", map[string]any{})
	if err != nil {
		return nil, errors.New("auth inventory unavailable")
	}
	var result hostAuthListResponse
	if json.Unmarshal(raw, &result) != nil {
		return nil, errors.New("invalid auth inventory response")
	}
	return result.Files, nil
}

func (c *managementAuthClient) Read(ctx context.Context, index string) (lifecycleSnapshot, error) {
	var snapshot lifecycleSnapshot
	entries, err := c.List(ctx)
	if err != nil {
		return snapshot, err
	}
	count := 0
	for _, entry := range entries {
		if entry.AuthIndex == index {
			snapshot.Entry = entry
			count++
		}
	}
	if index == "" || count != 1 {
		return snapshot, errors.New("auth identity missing or ambiguous")
	}
	return c.readEntry(ctx, snapshot.Entry)
}

// Reconciliation already has a fresh inventory. Do not enumerate the entire
// account pool once per account; mutations still call Read for a fresh lookup.
func (c *managementAuthClient) readEntry(ctx context.Context, entry hostAuthFileEntry) (lifecycleSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return lifecycleSnapshot{}, err
	}
	snapshot := lifecycleSnapshot{Entry: entry}
	index := entry.AuthIndex
	e := snapshot.Entry
	if !strings.EqualFold(firstNonEmptyString(e.Provider, e.Type), "codex") || e.RuntimeOnly || pluginOwnedHostAuthEntry(e) || e.Path == "" || fileNameIfJSON(e.Name) == "" {
		return snapshot, errors.New("auth is not a supported physical Codex OAuth credential")
	}
	raw, err := c.call("host.auth.get_runtime", hostAuthGetRequest{AuthIndex: index})
	if err != nil {
		return snapshot, errors.New("runtime auth unavailable")
	}
	var runtime hostAuthRuntimeResponse
	if json.Unmarshal(raw, &runtime) != nil || runtime.Auth.AuthIndex != index || runtime.Auth.ID != e.ID || runtime.Auth.Name != e.Name {
		return snapshot, errors.New("runtime auth identity changed")
	}
	snapshot.Entry = runtime.Auth
	snapshot.Entry.Disabled = runtime.Auth.Disabled || strings.EqualFold(runtime.Auth.Status, "disabled")
	raw, err = c.call("host.auth.get", hostAuthGetRequest{AuthIndex: index})
	if err != nil {
		return snapshot, errors.New("physical auth unavailable")
	}
	var physical hostAuthGetResponse
	if json.Unmarshal(raw, &physical) != nil || physical.AuthIndex != index || physical.Name != e.Name || physical.Path != e.Path {
		return snapshot, errors.New("physical auth identity changed")
	}
	if json.Unmarshal(physical.JSON, &snapshot.Data) != nil || snapshot.Data == nil {
		return snapshot, errors.New("invalid physical auth JSON")
	}
	if raw, ok := snapshot.Data["disabled"]; ok && json.Unmarshal(raw, &snapshot.FileDisabled) != nil {
		return snapshot, errors.New("invalid physical disabled flag")
	}
	credential := map[string]json.RawMessage{}
	identity := map[string]json.RawMessage{}
	for _, key := range []string{"access_token", "refresh_token", "id_token", "account_id", "workspace_id", "chatgpt_account_id"} {
		if v, ok := snapshot.Data[key]; ok {
			credential[key] = v
		}
	}
	for _, key := range []string{"account_id", "workspace_id", "chatgpt_account_id"} {
		if v, ok := snapshot.Data[key]; ok {
			identity[key] = v
		}
	}
	if lifecycleString(snapshot.Data, "access_token") == "" {
		return snapshot, errors.New("Codex access credential unavailable")
	}
	snapshot.Fingerprint = lifecycleHash(credential)
	snapshot.Identity = lifecycleHash(identity)
	content := make(map[string]json.RawMessage, len(snapshot.Data))
	for key, value := range snapshot.Data {
		if key != "disabled" {
			content[key] = value
		}
	}
	snapshot.ContentHash = lifecycleHash(content)
	return snapshot, nil
}

func lifecycleString(data map[string]json.RawMessage, key string) string {
	var v string
	_ = json.Unmarshal(data[key], &v)
	return v
}

func lifecycleHash(data map[string]json.RawMessage) string {
	// Decode with UseNumber so whitespace/key order cannot change a fingerprint,
	// and large numeric metadata cannot be rounded in preservation comparisons.
	raw, _ := json.Marshal(data)
	var normalized any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	_ = d.Decode(&normalized)
	raw, _ = json.Marshal(normalized)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (c *managementAuthClient) SetDisabled(ctx context.Context, s lifecycleSnapshot, disabled bool) error {
	if !c.Ready() {
		return errors.New("management API environment is not configured")
	}
	raw, _ := json.Marshal(map[string]any{"name": s.Entry.Name, "auth_index": s.Entry.AuthIndex, "disabled": disabled})
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, c.baseURL+"/v0/management/auth-files/status", bytes.NewReader(raw))
	if err != nil {
		return errors.New("invalid management API address")
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Content-Type", "application/json")
	response, err := c.client.Do(req)
	if err != nil {
		return errors.New("management status request failed; read-back required")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("management status HTTP %d", response.StatusCode)
	}
	return nil
}
