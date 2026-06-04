package db

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

type DB struct {
	*sql.DB
}

func Open(path string) (*DB, error) {
	dbPath, err := normalizeDatabasePath(path)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return nil, fmt.Errorf("create database dir: %w", err)
	}

	sqlDB, err := sql.Open("sqlite", databaseDSN(dbPath))
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	db := &DB{sqlDB}
	if err := db.migrate(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("migrate database: %w", err)
	}

	return db, nil
}

func normalizeDatabasePath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("database path is empty")
	}
	if strings.HasSuffix(path, string(filepath.Separator)) {
		return "", fmt.Errorf("database path is a directory; provide a SQLite file path")
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve database path: %w", err)
	}
	if info, err := os.Stat(absPath); err == nil {
		if info.IsDir() {
			return "", fmt.Errorf("database path is a directory; provide a SQLite file path")
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("stat database path: %w", err)
	}

	return absPath, nil
}

func databaseDSN(path string) string {
	query := url.Values{}
	query.Add("_pragma", "journal_mode(wal)")
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", "busy_timeout(5000)")

	dsn := url.URL{
		Scheme:   "file",
		Path:     path,
		RawQuery: query.Encode(),
	}
	return dsn.String()
}

func (db *DB) migrate() error {
	var ver int
	if err := db.QueryRow("PRAGMA user_version").Scan(&ver); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	switch ver {
	case 0:
		// Fresh database — apply full schema.
		if _, err := db.Exec(schemaSQL); err != nil {
			return fmt.Errorf("apply schema: %w", err)
		}
		if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
			return fmt.Errorf("set schema version: %w", err)
		}
		return nil
	case 1:
		if _, err := db.Exec(schemaV1ToV2SQL); err != nil {
			return fmt.Errorf("migrate schema v1->v2: %w", err)
		}
		// Recompute normalized search text using the same path as write-time updates,
		// so historical pages benefit from the new retrieval behavior immediately.
		if err := db.backfillPageSearchText(); err != nil {
			return fmt.Errorf("backfill search text: %w", err)
		}
		if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
			return fmt.Errorf("set schema version: %w", err)
		}
		return nil
	case schemaVersion:
		return nil
	default:
		return fmt.Errorf("unsupported schema version: %d", ver)
	}
}
