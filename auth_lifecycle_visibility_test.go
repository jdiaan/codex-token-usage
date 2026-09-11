package main

import (
	"context"
	"strconv"
	"testing"
	"time"
)

func TestNativeVisibleBansAndRateRecovery(t *testing.T) {
	c, host, clock := nativeTestController(t)
	lifecycleFailure(t, c, "a", 401, `{}`)
	lifecycleFailure(t, c, "b", 402, `{}`)
	lifecycleFailure(t, c, "c", 429, `{"error":{"type":"rate_limit_exceeded","retry_after":30}}`)
	lifecycleFailure(t, c, "d", 429, `{"error":{"type":"usage_limit_reached","resets_in_seconds":3600}}`)
	db, _, _ := c.store.open(context.Background())
	rows, err := queryLifecycleAutobans(context.Background(), db, clock.now.Unix())
	if err != nil || len(rows) != 4 {
		t.Fatalf("visible bans=%+v err=%v", rows, err)
	}
	for _, row := range rows {
		if row.Lifecycle == nil || !row.Lifecycle.Disabled || !host.accounts[row.AuthIndex].FileDisabled || row.BannedAtText == "" {
			t.Fatalf("ban must reflect confirmed physical disable: %+v", row)
		}
		if row.LastStatusCode == 429 && (row.ResetAtText == "" || row.SecondsRemaining <= 0) {
			t.Fatalf("429 recovery missing: %+v", row)
		}
		if row.LastStatusCode != 429 && (row.ResetAt != 0 || row.SecondsRemaining != -1) {
			t.Fatalf("permanent auth failure acquired recovery timer: %+v", row)
		}
	}
	clock.now = clock.now.Add(time.Minute)
	c.reconcile(context.Background())
	rows, err = queryLifecycleAutobans(context.Background(), db, clock.now.Unix())
	if err != nil || len(rows) != 3 || host.accounts["c"].Entry.Disabled || host.accounts["c"].FileDisabled {
		t.Fatalf("rate recovery did not remove row: %+v %v", rows, err)
	}
	s := lifecycleStateForTest(t, c, "c")
	if s.State != authHealthy || s.BlockedState != "" {
		t.Fatalf("recovered auth retains active failure: %+v", s)
	}
	clock.now = clock.now.Add(time.Hour)
	c.reconcile(context.Background())
	rows, _ = queryLifecycleAutobans(context.Background(), db, clock.now.Unix())
	if len(rows) != 2 || !host.accounts["a"].FileDisabled || !host.accounts["b"].FileDisabled || host.accounts["d"].FileDisabled {
		t.Fatalf("quota and permanent recovery mismatch: %+v", rows)
	}
}

func TestNativePendingAndManualDisableAreSeparateFromAutomaticBans(t *testing.T) {
	c, host, clock := nativeTestController(t)
	host.ready = false
	lifecycleFailure(t, c, "a", 401, `{}`)
	db, _, _ := c.store.open(context.Background())
	bans, err := queryLifecycleAutobans(context.Background(), db, clock.Now().Unix())
	pending, pendingErr := queryLifecycleRows(context.Background(), db, clock.Now().Unix(), true)
	if err != nil || pendingErr != nil || len(bans) != 0 || len(pending) != 1 || pending[0].Lifecycle.Disabled {
		t.Fatalf("bans=%+v pending=%+v", bans, pending)
	}
	host.ready = true
	state := lifecycleStateForTest(t, c, "b")
	if _, err = c.action(context.Background(), lifecycleActionRequest{AuthIndex: "b", Version: state.Version, Action: "disable"}); err != nil {
		t.Fatal(err)
	}
	bans, err = queryLifecycleAutobans(context.Background(), db, clock.Now().Unix())
	if err != nil || len(bans) != 0 {
		t.Fatalf("manual disable included in automatic list: %+v %v", bans, err)
	}
}

func TestNativeSuccessfulQuotaExhaustionIsolatesAuth(t *testing.T) {
	c, host, _ := nativeTestController(t)
	db, _, _ := c.store.open(context.Background())
	rec := usageRecord{Provider: "codex", AuthID: "a.json", AuthIndex: "a", RequestedAt: c.clock.Now(), ResponseHeaders: map[string][]string{"x-codex-primary-used-percent": {"100"}, "x-codex-primary-reset-at": {"2000000300"}}}
	if err := observeAuthLifecycle(context.Background(), db, rec); err != nil {
		t.Fatal(err)
	}
	c.reconcile(context.Background())
	s := lifecycleStateForTest(t, c, "a")
	if !host.accounts["a"].FileDisabled || s.State != authQuotaCooldown || s.RecoverAt != 2000000300 {
		t.Fatalf("successful exhausted quota not isolated: %+v", s)
	}
}

func TestNativePermanentFailureSupersedesCooldown(t *testing.T) {
	for _, status := range []int{401, 402} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			c, host, clock := nativeTestController(t)
			lifecycleFailure(t, c, "a", 429, `{"error":{"type":"rate_limit_exceeded","retry_after":30}}`)
			lifecycleFailure(t, c, "a", status, `{}`)
			clock.now = clock.now.Add(time.Minute)
			c.reconcile(context.Background())
			s := lifecycleStateForTest(t, c, "a")
			if !host.accounts["a"].FileDisabled || s.RecoverAt != 0 || s.LastHTTPStatus != status {
				t.Fatalf("permanent failure automatically recovered: %+v", s)
			}
		})
	}
}

func TestNativeQuotaHeadersOverrideShortRateClassification(t *testing.T) {
	now := time.Unix(2000000000, 0)
	d := classifyAuthFailure(usageRecord{Failed: true, Failure: usageFailure{StatusCode: 429, Body: `{"error":{"type":"rate_limit_exceeded","retry_after":30}}`}, ResponseHeaders: map[string][]string{"x-codex-primary-used-percent": {"100"}, "x-codex-primary-reset-at": {"2000003600"}}}, now)
	if d.State != authQuotaCooldown || !d.Disable || d.RecoverAt != now.Add(time.Hour).Unix() {
		t.Fatalf("exhausted window lost to short rate limit: %+v", d)
	}
}
