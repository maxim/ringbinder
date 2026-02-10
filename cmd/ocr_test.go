package cmd

import (
	"context"
	"testing"
	"time"

	"github.com/maxim/ringbinder/internal/db"
	"github.com/maxim/ringbinder/internal/ocr"
)

func TestProcessOCR_SkipsAlreadyOCRdContent(t *testing.T) {
	t.Parallel()

	database, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	now := time.Now().UTC()
	contentID, err := database.InsertContent("shared-checksum", 1)
	if err != nil {
		t.Fatalf("InsertContent() error = %v", err)
	}
	if _, err := database.InsertDocument("/docs/a.pdf", contentID, now, now); err != nil {
		t.Fatalf("InsertDocument(a) error = %v", err)
	}
	if _, err := database.InsertDocument("/docs/b.pdf", contentID, now, now); err != nil {
		t.Fatalf("InsertDocument(b) error = %v", err)
	}

	provider := &fakeOCRProvider{
		pages: []ocr.PageResult{
			{PageIndex: 0, Markdown: "raw markdown"},
		},
	}

	if err := processOCR(context.Background(), database, provider, false); err != nil {
		t.Fatalf("processOCR() error = %v", err)
	}

	if provider.calls != 1 {
		t.Fatalf("provider OCR calls = %d, want 1", provider.calls)
	}

	content, err := database.GetContentByChecksum("shared-checksum")
	if err != nil {
		t.Fatalf("GetContentByChecksum() error = %v", err)
	}
	if content == nil {
		t.Fatalf("content not found")
	}
	if content.OCRPending {
		t.Fatalf("content OCRPending = true, want false")
	}

	var pageCount int
	if err := database.QueryRow("SELECT COUNT(*) FROM pages WHERE content_id = ?", contentID).Scan(&pageCount); err != nil {
		t.Fatalf("count pages error = %v", err)
	}
	if pageCount != 1 {
		t.Fatalf("stored page count = %d, want 1", pageCount)
	}
}

type fakeOCRProvider struct {
	calls int
	pages []ocr.PageResult
}

func (p *fakeOCRProvider) OCRFile(ctx context.Context, filePath, fileType string) ([]ocr.PageResult, error) {
	p.calls++
	return p.pages, nil
}

func (p *fakeOCRProvider) PricePerPage() float64 {
	return 0
}
