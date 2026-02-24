package format

import (
	"encoding/json"
	"io"

	"github.com/maxim/ringbinder/internal/db"
)

type findResultJSON struct {
	Path         string  `json:"path"`
	PageIndex    int     `json:"page_index"`
	PageCount    int     `json:"page_count"`
	Snippet      string  `json:"snippet"`
	Rank         float64 `json:"rank"`
	SearchSource string  `json:"search_source"`
}

// WriteFindResultsNDJSON writes one JSON object per line so callers can stream
// and paginate without buffering the entire result set in memory.
func WriteFindResultsNDJSON(w io.Writer, results []db.SearchResult) error {
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)

	for _, result := range results {
		payload := findResultJSON{
			Path:         result.Path,
			PageIndex:    result.PageIndex,
			PageCount:    result.PageCount,
			Snippet:      result.Snippet,
			Rank:         result.Rank,
			SearchSource: result.SearchSource,
		}
		if err := encoder.Encode(payload); err != nil {
			return err
		}
	}

	return nil
}
