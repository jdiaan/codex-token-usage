package main

import (
	"context"
	"database/sql"
	"net/http"
	"path/filepath"
	"testing"
)

func TestEnsureInvalidAuthColumnsMigratesAndIsIdempotent(t *testing.T) {
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`
CREATE TABLE invalid_auths (
  auth_id TEXT PRIMARY KEY,
  auth_index TEXT NOT NULL DEFAULT '',
  source TEXT NOT NULL DEFAULT '',
  provider TEXT NOT NULL DEFAULT '',
  reason TEXT NOT NULL DEFAULT '',
  invalidated_at INTEGER NOT NULL,
  active INTEGER NOT NULL DEFAULT 1,
  last_status_code INTEGER NOT NULL DEFAULT 401,
  auth_file TEXT NOT NULL DEFAULT '',
  auth_file_mtime INTEGER NOT NULL DEFAULT 0
);
INSERT INTO invalid_auths(auth_id,auth_index,source,provider,reason,invalidated_at,auth_file)
VALUES
  ('file-id','file-index','user@example.com','codex','401',1,'account.json'),
  ('runtime-id','runtime-index','memory','codex','401',1,''),
  ('old-id','old-index','old@example.com','codex','401',1,''),
  ('runtime-looking.json','runtime-looking.json','runtime@example.com','codex','401',1,'');`); err != nil {
		t.Fatal(err)
	}

	if err := ensureInvalidAuthColumns(context.Background(), db); err != nil {
		t.Fatalf("migration pass 1: %v", err)
	}
	if _, err := db.Exec(`
UPDATE invalid_auths SET auth_source_kind=' FILE ' WHERE auth_id='file-id';
UPDATE invalid_auths SET auth_source_kind='Runtime_Only' WHERE auth_id='runtime-id';
UPDATE invalid_auths SET auth_source_kind='unknown' WHERE auth_id='old-id';`); err != nil {
		t.Fatal(err)
	}
	for i := 2; i <= 3; i++ {
		if err := ensureInvalidAuthColumns(context.Background(), db); err != nil {
			t.Fatalf("migration pass %d: %v", i, err)
		}
	}

	want := map[string]string{
		"file-id":              authSourceKindFile,
		"runtime-id":           authSourceKindRuntimeOnly,
		"old-id":               authSourceKindLegacy,
		"runtime-looking.json": authSourceKindLegacy,
	}
	rows, err := db.Query(`SELECT auth_id, auth_source_kind FROM invalid_auths`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var authID, kind string
		if err := rows.Scan(&authID, &kind); err != nil {
			t.Fatal(err)
		}
		if kind != want[authID] {
			t.Fatalf("auth %q kind = %q, want %q", authID, kind, want[authID])
		}
		delete(want, authID)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(want) != 0 {
		t.Fatalf("missing migrated rows: %+v", want)
	}
	var indexes int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_invalid_auths_source_kind_active'`).Scan(&indexes); err != nil {
		t.Fatal(err)
	}
	if indexes != 1 {
		t.Fatalf("source-kind index count = %d, want 1", indexes)
	}
}

func TestClassifyAndFilterInvalidAuthRowsUsesExactHostInventory(t *testing.T) {
	inventory := []configuredAccount{
		{AuthID: "file-id", AuthIndex: "file-index", AuthFile: "file.json", AuthSourceKind: authSourceKindFile},
		{AuthID: "runtime-id", AuthIndex: "runtime-index", AuthSourceKind: authSourceKindRuntimeOnly},
	}
	rows := []invalidAuthRow{
		{AuthID: "file-id", AuthIndex: "file-index", LastStatusCode: http.StatusUnauthorized},
		{AuthID: "runtime-id", AuthIndex: "runtime-index", LastStatusCode: http.StatusUnauthorized},
		{AuthID: "hidden-runtime-id", AuthIndex: "hidden-runtime-index", AuthSourceKind: authSourceKindRuntimeOnly, LastStatusCode: http.StatusUnauthorized},
		{AuthID: "missing-id", AuthFile: "missing.json", LastStatusCode: http.StatusUnauthorized},
		{Source: "duplicate@example.com", LastStatusCode: http.StatusUnauthorized},
	}
	rows = classifyInvalidAuthRows(rows, inventory)
	if rows[0].AuthSourceKind != authSourceKindFile || rows[1].AuthSourceKind != authSourceKindRuntimeOnly {
		t.Fatalf("classified rows = %+v", rows)
	}
	if rows[2].AuthSourceKind != authSourceKindRuntimeOnly || rows[3].AuthSourceKind != authSourceKindLegacy || rows[4].AuthSourceKind != authSourceKindLegacy {
		t.Fatalf("inferred stale rows = %+v", rows)
	}
	filtered := filterMissingInvalidAuthRows(rows, inventory, true)
	if len(filtered) != 3 || filtered[0].AuthID != "file-id" || filtered[1].AuthID != "runtime-id" || filtered[2].AuthID != "hidden-runtime-id" {
		t.Fatalf("filtered rows = %+v, want exact file plus visible and hidden runtime records", filtered)
	}
}

func TestInvalidAuthSummaryFilters401And403Separately(t *testing.T) {
	rows := []invalidAuthRow{
		{AuthID: "unauthorized", LastStatusCode: http.StatusUnauthorized},
		{AuthID: "payment", LastStatusCode: http.StatusPaymentRequired},
		{AuthID: "forbidden", LastStatusCode: http.StatusForbidden},
		{AuthID: "unknown", LastStatusCode: 0},
	}
	unauthorized := filterUnauthorizedInvalidAuths(rows)
	if len(unauthorized) != 1 || unauthorized[0].AuthID != "unauthorized" {
		t.Fatalf("401 rows = %+v", unauthorized)
	}
	forbidden := filterForbiddenInvalidAuths(rows)
	if len(forbidden) != 1 || forbidden[0].AuthID != "forbidden" {
		t.Fatalf("403 rows = %+v", forbidden)
	}
	var account accountRow
	applyInvalidAuthToAccount(&account, forbidden[0])
	if !account.InvalidAuth || account.InvalidAuthStatusCode != http.StatusForbidden {
		t.Fatalf("403 account state = %+v", account)
	}
}
