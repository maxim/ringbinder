package cmd

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maxim/ringbinder/internal/db"
	"github.com/maxim/ringbinder/internal/ocr"
)

func TestProcessOCR_SkipsAlreadyOCRdContent(t *testing.T) {
	t.Parallel()

	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
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

	if err := processOCR(context.Background(), database, provider, false, 4); err != nil {
		t.Fatalf("processOCR() error = %v", err)
	}

	if provider.calls.Load() != 1 {
		t.Fatalf("provider OCR calls = %d, want 1", provider.calls.Load())
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

func TestProcessOCR_ConcurrentExecution(t *testing.T) {
	t.Parallel()

	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	now := time.Now().UTC()
	for i := 0; i < 8; i++ {
		contentID, err := database.InsertContent(fmt.Sprintf("checksum-%d", i), 1)
		if err != nil {
			t.Fatalf("InsertContent(%d) error = %v", i, err)
		}
		if _, err := database.InsertDocument(fmt.Sprintf("/docs/%d.pdf", i), contentID, now, now); err != nil {
			t.Fatalf("InsertDocument(%d) error = %v", i, err)
		}
	}

	provider := &fakeOCRProvider{
		delay: 100 * time.Millisecond,
		pages: []ocr.PageResult{
			{PageIndex: 0, Markdown: "raw markdown"},
		},
	}

	if err := processOCR(context.Background(), database, provider, false, 4); err != nil {
		t.Fatalf("processOCR() error = %v", err)
	}

	if provider.peak.Load() <= 1 {
		t.Fatalf("max concurrent calls = %d, want > 1", provider.peak.Load())
	}
	if provider.calls.Load() != 8 {
		t.Fatalf("provider OCR calls = %d, want 8", provider.calls.Load())
	}

	pending, err := database.PendingContents()
	if err != nil {
		t.Fatalf("PendingContents() error = %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending contents = %d, want 0", len(pending))
	}

	var pageCount int
	if err := database.QueryRow("SELECT COUNT(*) FROM pages").Scan(&pageCount); err != nil {
		t.Fatalf("count pages error = %v", err)
	}
	if pageCount != 8 {
		t.Fatalf("stored page count = %d, want 8", pageCount)
	}
}

func TestProcessOCR_ErrorDoesNotBlockOthers(t *testing.T) {
	t.Parallel()

	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	now := time.Now().UTC()
	for i := 0; i < 6; i++ {
		contentID, err := database.InsertContent(fmt.Sprintf("checksum-%d", i), 1)
		if err != nil {
			t.Fatalf("InsertContent(%d) error = %v", i, err)
		}
		if _, err := database.InsertDocument(fmt.Sprintf("/docs/%d.pdf", i), contentID, now, now); err != nil {
			t.Fatalf("InsertDocument(%d) error = %v", i, err)
		}
	}

	provider := &fakeOCRProvider{
		pages: []ocr.PageResult{
			{PageIndex: 0, Markdown: "raw markdown"},
		},
		errByPath: map[string]error{
			"/docs/1.pdf": errors.New("boom"),
			"/docs/4.pdf": errors.New("boom"),
		},
	}

	if err := processOCR(context.Background(), database, provider, false, 4); err != nil {
		t.Fatalf("processOCR() error = %v", err)
	}

	if provider.calls.Load() != 6 {
		t.Fatalf("provider OCR calls = %d, want 6", provider.calls.Load())
	}

	pending, err := database.PendingContents()
	if err != nil {
		t.Fatalf("PendingContents() error = %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("pending contents = %d, want 2", len(pending))
	}

	var pageCount int
	if err := database.QueryRow("SELECT COUNT(*) FROM pages").Scan(&pageCount); err != nil {
		t.Fatalf("count pages error = %v", err)
	}
	if pageCount != 4 {
		t.Fatalf("stored page count = %d, want 4", pageCount)
	}
}

func TestProcessOCR_TimeoutDoesNotCancelOthers(t *testing.T) {
	t.Parallel()

	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	now := time.Now().UTC()
	for i := 0; i < 6; i++ {
		contentID, err := database.InsertContent(fmt.Sprintf("checksum-%d", i), 1)
		if err != nil {
			t.Fatalf("InsertContent(%d) error = %v", i, err)
		}
		if _, err := database.InsertDocument(fmt.Sprintf("/docs/%d.pdf", i), contentID, now, now); err != nil {
			t.Fatalf("InsertDocument(%d) error = %v", i, err)
		}
	}

	provider := &fakeOCRProvider{
		pages: []ocr.PageResult{
			{PageIndex: 0, Markdown: "raw markdown"},
		},
		errByPath: map[string]error{
			"/docs/1.pdf": context.DeadlineExceeded,
		},
	}

	if err := processOCR(context.Background(), database, provider, false, 4); err != nil {
		t.Fatalf("processOCR() error = %v, want nil", err)
	}

	if provider.calls.Load() != 6 {
		t.Fatalf("provider OCR calls = %d, want 6", provider.calls.Load())
	}

	pending, err := database.PendingContents()
	if err != nil {
		t.Fatalf("PendingContents() error = %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending contents = %d, want 1", len(pending))
	}

	var pageCount int
	if err := database.QueryRow("SELECT COUNT(*) FROM pages").Scan(&pageCount); err != nil {
		t.Fatalf("count pages error = %v", err)
	}
	if pageCount != 5 {
		t.Fatalf("stored page count = %d, want 5", pageCount)
	}
}

func TestProcessOCR_ContextCancellation(t *testing.T) {
	t.Parallel()

	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	now := time.Now().UTC()
	for i := 0; i < 8; i++ {
		contentID, err := database.InsertContent(fmt.Sprintf("checksum-%d", i), 1)
		if err != nil {
			t.Fatalf("InsertContent(%d) error = %v", i, err)
		}
		if _, err := database.InsertDocument(fmt.Sprintf("/docs/%d.pdf", i), contentID, now, now); err != nil {
			t.Fatalf("InsertDocument(%d) error = %v", i, err)
		}
	}

	provider := &fakeOCRProvider{
		delay:       5 * time.Second,
		pages:       []ocr.PageResult{{PageIndex: 0, Markdown: "raw markdown"}},
		firstCallCh: make(chan struct{}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		<-provider.firstCallCh
		cancel()
	}()

	start := time.Now()
	err = processOCR(ctx, database, provider, false, 4)
	elapsed := time.Since(start)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("processOCR() error = %v, want context.Canceled", err)
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("processOCR() took %v after cancellation, want <= 1.5s", elapsed)
	}
}

type fakeOCRProvider struct {
	delay       time.Duration
	pages       []ocr.PageResult
	errByPath   map[string]error
	firstCallCh chan struct{}

	firstCallOnce sync.Once
	calls         atomic.Int64
	inFlight      atomic.Int32
	peak          atomic.Int32
}

func (p *fakeOCRProvider) OCRFile(ctx context.Context, filePath, fileType string) ([]ocr.PageResult, error) {
	if p.firstCallCh != nil {
		p.firstCallOnce.Do(func() {
			close(p.firstCallCh)
		})
	}

	p.calls.Add(1)
	inFlight := p.inFlight.Add(1)
	for {
		peak := p.peak.Load()
		if inFlight <= peak || p.peak.CompareAndSwap(peak, inFlight) {
			break
		}
	}
	defer p.inFlight.Add(-1)

	if p.delay > 0 {
		timer := time.NewTimer(p.delay)
		defer timer.Stop()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
		}
	}

	if err, ok := p.errByPath[filePath]; ok {
		return nil, err
	}

	pages := make([]ocr.PageResult, len(p.pages))
	copy(pages, p.pages)
	return pages, nil
}

func (p *fakeOCRProvider) PricePerPage() float64 {
	return 0
}
