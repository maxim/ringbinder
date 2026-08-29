package format

import (
	"bytes"
	"testing"

	"github.com/maxim/ringbinder/internal/db"
)

func TestWriteFindResultsNDJSON_GoldenShape(t *testing.T) {
	t.Parallel()

	model := "provider-model-v1"
	results := []db.SearchResult{
		{
			Path:         "/docs/a.pdf",
			PageIndex:    0,
			PageCount:    4,
			Snippet:      "alpha >>>beta<<<",
			Rank:         0.1234,
			SearchSource: "fts",
			Model:        &model,
		},
		{
			Path:         "/docs/b.pdf",
			PageIndex:    3,
			PageCount:    7,
			Snippet:      "",
			Rank:         1.5,
			SearchSource: "path",
			OCRPending:   true,
		},
	}

	var got bytes.Buffer
	if err := WriteFindResultsNDJSON(&got, results); err != nil {
		t.Fatalf("WriteFindResultsNDJSON() error = %v", err)
	}

	const want = "{\"path\":\"/docs/a.pdf\",\"page_index\":0,\"page_count\":4,\"snippet\":\"alpha >>>beta<<<\",\"rank\":0.1234,\"search_source\":\"fts\",\"model\":\"provider-model-v1\",\"ocr_pending\":false}\n" +
		"{\"path\":\"/docs/b.pdf\",\"page_index\":3,\"page_count\":7,\"snippet\":\"\",\"rank\":1.5,\"search_source\":\"path\",\"model\":null,\"ocr_pending\":true}\n"

	if got.String() != want {
		t.Fatalf("WriteFindResultsNDJSON() output mismatch\n got: %q\nwant: %q", got.String(), want)
	}
}
