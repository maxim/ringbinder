package db

import (
	"testing"
	"time"
)

func TestOpen_FreshDB(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	db, err := Open(dir)
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
		"INSERT INTO pages (content_id, page_index, markdown) VALUES (?, ?, ?)",
		contentID, 0, "alpha beta gamma",
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
