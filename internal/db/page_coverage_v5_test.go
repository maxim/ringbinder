package db

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestPartialCanonicalPagesRemainHiddenUntilExactCoverage(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "coverage.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	contentID, err := insertTestDocumentWithContent(
		database, "/docs/pending-report.pdf", "coverage", 3,
	)
	if err != nil {
		t.Fatal(err)
	}
	model := "provider-model-v1"
	if err := database.UpsertContentPages(contentID, []PageInput{{
		PageIndex: 0, Markdown: "alpha evidence", Model: &model,
	}}); err != nil {
		t.Fatal(err)
	}
	var canonical, indexed int
	if err := database.QueryRow(`SELECT COUNT(*) FROM pages WHERE content_id = ?`, contentID).Scan(&canonical); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM pages_fts`).Scan(&indexed); err != nil {
		t.Fatal(err)
	}
	if canonical != 1 || indexed != 1 {
		t.Fatalf("canonical = %d, indexed = %d; partial row must be retained and trigger-indexed", canonical, indexed)
	}
	results, err := database.SearchWithOptions(SearchOptions{
		Query: "alpha", Limit: 10, IncludePathLike: false, UseTrigram: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("partial OCR search results = %+v, want hidden", results)
	}
	pathResults, err := database.Search("pending-report")
	if err != nil {
		t.Fatal(err)
	}
	if len(pathResults) != 1 || pathResults[0].SearchSource != searchSourcePath ||
		!pathResults[0].OCRPending || pathResults[0].Model != nil {
		t.Fatalf("path results = %+v, want pending path-only result", pathResults)
	}
	if _, err := database.GetPageMarkdownByPathAndIndex("/docs/pending-report.pdf", 0); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("partial page read error = %v, want sql.ErrNoRows", err)
	}
	partialRange, err := database.GetPagesMarkdownByPathAndRange("/docs/pending-report.pdf", 0, 2)
	if err != nil || len(partialRange) != 0 {
		t.Fatalf("partial range = %+v, %v; want hidden", partialRange, err)
	}

	if err := database.UpsertContentPages(contentID, []PageInput{{
		PageIndex: 2, Markdown: "omega", Model: &model,
	}}); err != nil {
		t.Fatal(err)
	}
	ranges, err := database.MissingPageRanges(contentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ranges) != 1 || ranges[0] != (PageRange{Start: 1, End: 2}) {
		t.Fatalf("missing ranges = %+v, want page 2", ranges)
	}
	if err := database.UpsertContentPages(contentID, []PageInput{{
		PageIndex: 1, Markdown: "middle", Model: &model,
	}}); err != nil {
		t.Fatal(err)
	}
	results, err = database.Search("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].OCRPending || results[0].Model == nil ||
		*results[0].Model != model {
		t.Fatalf("complete OCR search results = %+v", results)
	}
	pages, err := database.GetPagesMarkdownByPathAndRange("/docs/pending-report.pdf", 0, 2)
	if err != nil || len(pages) != 3 {
		t.Fatalf("complete range = %+v, %v", pages, err)
	}
}

func TestMissingPageRangesWithinCoalescesOnlyRequestedIndexes(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "ranges.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	contentID, err := insertTestDocumentWithContent(database, "/docs/ranges.pdf", "ranges", 7)
	if err != nil {
		t.Fatal(err)
	}
	model := "existing"
	if err := database.UpsertContentPages(contentID, []PageInput{
		{PageIndex: 0, Markdown: "done", Model: &model},
		{PageIndex: 3, Markdown: "done", Model: &model},
	}); err != nil {
		t.Fatal(err)
	}

	ranges, err := database.MissingPageRangesWithin(contentID, 2, 6)
	if err != nil {
		t.Fatal(err)
	}
	want := []PageRange{{Start: 2, End: 3}, {Start: 4, End: 6}}
	if len(ranges) != len(want) || ranges[0] != want[0] || ranges[1] != want[1] {
		t.Fatalf("missing ranges within bounds = %+v, want %+v", ranges, want)
	}
}

func TestPageRangeUpsertRejectsInvalidIndexesWithoutDeletingRetainedRows(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "validation.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	contentID, err := insertTestDocumentWithContent(database, "/docs/validation.pdf", "validation", 2)
	if err != nil {
		t.Fatal(err)
	}
	model := "provider-v1"
	if err := database.UpsertContentPages(contentID, []PageInput{{
		PageIndex: 0, Markdown: "retained", Model: &model,
	}}); err != nil {
		t.Fatal(err)
	}
	invalid := [][]PageInput{
		{{PageIndex: -1, Markdown: "bad", Model: &model}},
		{{PageIndex: 2, Markdown: "bad", Model: &model}},
		{
			{PageIndex: 1, Markdown: "would be rolled back", Model: &model},
			{PageIndex: 1, Markdown: "duplicate", Model: &model},
		},
	}
	for _, pages := range invalid {
		if err := database.UpsertContentPages(contentID, pages); err == nil {
			t.Fatalf("UpsertContentPages(%+v) error = nil", pages)
		}
	}
	var count int
	var markdown string
	if err := database.QueryRow(
		`SELECT COUNT(*), MAX(markdown) FROM pages WHERE content_id = ?`, contentID,
	).Scan(&count, &markdown); err != nil {
		t.Fatal(err)
	}
	if count != 1 || markdown != "retained" {
		t.Fatalf("retained pages = %d, %q", count, markdown)
	}
}
