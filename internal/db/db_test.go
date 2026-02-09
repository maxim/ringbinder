package db

import (
	"strings"
	"testing"
	"time"
)

func TestMigrate_V2ToV3_BackfillsFrontmatter(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	seedDB, err := Open(dir)
	if err != nil {
		t.Fatalf("Open(seed) error = %v", err)
	}

	now := time.Now().UTC()
	path := "/Users/max/Documents/archive/report.pdf"
	docID, err := seedDB.InsertDocument(path, "checksum", now, now, 1)
	if err != nil {
		t.Fatalf("InsertDocument() error = %v", err)
	}
	if err := seedDB.UpsertPage(docID, 0, "legacy markdown body"); err != nil {
		t.Fatalf("UpsertPage() error = %v", err)
	}

	if _, err := seedDB.Exec("PRAGMA user_version = 2"); err != nil {
		t.Fatalf("set user_version error = %v", err)
	}
	if err := seedDB.Close(); err != nil {
		t.Fatalf("Close(seed) error = %v", err)
	}

	migratedDB, err := Open(dir)
	if err != nil {
		t.Fatalf("Open(migrated) error = %v", err)
	}
	t.Cleanup(func() { _ = migratedDB.Close() })

	var markdown string
	if err := migratedDB.QueryRow("SELECT markdown FROM pages WHERE document_id = ? AND page_index = 0", docID).Scan(&markdown); err != nil {
		t.Fatalf("query page markdown error = %v", err)
	}

	if !strings.HasPrefix(markdown, "---\nfile: report.pdf\n") {
		t.Fatalf("markdown prefix = %q, want front-matter", markdown)
	}
	if !strings.Contains(markdown, "\npage: 1\n") {
		t.Fatalf("markdown does not contain page: 1: %q", markdown)
	}
	if !strings.HasSuffix(markdown, "legacy markdown body") {
		t.Fatalf("markdown suffix = %q, want legacy body", markdown)
	}

	results, err := migratedDB.Search("report.pdf")
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("Search() returned %d rows, want 1", len(results))
	}
	if results[0].Path != path {
		t.Fatalf("result path = %q, want %q", results[0].Path, path)
	}
}
