package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/maxim/ringbinder/internal/checksum"
	"github.com/maxim/ringbinder/internal/db"
	"github.com/maxim/ringbinder/internal/ocr"
)

type rangeCall struct {
	start int
	end   int
}

type fakeRangeProvider struct {
	mu       sync.Mutex
	calls    []rangeCall
	inFlight int
	peak     int
	fn       func(context.Context, string, int, int) (ocr.RangeResult, error)
}

func (p *fakeRangeProvider) OCRRangeResult(
	ctx context.Context,
	path, _ string,
	start, end int,
) (ocr.RangeResult, error) {
	p.mu.Lock()
	p.calls = append(p.calls, rangeCall{start: start, end: end})
	p.inFlight++
	p.peak = max(p.peak, p.inFlight)
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.inFlight--
		p.mu.Unlock()
	}()
	return p.fn(ctx, path, start, end)
}

func (p *fakeRangeProvider) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.calls)
}

func TestProcessOCRChainRequestsOnlySparseMissingRanges(t *testing.T) {
	database := openOCRChainTestDB(t)
	contentID, _ := addOCRChainFile(t, database, "sparse.pdf", 3)
	oldModel := "historical-model"
	if err := database.UpsertContentPages(contentID, []db.PageInput{
		{PageIndex: 0, Markdown: "existing zero", Model: &oldModel},
		{PageIndex: 2, Markdown: "existing two", Model: &oldModel},
	}); err != nil {
		t.Fatal(err)
	}
	provider := successfulRangeProvider("response-model-v2", 0)
	providers := testProviderChain(modelMistral, "requested-model", provider, 4)
	var out, errOut bytes.Buffer
	if err := processOCRChain(
		context.Background(), database, providers, []string{modelMistral}, 0, &out, &errOut,
	); err != nil {
		t.Fatalf("processOCRChain() error = %v, stderr = %s", err, errOut.String())
	}
	provider.mu.Lock()
	calls := append([]rangeCall(nil), provider.calls...)
	provider.mu.Unlock()
	if len(calls) != 1 || calls[0] != (rangeCall{start: 1, end: 2}) {
		t.Fatalf("calls = %+v, want only missing page 2", calls)
	}
	var retained, filled, model string
	if err := database.QueryRow(
		`SELECT
		 MAX(CASE WHEN page_index = 0 THEN markdown END),
		 MAX(CASE WHEN page_index = 1 THEN markdown END),
		 MAX(CASE WHEN page_index = 1 THEN model END)
		 FROM pages WHERE content_id = ?`, contentID,
	).Scan(&retained, &filled, &model); err != nil {
		t.Fatal(err)
	}
	if retained != "existing zero" || filled != "page 1" || model != "response-model-v2" {
		t.Fatalf("retained = %q, filled = %q, model = %q", retained, filled, model)
	}
}

func TestProcessOCRChainRecursivelyIsolatesFallbackPages(t *testing.T) {
	database := openOCRChainTestDB(t)
	contentID, _ := addOCRChainFile(t, database, "mixed.pdf", 4)
	preferred := &fakeRangeProvider{fn: func(_ context.Context, _ string, start, end int) (ocr.RangeResult, error) {
		if start == 0 && end == 4 {
			partial := successfulRange("mistral-exact", 0, 2, 100_000)
			return partial, ocr.DocumentFailure(errors.New("later chunk safety refusal"))
		}
		if start <= 2 && 2 < end {
			return ocr.RangeResult{Billing: ocr.BillingReport{KnownCost: 100_000}}, ocr.DocumentFailure(errors.New("safety refusal"))
		}
		return successfulRange("mistral-exact", start, end, 200_000), nil
	}}
	fallback := successfulRangeProvider("gemini-exact", 300_000)
	providers := ocrProviderChain{
		modelMistral: {defaultModel: "mistral-requested", provider: preferred, slots: make(chan struct{}, 4)},
		modelGemini:  {defaultModel: "gemini-requested", provider: fallback, slots: make(chan struct{}, 20)},
	}
	var out, errOut bytes.Buffer
	if err := processOCRChain(
		context.Background(), database, providers,
		[]string{modelMistral, modelGemini}, 0, &out, &errOut,
	); err != nil {
		t.Fatalf("processOCRChain() error = %v, stderr = %s", err, errOut.String())
	}
	rows, err := database.Query(
		`SELECT page_index, model FROM pages WHERE content_id = ? ORDER BY page_index`, contentID,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var index int
		var model string
		if err := rows.Scan(&index, &model); err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprintf("%d:%s", index, model))
	}
	want := "[0:mistral-exact 1:mistral-exact 2:gemini-exact 3:mistral-exact]"
	if fmt.Sprint(got) != want {
		t.Fatalf("page models = %v, want %s", got, want)
	}
	if fallback.callCount() != 1 {
		t.Fatalf("fallback calls = %d, want only irreducible failed page", fallback.callCount())
	}
	if !strings.Contains(errOut.String(), "splitting the range") ||
		!strings.Contains(errOut.String(), "falling back to gemini-requested") {
		t.Fatalf("fallback log = %q", errOut.String())
	}
	for _, want := range []string{
		"mistral-exact: 3",
		"gemini-exact: 1",
		"mistral-exact: $0.0003",
		"mistral-requested: $0.0002",
		"gemini-exact: $0.0003",
		"Total: $0.0008",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("summary = %q, want %q", out.String(), want)
		}
	}
}

func TestProcessOCRChainRetainsPartialPagesBeforeTemporaryFailure(t *testing.T) {
	database := openOCRChainTestDB(t)
	contentID, _ := addOCRChainFile(t, database, "partial-temporary.pdf", 4)
	provider := &fakeRangeProvider{fn: func(context.Context, string, int, int) (ocr.RangeResult, error) {
		return successfulRange("exact-v1", 0, 2, 25), ocr.TemporaryFailure(context.DeadlineExceeded)
	}}
	providers := testProviderChain(modelMistral, "requested", provider, 4)
	var out, errOut bytes.Buffer
	err := processOCRChain(
		context.Background(), database, providers, []string{modelMistral}, 0, &out, &errOut,
	)
	if err == nil {
		t.Fatal("processOCRChain() error = nil")
	}
	var pages int
	if queryErr := database.QueryRow(
		`SELECT COUNT(*) FROM pages WHERE content_id = ?`, contentID,
	).Scan(&pages); queryErr != nil {
		t.Fatal(queryErr)
	}
	missing, queryErr := database.MissingPageIndexes(contentID)
	if queryErr != nil {
		t.Fatal(queryErr)
	}
	if pages != 2 || fmt.Sprint(missing) != "[2 3]" {
		t.Fatalf("retained pages = %d, missing = %v", pages, missing)
	}
}

func TestProcessOCRChainTemporaryAndUnclassifiedFailuresDoNotFallback(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "temporary", err: ocr.TemporaryFailure(context.DeadlineExceeded)},
		{name: "unclassified", err: errors.New("malformed successful output")},
	} {
		t.Run(test.name, func(t *testing.T) {
			database := openOCRChainTestDB(t)
			_, _ = addOCRChainFile(t, database, test.name+".pdf", 1)
			preferred := &fakeRangeProvider{fn: func(context.Context, string, int, int) (ocr.RangeResult, error) {
				return ocr.RangeResult{}, test.err
			}}
			fallback := successfulRangeProvider("fallback-exact", 0)
			providers := ocrProviderChain{
				modelMistral: {defaultModel: "preferred", provider: preferred, slots: make(chan struct{}, 4)},
				modelGemini:  {defaultModel: "fallback", provider: fallback, slots: make(chan struct{}, 20)},
			}
			var out, errOut bytes.Buffer
			err := processOCRChain(
				context.Background(), database, providers,
				[]string{modelMistral, modelGemini}, 0, &out, &errOut,
			)
			if err == nil || !strings.Contains(err.Error(), "remain pending") {
				t.Fatalf("processOCRChain() error = %v", err)
			}
			if fallback.callCount() != 0 {
				t.Fatalf("fallback calls = %d, want 0", fallback.callCount())
			}
		})
	}
}

func TestProcessOCRChainMalformedSuccessfulOutputDoesNotFallback(t *testing.T) {
	database := openOCRChainTestDB(t)
	contentID, _ := addOCRChainFile(t, database, "malformed.pdf", 2)
	preferred := &fakeRangeProvider{fn: func(context.Context, string, int, int) (ocr.RangeResult, error) {
		return ocr.RangeResult{Pages: []ocr.PageResult{
			{PageIndex: 0, Markdown: "one", Model: "exact"},
			{PageIndex: 0, Markdown: "duplicate", Model: "exact"},
		}}, nil
	}}
	fallback := successfulRangeProvider("fallback", 0)
	providers := ocrProviderChain{
		modelMistral: {defaultModel: "preferred", provider: preferred, slots: make(chan struct{}, 4)},
		modelGemini:  {defaultModel: "fallback", provider: fallback, slots: make(chan struct{}, 20)},
	}
	var out, errOut bytes.Buffer
	err := processOCRChain(
		context.Background(), database, providers,
		[]string{modelMistral, modelGemini}, 0, &out, &errOut,
	)
	if err == nil || fallback.callCount() != 0 {
		t.Fatalf("error = %v, fallback calls = %d", err, fallback.callCount())
	}
	var pages int
	if queryErr := database.QueryRow(`SELECT COUNT(*) FROM pages WHERE content_id = ?`, contentID).Scan(&pages); queryErr != nil {
		t.Fatal(queryErr)
	}
	if pages != 0 {
		t.Fatalf("stored malformed pages = %d", pages)
	}
}

func TestProcessOCRChainAllPermanentRefusalsRestartOnNextRun(t *testing.T) {
	database := openOCRChainTestDB(t)
	_, _ = addOCRChainFile(t, database, "refused.pdf", 1)
	first := &fakeRangeProvider{fn: func(context.Context, string, int, int) (ocr.RangeResult, error) {
		return ocr.RangeResult{}, ocr.DocumentFailure(errors.New("first refusal"))
	}}
	second := &fakeRangeProvider{fn: func(context.Context, string, int, int) (ocr.RangeResult, error) {
		return ocr.RangeResult{}, ocr.DocumentFailure(errors.New("second refusal"))
	}}
	providers := ocrProviderChain{
		modelMistral: {defaultModel: "first", provider: first, slots: make(chan struct{}, 4)},
		modelGemini:  {defaultModel: "second", provider: second, slots: make(chan struct{}, 20)},
	}
	for run := 1; run <= 2; run++ {
		var out, errOut bytes.Buffer
		err := processOCRChain(
			context.Background(), database, providers,
			[]string{modelMistral, modelGemini}, 0, &out, &errOut,
		)
		if err == nil {
			t.Fatalf("run %d error = nil", run)
		}
	}
	if first.callCount() != 2 || second.callCount() != 2 {
		t.Fatalf("calls after two runs = %d, %d; want chain restarted", first.callCount(), second.callCount())
	}
}

func TestProcessOCRChainTemporaryFailureStopsOnlyThatContent(t *testing.T) {
	database := openOCRChainTestDB(t)
	failedID, failedPath := addOCRChainFile(t, database, "item-temporary.pdf", 3)
	otherID, _ := addOCRChainFile(t, database, "other-success.pdf", 1)
	model := "existing"
	if err := database.UpsertContentPages(failedID, []db.PageInput{{
		PageIndex: 1, Markdown: "separator", Model: &model,
	}}); err != nil {
		t.Fatal(err)
	}
	var failedCalls atomic.Int32
	provider := &fakeRangeProvider{fn: func(_ context.Context, path string, start, end int) (ocr.RangeResult, error) {
		if path == failedPath {
			failedCalls.Add(1)
			if start == 0 {
				return ocr.RangeResult{}, ocr.TemporaryFailure(errors.New("timeout"))
			}
		}
		return successfulRange("exact", start, end, 0), nil
	}}
	providers := testProviderChain(modelMistral, "requested", provider, 4)
	var out, errOut bytes.Buffer
	if err := processOCRChain(
		context.Background(), database, providers, []string{modelMistral}, 0, &out, &errOut,
	); err == nil {
		t.Fatal("processOCRChain() error = nil")
	}
	if failedCalls.Load() != 1 {
		t.Fatalf("failed-content range calls = %d, want first range only", failedCalls.Load())
	}
	other, err := database.GetContentByID(otherID)
	if err != nil || other == nil || other.OCRPending {
		t.Fatalf("other content = %+v, %v", other, err)
	}
}

func TestProcessOCRChainGlobalFailureCancelsRun(t *testing.T) {
	database := openOCRChainTestDB(t)
	_, globalPath := addOCRChainFile(t, database, "global.pdf", 1)
	slowID, slowPath := addOCRChainFile(t, database, "slow.pdf", 1)
	slowStarted := make(chan struct{})
	provider := &fakeRangeProvider{fn: func(ctx context.Context, path string, start, end int) (ocr.RangeResult, error) {
		if path == slowPath {
			close(slowStarted)
			<-ctx.Done()
			// Even a provider that returns late success after cancellation must not
			// cross the final SQLite write boundary.
			return successfulRange("late-success", start, end, 0), nil
		}
		if path == globalPath {
			<-slowStarted
			return ocr.RangeResult{}, ocr.GlobalFailure(errors.New("invalid credentials"))
		}
		panic("unexpected path")
	}}
	providers := testProviderChain(modelMistral, "requested", provider, 4)
	var out, errOut bytes.Buffer
	err := processOCRChain(
		context.Background(), database, providers, []string{modelMistral}, 0, &out, &errOut,
	)
	if err == nil || !strings.Contains(err.Error(), "invalid credentials") {
		t.Fatalf("processOCRChain() error = %v", err)
	}
	var slowPages int
	if queryErr := database.QueryRow(
		`SELECT COUNT(*) FROM pages WHERE content_id = ?`, slowID,
	).Scan(&slowPages); queryErr != nil {
		t.Fatal(queryErr)
	}
	if slowPages != 0 {
		t.Fatalf("late pages committed after global cancellation = %d", slowPages)
	}
}

func TestProcessOCRChainRejectsChecksumMutationBeforeCommit(t *testing.T) {
	database := openOCRChainTestDB(t)
	contentID, path := addOCRChainFile(t, database, "mutation.pdf", 1)
	provider := &fakeRangeProvider{fn: func(_ context.Context, path string, start, end int) (ocr.RangeResult, error) {
		if err := os.WriteFile(path, []byte("changed during OCR"), 0o600); err != nil {
			return ocr.RangeResult{}, err
		}
		return successfulRange("exact", start, end, 0), nil
	}}
	providers := testProviderChain(modelMistral, "requested", provider, 4)
	var out, errOut bytes.Buffer
	err := processOCRChain(
		context.Background(), database, providers, []string{modelMistral}, 0, &out, &errOut,
	)
	if err == nil {
		t.Fatal("processOCRChain() error = nil")
	}
	var pages int
	if queryErr := database.QueryRow(`SELECT COUNT(*) FROM pages WHERE content_id = ?`, contentID).Scan(&pages); queryErr != nil {
		t.Fatal(queryErr)
	}
	if pages != 0 || !strings.Contains(errOut.String(), "changed during OCR input processing") {
		t.Fatalf("pages = %d, stderr = %q, path = %s", pages, errOut.String(), path)
	}
}

func TestProcessOCRRangeRevalidatesChecksumInsideWriteBoundary(t *testing.T) {
	database := openOCRChainTestDB(t)
	contentID, path := addOCRChainFile(t, database, "locked-mutation.pdf", 1)
	content, err := database.GetContentByID(contentID)
	if err != nil || content == nil {
		t.Fatalf("content = %+v, %v", content, err)
	}
	providerReturned := make(chan struct{})
	provider := &fakeRangeProvider{fn: func(context.Context, string, int, int) (ocr.RangeResult, error) {
		close(providerReturned)
		return successfulRange("exact", 0, 1, 0), nil
	}}
	providers := testProviderChain(modelMistral, "requested", provider, 4)
	var writeMu sync.Mutex
	writeMu.Lock()
	var errOut bytes.Buffer
	outcomeCh := make(chan rangeOutcome, 1)
	go func() {
		outcomeCh <- processOCRRange(
			context.Background(), database, *content, path, "pdf",
			db.PageRange{Start: 0, End: 1}, 0, providers, []string{modelMistral},
			&writeMu, newOCRRunTotals(), &errOut,
		)
	}()
	<-providerReturned
	if err := os.WriteFile(path, []byte("changed while waiting for write"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeMu.Unlock()
	outcome := <-outcomeCh
	if !outcome.stopContent || outcome.globalErr != nil {
		t.Fatalf("outcome = %+v", outcome)
	}
	var pages int
	if err := database.QueryRow(`SELECT COUNT(*) FROM pages WHERE content_id = ?`, contentID).Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if pages != 0 {
		t.Fatalf("committed pages = %d, want 0", pages)
	}
}

func TestProcessOCRChainFixedProviderConcurrency(t *testing.T) {
	for _, test := range []struct {
		model string
		limit int
		items int
	}{
		{model: modelMistral, limit: mistralConcurrency, items: 12},
		{model: modelGemini, limit: geminiConcurrency, items: 25},
	} {
		t.Run(test.model, func(t *testing.T) {
			database := openOCRChainTestDB(t)
			for i := 0; i < test.items; i++ {
				_, _ = addOCRChainFile(t, database, fmt.Sprintf("%s-%d.png", test.model, i), 1)
			}
			started := make(chan struct{}, test.items)
			release := make(chan struct{})
			provider := successfulRangeProvider(test.model+"-exact", 0)
			provider.fn = func(ctx context.Context, _ string, start, end int) (ocr.RangeResult, error) {
				started <- struct{}{}
				select {
				case <-release:
				case <-ctx.Done():
					return ocr.RangeResult{}, ctx.Err()
				}
				return successfulRange(test.model+"-exact", start, end, 0), nil
			}
			providers := testProviderChain(test.model, test.model+"-requested", provider, test.limit)
			var out, errOut bytes.Buffer
			done := make(chan error, 1)
			go func() {
				done <- processOCRChain(
					context.Background(), database, providers, []string{test.model}, 0, &out, &errOut,
				)
			}()
			for i := 0; i < test.limit; i++ {
				select {
				case <-started:
				case <-time.After(5 * time.Second):
					t.Fatalf("timed out after %d of %d provider calls", i, test.limit)
				}
			}
			select {
			case <-started:
				t.Fatalf("%s exceeded fixed concurrency %d", test.model, test.limit)
			case <-time.After(20 * time.Millisecond):
			}
			close(release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			provider.mu.Lock()
			peak := provider.peak
			provider.mu.Unlock()
			if peak != test.limit {
				t.Fatalf("peak %s concurrency = %d, want fixed limit %d", test.model, peak, test.limit)
			}
		})
	}
}

func TestProcessOCRChainIgnoresUnselectedPendingContentForExitStatus(t *testing.T) {
	database := openOCRChainTestDB(t)
	_, _ = addOCRChainFile(t, database, "selected.png", 1)
	_, _ = addOCRChainFile(t, database, "unselected.png", 1)
	provider := successfulRangeProvider("exact", 0)
	providers := testProviderChain(modelMistral, "requested", provider, 4)
	var out, errOut bytes.Buffer
	if err := processOCRChain(
		context.Background(), database, providers, []string{modelMistral}, 1, &out, &errOut,
	); err != nil {
		t.Fatalf("processOCRChain() error = %v", err)
	}
	pending, err := database.PendingContents()
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %+v, %v; want unselected item only", pending, err)
	}
}

func openOCRChainTestDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "chain.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func addOCRChainFile(t *testing.T, database *db.DB, name string, pages int) (int64, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("stable "+name), 0o600); err != nil {
		t.Fatal(err)
	}
	digest, err := checksum.SHA256File(path)
	if err != nil {
		t.Fatal(err)
	}
	contentID, err := database.InsertContent(digest, pages)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := database.InsertDocument(path, contentID, now, now); err != nil {
		t.Fatal(err)
	}
	return contentID, path
}

func successfulRangeProvider(model string, cost ocr.Currency) *fakeRangeProvider {
	return &fakeRangeProvider{fn: func(_ context.Context, _ string, start, end int) (ocr.RangeResult, error) {
		return successfulRange(model, start, end, cost), nil
	}}
}

func successfulRange(model string, start, end int, cost ocr.Currency) ocr.RangeResult {
	pages := make([]ocr.PageResult, 0, end-start)
	for index := start; index < end; index++ {
		pages = append(pages, ocr.PageResult{
			PageIndex: index,
			Markdown:  fmt.Sprintf("page %d", index),
			Model:     model,
		})
	}
	return ocr.RangeResult{Pages: pages, Billing: ocr.BillingReport{KnownCost: cost}}
}

func testProviderChain(
	selector, exact string,
	provider ocr.Provider,
	limit int,
) ocrProviderChain {
	return ocrProviderChain{
		selector: {
			defaultModel: exact,
			provider:     provider,
			slots:        make(chan struct{}, limit),
		},
	}
}
