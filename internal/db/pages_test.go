package db

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestBuildFTSQuery(t *testing.T) {
	t.Parallel()

	if got := buildFTSQuery("hello world"); got != `"hello" AND "world"` {
		t.Fatalf("buildFTSQuery() = %q, want %q", got, `"hello" AND "world"`)
	}
}

func TestBuildFTSQueryTokens(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		tokens []string
		mode   string
		want   string
	}{
		{
			name:   "and mode",
			tokens: []string{"hello", "world"},
			mode:   "and",
			want:   `"hello" AND "world"`,
		},
		{
			name:   "or mode",
			tokens: []string{"hello", "world"},
			mode:   "or",
			want:   `"hello" OR "world"`,
		},
		{
			name:   "embedded quotes",
			tokens: []string{"has", `"quotes"`},
			mode:   "and",
			want:   `"has" AND """quotes"""`,
		},
		{
			name:   "empty tokens",
			tokens: nil,
			mode:   "and",
			want:   `""`,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := buildFTSQueryTokens(tt.tokens, tt.mode); got != tt.want {
				t.Fatalf("buildFTSQueryTokens(%v, %q) = %q, want %q", tt.tokens, tt.mode, got, tt.want)
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

func TestSearchWithOptions_ModeOR(t *testing.T) {
	t.Parallel()

	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	contentID, err := insertTestDocumentWithContent(database, "/docs/mode.pdf", "checksum", 1)
	if err != nil {
		t.Fatalf("insertTestDocumentWithContent() error = %v", err)
	}
	if err := database.UpsertPage(contentID, 0, "alpha term"); err != nil {
		t.Fatalf("UpsertPage() error = %v", err)
	}

	andResults, err := database.SearchWithOptions(SearchOptions{
		Query:           "alpha missing",
		Mode:            "and",
		Limit:           50,
		Offset:          0,
		IncludePathLike: true,
	})
	if err != nil {
		t.Fatalf("SearchWithOptions(and) error = %v", err)
	}
	if len(andResults) != 0 {
		t.Fatalf("SearchWithOptions(and) returned %d rows, want 0", len(andResults))
	}

	orResults, err := database.SearchWithOptions(SearchOptions{
		Query:           "alpha missing",
		Mode:            "or",
		Limit:           50,
		Offset:          0,
		IncludePathLike: true,
	})
	if err != nil {
		t.Fatalf("SearchWithOptions(or) error = %v", err)
	}
	if len(orResults) != 1 {
		t.Fatalf("SearchWithOptions(or) returned %d rows, want 1", len(orResults))
	}
	if orResults[0].Path != "/docs/mode.pdf" {
		t.Fatalf("result path = %q, want %q", orResults[0].Path, "/docs/mode.pdf")
	}
}

func TestSearchWithOptions_LimitOffset(t *testing.T) {
	t.Parallel()

	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	contentID, err := insertTestDocumentWithContent(database, "/docs/pagination.pdf", "checksum", 5)
	if err != nil {
		t.Fatalf("insertTestDocumentWithContent() error = %v", err)
	}

	for pageIndex := 0; pageIndex < 5; pageIndex++ {
		if err := database.UpsertPage(contentID, pageIndex, "alpha"); err != nil {
			t.Fatalf("UpsertPage(%d) error = %v", pageIndex, err)
		}
	}

	results, err := database.SearchWithOptions(SearchOptions{
		Query:           "alpha",
		Mode:            "and",
		Limit:           2,
		Offset:          1,
		IncludePathLike: true,
	})
	if err != nil {
		t.Fatalf("SearchWithOptions() error = %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("SearchWithOptions() returned %d rows, want 2", len(results))
	}
	if results[0].PageIndex != 1 || results[1].PageIndex != 2 {
		t.Fatalf("page indices = [%d, %d], want [1, 2]", results[0].PageIndex, results[1].PageIndex)
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

func TestGetPageMarkdownByPathAndIndex(t *testing.T) {
	t.Parallel()

	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	contentID, err := insertTestDocumentWithContent(database, "/docs/read.pdf", "checksum", 3)
	if err != nil {
		t.Fatalf("insertTestDocumentWithContent() error = %v", err)
	}
	if err := database.UpsertPage(contentID, 0, "page-zero"); err != nil {
		t.Fatalf("UpsertPage(0) error = %v", err)
	}
	if err := database.UpsertPage(contentID, 1, "page-one"); err != nil {
		t.Fatalf("UpsertPage(1) error = %v", err)
	}

	markdown, err := database.GetPageMarkdownByPathAndIndex("/docs/read.pdf", 1)
	if err != nil {
		t.Fatalf("GetPageMarkdownByPathAndIndex() error = %v", err)
	}
	if markdown != "page-one" {
		t.Fatalf("markdown = %q, want %q", markdown, "page-one")
	}

	_, err = database.GetPageMarkdownByPathAndIndex("/docs/read.pdf", 9)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetPageMarkdownByPathAndIndex(missing) error = %v, want sql.ErrNoRows", err)
	}
}

func TestGetPagesMarkdownByPathAndRange(t *testing.T) {
	t.Parallel()

	database, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	contentID, err := insertTestDocumentWithContent(database, "/docs/range.pdf", "checksum", 5)
	if err != nil {
		t.Fatalf("insertTestDocumentWithContent() error = %v", err)
	}

	for pageIndex := 0; pageIndex < 5; pageIndex++ {
		if err := database.UpsertPage(contentID, pageIndex, fmt.Sprintf("page-%d", pageIndex)); err != nil {
			t.Fatalf("UpsertPage(%d) error = %v", pageIndex, err)
		}
	}

	pages, err := database.GetPagesMarkdownByPathAndRange("/docs/range.pdf", 1, 3)
	if err != nil {
		t.Fatalf("GetPagesMarkdownByPathAndRange() error = %v", err)
	}
	if len(pages) != 3 {
		t.Fatalf("GetPagesMarkdownByPathAndRange() returned %d rows, want 3", len(pages))
	}
	if pages[0].PageIndex != 1 || pages[1].PageIndex != 2 || pages[2].PageIndex != 3 {
		t.Fatalf("page indices = [%d, %d, %d], want [1, 2, 3]", pages[0].PageIndex, pages[1].PageIndex, pages[2].PageIndex)
	}
	if pages[0].Markdown != "page-1" || pages[1].Markdown != "page-2" || pages[2].Markdown != "page-3" {
		t.Fatalf("markdown values = [%q, %q, %q], want [page-1 page-2 page-3]", pages[0].Markdown, pages[1].Markdown, pages[2].Markdown)
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
