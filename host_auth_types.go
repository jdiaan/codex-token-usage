package main

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"
)

type hostCallFunc func(method string, payload any) (json.RawMessage, error)

var hostAuthCaller hostCallFunc = callHost

type hostAuthListResponse struct {
	Files []hostAuthFileEntry `json:"files"`
}

type hostAuthFileEntry struct {
	Account       string `json:"account"`
	AccountType   string `json:"account_type"`
	AuthIndex     string `json:"auth_index"`
	Disabled      bool   `json:"disabled"`
	Email         string `json:"email"`
	Expired       bool   `json:"expired"`
	ID            string `json:"id"`
	Label         string `json:"label"`
	Name          string `json:"name"`
	Note          string `json:"note"`
	Path          string `json:"path"`
	Plan          string `json:"plan"`
	PlanType      string `json:"plan_type"`
	Prefix        string `json:"prefix"`
	Priority      int    `json:"priority"`
	Provider      string `json:"provider"`
	Source        string `json:"source"`
	Status        string `json:"status"`
	StatusMessage string `json:"status_message"`
	Subscription  string `json:"subscription"`
	Tag           string `json:"tag"`
	Type          string `json:"type"`
	Unavailable   bool   `json:"unavailable"`
	RuntimeOnly   bool   `json:"runtime_only"`
	ModTime       string `json:"modtime"`
	UpdatedAt     string `json:"updated_at"`
}

type hostAuthGetRequest struct {
	AuthIndex string `json:"auth_index"`
}

type hostAuthGetResponse struct {
	AuthIndex string          `json:"auth_index"`
	Name      string          `json:"name,omitempty"`
	Path      string          `json:"path,omitempty"`
	JSON      json.RawMessage `json:"json"`
}

type hostAuthRuntimeResponse struct {
	Auth hostAuthFileEntry `json:"auth"`
}

type authSourceDiagnostics struct {
	Source             string `json:"source"`
	Authoritative      bool   `json:"authoritative"`
	Accounts           int    `json:"accounts"`
	HostEntries        int    `json:"host_entries,omitempty"`
	RegisteredFiles    int    `json:"registered_files,omitempty"`
	DiskCodexFiles     int    `json:"disk_codex_files,omitempty"`
	OtherProviders     int    `json:"other_providers,omitempty"`
	RuntimeOnly        int    `json:"runtime_only,omitempty"`
	LegacyEntries      int    `json:"legacy_entries,omitempty"`
	UnknownProviders   int    `json:"unknown_providers,omitempty"`
	PluginDataEntries  int    `json:"plugin_data_entries,omitempty"`
	WaitingRuntimeLoad int    `json:"waiting_runtime_load,omitempty"`
	MetadataReadErrors int    `json:"metadata_read_errors"`
	LastSuccessAt      string `json:"last_success_at,omitempty"`
	LastError          string `json:"last_error,omitempty"`
}

func cloneConfiguredAccounts(accounts []configuredAccount) []configuredAccount {
	return append([]configuredAccount(nil), accounts...)
}

func parseHostAuthUpdatedAt(value string) int64 {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if unix, err := strconv.ParseInt(value, 10, 64); err == nil {
		return normalizeUnixSeconds(unix)
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.Unix()
		}
	}
	return 0
}

func lockMutexWithContext(ctx context.Context, mu *sync.Mutex) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if mu.TryLock() {
		return nil
	}
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if mu.TryLock() {
				return nil
			}
		}
	}
}
