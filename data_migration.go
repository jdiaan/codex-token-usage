package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var pluginDataMigrationMu sync.Mutex

// The installer owns plugins/, while data/ survives replacement of a plugin.
// Keep the source intact. Publish only complete files, without replacing a
// destination created by another process. An interrupted attempt is retryable.
func migrateDefaultPluginData(ctx context.Context) error {
	if strings.TrimSpace(os.Getenv("CPA_TOKEN_USAGE_DIR")) != "" {
		return nil
	}
	if err := lockMutexWithContext(ctx, &pluginDataMigrationMu); err != nil {
		return err
	}
	defer pluginDataMigrationMu.Unlock()
	base, err := cliProxyDataDir()
	if err != nil {
		return err
	}
	oldDir := filepath.Join(base, "plugins", pluginID)
	newDir := filepath.Join(base, "data", pluginID)
	oldDB, newDB := filepath.Join(oldDir, "usage.db"), filepath.Join(newDir, "usage.db")
	exists, err := regularDataFile(newDB)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	oldExists, err := regularDataFile(oldDB)
	if err != nil {
		return err
	}
	var snapshot string
	if oldExists {
		if err = os.MkdirAll(newDir, 0755); err != nil {
			return err
		}
		snapshot, err = snapshotLegacyDatabase(ctx, oldDB, newDir)
		if err != nil {
			return fmt.Errorf("migrate plugin database (original retained at %s): %w", oldDB, err)
		}
		defer os.Remove(snapshot)
	}
	if strings.TrimSpace(os.Getenv("CPA_MODEL_PRICE_FILE")) == "" {
		for _, name := range []string{modelPriceCacheFileName, legacyModelPriceJSONFileName} {
			found, statErr := regularDataFile(filepath.Join(oldDir, name))
			if statErr != nil {
				return statErr
			}
			if !found {
				continue
			}
			if err = copyPluginDataFile(filepath.Join(oldDir, name), filepath.Join(newDir, modelPriceCacheFileName)); err != nil {
				return err
			}
			break
		}
	}
	if snapshot != "" {
		if err = publishPluginDataFile(snapshot, newDB); err != nil {
			return fmt.Errorf("publish migrated plugin database: %w", err)
		}
	}
	return nil
}

func regularDataFile(path string) (bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect plugin data %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("plugin data path is not a regular file: %s", path)
	}
	return true, nil
}

func readonlySQLiteDSN(path string) string {
	path = filepath.ToSlash(path)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	u := url.URL{Scheme: "file", Path: path}
	return u.String() + "?mode=ro&_busy_timeout=5000"
}

func snapshotLegacyDatabase(ctx context.Context, source, directory string) (result string, err error) {
	f, err := os.CreateTemp(directory, ".usage-migration-*.tmp")
	if err != nil {
		return "", err
	}
	name := f.Name()
	defer func() {
		if result == "" {
			os.Remove(name)
		}
	}()
	if err = f.Close(); err != nil {
		return "", err
	}
	db, err := sql.Open("sqlite3", readonlySQLiteDSN(source))
	if err != nil {
		return "", err
	}
	defer db.Close()
	// VACUUM INTO reads the committed SQLite snapshot, including live WAL pages.
	if _, err = db.ExecContext(ctx, `VACUUM INTO ?`, name); err != nil {
		return "", err
	}
	check, err := sql.Open("sqlite3", readonlySQLiteDSN(name))
	if err != nil {
		return "", err
	}
	problems, checkErr := sqliteIntegrityProblems(ctx, check, 0)
	closeErr := check.Close()
	if checkErr != nil {
		return "", checkErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	if len(problems) != 1 || problems[0] != "ok" {
		return "", fmt.Errorf("migrated database integrity check failed: %v", problems)
	}
	f, err = os.OpenFile(name, os.O_RDWR, 0600)
	if err != nil {
		return "", err
	}
	err = f.Sync()
	closeErr = f.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	return name, nil
}

func publishPluginDataFile(source, destination string) error {
	// A hard link publishes atomically and cannot overwrite existing data.
	err := os.Link(source, destination)
	if errors.Is(err, os.ErrExist) {
		_, err = regularDataFile(destination)
	}
	return err
}

func copyPluginDataFile(source, destination string) error {
	exists, err := regularDataFile(destination)
	if err != nil || exists {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.CreateTemp(filepath.Dir(destination), ".price-migration-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(out.Name())
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Sync()
	}
	closeErr := out.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return publishPluginDataFile(out.Name(), destination)
}
