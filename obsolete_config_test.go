package main

import (
	"reflect"
	"testing"
)

func TestObsoleteConfigKeysAreNamesOnlyAndClear(t *testing.T) {
	setObsoleteConfigKeys([]byte("scheduling_mode: legacy\nFree 并发上限: 7\naccount_protection_enabled: true\n最大并发账号数: 2\n"))
	defer setObsoleteConfigKeys(nil)
	want := []string{"Free 并发上限", "account_protection_enabled", "scheduling_mode"}
	if got := obsoleteConfigKeys(); !reflect.DeepEqual(got, want) {
		t.Fatalf("keys = %q, want %q", got, want)
	}
	setObsoleteConfigKeys([]byte("最大并发账号数: 2\n"))
	if got := obsoleteConfigKeys(); len(got) != 0 {
		t.Fatalf("obsolete keys survived update: %q", got)
	}
}
