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
		case 3:
			if err := db.migrateV3ToV4(); err != nil {
				return fmt.Errorf("migrate 3->4: %w", err)
			}
			ver = 4
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

func (db *DB) migrateV3ToV4() (err error) {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	if _, err = tx.Exec(
		`CREATE TABLE contents (
		    id          INTEGER PRIMARY KEY AUTOINCREMENT,
		    checksum    TEXT    NOT NULL UNIQUE,
		    page_count  INTEGER NOT NULL DEFAULT 1,
		    ocr_pending INTEGER NOT NULL DEFAULT 1
		)`,
	); err != nil {
		return err
	}

	if _, err = tx.Exec(
		`INSERT INTO contents (checksum, page_count, ocr_pending)
		 SELECT checksum, MAX(page_count), MIN(ocr_pending)
		 FROM documents
		 GROUP BY checksum`,
	); err != nil {
		return err
	}

	if _, err = tx.Exec("ALTER TABLE documents ADD COLUMN content_id INTEGER"); err != nil {
		return err
	}

	if _, err = tx.Exec(
		`UPDATE documents
		 SET content_id = (
		   SELECT c.id
		   FROM contents c
		   WHERE c.checksum = documents.checksum
		 )`,
	); err != nil {
		return err
	}

	type migratedPage struct {
		contentID int64
		pageIndex int
		markdown  string
	}

	rows, err := tx.Query(
		`SELECT d.content_id, p.page_index, p.markdown
		 FROM pages p
		 JOIN documents d ON d.id = p.document_id
		 ORDER BY d.content_id, d.id, p.page_index`,
	)
	if err != nil {
		return err
	}

	var pages []migratedPage
	seen := make(map[string]bool)
	for rows.Next() {
		var p migratedPage
		if err = rows.Scan(&p.contentID, &p.pageIndex, &p.markdown); err != nil {
			_ = rows.Close()
			return err
		}

		key := fmt.Sprintf("%d:%d", p.contentID, p.pageIndex)
		if seen[key] {
			continue
		}
		seen[key] = true
		p.markdown = StripPageFrontmatter(p.markdown)
		pages = append(pages, p)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}

	if _, err = tx.Exec("DROP TRIGGER IF EXISTS pages_ai"); err != nil {
		return err
	}
	if _, err = tx.Exec("DROP TRIGGER IF EXISTS pages_ad"); err != nil {
		return err
	}
	if _, err = tx.Exec("DROP TRIGGER IF EXISTS pages_au"); err != nil {
		return err
	}
	if _, err = tx.Exec("DROP TABLE IF EXISTS pages_fts"); err != nil {
		return err
	}
	if _, err = tx.Exec("DROP TABLE pages"); err != nil {
		return err
	}

	if _, err = tx.Exec(
		`CREATE TABLE pages (
		    id          INTEGER PRIMARY KEY AUTOINCREMENT,
		    content_id  INTEGER NOT NULL REFERENCES contents(id) ON DELETE CASCADE,
		    page_index  INTEGER NOT NULL,
		    markdown    TEXT    NOT NULL DEFAULT '',
		    UNIQUE(content_id, page_index)
		)`,
	); err != nil {
		return err
	}

	for _, p := range pages {
		if _, err = tx.Exec(
			"INSERT INTO pages (content_id, page_index, markdown) VALUES (?, ?, ?)",
			p.contentID, p.pageIndex, p.markdown,
		); err != nil {
			return err
		}
	}

	if _, err = tx.Exec("ALTER TABLE documents RENAME TO documents_old"); err != nil {
		return err
	}

	if _, err = tx.Exec(
		`CREATE TABLE documents (
		    id          INTEGER PRIMARY KEY AUTOINCREMENT,
		    path        TEXT    NOT NULL UNIQUE,
		    content_id  INTEGER NOT NULL REFERENCES contents(id),
		    created_at  TEXT    NOT NULL,
		    modified_at TEXT    NOT NULL,
		    deleted     INTEGER NOT NULL DEFAULT 0
		)`,
	); err != nil {
		return err
	}

	if _, err = tx.Exec(
		`INSERT INTO documents (id, path, content_id, created_at, modified_at, deleted)
		 SELECT id, path, content_id, created_at, modified_at, deleted
		 FROM documents_old`,
	); err != nil {
		return err
	}

	if _, err = tx.Exec("DROP TABLE documents_old"); err != nil {
		return err
	}

	if _, err = tx.Exec(
		"CREATE INDEX idx_contents_ocr_pending ON contents(ocr_pending)",
	); err != nil {
		return err
	}
	if _, err = tx.Exec(
		"CREATE INDEX idx_documents_path ON documents(path)",
	); err != nil {
		return err
	}
	if _, err = tx.Exec(
		"CREATE INDEX idx_documents_content_id ON documents(content_id) WHERE deleted = 0",
	); err != nil {
		return err
	}

	if _, err = tx.Exec(
		`CREATE VIRTUAL TABLE pages_fts USING fts5(
		    markdown,
		    content='pages',
		    content_rowid='id'
		)`,
	); err != nil {
		return err
	}

	if _, err = tx.Exec(
		`CREATE TRIGGER pages_ai AFTER INSERT ON pages BEGIN
		    INSERT INTO pages_fts(rowid, markdown) VALUES (new.id, new.markdown);
		END`,
	); err != nil {
		return err
	}

	if _, err = tx.Exec(
		`CREATE TRIGGER pages_ad AFTER DELETE ON pages BEGIN
		    INSERT INTO pages_fts(pages_fts, rowid, markdown) VALUES('delete', old.id, old.markdown);
		END`,
	); err != nil {
		return err
	}

	if _, err = tx.Exec(
		`CREATE TRIGGER pages_au AFTER UPDATE ON pages BEGIN
		    INSERT INTO pages_fts(pages_fts, rowid, markdown) VALUES('delete', old.id, old.markdown);
		    INSERT INTO pages_fts(rowid, markdown) VALUES (new.id, new.markdown);
		END`,
	); err != nil {
		return err
	}

	if _, err = tx.Exec("INSERT INTO pages_fts(pages_fts) VALUES('rebuild')"); err != nil {
		return err
	}

	if err = tx.Commit(); err != nil {
		return err
	}
	return nil
}
