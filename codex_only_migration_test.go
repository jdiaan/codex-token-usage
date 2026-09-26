package main

import (
	"context"
	"testing"
)

func TestCodexOnlyMigrationRetainsOAuthAndIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{
		`DELETE FROM store_state WHERE key='codex_only_migration_v1'`,
		`INSERT INTO usage_events(requested_at,provider,auth_type,auth_id) VALUES(1,'codex','oauth','oauth-a'),(2,'codex','apikey','codex:apikey:a'),(3,'xai','oauth','xai-a'),(4,'openai','oauth','other-a')`,
		`INSERT INTO summary_cache(cache_key) VALUES('old')`,
		`CREATE TABLE xai_account_states(id TEXT)`,
		`INSERT INTO xai_account_states(id) VALUES('xai-a')`,
	}
	for _, q := range statements {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrateCodexOnly(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM usage_events WHERE auth_id='oauth-a'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("OAuth usage: %d %v", count, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM usage_events`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("usage: %d %v", count, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM summary_cache`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("cache: %d %v", count, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='xai_account_states'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("xAI table: %d %v", count, err)
	}
	if _, err := db.Exec(`INSERT INTO usage_events(requested_at,provider,auth_type,auth_id) VALUES(5,'codex','oauth','oauth-b')`); err != nil {
		t.Fatal(err)
	}
	if err := migrateCodexOnly(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM usage_events`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("second run: %d %v", count, err)
	}
}

func TestCodexOnlyMigrationRollsBackOnFailure(t *testing.T) {
	s := newTestStore(t)
	db, _, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`DELETE FROM store_state WHERE key='codex_only_migration_v1'`,
		`INSERT INTO usage_events(requested_at,provider,auth_id) VALUES(1,'xai','xai-a')`,
		`INSERT INTO summary_cache(cache_key) VALUES('old')`,
		`CREATE TRIGGER block_usage_delete BEFORE DELETE ON usage_events BEGIN SELECT RAISE(ABORT,'blocked'); END`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrateCodexOnly(context.Background(), db); err == nil {
		t.Fatal("expected migration failure")
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM usage_events`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("usage rollback: %d %v", count, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM summary_cache`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("cache rollback: %d %v", count, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM store_state WHERE key='codex_only_migration_v1'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("marker rollback: %d %v", count, err)
	}
}
