package main

import (
	"context"
	"database/sql"
	"net/http"
	"testing"
	"time"
)

func TestQuotaWindowLabelUsesReportedDuration(t *testing.T) {
	tests := []struct {
		name string
		d    time.Duration
		want string
	}{
		{"five hour", 5 * time.Hour, "5h"},
		{"week", 7 * 24 * time.Hour, "7d"},
		{"month", 30 * 24 * time.Hour, "month"},
		{"fourteen days", 14 * 24 * time.Hour, "14d"},
		{"two hours", 2 * time.Hour, "2h"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := quotaWindowLabelForDuration(tt.d); got != tt.want {
				t.Fatalf("quotaWindowLabelForDuration(%s)=%q, want %q", tt.d, got, tt.want)
			}
		})
	}
}

func TestQuotaWindowLabelDoesNotInferMissingWindow(t *testing.T) {
	if got := quotaWindowLabel(quotaWindowSnapshot{Presence: quotaWindowAbsent}); got != "" {
		t.Fatalf("absent quota window label=%q, want empty", got)
	}
	if got := quotaWindowLabel(quotaWindowSnapshot{}); got != "window" {
		t.Fatalf("unknown quota window label=%q, want window", got)
	}
}

func TestClassifyCodexBanUsesDynamicWindowMetadata(t *testing.T) {
	now := int64(1700000000)
	primaryReset := now + 2*24*60*60
	secondaryReset := now + 30*24*60*60
	primaryPct, secondaryPct := 100.0, 100.0
	reset, window, reason := classifyCodexBan(map[string][]string{
		"x-codex-primary-limit-window-seconds":   {"172800"},
		"x-codex-secondary-limit-window-seconds": {"2592000"},
	}, &primaryPct, &primaryReset, &secondaryPct, &secondaryReset, now)
	if reset != secondaryReset || window != "month" || reason != "primary and secondary windows are full" {
		t.Fatalf("dynamic ban=%d/%q/%q, want %d/month/full", reset, window, reason, secondaryReset)
	}
}

func TestClassifyCodexBanAcceptsLegacyMinutesMetadata(t *testing.T) {
	now := int64(1700000000)
	resetAt := now + 5*60*60
	pct := 100.0
	reset, window, _ := classifyCodexBan(map[string][]string{"x-codex-primary-window-minutes": {"300"}}, &pct, &resetAt, nil, nil, now)
	if reset != resetAt || window != "5h" {
		t.Fatalf("legacy minutes metadata=%d/%q, want %d/5h", reset, window, resetAt)
	}
}

func TestClassifyCodexBanRetryAfterFallbackDoesNotInventFiveHours(t *testing.T) {
	now := int64(1700000000)
	reset, window, reason := classifyCodexBan(map[string][]string{"retry-after": {"42"}}, nil, nil, nil, nil, now)
	if reset != now+42 || window != "rate_limit" || reason != "fallback: retry-after header" {
		t.Fatalf("retry-after fallback=%d/%q/%q, want %d/rate_limit/fallback", reset, window, reason, now+42)
	}
}

func TestCodexBanWithoutWindowMetadataIsUnknown(t *testing.T) {
	now := int64(1700000000)
	resetAt := now + 3600
	pct := 100.0
	reset, window, _ := classifyCodexBan(nil, &pct, &resetAt, nil, nil, now)
	if reset != resetAt || window != "window" {
		t.Fatalf("missing metadata=%d/%q, want %d/window", reset, window, resetAt)
	}
	if isReleasable429Autoban(autobanRow{Window: window, LastStatusCode: http.StatusTooManyRequests}) == false {
		t.Fatal("unknown dynamic quota window should remain releasable as a 429 ban")
	}
}

func TestClassifyCodexBanWithoutResetUsesShortCooldown(t *testing.T) {
	now := int64(1700000000)
	reset, window, reason := classifyCodexBan(nil, nil, nil, nil, nil, now)
	if reset != now+10*60 || window != "rate_limit" || reason != "fallback: quota window metadata unavailable" {
		t.Fatalf("missing reset fallback=%d/%q/%q, want %d/rate_limit/cooldown", reset, window, reason, now+10*60)
	}
}

func TestQuotaProbePreservesAbsentAndDynamicWindows(t *testing.T) {
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if _, err := db.Exec(`
INSERT INTO quota_trigger_runs (
  auth_id, auth_index, source, provider, auth_file, auth_file_mtime, mode, status, http_status,
  started_at, finished_at, primary_used_percent, primary_reset_at, secondary_used_percent, secondary_reset_at,
  primary_window_presence, primary_limit_window_seconds, primary_reset_after_seconds,
  secondary_window_presence, secondary_limit_window_seconds, secondary_reset_after_seconds
) VALUES ('dynamic', 'dynamic.json', 'dynamic', 'codex', 'dynamic.json', 11, 'probe', 'success', 200,
  ?, ?, NULL, NULL, 21, ?, 'absent', NULL, NULL, 'present', ?, ?)`,
		now-2, now-1, now+14*24*60*60, 14*24*60*60, 14*24*60*60-1); err != nil {
		t.Fatal(err)
	}
	accounts := []accountRow{{AuthID: "dynamic", AuthIndex: "dynamic.json", Source: "dynamic", AuthFile: "dynamic.json", AuthFileMTime: 11}}
	applyLatestQuotaSnapshots(context.Background(), db, accounts, now-3600)
	if accounts[0].PrimaryQuotaWindowPresence != string(quotaWindowAbsent) || accounts[0].PrimaryUsedPercent != nil {
		t.Fatalf("primary absent snapshot=%+v", accounts[0])
	}
	if accounts[0].SecondaryQuotaWindowPresence != string(quotaWindowPresent) || accounts[0].SecondaryQuotaWindow != "14d" || accounts[0].SecondaryQuotaWindowSeconds != 14*24*60*60 {
		t.Fatalf("secondary dynamic snapshot=%+v", accounts[0])
	}
}

func TestPlanQuotaWindowsKeepReportedSlots(t *testing.T) {
	primaryPercent, secondaryPercent := 12.0, 34.0
	primaryReset, secondaryReset := int64(4102444800), int64(4103049600)
	primary := quotaWindowSnapshot{
		Presence: quotaWindowPresent,
		Percent:  sql.NullFloat64{Float64: primaryPercent, Valid: true},
		ResetAt:  sql.NullInt64{Int64: primaryReset, Valid: true},
		LimitWindowSeconds: sql.NullInt64{
			Int64: int64((5 * time.Hour) / time.Second), Valid: true,
		},
	}
	secondary := quotaWindowSnapshot{
		Presence: quotaWindowPresent,
		Percent:  sql.NullFloat64{Float64: secondaryPercent, Valid: true},
		ResetAt:  sql.NullInt64{Int64: secondaryReset, Valid: true},
		LimitWindowSeconds: sql.NullInt64{
			Int64: int64((7 * 24 * time.Hour) / time.Second), Valid: true,
		},
	}
	plus := accountRow{PlanType: "plus"}
	applyAccountQuotaSnapshot(&plus, primary, secondary)
	if plus.PrimaryUsedPercent == nil || plus.SecondaryUsedPercent == nil || plus.PrimaryQuotaWindow != "5h" || plus.SecondaryQuotaWindow != "7d" {
		t.Fatalf("plus must retain both reported windows: %+v", plus)
	}
	free := accountRow{PlanType: "free"}
	applyAccountQuotaSnapshot(&free, primary, quotaWindowSnapshot{Presence: quotaWindowAbsent})
	if free.PrimaryUsedPercent == nil || free.SecondaryUsedPercent != nil || free.SecondaryQuotaWindowPresence != string(quotaWindowAbsent) {
		t.Fatalf("free single reported window=%+v", free)
	}
}

func TestFreePlansDoNotContributeSecondaryQuotaEstimates(t *testing.T) {
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	used := 50.0
	accounts := []accountRow{{
		AuthID: "free", AuthIndex: "free.json", PlanType: "free", Requests: 1,
		SecondaryUsedPercent: &used, SecondaryWindowTokens: 500,
	}}
	totals := totalsRow{}
	applySecondaryQuotaEstimates(context.Background(), db, accounts, &totals, time.Now().Add(-time.Hour).Unix())
	if accounts[0].SecondaryQuotaTotalEstimate != 0 || accounts[0].SecondaryQuotaRemainingEstimate != 0 || totals.SecondaryQuotaEstimatedAccounts != 0 || totals.SecondaryQuotaTotalEstimate != 0 {
		t.Fatalf("free account must not contribute window 2 estimates: account=%+v totals=%+v", accounts[0], totals)
	}
}

func TestExpiredQuotaProbePreservesWindowShapeButClearsStaleValues(t *testing.T) {
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if _, err := db.Exec(`
INSERT INTO quota_trigger_runs (
  auth_id, auth_index, source, provider, auth_file, auth_file_mtime, mode, status, http_status,
  started_at, finished_at, primary_used_percent, primary_reset_at,
  primary_window_presence, primary_limit_window_seconds, primary_reset_after_seconds
) VALUES ('expired', 'expired.json', 'expired', 'codex', 'expired.json', 7, 'probe', 'success', 200,
  ?, ?, 88, ?, 'present', ?, ?)`,
		now-20, now-10, now-1, 5*60*60, 5*60*60); err != nil {
		t.Fatal(err)
	}
	accounts := []accountRow{{AuthID: "expired", AuthIndex: "expired.json", Source: "expired", AuthFile: "expired.json", AuthFileMTime: 7}}
	applyLatestQuotaSnapshots(context.Background(), db, accounts, now-3600)
	if accounts[0].PrimaryQuotaWindowPresence != string(quotaWindowPresent) || accounts[0].PrimaryQuotaWindow != "5h" || accounts[0].PrimaryUsedPercent != nil || accounts[0].PrimaryResetAt != nil {
		t.Fatalf("expired snapshot=%+v", accounts[0])
	}
}
