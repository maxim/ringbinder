package cmd

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
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

	if err := processOCR(context.Background(), database, provider, 0, 4); err != nil {
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

	if err := processOCR(context.Background(), database, provider, 0, 4); err != nil {
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

	if err := processOCR(context.Background(), database, provider, 0, 4); err != nil {
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

func TestProcessOCR_LimitCapsAttemptsAndLeavesFailuresPending(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	now := time.Now().UTC()
	contentIDs := make([]int64, 3)
	for i := range contentIDs {
		contentIDs[i], err = database.InsertContent(fmt.Sprintf("limited-%d", i), i+1)
		if err != nil {
			t.Fatalf("InsertContent(%d) error = %v", i, err)
		}
		if _, err := database.InsertDocument(fmt.Sprintf("/docs/%d.pdf", i), contentIDs[i], now, now); err != nil {
			t.Fatalf("InsertDocument(%d) error = %v", i, err)
		}
	}

	provider := &fakeOCRProvider{
		pages:     []ocr.PageResult{{PageIndex: 0, Markdown: "text"}},
		errByPath: map[string]error{"/docs/0.pdf": errors.New("boom")},
	}
	output := captureStdout(t, func() {
		if err := processOCR(context.Background(), database, provider, 2, 1); err != nil {
			t.Fatalf("processOCR() error = %v", err)
		}
	})
	if !strings.Contains(output, "Processing 2 of 3 pending content item(s).\n") {
		t.Fatalf("output = %q, want truncated batch message", output)
	}
	if provider.calls.Load() != 2 {
		t.Fatalf("provider calls = %d, want 2 selected attempts", provider.calls.Load())
	}

	pending, err := database.PendingContents()
	if err != nil {
		t.Fatalf("PendingContents() error = %v", err)
	}
	if len(pending) != 2 || pending[0].ID != contentIDs[0] || pending[1].ID != contentIDs[2] {
		t.Fatalf("pending contents = %+v, want failed first and unselected third", pending)
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

	if err := processOCR(context.Background(), database, provider, 0, 4); err != nil {
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
	err = processOCR(ctx, database, provider, 0, 4)
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
	if _, err := database.Exec("UPDATE contents SET ocr_pending = 1 WHERE id = ?", contentID); err != nil {
		t.Fatalf("mark content pending: %v", err)
	}

	provider := &fakeOCRProvider{pages: []ocr.PageResult{
		{PageIndex: 0, Markdown: "new 0"},
		{PageIndex: 1, Markdown: "new 1"},
		{PageIndex: 2, Markdown: "new 2"},
	}}
	if err := processOCR(context.Background(), database, provider, 0, 1); err != nil {
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

func TestProcessOCR_PrintsOneKnownCostLineForPartialFailure(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	now := time.Now().UTC()
	for i := 0; i < 2; i++ {
		contentID, err := database.InsertContent(fmt.Sprintf("billing-%d", i), 1)
		if err != nil {
			t.Fatalf("InsertContent() error = %v", err)
		}
		if _, err := database.InsertDocument(fmt.Sprintf("/docs/%d.pdf", i), contentID, now, now); err != nil {
			t.Fatalf("InsertDocument() error = %v", err)
		}
	}

	provider := &fakeOCRProvider{
		pages: []ocr.PageResult{{PageIndex: 0, Markdown: "text"}},
		reportByPath: map[string]ocr.BillingReport{
			"/docs/0.pdf": {KnownCost: 700_000},
			"/docs/1.pdf": {KnownCost: 680_000, Indeterminate: true},
		},
		errByPath: map[string]error{"/docs/1.pdf": errors.New("invalid output")},
	}

	output := captureStdout(t, func() {
		if err := processOCR(context.Background(), database, provider, 0, 2); err != nil {
			t.Fatalf("processOCR() error = %v", err)
		}
	})
	want := "Known OCR cost: $0.0014 (actual cost may be higher)"
	if strings.Count(output, want) != 1 {
		t.Fatalf("output = %q, want exactly one %q line", output, want)
	}
}

func TestProcessOCR_PrintsCompleteCostLine(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	contentID, err := database.InsertContent("complete-billing", 1)
	if err != nil {
		t.Fatalf("InsertContent() error = %v", err)
	}
	if _, err := database.InsertDocument("/docs/page.pdf", contentID, time.Now().UTC(), time.Now().UTC()); err != nil {
		t.Fatalf("InsertDocument() error = %v", err)
	}
	provider := &fakeOCRProvider{
		pages:  []ocr.PageResult{{PageIndex: 0, Markdown: "text"}},
		report: ocr.BillingReport{KnownCost: 1_380_000},
	}

	output := captureStdout(t, func() {
		if err := processOCR(context.Background(), database, provider, 0, 1); err != nil {
			t.Fatalf("processOCR() error = %v", err)
		}
	})
	if strings.Count(output, "OCR cost: $0.0014") != 1 || strings.Contains(output, "Known OCR cost") {
		t.Fatalf("output = %q, want one complete cost line", output)
	}
}

func TestProcessOCR_PrintsCostWhenDatabaseWriteFails(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}

	contentID, err := database.InsertContent("write-failure-billing", 1)
	if err != nil {
		t.Fatalf("InsertContent() error = %v", err)
	}
	if _, err := database.InsertDocument("/docs/page.pdf", contentID, time.Now().UTC(), time.Now().UTC()); err != nil {
		t.Fatalf("InsertDocument() error = %v", err)
	}
	started := make(chan struct{})
	provider := &fakeOCRProvider{
		delay:       50 * time.Millisecond,
		firstCallCh: started,
		pages:       []ocr.PageResult{{PageIndex: 0, Markdown: "text"}},
		report:      ocr.BillingReport{KnownCost: 1_380_000},
	}
	closed := make(chan struct{})
	go func() {
		<-started
		_ = database.Close()
		close(closed)
	}()

	output := captureStdout(t, func() {
		if err := processOCR(context.Background(), database, provider, 0, 1); err != nil {
			t.Fatalf("processOCR() error = %v", err)
		}
	})
	<-closed
	if strings.Count(output, "OCR cost: $0.0014") != 1 {
		t.Fatalf("output = %q, want billed provider call after write failure", output)
	}

	reopened, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen database error = %v", err)
	}
	defer reopened.Close()
	pending, err := reopened.PendingContents()
	if err != nil {
		t.Fatalf("PendingContents() error = %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending contents = %d, want failed write to remain pending", len(pending))
	}
}

func TestProcessOCR_NoWorkOmitsCostLine(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	output := captureStdout(t, func() {
		if err := processOCR(context.Background(), database, &fakeOCRProvider{}, 0, 1); err != nil {
			t.Fatalf("processOCR() error = %v", err)
		}
	})
	if !strings.Contains(output, "No documents pending OCR.") || strings.Contains(output, "OCR cost:") {
		t.Fatalf("output = %q, want no-work message without cost", output)
	}
}

func TestRunOCR_RejectsNonPositiveExplicitLimit(t *testing.T) {
	for _, limit := range []string{"0", "-1"} {
		t.Run(limit, func(t *testing.T) {
			resetCommandState(t)
			cmd := commandWithDatabaseFlag(t, filepath.Join(t.TempDir(), "ocr.db"))
			cmd.Flags().String("model", "", "")
			cmd.Flags().Int("concurrency", 0, "")
			cmd.Flags().Int("limit", 0, "")
			if err := cmd.Flags().Set("model", modelMistral); err != nil {
				t.Fatalf("Set(model) error = %v", err)
			}
			if err := cmd.Flags().Set("concurrency", "1"); err != nil {
				t.Fatalf("Set(concurrency) error = %v", err)
			}
			if err := cmd.Flags().Set("limit", limit); err != nil {
				t.Fatalf("Set(limit) error = %v", err)
			}

			err := runOCR(cmd, nil)
			if err == nil || err.Error() != "--limit must be >= 1" {
				t.Fatalf("runOCR() error = %v, want --limit validation error", err)
			}
		})
	}
}

type fakeOCRProvider struct {
	delay        time.Duration
	pages        []ocr.PageResult
	report       ocr.BillingReport
	reportByPath map[string]ocr.BillingReport
	errByPath    map[string]error
	firstCallCh  chan struct{}

	firstCallOnce sync.Once
	calls         atomic.Int64
	inFlight      atomic.Int32
	peak          atomic.Int32
}

func (p *fakeOCRProvider) OCRFile(ctx context.Context, filePath, fileType string) ([]ocr.PageResult, ocr.BillingReport, error) {
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
			return nil, ocr.BillingReport{}, ctx.Err()
		case <-timer.C:
		}
	}

	report := p.report
	if pathReport, ok := p.reportByPath[filePath]; ok {
		report = pathReport
	}
	if err, ok := p.errByPath[filePath]; ok {
		return nil, report, err
	}

	pages := make([]ocr.PageResult, len(p.pages))
	copy(pages, p.pages)
	return pages, report, nil
}
