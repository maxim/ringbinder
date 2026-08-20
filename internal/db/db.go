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
		// Fresh database — apply full schema and version as one restart-safe unit.
		if err := db.applySchemaVersion(schemaSQL, schemaVersion); err != nil {
			return fmt.Errorf("apply schema: %w", err)
		}
		return nil
	case 1:
		if err := db.migrateV1ToCurrent(); err != nil {
			return fmt.Errorf("migrate schema v1->v%d: %w", schemaVersion, err)
		}
		return nil
	case 2:
		if err := db.applySchemaVersion(schemaV2ToV3SQL, schemaVersion); err != nil {
			return fmt.Errorf("migrate schema v2->v3: %w", err)
		}
		return nil
	case schemaVersion:
		return nil
	default:
		return fmt.Errorf("unsupported schema version: %d", ver)
	}
}

func (db *DB) migrateV1ToCurrent() (err error) {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if _, err = tx.Exec(schemaV1ToV2SQL); err != nil {
		return err
	}
	// Recompute normalized search text in the same transaction as both schema
	// steps so interruption cannot leave v2 columns stamped as schema v1.
	if err = backfillPageSearchTextTx(tx); err != nil {
		return err
	}
	if _, err = tx.Exec(schemaV2ToV3SQL); err != nil {
		return err
	}
	if _, err = tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) applySchemaVersion(schema string, version int) (err error) {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if _, err = tx.Exec(schema); err != nil {
		return err
	}
	if _, err = tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		return err
	}
	return tx.Commit()
}
