package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func migrationPaths(t *testing.T) (string, string) {
	t.Helper()
	home := filepath.Join(t.TempDir(), "user with space #1")
	withUserHomeDir(t, home, nil)
	clearPathOverrides(t)
	oldDir := filepath.Join(home, ".cli-proxy-api", "plugins", pluginID)
	newDir := filepath.Join(home, ".cli-proxy-api", "data", pluginID)
	if err := os.MkdirAll(oldDir, 0755); err != nil {
		t.Fatal(err)
	}
	return oldDir, newDir
}

func migrationDatabase(t *testing.T, path string) *sql.DB {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	db, err := openSQLiteDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err = initializeSQLiteStore(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestDefaultDataMigrationPreservesWALAndLifecycleAcrossUpgrade(t *testing.T) {
	oldDir, newDir := migrationPaths(t)
	db := migrationDatabase(t, filepath.Join(oldDir, "usage.db"))
	if _, err := db.Exec(`PRAGMA wal_autocheckpoint=0`); err != nil {
		t.Fatal(err)
	}
	state := authLifecycleState{AuthIndex: "a", AuthID: "a.json", State: authQuotaCooldown, Disabled: true, DisabledByPlugin: true, RecoverAt: 2000000300, PendingAction: "enable", SyncStatus: "pending"}
	if err := saveLifecycleState(context.Background(), db, &state, time.Unix(2000000000, 0), "status_intent"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE upgrade_marker(value TEXT); INSERT INTO upgrade_marker VALUES('committed in WAL')`); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(oldDir, "usage.db-wal")); err != nil || info.Size() == 0 {
		t.Fatalf("no live WAL: %v", err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE upgrade_marker SET value='uncommitted'`); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldDir, modelPriceCacheFileName), []byte(`{"price":42}`), 0600); err != nil {
		t.Fatal(err)
	}
	// Simulate a stopped plugin whose database still has committed WAL pages.
	s := &store{}
	t.Cleanup(s.close)
	migrated, path, err := s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(newDir, "usage.db") {
		t.Fatalf("path=%s", path)
	}
	got, err := loadLifecycleState(context.Background(), migrated, "a")
	if err != nil || got != state {
		t.Fatalf("lost state: %+v %v", got, err)
	}
	var marker string
	if err = migrated.QueryRow(`SELECT value FROM upgrade_marker`).Scan(&marker); err != nil || marker != "committed in WAL" {
		t.Fatalf("lost WAL: %q %v", marker, err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err = migrated.Exec(`UPDATE upgrade_marker SET value='new data'`); err != nil {
		t.Fatal(err)
	}
	s.close()
	// A stale old database must never overwrite new activity on another upgrade.
	migrated, _, err = s.open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err = migrated.QueryRow(`SELECT value FROM upgrade_marker`).Scan(&marker); err != nil || marker != "new data" {
		t.Fatalf("upgrade overwritten: %q %v", marker, err)
	}
	for _, dir := range []string{oldDir, newDir} {
		if raw, err := os.ReadFile(filepath.Join(dir, modelPriceCacheFileName)); err != nil || string(raw) != `{"price":42}` {
			t.Fatalf("price missing: %s %v", dir, err)
		}
	}
	if _, err = os.Stat(filepath.Join(oldDir, "usage.db")); err != nil {
		t.Fatal("original database removed")
	}
}

func TestDataMigrationCacheFailureDoesNotPublishDatabase(t *testing.T) {
	oldDir, newDir := migrationPaths(t)
	migrationDatabase(t, filepath.Join(oldDir, "usage.db"))
	if err := os.WriteFile(filepath.Join(oldDir, modelPriceCacheFileName), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	obstruction := filepath.Join(newDir, modelPriceCacheFileName)
	if err := os.MkdirAll(obstruction, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := usageDBPath(); err == nil {
		t.Fatal("cache migration failure was hidden")
	}
	if _, err := os.Stat(filepath.Join(newDir, "usage.db")); !os.IsNotExist(err) {
		t.Fatal("database published before migration completed")
	}
	if err := os.Remove(obstruction); err != nil {
		t.Fatal(err)
	}
	if _, err := usageDBPath(); err != nil {
		t.Fatal(err)
	}
}

func TestDataMigrationFailureNeverCreatesEmptyDatabase(t *testing.T) {
	oldDir, newDir := migrationPaths(t)
	if err := os.WriteFile(filepath.Join(oldDir, "usage.db"), []byte("damaged original"), 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		s := &store{}
		if db, _, err := s.open(context.Background()); err == nil || db != nil {
			s.close()
			t.Fatal("silently opened new database")
		}
		if _, err := os.Stat(filepath.Join(newDir, "usage.db")); !os.IsNotExist(err) {
			t.Fatalf("published failed migration: %v", err)
		}
	}
	if raw, err := os.ReadFile(filepath.Join(oldDir, "usage.db")); err != nil || string(raw) != "damaged original" {
		t.Fatal("source modified")
	}
}

func TestDataMigrationInterruptedAttemptAndConcurrentRetry(t *testing.T) {
	oldDir, newDir := migrationPaths(t)
	db := migrationDatabase(t, filepath.Join(oldDir, "usage.db"))
	if _, err := db.Exec(`CREATE TABLE preserved(value INTEGER); INSERT INTO preserved VALUES(7)`); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(newDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(newDir, ".usage-migration-interrupted.tmp"), []byte("incomplete"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := migrateDefaultPluginData(ctx); err == nil {
		t.Fatal("canceled migration succeeded")
	}
	if _, err := os.Stat(filepath.Join(newDir, "usage.db")); !os.IsNotExist(err) {
		t.Fatal("canceled snapshot published")
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := migrateDefaultPluginData(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	newDB := migrationDatabase(t, filepath.Join(newDir, "usage.db"))
	var n int
	if err := newDB.QueryRow(`SELECT value FROM preserved`).Scan(&n); err != nil || n != 7 {
		t.Fatalf("retry lost source: %d %v", n, err)
	}
}

func TestDataMigrationHonorsExistingDatabaseAndExplicitDirectory(t *testing.T) {
	for _, override := range []bool{false, true} {
		t.Run(map[bool]string{false: "existing", true: "override"}[override], func(t *testing.T) {
			oldDir, newDir := migrationPaths(t)
			if err := os.WriteFile(filepath.Join(oldDir, "usage.db"), []byte("must not open"), 0600); err != nil {
				t.Fatal(err)
			}
			if override {
				newDir = filepath.Join(t.TempDir(), "configured")
				t.Setenv("CPA_TOKEN_USAGE_DIR", newDir)
			}
			db := migrationDatabase(t, filepath.Join(newDir, "usage.db"))
			if _, err := db.Exec(`CREATE TABLE existing(value INTEGER); INSERT INTO existing VALUES(9)`); err != nil {
				t.Fatal(err)
			}
			s := &store{}
			defer s.close()
			opened, path, err := s.open(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			var n int
			if err = opened.QueryRow(`SELECT value FROM existing`).Scan(&n); err != nil || n != 9 || path != filepath.Join(newDir, "usage.db") {
				t.Fatalf("existing changed %s %d %v", path, n, err)
			}
		})
	}
}

func TestPriceOnlyMigrationPreservesLegacyJSONAndOverride(t *testing.T) {
	for _, override := range []bool{false, true} {
		t.Run(map[bool]string{false: "default", true: "override"}[override], func(t *testing.T) {
			oldDir, newDir := migrationPaths(t)
			raw := []byte(`{"model":{"input_cost_per_token":0.001}}`)
			if err := os.WriteFile(filepath.Join(oldDir, legacyModelPriceJSONFileName), raw, 0600); err != nil {
				t.Fatal(err)
			}
			if override {
				t.Setenv("CPA_MODEL_PRICE_FILE", filepath.Join(t.TempDir(), "custom.cache"))
			}
			if err := migrateLegacyModelPriceFile(); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(newDir, modelPriceCacheFileName))
			if override {
				if !os.IsNotExist(err) {
					t.Fatal("ignored price override")
				}
			} else if err != nil || string(got) != string(raw) {
				t.Fatalf("price migration: %v", err)
			}
			if _, err = os.Stat(filepath.Join(oldDir, legacyModelPriceJSONFileName)); err != nil {
				t.Fatal("original price removed")
			}
		})
	}
}
