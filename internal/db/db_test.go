package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpen_FreshDB(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ringbinder.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	now := time.Now().UTC().Format(time.RFC3339Nano)

	res, err := db.Exec(
		"INSERT INTO contents (checksum, page_count, ocr_pending) VALUES (?, ?, ?)",
		"checksum-1", 1, 0,
	)
	if err != nil {
		t.Fatalf("insert contents error = %v", err)
	}
	contentID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("LastInsertId(contents) error = %v", err)
	}

	if _, err := db.Exec(
		`INSERT INTO documents (path, content_id, created_at, modified_at, deleted)
		 VALUES (?, ?, ?, ?, 0)`,
		"/docs/report.pdf", contentID, now, now,
	); err != nil {
		t.Fatalf("insert documents error = %v", err)
	}

	if _, err := db.Exec(
		"INSERT INTO pages (content_id, page_index, markdown, search_text) VALUES (?, ?, ?, ?)",
		contentID, 0, "alpha beta gamma", "alpha beta gamma",
	); err != nil {
		t.Fatalf("insert pages error = %v", err)
	}

	var userVersion int
	if err := db.QueryRow("PRAGMA user_version").Scan(&userVersion); err != nil {
		t.Fatalf("read user_version error = %v", err)
	}
	if userVersion != schemaVersion {
		t.Fatalf("user_version = %d, want %d", userVersion, schemaVersion)
	}

	results, err := db.Search("alpha")
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("Search() results = %d, want 1", len(results))
	}
	if results[0].Path != "/docs/report.pdf" {
		t.Fatalf("Search() path = %q, want %q", results[0].Path, "/docs/report.pdf")
	}
	if results[0].PageIndex != 0 {
		t.Fatalf("Search() page_index = %d, want 0", results[0].PageIndex)
	}
	if results[0].PageCount != 1 {
		t.Fatalf("Search() page_count = %d, want 1", results[0].PageCount)
	}
}

func TestOpen_MigratesV1ToV2(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	rawDBPath := filepath.Join(dir, "ringbinder.db")

	rawDB, err := sql.Open("sqlite", rawDBPath)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}

	if _, err := rawDB.Exec(v1SchemaForMigrationTest); err != nil {
		_ = rawDB.Close()
		t.Fatalf("apply v1 schema error = %v", err)
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := rawDB.Exec("INSERT INTO contents (checksum, page_count, ocr_pending) VALUES (?, ?, ?)", "checksum-v1", 1, 0)
	if err != nil {
		_ = rawDB.Close()
		t.Fatalf("insert contents error = %v", err)
	}
	contentID, err := res.LastInsertId()
	if err != nil {
		_ = rawDB.Close()
		t.Fatalf("LastInsertId(contents) error = %v", err)
	}

	if _, err := rawDB.Exec(
		`INSERT INTO documents (path, content_id, created_at, modified_at, deleted)
		 VALUES (?, ?, ?, ?, 0)`,
		"/docs/migrate.pdf", contentID, now, now,
	); err != nil {
		_ = rawDB.Close()
		t.Fatalf("insert documents error = %v", err)
	}

	if _, err := rawDB.Exec(
		"INSERT INTO pages (content_id, page_index, markdown) VALUES (?, ?, ?)",
		contentID, 0, "# Reimbursement Form",
	); err != nil {
		_ = rawDB.Close()
		t.Fatalf("insert pages error = %v", err)
	}

	if _, err := rawDB.Exec("PRAGMA user_version = 1"); err != nil {
		_ = rawDB.Close()
		t.Fatalf("set v1 user_version error = %v", err)
	}
	if err := rawDB.Close(); err != nil {
		t.Fatalf("rawDB.Close() error = %v", err)
	}

	database, err := Open(rawDBPath)
	if err != nil {
		t.Fatalf("Open() migrate error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	var userVersion int
	if err := database.QueryRow("PRAGMA user_version").Scan(&userVersion); err != nil {
		t.Fatalf("read user_version error = %v", err)
	}
	if userVersion != schemaVersion {
		t.Fatalf("user_version = %d, want %d", userVersion, schemaVersion)
	}

	results, err := database.SearchWithOptions(SearchOptions{
		Query:           "imburse",
		Mode:            "and",
		Limit:           10,
		Offset:          0,
		IncludePathLike: false,
		UseTrigram:      true,
	})
	if err != nil {
		t.Fatalf("SearchWithOptions() error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("SearchWithOptions() results = %d, want 1", len(results))
	}
	if results[0].Path != "/docs/migrate.pdf" {
		t.Fatalf("result path = %q, want %q", results[0].Path, "/docs/migrate.pdf")
	}
}

func TestOpen_CreatesParentDirectory(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "missing", "nested", "ringbinder.db")
	database, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("database file stat error = %v", err)
	}
}

func TestOpen_SupportsURICharactersInPath(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "dir with spaces", "db?name#1.sqlite")
	database, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("database file stat error = %v", err)
	}
}

func TestOpen_RelativePathIsResolvedFromWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	database, err := Open(filepath.Join("data", "ringbinder.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	wantPath := filepath.Join(dir, "data", "ringbinder.db")
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("database file stat error = %v", err)
	}
}

func TestOpen_RejectsDirectoryPath(t *testing.T) {
	t.Parallel()

	_, err := Open(t.TempDir())
	if err == nil {
		t.Fatalf("Open() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "database path is a directory") {
		t.Fatalf("Open() error = %q, want directory error", err.Error())
	}
}

func TestOpen_RejectsTrailingSeparator(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "new-db-dir") + string(filepath.Separator)
	_, err := Open(path)
	if err == nil {
		t.Fatalf("Open() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "database path is a directory") {
		t.Fatalf("Open() error = %q, want directory error", err.Error())
	}
}

var v1SchemaForMigrationTest = fmt.Sprintf(`
CREATE TABLE contents (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    checksum    TEXT    NOT NULL UNIQUE,
    page_count  INTEGER NOT NULL DEFAULT 1,
    ocr_pending INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE documents (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    path        TEXT    NOT NULL UNIQUE,
    content_id  INTEGER NOT NULL REFERENCES contents(id),
    created_at  TEXT    NOT NULL,
    modified_at TEXT    NOT NULL,
    deleted     INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_contents_ocr_pending
    ON contents(ocr_pending);

CREATE INDEX idx_documents_path
    ON documents(path);

CREATE INDEX idx_documents_content_id
    ON documents(content_id) WHERE deleted = 0;

CREATE TABLE pages (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    content_id  INTEGER NOT NULL REFERENCES contents(id) ON DELETE CASCADE,
    page_index  INTEGER NOT NULL,
    markdown    TEXT    NOT NULL DEFAULT '',
    UNIQUE(content_id, page_index)
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

PRAGMA user_version = %d;
`, 1)
