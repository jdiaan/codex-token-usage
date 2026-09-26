package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *store {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CPA_TOKEN_USAGE_DIR", dir)
	t.Setenv("CPA_AUTH_DIR", filepath.Join(dir, "auth"))
	s := &store{}
	t.Cleanup(s.close)
	return s
}

func TestSummaryCacheKeyCanonicalizesAndClamps(t *testing.T) {
	tests := []struct {
		in   summaryCacheKey
		want summaryCacheKey
	}{
		{in: summaryCacheKey{Window: "24h\\", Limit: 50}, want: summaryCacheKey{Window: "24h", Limit: 50}},
		{in: summaryCacheKey{Window: " TODAY ", Limit: 10}, want: summaryCacheKey{Window: "today", Limit: 10}},
		{in: summaryCacheKey{Window: "all", Limit: 9000}, want: summaryCacheKey{Window: "all", Limit: 5000}},
		{in: summaryCacheKey{}, want: summaryCacheKey{Window: "24h", Limit: 50}},
	}
	for _, test := range tests {
		if got := normalizeSummaryCacheKey(test.in); got != test.want {
			t.Fatalf("normalizeSummaryCacheKey(%+v)=%+v, want %+v", test.in, got, test.want)
		}
	}
}

func TestSummaryMemoryCacheIsBounded(t *testing.T) {
	m := &summaryPrecomputeManager{}
	now := time.Now()
	for i := 1; i <= summaryMemoryMaxEntries+20; i++ {
		m.rememberMemory(summaryCacheKey{Window: "24h", Limit: i}, summaryCacheEntry{
			data:     map[string]any{"limit": i},
			cachedAt: now.Add(time.Duration(i) * time.Millisecond),
		})
	}
	if got := len(m.entries); got != summaryMemoryMaxEntries {
		t.Fatalf("memory cache entries=%d, want %d", got, summaryMemoryMaxEntries)
	}
}

func TestSummarySQLiteCacheIsCanonicalAndBounded(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	for i := 1; i <= summaryStorageMaxEntries+20; i++ {
		if err := s.saveSummaryCacheEntry(ctx, summaryCacheKey{Window: "24h", Limit: i}, summaryCacheEntry{
			data:     map[string]any{"limit": i},
			cachedAt: time.Now().Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatal(err)
		}
	}
	db, _, err := s.open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM summary_cache`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != summaryStorageMaxEntries {
		t.Fatalf("SQLite cache entries=%d, want %d", count, summaryStorageMaxEntries)
	}
	if err := s.saveSummaryCacheEntry(ctx, summaryCacheKey{Window: "bad-window", Limit: 50}, summaryCacheEntry{
		data: map[string]any{"ok": true}, cachedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	var window string
	if err := db.QueryRow(`SELECT window FROM summary_cache WHERE cache_key='24h|50'`).Scan(&window); err != nil {
		t.Fatal(err)
	}
	if window != "24h" {
		t.Fatalf("stored window=%q, want canonical 24h", window)
	}
}

func TestSummarySyncRefreshesAfterUsageRevisionChange(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	cfg := normalizePluginConfig(defaultPluginConfig())
	cfg.SummaryPrecomputeMode = "active_dirty"

	m := &summaryPrecomputeManager{}
	data, err := m.summary(ctx, s, "24h", 50)
	if err != nil {
		t.Fatalf("first summary: %v", err)
	}
	if totals, ok := data["totals"].(totalsRow); !ok || totals.Requests != 0 {
		t.Fatalf("initial totals = %#v, want 0 requests", data["totals"])
	}
	if _, ok := data["store_revision"]; !ok {
		t.Fatalf("summary missing store_revision")
	}

	if err := s.recordUsage(ctx, usageRecord{
		Provider:     "codex",
		ExecutorType: "CodexExecutor",
		Model:        "gpt-5.5",
		AuthID:       "alice@example.com",
		AuthIndex:    "alice.cpa.json",
		Source:       "alice@example.com",
		RequestedAt:  time.Now(),
		Detail:       usageDetail{InputTokens: 11, OutputTokens: 22, TotalTokens: 33},
	}); err != nil {
		t.Fatalf("record usage: %v", err)
	}

	data, err = m.summarySync(ctx, s, "24h", 50)
	if err != nil {
		t.Fatalf("second summary: %v", err)
	}
	totals, ok := data["totals"].(totalsRow)
	if !ok {
		t.Fatalf("totals type = %T", data["totals"])
	}
	if totals.Requests != 1 || totals.TotalTokens != 33 {
		t.Fatalf("totals after usage = %+v, want one fresh request", totals)
	}
	if pre, ok := data["precompute"].(summaryPrecomputeInfo); ok && pre.Hit {
		t.Fatalf("summary reused stale cache after usage revision changed: %+v", pre)
	}
}

func TestSummaryReturnsStaleCacheWhileRevisionRefreshRuns(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	m := &summaryPrecomputeManager{}
	data, err := m.summary(ctx, s, "24h", 50)
	if err != nil {
		t.Fatal(err)
	}
	if totals := data["totals"].(totalsRow); totals.Requests != 0 {
		t.Fatalf("initial requests = %d", totals.Requests)
	}
	if err := s.recordUsage(ctx, usageRecord{
		Provider: "codex", AuthID: "alice", AuthIndex: "alice", Source: "alice",
		RequestedAt: time.Now(), Detail: usageDetail{TotalTokens: 10},
	}); err != nil {
		t.Fatal(err)
	}
	key := normalizeSummaryCacheKey(summaryCacheKey{Window: "24h", Limit: 50})
	m.mu.Lock()
	if m.refreshing == nil {
		m.refreshing = map[summaryCacheKey]bool{}
	}
	m.refreshing[key] = true
	m.mu.Unlock()
	data, err = m.summary(ctx, s, "24h", 50)
	if err != nil {
		t.Fatal(err)
	}
	if totals := data["totals"].(totalsRow); totals.Requests != 0 {
		t.Fatalf("stale response requests = %d, want cached 0", totals.Requests)
	}
	pre, ok := data["precompute"].(summaryPrecomputeInfo)
	if !ok || !pre.Hit || !pre.Stale || pre.Synchronous || pre.Reason != "revision_stale" {
		t.Fatalf("precompute = %#v, want asynchronous revision-stale hit", data["precompute"])
	}
}

func TestSummaryAsyncRefreshIsThrottledWithinPrecomputeInterval(t *testing.T) {
	cfg := normalizePluginConfig(defaultPluginConfig())
	key := normalizeSummaryCacheKey(summaryCacheKey{Window: "24h", Limit: 50})
	m := &summaryPrecomputeManager{
		entries: map[summaryCacheKey]summaryCacheEntry{
			key: {data: map[string]any{"ok": true}, cachedAt: time.Now(), revision: "old"},
		},
		refreshing: map[summaryCacheKey]bool{},
	}
	m.refreshAsyncThrottled(nil, cfg, key)
	m.mu.Lock()
	refreshing := m.refreshing[key]
	m.mu.Unlock()
	if refreshing {
		t.Fatal("recent cache entry unexpectedly started another asynchronous refresh")
	}
}

func TestSummaryMaintenanceSkipsWhenRevisionAndAuthFilesUnchanged(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	globalStore = s
	t.Cleanup(func() { globalStore = &store{} })

	if err := s.recordUsage(ctx, usageRecord{
		Provider:    "codex",
		Model:       "gpt-5.5",
		AuthID:      "alice@example.com",
		AuthIndex:   "alice.cpa.json",
		Source:      "alice@example.com",
		RequestedAt: time.Now(),
		Detail:      usageDetail{TotalTokens: 1},
	}); err != nil {
		t.Fatalf("record usage: %v", err)
	}

	m := &summaryMaintenanceManager{}
	m.run(ctx)
	first := m.status()
	if first.SkippedReason != "" {
		t.Fatalf("first maintenance skipped unexpectedly: %+v", first)
	}
	m.run(ctx)
	second := m.status()
	if second.SkippedReason != "unchanged" {
		t.Fatalf("second maintenance skipped_reason = %q, want unchanged; state=%+v", second.SkippedReason, second)
	}
	if second.LastProcessedUsageEventID == 0 {
		t.Fatalf("maintenance did not record processed usage event id: %+v", second)
	}
}

func TestSummaryMaintenanceUsesLightModeAfterNewUsageWithoutAuthFileChange(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	globalStore = s
	t.Cleanup(func() { globalStore = &store{} })

	m := &summaryMaintenanceManager{}
	m.run(ctx)
	first := m.status()
	if first.LastMode != "full" {
		t.Fatalf("first maintenance mode = %q, want full; state=%+v", first.LastMode, first)
	}

	if err := s.recordUsage(ctx, usageRecord{
		Provider:    "codex",
		Model:       "gpt-5.5",
		AuthID:      "alice@example.com",
		AuthIndex:   "alice.cpa.json",
		Source:      "alice@example.com",
		RequestedAt: time.Now(),
		Detail:      usageDetail{TotalTokens: 2},
	}); err != nil {
		t.Fatalf("record usage: %v", err)
	}

	m.run(ctx)
	second := m.status()
	if second.LastMode != "light" {
		t.Fatalf("maintenance mode after new usage = %q, want light; state=%+v", second.LastMode, second)
	}
	if second.SkippedReason != "" {
		t.Fatalf("light maintenance should run, not skip: %+v", second)
	}
}

func TestConfiguredAuthFilesCacheInvalidatesOnFileChange(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CPA_AUTH_DIR", dir)
	path := filepath.Join(dir, "alice.json")
	if err := os.WriteFile(path, []byte(`{"provider":"codex","email":"alice@example.com","plan_type":"plus"}`), 0600); err != nil {
		t.Fatal(err)
	}
	first := readConfiguredAuthFiles()
	if len(first) != 1 || first[0].PlanType != "plus" {
		t.Fatalf("first read = %+v", first)
	}
	first[0].PlanType = "mutated"
	second := readConfiguredAuthFiles()
	if len(second) != 1 || second[0].PlanType != "plus" {
		t.Fatalf("cached clone was mutated: %+v", second)
	}
	if err := os.WriteFile(path, []byte(`{"provider":"codex","email":"alice@example.com","plan_type":"team","name":"changed"}`), 0600); err != nil {
		t.Fatal(err)
	}
	third := readConfiguredAuthFiles()
	if len(third) != 1 || third[0].PlanType != "team" {
		t.Fatalf("cache did not invalidate: %+v", third)
	}
}

func TestConfiguredAuthPlanTypeUsesExplicitValueThenJWTFallback(t *testing.T) {
	jwt := func(claims map[string]any) string {
		t.Helper()
		header, err := json.Marshal(map[string]any{"alg": "none"})
		if err != nil {
			t.Fatal(err)
		}
		payload, err := json.Marshal(claims)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".test"
	}
	access := jwt(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_plan_type": "plus"}})
	idToken := jwt(map[string]any{"https://api.openai.com/auth.chatgpt_plan_type": "team"})

	if got := configuredAuthPlanType(map[string]any{"plan_type": "Pro", "access_token": access}); got != "pro" {
		t.Fatalf("explicit plan type = %q, want pro", got)
	}
	if got := configuredAuthPlanType(map[string]any{"access_token": access}); got != "plus" {
		t.Fatalf("access token plan type = %q, want plus", got)
	}
	if got := configuredAuthPlanType(map[string]any{"id_token": idToken}); got != "team" {
		t.Fatalf("id token plan type = %q, want team", got)
	}
	if got := configuredAuthPlanType(map[string]any{"access_token": "not-a-jwt"}); got != "" {
		t.Fatalf("unknown plan type = %q, want empty", got)
	}
}

func TestExternalUseScanIsCappedAt24Hours(t *testing.T) {
	now := time.Now().Unix()
	if got := externalUseScanSince(0, now); got != now-int64((24*time.Hour)/time.Second) {
		t.Fatalf("all-window scan since = %d", got)
	}
	recent := now - int64(time.Hour/time.Second)
	if got := externalUseScanSince(recent, now); got != recent {
		t.Fatalf("recent scan since = %d, want %d", got, recent)
	}
}

func TestQueryHasXAIUsageUsesProviderIndexPath(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	db, _, err := s.open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if found, err := queryHasXAIUsage(ctx, db, 0); err != nil || found {
		t.Fatalf("empty xAI usage found=%v err=%v", found, err)
	}
	if err := s.recordUsage(ctx, usageRecord{
		Provider: "xai", AuthID: "grok", AuthIndex: "grok", Source: "grok",
		RequestedAt: time.Now(), Detail: usageDetail{TotalTokens: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if found, err := queryHasXAIUsage(ctx, db, 0); err != nil || !found {
		t.Fatalf("xAI usage found=%v err=%v", found, err)
	}
}
