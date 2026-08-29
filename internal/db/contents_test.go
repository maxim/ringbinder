package db

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGetContentByChecksum(t *testing.T) {
	t.Parallel()

	database, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	got, err := database.GetContentByChecksum("missing")
	if err != nil {
		t.Fatalf("GetContentByChecksum(missing) error = %v", err)
	}
	if got != nil {
		t.Fatalf("GetContentByChecksum(missing) = %+v, want nil", got)
	}

	id, err := database.InsertContent("abc123", 7)
	if err != nil {
		t.Fatalf("InsertContent() error = %v", err)
	}

	got, err = database.GetContentByChecksum("abc123")
	if err != nil {
		t.Fatalf("GetContentByChecksum(existing) error = %v", err)
	}
	if got == nil {
		t.Fatalf("GetContentByChecksum(existing) = nil, want content")
	}
	if got.ID != id {
		t.Fatalf("content id = %d, want %d", got.ID, id)
	}
	if got.PageCount != 7 {
		t.Fatalf("content page_count = %d, want 7", got.PageCount)
	}
	if !got.OCRPending {
		t.Fatalf("content OCRPending = false, want true")
	}
}

func TestInsertContent(t *testing.T) {
	t.Parallel()

	database, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	id, err := database.InsertContent("dup-checksum", 1)
	if err != nil {
		t.Fatalf("InsertContent(first) error = %v", err)
	}
	if id <= 0 {
		t.Fatalf("InsertContent(first) id = %d, want > 0", id)
	}

	if _, err := database.InsertContent("dup-checksum", 1); err == nil {
		t.Fatalf("InsertContent(duplicate) error = nil, want UNIQUE constraint error")
	} else if !strings.Contains(strings.ToLower(err.Error()), "unique") {
		t.Fatalf("InsertContent(duplicate) error = %v, want UNIQUE constraint error", err)
	}
}

func TestContentCompletesOnlyWithExactPageCoverage(t *testing.T) {
	t.Parallel()

	database, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	id, err := database.InsertContent("ocr-done", 2)
	if err != nil {
		t.Fatalf("InsertContent() error = %v", err)
	}

	if err := database.UpsertPage(id, 0, "first"); err != nil {
		t.Fatalf("UpsertPage(0) error = %v", err)
	}
	content, err := database.GetContentByChecksum("ocr-done")
	if err != nil || content == nil || !content.OCRPending {
		t.Fatalf("partial content = %+v, %v; want pending", content, err)
	}
	if err := database.UpsertPage(id, 1, "second"); err != nil {
		t.Fatalf("UpsertPage(1) error = %v", err)
	}

	content, err = database.GetContentByChecksum("ocr-done")
	if err != nil {
		t.Fatalf("GetContentByChecksum() error = %v", err)
	}
	if content == nil {
		t.Fatalf("GetContentByChecksum() = nil, want content")
	}
	if content.OCRPending {
		t.Fatalf("content OCRPending = true, want false")
	}
}

func TestCleanupOrphanContents(t *testing.T) {
	t.Parallel()

	database, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	now := time.Now().UTC()

	liveContentID, err := database.InsertContent("live-checksum", 1)
	if err != nil {
		t.Fatalf("InsertContent(live) error = %v", err)
	}
	if _, err := database.InsertDocument("/docs/live.pdf", liveContentID, now, now); err != nil {
		t.Fatalf("InsertDocument(live) error = %v", err)
	}

	orphanContentID, err := database.InsertContent("orphan-checksum", 2)
	if err != nil {
		t.Fatalf("InsertContent(orphan) error = %v", err)
	}
	if _, err := database.InsertDocument("/docs/orphan.pdf", orphanContentID, now, now); err != nil {
		t.Fatalf("InsertDocument(orphan) error = %v", err)
	}
	if err := database.UpsertPage(orphanContentID, 0, "orphan page"); err != nil {
		t.Fatalf("UpsertPage(orphan) error = %v", err)
	}

	trueOrphanID, err := database.InsertContent("true-orphan", 1)
	if err != nil {
		t.Fatalf("InsertContent(true orphan) error = %v", err)
	}
	if err := database.UpsertPage(trueOrphanID, 0, "unreferenced page"); err != nil {
		t.Fatalf("UpsertPage(true orphan) error = %v", err)
	}

	seenPaths := map[string]bool{"/docs/live.pdf": true}
	if _, err := database.SoftDeleteMissing(seenPaths, []string{"/docs"}); err != nil {
		t.Fatalf("SoftDeleteMissing() error = %v", err)
	}

	deleted, err := database.CleanupOrphanContents()
	if err != nil {
		t.Fatalf("CleanupOrphanContents() error = %v", err)
	}
	if deleted != 1 {
		t.Fatalf("CleanupOrphanContents() deleted %d rows, want 1", deleted)
	}

	liveContent, err := database.GetContentByChecksum("live-checksum")
	if err != nil {
		t.Fatalf("GetContentByChecksum(live) error = %v", err)
	}
	if liveContent == nil {
		t.Fatalf("live content removed unexpectedly")
	}

	softDeletedContent, err := database.GetContentByChecksum("orphan-checksum")
	if err != nil {
		t.Fatalf("GetContentByChecksum(soft deleted) error = %v", err)
	}
	if softDeletedContent == nil {
		t.Fatalf("soft-deleted content was removed")
	}

	var trueOrphanPages int
	if err := database.QueryRow("SELECT COUNT(*) FROM pages WHERE content_id = ?", trueOrphanID).Scan(&trueOrphanPages); err != nil {
		t.Fatalf("count true orphan pages error = %v", err)
	}
	if trueOrphanPages != 0 {
		t.Fatalf("true orphan pages = %d, want 0", trueOrphanPages)
	}
}
