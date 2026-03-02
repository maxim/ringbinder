package db

import (
	"fmt"
	"testing"
	"time"
)

func TestInsertDocument_WithContentID(t *testing.T) {
	t.Parallel()

	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	now := time.Now().UTC()
	contentID, err := database.InsertContent("content-a", 3)
	if err != nil {
		t.Fatalf("InsertContent() error = %v", err)
	}

	docID, err := database.InsertDocument("/docs/a.pdf", contentID, now, now)
	if err != nil {
		t.Fatalf("InsertDocument() error = %v", err)
	}
	if docID <= 0 {
		t.Fatalf("InsertDocument() id = %d, want > 0", docID)
	}

	doc, err := database.GetDocumentByPath("/docs/a.pdf")
	if err != nil {
		t.Fatalf("GetDocumentByPath() error = %v", err)
	}
	if doc == nil {
		t.Fatalf("GetDocumentByPath() = nil, want document")
	}
	if doc.ContentID != contentID {
		t.Fatalf("document content_id = %d, want %d", doc.ContentID, contentID)
	}
	if doc.Checksum != "content-a" {
		t.Fatalf("document checksum = %q, want %q", doc.Checksum, "content-a")
	}
	if doc.PageCount != 3 {
		t.Fatalf("document page_count = %d, want 3", doc.PageCount)
	}
}

func TestUpdateDocument_ChangesContentID(t *testing.T) {
	t.Parallel()

	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	now := time.Now().UTC()
	contentA, err := database.InsertContent("content-a", 1)
	if err != nil {
		t.Fatalf("InsertContent(content-a) error = %v", err)
	}
	contentB, err := database.InsertContent("content-b", 2)
	if err != nil {
		t.Fatalf("InsertContent(content-b) error = %v", err)
	}

	docID, err := database.InsertDocument("/docs/a.pdf", contentA, now, now)
	if err != nil {
		t.Fatalf("InsertDocument() error = %v", err)
	}

	updatedTime := now.Add(5 * time.Minute)
	if err := database.UpdateDocument(docID, contentB, updatedTime); err != nil {
		t.Fatalf("UpdateDocument() error = %v", err)
	}

	doc, err := database.GetDocumentByPath("/docs/a.pdf")
	if err != nil {
		t.Fatalf("GetDocumentByPath() error = %v", err)
	}
	if doc == nil {
		t.Fatalf("GetDocumentByPath() = nil, want document")
	}
	if doc.ContentID != contentB {
		t.Fatalf("document content_id = %d, want %d", doc.ContentID, contentB)
	}
	if doc.Checksum != "content-b" {
		t.Fatalf("document checksum = %q, want %q", doc.Checksum, "content-b")
	}
	if doc.PageCount != 2 {
		t.Fatalf("document page_count = %d, want 2", doc.PageCount)
	}
	if !doc.ModifiedAt.Equal(updatedTime) {
		t.Fatalf("document modified_at = %s, want %s", doc.ModifiedAt.Format(time.RFC3339Nano), updatedTime.Format(time.RFC3339Nano))
	}
}

func TestAllDocuments_IncludesNonPending(t *testing.T) {
	t.Parallel()

	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	now := time.Now().UTC()
	contentA, err := database.InsertContent("aaa", 3)
	if err != nil {
		t.Fatalf("InsertContent(aaa) error = %v", err)
	}
	contentB, err := database.InsertContent("bbb", 2)
	if err != nil {
		t.Fatalf("InsertContent(bbb) error = %v", err)
	}
	if err := database.MarkContentOCRDone(contentB); err != nil {
		t.Fatalf("MarkContentOCRDone() error = %v", err)
	}

	id1, err := database.InsertDocument("/docs/a.pdf", contentA, now, now)
	if err != nil {
		t.Fatalf("InsertDocument(a) error = %v", err)
	}
	id2, err := database.InsertDocument("/docs/b.pdf", contentB, now, now)
	if err != nil {
		t.Fatalf("InsertDocument(b) error = %v", err)
	}

	contentDeleted, err := database.InsertContent("ddd", 1)
	if err != nil {
		t.Fatalf("InsertContent(ddd) error = %v", err)
	}
	if _, err := database.InsertDocument("/docs/deleted.pdf", contentDeleted, now, now); err != nil {
		t.Fatalf("InsertDocument(deleted) error = %v", err)
	}

	seenPaths := map[string]bool{"/docs/a.pdf": true, "/docs/b.pdf": true}
	if _, err := database.SoftDeleteMissing(seenPaths, []string{"/docs"}); err != nil {
		t.Fatalf("SoftDeleteMissing() error = %v", err)
	}

	pending, err := database.PendingDocuments()
	if err != nil {
		t.Fatalf("PendingDocuments() error = %v", err)
	}
	if len(pending) != 1 || pending[0].ID != id1 {
		t.Fatalf("PendingDocuments() returned %d docs, want 1 (id=%d)", len(pending), id1)
	}

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

func TestListDocuments(t *testing.T) {
	t.Parallel()

	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	makeTime := func(day int) time.Time {
		t.Helper()
		return time.Date(2025, time.January, day, 0, 0, 0, 0, time.UTC)
	}

	insertDocument := func(path string, createdAt time.Time, pageCount int) int64 {
		t.Helper()
		contentID, err := database.InsertContent(fmt.Sprintf("checksum-%s", path), pageCount)
		if err != nil {
			t.Fatalf("InsertContent(%q) error = %v", path, err)
		}
		docID, err := database.InsertDocument(path, contentID, createdAt, createdAt)
		if err != nil {
			t.Fatalf("InsertDocument(%q) error = %v", path, err)
		}
		return docID
	}

	firstID := insertDocument("/docs/first.pdf", makeTime(1), 1)
	secondID := insertDocument("/docs/second.pdf", makeTime(2), 2)
	thirdID := insertDocument("/docs/third.pdf", makeTime(3), 3)
	deletedID := insertDocument("/docs/deleted.pdf", makeTime(4), 4)

	if _, err := database.Exec("UPDATE documents SET deleted = 1 WHERE id = ?", deletedID); err != nil {
		t.Fatalf("mark deleted error = %v", err)
	}

	assertPaths := func(docs []Document, want []string) {
		t.Helper()
		if len(docs) != len(want) {
			t.Fatalf("ListDocuments() returned %d docs, want %d", len(docs), len(want))
		}
		for i, doc := range docs {
			if doc.Path != want[i] {
				t.Fatalf("ListDocuments() doc[%d].Path = %q, want %q", i, doc.Path, want[i])
			}
			if doc.ID == deletedID {
				t.Fatalf("ListDocuments() included deleted document id %d", deletedID)
			}
		}
	}

	all, err := database.ListDocuments(ListOptions{})
	if err != nil {
		t.Fatalf("ListDocuments(all) error = %v", err)
	}
	assertPaths(all, []string{"/docs/third.pdf", "/docs/second.pdf", "/docs/first.pdf"})

	after := makeTime(2)
	afterDocs, err := database.ListDocuments(ListOptions{After: &after})
	if err != nil {
		t.Fatalf("ListDocuments(after) error = %v", err)
	}
	assertPaths(afterDocs, []string{"/docs/third.pdf", "/docs/second.pdf"})

	before := makeTime(3)
	beforeDocs, err := database.ListDocuments(ListOptions{Before: &before})
	if err != nil {
		t.Fatalf("ListDocuments(before) error = %v", err)
	}
	assertPaths(beforeDocs, []string{"/docs/second.pdf", "/docs/first.pdf"})

	rangeStart := makeTime(2)
	rangeEnd := makeTime(3)
	rangeDocs, err := database.ListDocuments(ListOptions{
		After:  &rangeStart,
		Before: &rangeEnd,
	})
	if err != nil {
		t.Fatalf("ListDocuments(range) error = %v", err)
	}
	assertPaths(rangeDocs, []string{"/docs/second.pdf"})

	pagedDocs, err := database.ListDocuments(ListOptions{
		Limit:  1,
		Offset: 1,
	})
	if err != nil {
		t.Fatalf("ListDocuments(limit/offset) error = %v", err)
	}
	assertPaths(pagedDocs, []string{"/docs/second.pdf"})

	if all[0].ID != thirdID || all[1].ID != secondID || all[2].ID != firstID {
		t.Fatalf("ListDocuments(all) returned ids [%d %d %d], want [%d %d %d]", all[0].ID, all[1].ID, all[2].ID, thirdID, secondID, firstID)
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
	contentPending, err := database.InsertContent("pending", 3)
	if err != nil {
		t.Fatalf("InsertContent(pending) error = %v", err)
	}
	contentDone, err := database.InsertContent("done", 2)
	if err != nil {
		t.Fatalf("InsertContent(done) error = %v", err)
	}
	if err := database.MarkContentOCRDone(contentDone); err != nil {
		t.Fatalf("MarkContentOCRDone() error = %v", err)
	}

	if _, err := database.InsertDocument("/docs/a.pdf", contentPending, now, now); err != nil {
		t.Fatalf("InsertDocument(a) error = %v", err)
	}
	if _, err := database.InsertDocument("/docs/a-copy.pdf", contentPending, now, now); err != nil {
		t.Fatalf("InsertDocument(a-copy) error = %v", err)
	}
	if _, err := database.InsertDocument("/docs/b.pdf", contentDone, now, now); err != nil {
		t.Fatalf("InsertDocument(b) error = %v", err)
	}

	contentDeleted, err := database.InsertContent("deleted", 10)
	if err != nil {
		t.Fatalf("InsertContent(deleted) error = %v", err)
	}
	if _, err := database.InsertDocument("/docs/deleted.pdf", contentDeleted, now, now); err != nil {
		t.Fatalf("InsertDocument(deleted) error = %v", err)
	}
	seenPaths := map[string]bool{
		"/docs/a.pdf":      true,
		"/docs/a-copy.pdf": true,
		"/docs/b.pdf":      true,
	}
	if _, err := database.SoftDeleteMissing(seenPaths, []string{"/docs"}); err != nil {
		t.Fatalf("SoftDeleteMissing() error = %v", err)
	}

	pendingContents, pendingPages, err := database.PendingStats()
	if err != nil {
		t.Fatalf("PendingStats() error = %v", err)
	}
	if pendingContents != 1 || pendingPages != 3 {
		t.Fatalf("PendingStats() = (%d, %d), want (1, 3)", pendingContents, pendingPages)
	}

	allDocs, allPages, err := database.AllStats()
	if err != nil {
		t.Fatalf("AllStats() error = %v", err)
	}
	if allDocs != 3 || allPages != 8 {
		t.Fatalf("AllStats() = (%d, %d), want (3, 8)", allDocs, allPages)
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
		contentID, err := database.InsertContent("checksum-"+path, 1)
		if err != nil {
			t.Fatalf("InsertContent(%q) error = %v", path, err)
		}
		if _, err := database.InsertDocument(path, contentID, now, now); err != nil {
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

func TestResetAllDocuments(t *testing.T) {
	t.Parallel()

	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	now := time.Now().UTC()

	contentWithPages, err := database.InsertContent("aaa", 2)
	if err != nil {
		t.Fatalf("InsertContent(with-pages) error = %v", err)
	}
	if _, err := database.InsertDocument("/docs/with-pages.pdf", contentWithPages, now, now); err != nil {
		t.Fatalf("InsertDocument(with-pages) error = %v", err)
	}
	if err := database.ReplaceContentPages(contentWithPages, []PageInput{
		{PageIndex: 0, Markdown: "page 1"},
		{PageIndex: 1, Markdown: "page 2"},
	}); err != nil {
		t.Fatalf("ReplaceContentPages() error = %v", err)
	}

	contentDeleted, err := database.InsertContent("bbb", 1)
	if err != nil {
		t.Fatalf("InsertContent(soft-deleted) error = %v", err)
	}
	if _, err := database.InsertDocument("/docs/soft-deleted.pdf", contentDeleted, now, now); err != nil {
		t.Fatalf("InsertDocument(soft-deleted) error = %v", err)
	}

	contentPending, err := database.InsertContent("ccc", 3)
	if err != nil {
		t.Fatalf("InsertContent(pending) error = %v", err)
	}
	if _, err := database.InsertDocument("/docs/pending.pdf", contentPending, now, now); err != nil {
		t.Fatalf("InsertDocument(pending) error = %v", err)
	}

	seenPaths := map[string]bool{
		"/docs/with-pages.pdf": true,
		"/docs/pending.pdf":    true,
	}
	if _, err := database.SoftDeleteMissing(seenPaths, []string{"/docs"}); err != nil {
		t.Fatalf("SoftDeleteMissing() error = %v", err)
	}

	resetCount, err := database.ResetAllDocuments()
	if err != nil {
		t.Fatalf("ResetAllDocuments() error = %v", err)
	}
	if resetCount != 3 {
		t.Fatalf("ResetAllDocuments() count = %d, want 3", resetCount)
	}

	docs, err := database.AllDocuments()
	if err != nil {
		t.Fatalf("AllDocuments() error = %v", err)
	}
	if len(docs) != 0 {
		t.Fatalf("AllDocuments() returned %d docs, want 0", len(docs))
	}

	var pageCount int
	if err := database.QueryRow("SELECT COUNT(*) FROM pages").Scan(&pageCount); err != nil {
		t.Fatalf("count pages error = %v", err)
	}
	if pageCount != 0 {
		t.Fatalf("pages row count = %d, want 0", pageCount)
	}

	var contentCount int
	if err := database.QueryRow("SELECT COUNT(*) FROM contents").Scan(&contentCount); err != nil {
		t.Fatalf("count contents error = %v", err)
	}
	if contentCount != 0 {
		t.Fatalf("contents row count = %d, want 0", contentCount)
	}
}
