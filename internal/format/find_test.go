package format

import (
	"strings"
	"testing"

	"github.com/maxim/ringbinder/internal/db"
)

func TestFormatFindResults_DefaultNoSnippet(t *testing.T) {
	t.Parallel()

	results := []db.SearchResult{
		{
			Path:      "/docs/alpha.pdf",
			PageIndex: 0,
			PageCount: 1,
			Snippet:   "this snippet should be hidden",
		},
	}

	got := FormatFindResults(results, false, false)
	if strings.Contains(got, "this snippet should be hidden") {
		t.Fatalf("FormatFindResults() output contains snippet in non-verbose mode:\n%s", got)
	}
}

func TestFormatFindResults_SinglePageNoPageNumber(t *testing.T) {
	t.Parallel()

	results := []db.SearchResult{
		{
			Path:      "/docs/solo.pdf",
			PageIndex: 0,
			PageCount: 1,
		},
	}

	got := FormatFindResults(results, false, false)
	if strings.Contains(got, "(page ") {
		t.Fatalf("FormatFindResults() output contains page label for single-page document:\n%s", got)
	}
}

func TestFormatFindResults_MultiPageShowsPageNumber(t *testing.T) {
	t.Parallel()

	results := []db.SearchResult{
		{
			Path:      "/docs/multi.pdf",
			PageIndex: 2,
			PageCount: 5,
		},
	}

	got := FormatFindResults(results, false, false)
	if !strings.Contains(got, "(page 3)") {
		t.Fatalf("FormatFindResults() output missing expected page label:\n%s", got)
	}
}

func TestFormatFindResults_VerboseShowsSnippet(t *testing.T) {
	t.Parallel()

	results := []db.SearchResult{
		{
			Path:      "/docs/verbose.pdf",
			PageIndex: 0,
			PageCount: 1,
			Snippet:   "visible snippet",
		},
	}

	got := FormatFindResults(results, true, false)
	if !strings.Contains(got, "    visible snippet") {
		t.Fatalf("FormatFindResults() output missing indented snippet:\n%s", got)
	}
}

func TestFormatFindResults_ColorHighlightsMatches(t *testing.T) {
	t.Parallel()

	results := []db.SearchResult{
		{
			Path:      "/docs/highlight.pdf",
			PageIndex: 0,
			PageCount: 1,
			Snippet:   "foo >>>bar<<< baz",
		},
	}

	got := FormatFindResults(results, true, true)
	if strings.Contains(got, ">>>") || strings.Contains(got, "<<<") {
		t.Fatalf("FormatFindResults() output still contains raw markers:\n%s", got)
	}
	if !strings.Contains(got, "\x1b[1;33mbar\x1b[0m") {
		t.Fatalf("FormatFindResults() output missing highlighted match:\n%s", got)
	}
}

func TestFormatFindResults_NoColorStripsMarkers(t *testing.T) {
	t.Parallel()

	results := []db.SearchResult{
		{
			Path:      "/docs/plain.pdf",
			PageIndex: 0,
			PageCount: 1,
			Snippet:   "foo >>>bar<<< baz",
		},
	}

	got := FormatFindResults(results, true, false)
	if strings.Contains(got, ">>>") || strings.Contains(got, "<<<") {
		t.Fatalf("FormatFindResults() output still contains markers in no-color mode:\n%s", got)
	}
	if !strings.Contains(got, "foo bar baz") {
		t.Fatalf("FormatFindResults() output missing cleaned snippet text:\n%s", got)
	}
}

func TestFormatFindResults_ResultCount(t *testing.T) {
	t.Parallel()

	results := []db.SearchResult{
		{Path: "/docs/a.pdf", PageIndex: 0, PageCount: 1},
		{Path: "/docs/b.pdf", PageIndex: 0, PageCount: 1},
	}

	got := FormatFindResults(results, false, false)
	if !strings.Contains(got, "2 result(s) found.") {
		t.Fatalf("FormatFindResults() output missing summary count:\n%s", got)
	}
}

func TestFormatFindResults_NonVerboseNoBlankLinesBetweenPaths(t *testing.T) {
	t.Parallel()

	results := []db.SearchResult{
		{Path: "/docs/a.pdf", PageIndex: 0, PageCount: 1},
		{Path: "/docs/b.pdf", PageIndex: 1, PageCount: 3},
	}

	got := FormatFindResults(results, false, false)
	if strings.Contains(got, "/docs/a.pdf\n\n/docs/b.pdf") {
		t.Fatalf("FormatFindResults() has blank lines between non-verbose results:\n%s", got)
	}
}

func TestFormatFindResults_VerboseIndentsAllSnippetLines(t *testing.T) {
	t.Parallel()

	results := []db.SearchResult{
		{
			Path:      "/docs/multiline.pdf",
			PageIndex: 0,
			PageCount: 1,
			Snippet:   "line one\nline two",
		},
	}

	got := FormatFindResults(results, true, false)
	if !strings.Contains(got, "    line one\n    line two") {
		t.Fatalf("FormatFindResults() does not indent all snippet lines:\n%s", got)
	}
}

func TestFormatFindResults_SummaryIsDimItalic(t *testing.T) {
	t.Parallel()

	results := []db.SearchResult{
		{
			Path:      "/docs/a.pdf",
			PageIndex: 0,
			PageCount: 1,
			Snippet:   "snippet text",
		},
	}

	got := FormatFindResults(results, true, true)
	if !strings.Contains(got, ansiDim+"snippet text"+ansiReset) {
		t.Fatalf("FormatFindResults() missing expected dim snippet styling:\n%s", got)
	}
	if !strings.Contains(got, ansiDim+ansiItalic+"1 result(s) found."+ansiReset) {
		t.Fatalf("FormatFindResults() summary line missing expected dim+italic styling:\n%s", got)
	}
}
