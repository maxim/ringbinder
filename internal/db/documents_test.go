package db

import (
	"testing"
	"time"
)

func TestAllDocuments_IncludesNonPending(t *testing.T) {
	t.Parallel()

	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	now := time.Now().UTC()

	// Insert two documents: one pending, one already OCR'd.
	id1, err := database.InsertDocument("/docs/a.pdf", "aaa", now, now, 3)
	if err != nil {
		t.Fatalf("InsertDocument(a) error = %v", err)
	}
	id2, err := database.InsertDocument("/docs/b.pdf", "bbb", now, now, 2)
	if err != nil {
		t.Fatalf("InsertDocument(b) error = %v", err)
	}
	// Mark b as OCR-done.
	if err := database.MarkOCRDone(id2); err != nil {
		t.Fatalf("MarkOCRDone() error = %v", err)
	}

	// Insert a deleted document that should be excluded.
	if _, err := database.InsertDocument("/docs/deleted.pdf", "ddd", now, now, 1); err != nil {
		t.Fatalf("InsertDocument(deleted) error = %v", err)
	}
	seenPaths := map[string]bool{"/docs/a.pdf": true, "/docs/b.pdf": true}
	if _, err := database.SoftDeleteMissing(seenPaths, []string{"/docs"}); err != nil {
		t.Fatalf("SoftDeleteMissing() error = %v", err)
	}

	// PendingDocuments should return only a.
	pending, err := database.PendingDocuments()
	if err != nil {
		t.Fatalf("PendingDocuments() error = %v", err)
	}
	if len(pending) != 1 || pending[0].ID != id1 {
		t.Fatalf("PendingDocuments() returned %d docs, want 1 (id=%d)", len(pending), id1)
	}

	// AllDocuments should return both a and b (but not deleted).
	all, err := database.AllDocuments()
	if err != nil {
		t.Fatalf("AllDocuments() error = %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("AllDocuments() returned %d docs, want 2", len(all))
	}
	ids := map[int64]bool{all[0].ID: true, all[1].ID: true}
	if !ids[id1] || !ids[id2] {
		t.Fatalf("AllDocuments() returned ids %v, want %d and %d", ids, id1, id2)
	}
}

func TestAllStats_IncludesNonPending(t *testing.T) {
	t.Parallel()

	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	now := time.Now().UTC()

	// Insert: pending (3 pages), done (2 pages), deleted (10 pages).
	if _, err := database.InsertDocument("/docs/a.pdf", "aaa", now, now, 3); err != nil {
		t.Fatalf("InsertDocument(a) error = %v", err)
	}
	id2, err := database.InsertDocument("/docs/b.pdf", "bbb", now, now, 2)
	if err != nil {
		t.Fatalf("InsertDocument(b) error = %v", err)
	}
	if err := database.MarkOCRDone(id2); err != nil {
		t.Fatalf("MarkOCRDone() error = %v", err)
	}
	if _, err := database.InsertDocument("/docs/deleted.pdf", "ddd", now, now, 10); err != nil {
		t.Fatalf("InsertDocument(deleted) error = %v", err)
	}
	seenPaths := map[string]bool{"/docs/a.pdf": true, "/docs/b.pdf": true}
	if _, err := database.SoftDeleteMissing(seenPaths, []string{"/docs"}); err != nil {
		t.Fatalf("SoftDeleteMissing() error = %v", err)
	}

	// PendingStats: 1 doc, 3 pages.
	pendingDocs, pendingPages, err := database.PendingStats()
	if err != nil {
		t.Fatalf("PendingStats() error = %v", err)
	}
	if pendingDocs != 1 || pendingPages != 3 {
		t.Fatalf("PendingStats() = (%d, %d), want (1, 3)", pendingDocs, pendingPages)
	}

	// AllStats: 2 docs, 5 pages (excludes deleted).
	allDocs, allPages, err := database.AllStats()
	if err != nil {
		t.Fatalf("AllStats() error = %v", err)
	}
	if allDocs != 2 || allPages != 5 {
		t.Fatalf("AllStats() = (%d, %d), want (2, 5)", allDocs, allPages)
	}
}

func TestSoftDeleteMissing_OnlyDeletesWithinRoots(t *testing.T) {
	t.Parallel()

	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	now := time.Now().UTC()
	mustInsertDocument := func(path string) {
		t.Helper()
		if _, err := database.InsertDocument(path, "checksum-"+path, now, now, 1); err != nil {
			t.Fatalf("InsertDocument(%q) error = %v", path, err)
		}
	}

	mustInsertDocument("/root1/a.pdf")
	mustInsertDocument("/root1/b.pdf")
	mustInsertDocument("/root2/c.pdf")

	seenPaths := map[string]bool{"/root1/a.pdf": true}
	deletedCount, err := database.SoftDeleteMissing(seenPaths, []string{"/root1"})
	if err != nil {
		t.Fatalf("SoftDeleteMissing() error = %v", err)
	}
	if deletedCount != 1 {
		t.Fatalf("SoftDeleteMissing() deleted %d rows, want 1", deletedCount)
	}

	docRoot1B, err := database.GetDocumentByPath("/root1/b.pdf")
	if err != nil {
		t.Fatalf("GetDocumentByPath(/root1/b.pdf) error = %v", err)
	}
	if docRoot1B == nil || !docRoot1B.Deleted {
		t.Fatalf("/root1/b.pdf deleted = false, want true")
	}

	docRoot2C, err := database.GetDocumentByPath("/root2/c.pdf")
	if err != nil {
		t.Fatalf("GetDocumentByPath(/root2/c.pdf) error = %v", err)
	}
	if docRoot2C == nil || docRoot2C.Deleted {
		t.Fatalf("/root2/c.pdf deleted = true, want false")
	}
}
