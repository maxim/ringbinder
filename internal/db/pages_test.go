package db

import (
	"fmt"
	"testing"
	"time"
)

func TestBuildFTSQuery(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		query string
		want  string
	}{
		{
			name:  "multiple words",
			query: "hello world",
			want:  `"hello" AND "world"`,
		},
		{
			name:  "single word",
			query: "single",
			want:  `"single"`,
		},
		{
			name:  "embedded quotes",
			query: `has "quotes"`,
			want:  `"has" AND """quotes"""`,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := buildFTSQuery(tt.query); got != tt.want {
				t.Fatalf("buildFTSQuery(%q) = %q, want %q", tt.query, got, tt.want)
			}
		})
	}
}

func TestSearch_MultiWordMatchesNonContiguous(t *testing.T) {
	t.Parallel()

	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	contentID, err := insertTestDocumentWithContent(database, "/docs/fox.pdf", "checksum", 1)
	if err != nil {
		t.Fatalf("insertTestDocumentWithContent() error = %v", err)
	}
	if err := database.UpsertPage(contentID, 0, "the quick brown fox jumps over the lazy dog"); err != nil {
		t.Fatalf("UpsertPage() error = %v", err)
	}

	results, err := database.Search("quick lazy")
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("Search() returned %d rows, want 1", len(results))
	}
	if results[0].Path != "/docs/fox.pdf" {
		t.Fatalf("result path = %q, want %q", results[0].Path, "/docs/fox.pdf")
	}
	if results[0].PageIndex != 0 {
		t.Fatalf("result page_index = %d, want 0", results[0].PageIndex)
	}
}

func TestSearch_ReturnsPageCount(t *testing.T) {
	t.Parallel()

	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	contentID, err := insertTestDocumentWithContent(database, "/docs/multi.pdf", "checksum", 7)
	if err != nil {
		t.Fatalf("insertTestDocumentWithContent() error = %v", err)
	}
	if err := database.UpsertPage(contentID, 3, "searchable content"); err != nil {
		t.Fatalf("UpsertPage() error = %v", err)
	}

	results, err := database.Search("searchable")
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("Search() returned %d rows, want 1", len(results))
	}
	if results[0].PageCount != 7 {
		t.Fatalf("result page_count = %d, want 7", results[0].PageCount)
	}
}

func TestSearch_MatchesFilename(t *testing.T) {
	t.Parallel()

	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	path := "/Users/max/Documents/report.pdf"
	contentID, err := insertTestDocumentWithContent(database, path, "checksum", 1)
	if err != nil {
		t.Fatalf("insertTestDocumentWithContent() error = %v", err)
	}
	if err := database.UpsertPage(contentID, 0, "page body"); err != nil {
		t.Fatalf("UpsertPage() error = %v", err)
	}

	results, err := database.Search("report.pdf")
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

func TestSearch_MatchesPath(t *testing.T) {
	t.Parallel()

	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	path := "/Users/max/Documents/invoices/report.pdf"
	contentID, err := insertTestDocumentWithContent(database, path, "checksum", 1)
	if err != nil {
		t.Fatalf("insertTestDocumentWithContent() error = %v", err)
	}
	if err := database.UpsertPage(contentID, 0, "page body"); err != nil {
		t.Fatalf("UpsertPage() error = %v", err)
	}

	results, err := database.Search("invoices")
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

func TestReplaceContentPages_Atomic(t *testing.T) {
	t.Parallel()

	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	contentID, err := insertTestDocumentWithContent(database, "/docs/atomic.pdf", "checksum", 5)
	if err != nil {
		t.Fatalf("insertTestDocumentWithContent() error = %v", err)
	}

	for i := 0; i < 5; i++ {
		if err := database.UpsertPage(contentID, i, "old-page"); err != nil {
			t.Fatalf("UpsertPage(old %d) error = %v", i, err)
		}
	}

	newPages := []PageInput{
		{PageIndex: 0, Markdown: "new-0"},
		{PageIndex: 1, Markdown: "new-1"},
		{PageIndex: 2, Markdown: "new-2"},
	}

	if err := database.ReplaceContentPages(contentID, newPages); err != nil {
		t.Fatalf("ReplaceContentPages() error = %v", err)
	}

	rows, err := database.Query("SELECT page_index, markdown FROM pages WHERE content_id = ? ORDER BY page_index", contentID)
	if err != nil {
		t.Fatalf("query pages error = %v", err)
	}
	defer rows.Close()

	var indices []int
	var markdowns []string
	for rows.Next() {
		var idx int
		var md string
		if err := rows.Scan(&idx, &md); err != nil {
			t.Fatalf("scan page row error = %v", err)
		}
		indices = append(indices, idx)
		markdowns = append(markdowns, md)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err() = %v", err)
	}

	if len(indices) != 3 {
		t.Fatalf("page count = %d, want 3", len(indices))
	}
	for i := 0; i < 3; i++ {
		if indices[i] != i {
			t.Fatalf("page index[%d] = %d, want %d", i, indices[i], i)
		}
		wantMarkdown := fmt.Sprintf("new-%d", i)
		if markdowns[i] != wantMarkdown {
			t.Fatalf("markdown[%d] = %q, want %q", i, markdowns[i], wantMarkdown)
		}
	}

	content, err := database.GetContentByChecksum("checksum")
	if err != nil {
		t.Fatalf("GetContentByChecksum() error = %v", err)
	}
	if content == nil {
		t.Fatalf("content not found")
	}
	if content.OCRPending {
		t.Fatalf("ocr_pending = true, want false")
	}
}

func insertTestDocumentWithContent(database *DB, path, checksum string, pageCount int) (int64, error) {
	now := time.Now().UTC()
	contentID, err := database.InsertContent(checksum, pageCount)
	if err != nil {
		return 0, err
	}
	if _, err := database.InsertDocument(path, contentID, now, now); err != nil {
		return 0, err
	}
	return contentID, nil
}
