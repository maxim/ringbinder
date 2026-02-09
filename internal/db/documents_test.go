package db

import (
	"testing"
	"time"
)

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
