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

func TestReplacePagesWhileActiveSkipsAlreadyCancelledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var writeMu sync.Mutex
	called := false

	err := replacePagesWhileActive(ctx, &writeMu, func() error {
		called = true
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("replacePagesWhileActive() error = %v, want context.Canceled", err)
	}
	if called {
		t.Fatalf("replace function was called")
	}
}

func TestReplacePagesWhileActiveChecksCancellationAfterWaiting(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var writeMu sync.Mutex
	writeMu.Lock()
	started := make(chan struct{})
	result := make(chan error, 1)
	called := false

	go func() {
		close(started)
		result <- replacePagesWhileActive(ctx, &writeMu, func() error {
			called = true
			return nil
		})
	}()
	<-started
	time.Sleep(20 * time.Millisecond)
	cancel()
	writeMu.Unlock()

	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("replacePagesWhileActive() error = %v, want context.Canceled", err)
	}
	if called {
		t.Fatalf("replace function was called")
	}
}

func TestProcessOCRFailedRedoPreservesCompletedPages(t *testing.T) {
	t.Parallel()

	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	contentID, err := database.InsertContent("redo-checksum", 2)
	if err != nil {
		t.Fatalf("InsertContent() error = %v", err)
	}
	path := "/docs/redo.pdf"
	now := time.Now().UTC()
	if _, err := database.InsertDocument(path, contentID, now, now); err != nil {
		t.Fatalf("InsertDocument() error = %v", err)
	}
	if err := database.ReplaceContentPages(contentID, []db.PageInput{
		{PageIndex: 0, Markdown: "old zero"},
		{PageIndex: 1, Markdown: "old one"},
	}); err != nil {
		t.Fatalf("ReplaceContentPages() error = %v", err)
	}

	provider := &fakeOCRProvider{errByPath: map[string]error{path: errors.New("later chunk failed")}}
	if err := processOCR(context.Background(), database, provider, true, 1); err != nil {
		t.Fatalf("processOCR() error = %v", err)
	}

	content, err := database.GetContentByChecksum("redo-checksum")
	if err != nil {
		t.Fatalf("GetContentByChecksum() error = %v", err)
	}
	if content == nil || content.OCRPending {
		t.Fatalf("content = %+v, want completed", content)
	}
	var count int
	var combined string
	if err := database.QueryRow("SELECT COUNT(*), GROUP_CONCAT(markdown, '|') FROM pages WHERE content_id = ? ORDER BY page_index", contentID).Scan(&count, &combined); err != nil {
		t.Fatalf("query pages: %v", err)
	}
	if count != 2 || combined != "old zero|old one" {
		t.Fatalf("stored pages = %d %q, want preserved old pages", count, combined)
	}

	calls := provider.calls.Load()
	if err := processOCR(context.Background(), database, provider, false, 1); err != nil {
		t.Fatalf("normal processOCR() error = %v", err)
	}
	if provider.calls.Load() != calls {
		t.Fatalf("normal retry called provider for completed failed redo")
	}
}

func TestProcessOCRReplacesAggregatedPagesOnceAndTrimsTail(t *testing.T) {
	t.Parallel()

	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	contentID, err := database.InsertContent("aggregate-checksum", 4)
	if err != nil {
		t.Fatalf("InsertContent() error = %v", err)
	}
	path := "/docs/aggregate.pdf"
	now := time.Now().UTC()
	if _, err := database.InsertDocument(path, contentID, now, now); err != nil {
		t.Fatalf("InsertDocument() error = %v", err)
	}
	if err := database.ReplaceContentPages(contentID, []db.PageInput{
		{PageIndex: 0, Markdown: "stale 0"},
		{PageIndex: 1, Markdown: "stale 1"},
		{PageIndex: 2, Markdown: "stale 2"},
		{PageIndex: 3, Markdown: "stale 3"},
	}); err != nil {
		t.Fatalf("seed pages: %v", err)
	}

	provider := &fakeOCRProvider{pages: []ocr.PageResult{
		{PageIndex: 0, Markdown: "new 0"},
		{PageIndex: 1, Markdown: "new 1"},
		{PageIndex: 2, Markdown: "new 2"},
	}}
	if err := processOCR(context.Background(), database, provider, true, 1); err != nil {
		t.Fatalf("processOCR() error = %v", err)
	}

	rows, err := database.Query("SELECT page_index, markdown FROM pages WHERE content_id = ? ORDER BY page_index", contentID)
	if err != nil {
		t.Fatalf("query pages: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var index int
		var markdown string
		if err := rows.Scan(&index, &markdown); err != nil {
			t.Fatalf("scan page: %v", err)
		}
		got = append(got, fmt.Sprintf("%d:%s", index, markdown))
	}
	if want := []string{"0:new 0", "1:new 1", "2:new 2"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("pages = %v, want %v", got, want)
	}
	if provider.calls.Load() != 1 {
		t.Fatalf("provider calls = %d, want 1 document-level call", provider.calls.Load())
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
