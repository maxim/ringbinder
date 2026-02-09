package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

type DB struct {
	*sql.DB
}

func Open(dir string) (*DB, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create config dir: %w", err)
	}

	dbPath := filepath.Join(dir, "ringbinder.db")
	sqlDB, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(wal)&_pragma=foreign_keys(1)")
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

func (db *DB) migrate() error {
	var ver int
	if err := db.QueryRow("PRAGMA user_version").Scan(&ver); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}

	if ver == 0 {
		// Fresh database — apply full schema
		if _, err := db.Exec(schemaSQL); err != nil {
			return fmt.Errorf("apply schema: %w", err)
		}
		if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
			return fmt.Errorf("set schema version: %w", err)
		}
		return nil
	}

	for ver < schemaVersion {
		switch ver {
		case 1:
			if _, err := db.Exec("ALTER TABLE pages DROP COLUMN annotations"); err != nil {
				return fmt.Errorf("migrate 1->2: %w", err)
			}
			ver = 2
		case 2:
			if err := db.migrateV2ToV3(); err != nil {
				return fmt.Errorf("migrate 2->3: %w", err)
			}
			ver = 3
		default:
			return fmt.Errorf("unsupported schema version: %d", ver)
		}

		if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", ver)); err != nil {
			return fmt.Errorf("set schema version %d: %w", ver, err)
		}
	}

	return nil
}

func (db *DB) migrateV2ToV3() (err error) {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	type pageRow struct {
		id        int64
		path      string
		pageIndex int
		markdown  string
	}

	rows, err := tx.Query(
		`SELECT p.id, d.path, p.page_index, p.markdown
		 FROM pages p
		 JOIN documents d ON d.id = p.document_id`,
	)
	if err != nil {
		return err
	}

	var allPages []pageRow
	for rows.Next() {
		var page pageRow
		if err := rows.Scan(&page.id, &page.path, &page.pageIndex, &page.markdown); err != nil {
			_ = rows.Close()
			return err
		}
		allPages = append(allPages, page)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	for _, page := range allPages {
		markdown := BuildPageMarkdown(page.path, page.pageIndex, page.markdown)
		if _, err := tx.Exec("UPDATE pages SET markdown = ? WHERE id = ?", markdown, page.id); err != nil {
			return err
		}
	}

	if err = tx.Commit(); err != nil {
		return err
	}
	return nil
}
