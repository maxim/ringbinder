package db

import (
	"database/sql"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const releasedV2SchemaForMigrationTest = `
CREATE TABLE contents (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    checksum TEXT NOT NULL UNIQUE,
    page_count INTEGER NOT NULL DEFAULT 1,
    ocr_pending INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE documents (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    path TEXT NOT NULL UNIQUE,
    content_id INTEGER NOT NULL REFERENCES contents(id),
    created_at TEXT NOT NULL,
    modified_at TEXT NOT NULL,
    deleted INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_contents_ocr_pending ON contents(ocr_pending);
CREATE INDEX idx_documents_path ON documents(path);
CREATE INDEX idx_documents_content_id ON documents(content_id) WHERE deleted = 0;
CREATE TABLE pages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    content_id INTEGER NOT NULL REFERENCES contents(id) ON DELETE CASCADE,
    page_index INTEGER NOT NULL,
    markdown TEXT NOT NULL DEFAULT '',
    search_text TEXT NOT NULL DEFAULT '',
    UNIQUE(content_id, page_index)
);
CREATE VIRTUAL TABLE pages_fts USING fts5(
    search_text, content='pages', content_rowid='id'
);
CREATE VIRTUAL TABLE pages_fts_trigram USING fts5(
    search_text, tokenize='trigram', content='pages', content_rowid='id'
);
CREATE TRIGGER pages_ai AFTER INSERT ON pages BEGIN
    INSERT INTO pages_fts(rowid, search_text) VALUES (new.id, new.search_text);
    INSERT INTO pages_fts_trigram(rowid, search_text) VALUES (new.id, new.search_text);
END;
CREATE TRIGGER pages_ad AFTER DELETE ON pages BEGIN
    INSERT INTO pages_fts(pages_fts, rowid, search_text) VALUES('delete', old.id, old.search_text);
    INSERT INTO pages_fts_trigram(pages_fts_trigram, rowid, search_text) VALUES('delete', old.id, old.search_text);
END;
CREATE TRIGGER pages_au AFTER UPDATE ON pages BEGIN
    INSERT INTO pages_fts(pages_fts, rowid, search_text) VALUES('delete', old.id, old.search_text);
    INSERT INTO pages_fts(rowid, search_text) VALUES (new.id, new.search_text);
    INSERT INTO pages_fts_trigram(pages_fts_trigram, rowid, search_text) VALUES('delete', old.id, old.search_text);
    INSERT INTO pages_fts_trigram(rowid, search_text) VALUES (new.id, new.search_text);
END;
PRAGMA user_version = 2;
`

func TestFreshV5UsesModelColumn(t *testing.T) {
	if schemaVersion != 5 {
		t.Fatalf("schemaVersion = %d, want unreleased version 5 revised in place", schemaVersion)
	}
	database, err := Open(filepath.Join(t.TempDir(), "fresh-v5.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	assertPagesModelColumn(t, database)
}

func TestOpenMigratesReleasedV2ToV5(t *testing.T) {
	path := filepath.Join(t.TempDir(), "released-v2.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(releasedV2SchemaForMigrationTest); err != nil {
		t.Fatalf("create released v2 fixture: %v", err)
	}
	if _, err := raw.Exec(`
		INSERT INTO contents (id, checksum, page_count, ocr_pending)
		VALUES (1, 'pending', 2, 1),
		       (2, 'complete', 1, 0),
		       (3, 'stranded', 2, 0),
		       (4, 'stale-pending', 2, 1),
		       (5, 'out-of-range-padding', 2, 0);
		INSERT INTO pages (content_id, page_index, markdown, search_text)
		VALUES (1, 0, 'partial', 'partial'),
		       (2, 0, 'historical', 'historical'),
		       (4, 0, 'complete one', 'complete one'),
		       (4, 1, 'complete two', 'complete two'),
		       (5, 0, 'only in-range page', 'only in-range page'),
		       (5, 2, 'out of range', 'out of range');
		INSERT INTO documents (path, content_id, created_at, modified_at, deleted)
		VALUES ('/docs/stranded.pdf', 3, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', 0),
		       ('/docs/stale-pending.pdf', 4, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', 0),
		       ('/docs/out-of-range.pdf', 5, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', 0);
	`); err != nil {
		t.Fatalf("seed v2 fixture: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	database, err := Open(path)
	if err != nil {
		t.Fatalf("Open() migration error = %v", err)
	}
	defer database.Close()
	var version int
	if err := database.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("schema version = %d, want %d", version, schemaVersion)
	}
	assertPagesModelColumn(t, database)
	rows, err := database.Query(`SELECT id, ocr_pending FROM contents ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var pending []int
	for rows.Next() {
		var id, value int
		if err := rows.Scan(&id, &value); err != nil {
			t.Fatal(err)
		}
		pending = append(pending, value)
	}
	if got := pending; len(got) != 5 || got[0] != 1 || got[1] != 0 || got[2] != 1 || got[3] != 0 || got[4] != 1 {
		t.Fatalf("migrated pending values = %v, want [1 0 1 0 1]", got)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	pendingContents, err := database.PendingContents()
	if err != nil {
		t.Fatal(err)
	}
	if len(pendingContents) != 2 || pendingContents[0].ID != 3 || pendingContents[1].ID != 5 {
		t.Fatalf("pending migrated contents = %+v, want contents 3 and 5", pendingContents)
	}
	var modelsPresent int
	if err := database.QueryRow(`SELECT COUNT(model) FROM pages`).Scan(&modelsPresent); err != nil {
		t.Fatal(err)
	}
	if modelsPresent != 0 {
		t.Fatalf("historical non-null OCR models = %d, want 0", modelsPresent)
	}
	for _, object := range []struct{ kind, name string }{
		{kind: "index", name: "idx_contents_ocr_pending"},
		{kind: "index", name: "idx_documents_path"},
		{kind: "index", name: "idx_documents_content_id"},
		{kind: "trigger", name: "pages_ai"},
		{kind: "trigger", name: "pages_ad"},
		{kind: "trigger", name: "pages_au"},
	} {
		var count int
		if err := database.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type = ? AND name = ?`,
			object.kind, object.name,
		).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("%s %s count = %d, want 1", object.kind, object.name, count)
		}
	}
	for _, table := range []string{"gemini_batches", "gemini_batch_contents", "gemini_batch_requests", "gemini_batch_cleanup"} {
		var count int
		if err := database.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table,
		).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("table %s count = %d, want 1", table, count)
		}
	}
	var staging int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'gemini_batch_pages'`,
	).Scan(&staging); err != nil {
		t.Fatal(err)
	}
	if staging != 0 {
		t.Fatalf("obsolete gemini_batch_pages table exists")
	}
}

func assertPagesModelColumn(t *testing.T, database *DB) {
	t.Helper()
	var modelColumns int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('pages') WHERE name = 'model'`,
	).Scan(&modelColumns); err != nil {
		t.Fatal(err)
	}
	if modelColumns != 1 {
		t.Fatalf("pages model column count = %d, want 1", modelColumns)
	}
}

func TestOpenRejectsUnreleasedV3AndV4(t *testing.T) {
	for _, version := range []int{3, 4} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "wip.db")
			raw, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := raw.Exec("PRAGMA user_version = " + strconv.Itoa(version)); err != nil {
				t.Fatal(err)
			}
			_ = raw.Close()
			_, err = Open(path)
			if err == nil || !strings.Contains(err.Error(), "one-off export") {
				t.Fatalf("Open(v%d) error = %v, want one-off transfer guidance", version, err)
			}
		})
	}
}
