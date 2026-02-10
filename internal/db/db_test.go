package db

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestMigrate_V2ToV4_StripsFrontmatter(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	legacyDB := openLegacyDB(t, dir)
	defer func() { _ = legacyDB.Close() }()

	if _, err := legacyDB.Exec(v3SchemaSQL); err != nil {
		t.Fatalf("apply v3 schema error = %v", err)
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := legacyDB.Exec(
		`INSERT INTO documents (id, path, checksum, created_at, modified_at, page_count, ocr_pending, deleted)
		 VALUES (1, ?, ?, ?, ?, 1, 1, 0)`,
		"/Users/max/Documents/archive/report.pdf", "checksum", now, now,
	); err != nil {
		t.Fatalf("insert legacy document error = %v", err)
	}
	if _, err := legacyDB.Exec(
		`INSERT INTO pages (document_id, page_index, markdown)
		 VALUES (1, 0, ?)`,
		"legacy markdown body",
	); err != nil {
		t.Fatalf("insert legacy page error = %v", err)
	}
	if _, err := legacyDB.Exec("PRAGMA user_version = 2"); err != nil {
		t.Fatalf("set user_version error = %v", err)
	}
	if err := legacyDB.Close(); err != nil {
		t.Fatalf("Close(legacy) error = %v", err)
	}

	migratedDB, err := Open(dir)
	if err != nil {
		t.Fatalf("Open(migrated) error = %v", err)
	}
	t.Cleanup(func() { _ = migratedDB.Close() })

	var markdown string
	if err := migratedDB.QueryRow(
		`SELECT p.markdown
		 FROM pages p
		 JOIN documents d ON d.content_id = p.content_id
		 WHERE d.path = ? AND p.page_index = 0`,
		"/Users/max/Documents/archive/report.pdf",
	).Scan(&markdown); err != nil {
		t.Fatalf("query page markdown error = %v", err)
	}
	if markdown != "legacy markdown body" {
		t.Fatalf("markdown = %q, want %q", markdown, "legacy markdown body")
	}

	results, err := migratedDB.Search("report.pdf")
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("Search() returned %d rows, want 1", len(results))
	}
	if results[0].Path != "/Users/max/Documents/archive/report.pdf" {
		t.Fatalf("result path = %q, want %q", results[0].Path, "/Users/max/Documents/archive/report.pdf")
	}
}

func TestMigrate_V3ToV4(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	legacyDB := openLegacyDB(t, dir)
	defer func() { _ = legacyDB.Close() }()

	if _, err := legacyDB.Exec(v3SchemaSQL); err != nil {
		t.Fatalf("apply v3 schema error = %v", err)
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	mustExec := func(query string, args ...any) {
		t.Helper()
		if _, err := legacyDB.Exec(query, args...); err != nil {
			t.Fatalf("exec failed: %v; query=%q", err, query)
		}
	}

	mustExec(
		`INSERT INTO documents (id, path, checksum, created_at, modified_at, page_count, ocr_pending, deleted)
		 VALUES (1, '/docs/a.pdf', 'dup', ?, ?, 2, 1, 0)`,
		now, now,
	)
	mustExec(
		`INSERT INTO documents (id, path, checksum, created_at, modified_at, page_count, ocr_pending, deleted)
		 VALUES (2, '/docs/a-copy.pdf', 'dup', ?, ?, 2, 0, 0)`,
		now, now,
	)
	mustExec(
		`INSERT INTO documents (id, path, checksum, created_at, modified_at, page_count, ocr_pending, deleted)
		 VALUES (3, '/docs/b.pdf', 'uniq', ?, ?, 1, 1, 0)`,
		now, now,
	)

	mustExec(
		`INSERT INTO pages (document_id, page_index, markdown)
		 VALUES (1, 0, ?)`,
		BuildPageMarkdown("/docs/a.pdf", 0, "duplicate body page 0"),
	)
	mustExec(
		`INSERT INTO pages (document_id, page_index, markdown)
		 VALUES (1, 1, ?)`,
		BuildPageMarkdown("/docs/a.pdf", 1, "duplicate body page 1"),
	)
	mustExec(
		`INSERT INTO pages (document_id, page_index, markdown)
		 VALUES (2, 0, ?)`,
		BuildPageMarkdown("/docs/a-copy.pdf", 0, "other copy should be ignored"),
	)
	mustExec(
		`INSERT INTO pages (document_id, page_index, markdown)
		 VALUES (3, 0, ?)`,
		BuildPageMarkdown("/docs/b.pdf", 0, "unique body"),
	)
	mustExec("PRAGMA user_version = 3")

	if err := legacyDB.Close(); err != nil {
		t.Fatalf("Close(legacy) error = %v", err)
	}

	migratedDB, err := Open(dir)
	if err != nil {
		t.Fatalf("Open(migrated) error = %v", err)
	}
	t.Cleanup(func() { _ = migratedDB.Close() })

	var contentsCount int
	if err := migratedDB.QueryRow("SELECT COUNT(*) FROM contents").Scan(&contentsCount); err != nil {
		t.Fatalf("count contents error = %v", err)
	}
	if contentsCount != 2 {
		t.Fatalf("contents count = %d, want 2", contentsCount)
	}

	var contentID1, contentID2, pending int
	if err := migratedDB.QueryRow(
		`SELECT d1.content_id, d2.content_id, c.ocr_pending
		 FROM documents d1
		 JOIN documents d2 ON d2.path = '/docs/a-copy.pdf'
		 JOIN contents c ON c.id = d1.content_id
		 WHERE d1.path = '/docs/a.pdf'`,
	).Scan(&contentID1, &contentID2, &pending); err != nil {
		t.Fatalf("query dedup content ids error = %v", err)
	}
	if contentID1 != contentID2 {
		t.Fatalf("dedup content ids differ: %d vs %d", contentID1, contentID2)
	}
	if pending != 0 {
		t.Fatalf("dedup content ocr_pending = %d, want 0", pending)
	}

	var sharedPages int
	if err := migratedDB.QueryRow("SELECT COUNT(*) FROM pages WHERE content_id = ?", contentID1).Scan(&sharedPages); err != nil {
		t.Fatalf("count shared pages error = %v", err)
	}
	if sharedPages != 2 {
		t.Fatalf("shared content page count = %d, want 2", sharedPages)
	}

	var markdown string
	if err := migratedDB.QueryRow(
		`SELECT markdown
		 FROM pages
		 WHERE content_id = ? AND page_index = 0`,
		contentID1,
	).Scan(&markdown); err != nil {
		t.Fatalf("query migrated markdown error = %v", err)
	}
	if strings.Contains(markdown, "file:") || strings.HasPrefix(markdown, "---\n") {
		t.Fatalf("markdown still has frontmatter: %q", markdown)
	}
	if markdown != "duplicate body page 0" {
		t.Fatalf("markdown = %q, want %q", markdown, "duplicate body page 0")
	}

	results, err := migratedDB.Search("duplicate")
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(results) < 2 {
		t.Fatalf("Search() returned %d rows, want at least 2", len(results))
	}

	paths := map[string]bool{}
	for _, result := range results {
		paths[result.Path] = true
	}
	if !paths["/docs/a.pdf"] || !paths["/docs/a-copy.pdf"] {
		t.Fatalf("search paths = %v, want both deduped document paths", paths)
	}
}

func openLegacyDB(t *testing.T, dir string) *sql.DB {
	t.Helper()

	sqlDB, err := sql.Open("sqlite", dir+"/ringbinder.db?_pragma=journal_mode(wal)&_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open legacy sqlite error = %v", err)
	}
	return sqlDB
}

const v3SchemaSQL = `
CREATE TABLE documents (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    path        TEXT    NOT NULL UNIQUE,
    checksum    TEXT    NOT NULL,
    created_at  TEXT    NOT NULL,
    modified_at TEXT    NOT NULL,
    page_count  INTEGER NOT NULL DEFAULT 1,
    ocr_pending INTEGER NOT NULL DEFAULT 1,
    deleted     INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_documents_ocr_pending
    ON documents(ocr_pending) WHERE deleted = 0;

CREATE INDEX idx_documents_path
    ON documents(path);

CREATE TABLE pages (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    document_id INTEGER NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    page_index  INTEGER NOT NULL,
    markdown    TEXT    NOT NULL DEFAULT '',
    UNIQUE(document_id, page_index)
);

CREATE VIRTUAL TABLE pages_fts USING fts5(
    markdown,
    content='pages',
    content_rowid='id'
);

CREATE TRIGGER pages_ai AFTER INSERT ON pages BEGIN
    INSERT INTO pages_fts(rowid, markdown) VALUES (new.id, new.markdown);
END;

CREATE TRIGGER pages_ad AFTER DELETE ON pages BEGIN
    INSERT INTO pages_fts(pages_fts, rowid, markdown) VALUES('delete', old.id, old.markdown);
END;

CREATE TRIGGER pages_au AFTER UPDATE ON pages BEGIN
    INSERT INTO pages_fts(pages_fts, rowid, markdown) VALUES('delete', old.id, old.markdown);
    INSERT INTO pages_fts(rowid, markdown) VALUES (new.id, new.markdown);
END;
`
