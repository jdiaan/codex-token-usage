package main

import (
	"context"
	"database/sql"
)

const codexOnlyMigrationKey = "codex_only_migration_v1"

// migrateCodexOnly deliberately has no backup path. A transaction makes the
// one-time deletion all-or-nothing; deployment operators who need old data
// must back up their database before installing this version.
func migrateCodexOnly(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var marker string
	err = tx.QueryRowContext(ctx, `SELECT value FROM store_state WHERE key=?`, codexOnlyMigrationKey).Scan(&marker)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == sql.ErrNoRows {
		if _, err = tx.ExecContext(ctx, `DELETE FROM usage_events WHERE NOT `+codexOAuthUsageSQL); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM summary_cache`); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DROP TABLE IF EXISTS xai_account_states`); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DROP TABLE IF EXISTS account_protection_reservations`); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO store_state(key,value) VALUES(?,?)`, codexOnlyMigrationKey, "done"); err != nil {
			return err
		}
	}
	return tx.Commit()
}
