package cmd

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/maxim/ringbinder/internal/db"
	"github.com/maxim/ringbinder/internal/ocr"
	"github.com/maxim/ringbinder/internal/progress"
)

// Regular OCR uses independent fixed service limits. They intentionally are not
// command settings, and a service's internal retries hold the same slot.
const (
	mistralConcurrency = 4
	geminiConcurrency  = 20
)

type modelRunner struct {
	defaultModel string
	provider     ocr.Provider
	slots        chan struct{}
}

type ocrProviderChain map[string]*modelRunner

func newOCRProviderChain(models []string, runAt time.Time) (ocrProviderChain, error) {
	providers := make(ocrProviderChain, len(models))
	for _, model := range models {
		var runner *modelRunner
		switch model {
		case modelMistral:
			provider, err := ocr.NewMistralClientFromEnv()
			if err != nil {
				return nil, err
			}
			runner = &modelRunner{
				defaultModel: ocr.MistralModel,
				provider:     provider,
				slots:        make(chan struct{}, mistralConcurrency),
			}
		case modelGemini:
			provider, err := ocr.NewGeminiClientFromEnv(runAt)
			if err != nil {
				return nil, err
			}
			runner = &modelRunner{
				defaultModel: ocr.GeminiDirectModel,
				provider:     provider,
				slots:        make(chan struct{}, geminiConcurrency),
			}
		default:
			return nil, fmt.Errorf("invalid OCR model %q: allowed values are mistral, gemini", model)
		}
		providers[model] = runner
	}
	return providers, nil
}

type ocrRunTotals struct {
	mu           sync.Mutex
	billing      map[string]ocr.BillingReport
	pagesByModel map[string]int
	logMu        sync.Mutex
}

func newOCRRunTotals() *ocrRunTotals {
	return &ocrRunTotals{
		billing:      make(map[string]ocr.BillingReport),
		pagesByModel: make(map[string]int),
	}
}

func (t *ocrRunTotals) addBilling(model string, report ocr.BillingReport) {
	if report.KnownCost == 0 && !report.Indeterminate {
		return
	}
	t.mu.Lock()
	current := t.billing[model]
	current.Add(report)
	t.billing[model] = current
	t.mu.Unlock()
}

func (t *ocrRunTotals) addPages(pages []ocr.PageResult) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, page := range pages {
		t.pagesByModel[page.Model]++
	}
}

func (t *ocrRunTotals) log(w io.Writer, format string, args ...any) {
	t.logMu.Lock()
	defer t.logMu.Unlock()
	fmt.Fprintf(w, format+"\n", args...)
}

type rangeOutcome struct {
	stopContent bool
	globalErr   error
}

type ocrProgressHooks struct {
	itemPath   func(int64, string)
	pagesSaved func(int)
}

func firstOCRProgressHook(hooks []*ocrProgressHooks) *ocrProgressHooks {
	if len(hooks) == 0 {
		return nil
	}
	return hooks[0]
}

func processOCRChain(
	ctx context.Context,
	database *db.DB,
	providers ocrProviderChain,
	models []string,
	limit int,
	out io.Writer,
	errOut io.Writer,
	coordinators ...*progressCoordinator,
) error {
	var coordinator *progressCoordinator
	if len(coordinators) > 0 {
		coordinator = coordinators[0]
	}

	batch, err := pendingContentBatch(database, limit)
	if err != nil {
		return fmt.Errorf("query contents: %w", err)
	}
	if batch.excluded > 0 {
		fmt.Fprintf(out, "Skipped %d document(s) already managed by batch OCR.\n", batch.excluded)
	}
	if len(batch.contents) == 0 {
		fmt.Fprintln(out, "No documents pending OCR.")
		return nil
	}
	if batch.truncated {
		fmt.Fprintf(out, "Processing %d of %d pending document(s).\n", len(batch.contents), batch.total)
	}

	var preparing *progress.Reporter
	if coordinator != nil {
		preparing = coordinator.StartPhase(progress.PhaseOptions{
			Label: "Preparing OCR", Total: len(batch.contents), Unit: "documents inspected",
		})
	}
	selectedMissing := 0
	for _, content := range batch.contents {
		if contextErr := ctx.Err(); contextErr != nil {
			if preparing != nil {
				coordinator.StopPhase(preparing)
			}
			return contextErr
		}
		missing, queryErr := database.MissingPageIndexes(content.ID)
		if queryErr != nil {
			if preparing != nil {
				coordinator.ClosePhase(preparing)
			}
			return fmt.Errorf("query missing pages for content %d: %w", content.ID, queryErr)
		}
		selectedMissing += len(missing)
		if preparing != nil {
			preparing.Advance()
		}
	}
	if preparing != nil {
		// The selected-documents line below is this phase's durable result.
		coordinator.ClosePhase(preparing)
	}
	fmt.Fprintf(out, "Selected: %d documents, %d missing pages\n", len(batch.contents), selectedMissing)

	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	totals := newOCRRunTotals()
	var writeMu sync.Mutex
	var pagesSaved atomic.Int64
	var processing *progress.Reporter
	var hooks *ocrProgressHooks
	if coordinator != nil {
		processing = coordinator.StartPhase(progress.PhaseOptions{
			Label: "OCR", Total: len(batch.contents), Unit: "documents attempted",
			Detail: fmt.Sprintf("0/%d pages saved", selectedMissing),
		})
		hooks = &ocrProgressHooks{
			itemPath: func(contentID int64, path string) {
				processing.StartItem(strconv.FormatInt(contentID, 10), path)
			},
			pagesSaved: func(count int) {
				saved := pagesSaved.Add(int64(count))
				processing.SetDetail(fmt.Sprintf("%d/%d pages saved", saved, selectedMissing))
			},
		}
	}
	jobs := make(chan db.Content, len(batch.contents))
	for _, content := range batch.contents {
		jobs <- content
	}
	close(jobs)
	workerCount := min(len(batch.contents), mistralConcurrency+geminiConcurrency)
	var workers sync.WaitGroup
	for range workerCount {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				// A buffered queue lets selection finish before providers begin. Check
				// cancellation on both sides of receive so bypassed work is never
				// reported as an attempted document.
				if runCtx.Err() != nil {
					return
				}
				select {
				case <-runCtx.Done():
					return
				case content, open := <-jobs:
					if !open || runCtx.Err() != nil {
						return
					}
					key := strconv.FormatInt(content.ID, 10)
					if processing != nil {
						processing.StartItem(key, "document "+key)
					}
					processContentOCR(
						runCtx, cancel, database, content, providers, models,
						&writeMu, totals, errOut, hooks,
					)
					if processing != nil {
						processing.FinishItem(key)
						processing.Advance()
					}
				}
			}
		}()
	}
	workers.Wait()

	if processing != nil {
		if ctx.Err() != nil {
			coordinator.StopPhase(processing)
		} else {
			// The OCR summary below is this phase's durable result.
			coordinator.ClosePhase(processing)
		}
	}

	completed := 0
	remainingContent := 0
	remainingPages := 0
	for _, content := range batch.contents {
		contentID := content.ID
		missing, queryErr := database.MissingPageIndexes(contentID)
		if queryErr != nil {
			if context.Cause(runCtx) == nil {
				cancel(fmt.Errorf("query remaining pages for content %d: %w", contentID, queryErr))
			}
			continue
		}
		if len(missing) == 0 {
			completed++
			continue
		}
		remainingContent++
		remainingPages += len(missing)
	}

	printOCRRunSummary(out, completed, remainingContent, remainingPages, totals)
	if cause := context.Cause(runCtx); cause != nil {
		return cause
	}
	if remainingPages > 0 {
		return fmt.Errorf(
			"%d selected document(s) remain pending with %d missing page(s)",
			remainingContent, remainingPages,
		)
	}
	return nil
}

func processContentOCR(
	ctx context.Context,
	cancel context.CancelCauseFunc,
	database *db.DB,
	content db.Content,
	providers ocrProviderChain,
	models []string,
	writeMu *sync.Mutex,
	totals *ocrRunTotals,
	errOut io.Writer,
	hooks ...*ocrProgressHooks,
) {
	hook := firstOCRProgressHook(hooks)
	ranges, err := database.MissingPageRanges(content.ID)
	if err != nil {
		cancel(fmt.Errorf("query missing ranges for content %d: %w", content.ID, err))
		return
	}
	if len(ranges) == 0 {
		if err := database.RecomputeContentPending(content.ID); err != nil {
			cancel(fmt.Errorf("recompute OCR status for content %d: %w", content.ID, err))
		}
		return
	}
	path, err := matchingContentPath(database, content.ID, content.Checksum)
	if err != nil {
		totals.log(errOut, "content %d: %v", content.ID, err)
		return
	}
	if hook != nil && hook.itemPath != nil {
		hook.itemPath(content.ID, path)
	}
	fileType := classifyPath(path)
	if fileType == "" {
		totals.log(errOut, "%s: unsupported OCR file type", path)
		return
	}

	for _, pageRange := range ranges {
		if ctx.Err() != nil {
			return
		}
		outcome := processOCRRange(
			ctx, database, content, path, fileType, pageRange, 0,
			providers, models, writeMu, totals, errOut, hooks...,
		)
		if outcome.globalErr != nil {
			cancel(outcome.globalErr)
			return
		}
		if outcome.stopContent {
			return
		}
	}
}

func processOCRRange(
	ctx context.Context,
	database *db.DB,
	content db.Content,
	path, fileType string,
	pageRange db.PageRange,
	modelIndex int,
	providers ocrProviderChain,
	models []string,
	writeMu *sync.Mutex,
	totals *ocrRunTotals,
	errOut io.Writer,
	hooks ...*ocrProgressHooks,
) rangeOutcome {
	hook := firstOCRProgressHook(hooks)
	if err := ctx.Err(); err != nil {
		return rangeOutcome{globalErr: err}
	}
	runner := providers[models[modelIndex]]
	result, providerErr := runOCRRange(ctx, runner, path, fileType, pageRange)
	totals.addBilling(responseModel(result.Pages, runner.defaultModel), result.Billing)

	err := providerErr
	if validationErr := validateOCRRangePages(pageRange, result.Pages, providerErr == nil); validationErr != nil {
		// Malformed output is unclassified even if the service also attached a
		// more permissive error. Do not persist an unverifiable subset.
		err = validationErr
		result.Pages = nil
	}
	if len(result.Pages) > 0 {
		inputs := make([]db.PageInput, len(result.Pages))
		for i, page := range result.Pages {
			model := page.Model
			inputs[i] = db.PageInput{
				PageIndex: page.PageIndex,
				Markdown:  page.Markdown,
				Model:     &model,
			}
		}
		var sourceErr error
		writeErr := replacePagesWhileActive(ctx, writeMu, func() error {
			// Hash inside the write boundary so a worker cannot validate, wait for
			// another SQLite writer, and then commit OCR for changed bytes.
			sourceErr = verifyContentPath(path, content.Checksum)
			if sourceErr != nil {
				return sourceErr
			}
			return database.ReplaceContentPagesDirect(content.ID, inputs, time.Now().UTC())
		})
		if sourceErr != nil {
			err = sourceErr
			result.Pages = nil
		} else if writeErr != nil {
			return rangeOutcome{globalErr: fmt.Errorf("store OCR pages for %s: %w", path, writeErr)}
		} else {
			totals.addPages(result.Pages)
			if hook != nil && hook.pagesSaved != nil {
				hook.pagesSaved(len(result.Pages))
			}
		}
	}
	if err == nil {
		return rangeOutcome{}
	}

	remaining := missingResultRanges(pageRange, result.Pages)
	kind := ocr.ClassifyFailure(err)
	rangeLabel := formatOCRPageRange(pageRange)
	switch kind {
	case ocr.FailureGlobal:
		return rangeOutcome{
			globalErr: fmt.Errorf("%s, %s, %s: %w", path, rangeLabel, runner.defaultModel, err),
		}
	case ocr.FailureTemporary, ocr.FailureUnclassified:
		totals.log(
			errOut,
			"%s, %s: %s failed (%s): %v; leaving missing pages pending",
			path, rangeLabel, runner.defaultModel, kind, err,
		)
		return rangeOutcome{stopContent: true}
	case ocr.FailureDocument:
		if len(result.Pages) > 0 {
			totals.log(
				errOut,
				"%s, %s: %s retained %d page(s) before a permanent failure: %v; isolating missing pages",
				path, rangeLabel, runner.defaultModel, len(result.Pages), err,
			)
			return processOCRSubranges(
				ctx, database, content, path, fileType, remaining, modelIndex,
				providers, models, writeMu, totals, errOut, hooks...,
			)
		}
		if pageRange.End-pageRange.Start > 1 {
			totals.log(
				errOut,
				"%s, %s: %s permanently failed: %v; splitting the range",
				path, rangeLabel, runner.defaultModel, err,
			)
			// Splitting keeps successful pages on the preferred model and sends only
			// irreducibly rejected single pages to the next configured model.
			mid := pageRange.Start + (pageRange.End-pageRange.Start)/2
			return processOCRSubranges(
				ctx, database, content, path, fileType,
				[]db.PageRange{
					{Start: pageRange.Start, End: mid},
					{Start: mid, End: pageRange.End},
				},
				modelIndex, providers, models, writeMu, totals, errOut, hooks...,
			)
		}
		if modelIndex+1 < len(models) {
			next := providers[models[modelIndex+1]]
			totals.log(
				errOut,
				"%s, %s: %s permanently failed: %v; falling back to %s",
				path, rangeLabel, runner.defaultModel, err, next.defaultModel,
			)
			return processOCRRange(
				ctx, database, content, path, fileType, pageRange, modelIndex+1,
				providers, models, writeMu, totals, errOut, hooks...,
			)
		}
		totals.log(
			errOut,
			"%s, %s: %s permanently failed: %v; no OCR models remain",
			path, rangeLabel, runner.defaultModel, err,
		)
		return rangeOutcome{}
	default:
		panic("unknown OCR failure classification")
	}
}

func processOCRSubranges(
	ctx context.Context,
	database *db.DB,
	content db.Content,
	path, fileType string,
	ranges []db.PageRange,
	modelIndex int,
	providers ocrProviderChain,
	models []string,
	writeMu *sync.Mutex,
	totals *ocrRunTotals,
	errOut io.Writer,
	hooks ...*ocrProgressHooks,
) rangeOutcome {
	var combined rangeOutcome
	for _, pageRange := range ranges {
		outcome := processOCRRange(
			ctx, database, content, path, fileType, pageRange, modelIndex,
			providers, models, writeMu, totals, errOut, hooks...,
		)
		if outcome.globalErr != nil || outcome.stopContent {
			return outcome
		}
	}
	return combined
}

func runOCRRange(
	ctx context.Context,
	runner *modelRunner,
	path, fileType string,
	pageRange db.PageRange,
) (ocr.RangeResult, error) {
	select {
	case runner.slots <- struct{}{}:
		defer func() { <-runner.slots }()
	case <-ctx.Done():
		return ocr.RangeResult{}, ctx.Err()
	}
	return runner.provider.OCRRangeResult(ctx, path, fileType, pageRange.Start, pageRange.End)
}

func validateOCRRangePages(
	pageRange db.PageRange,
	pages []ocr.PageResult,
	requireComplete bool,
) error {
	if len(pages) > pageRange.End-pageRange.Start ||
		(requireComplete && len(pages) != pageRange.End-pageRange.Start) {
		return fmt.Errorf(
			"invalid OCR response page count: got %d, want %d",
			len(pages), pageRange.End-pageRange.Start,
		)
	}
	seen := make(map[int]bool, len(pages))
	for _, page := range pages {
		if page.PageIndex < pageRange.Start || page.PageIndex >= pageRange.End || seen[page.PageIndex] {
			return fmt.Errorf("invalid OCR response page index %d", page.PageIndex)
		}
		if page.Model == "" {
			return fmt.Errorf("OCR response page %d omitted its exact model", page.PageIndex)
		}
		seen[page.PageIndex] = true
	}
	return nil
}

func missingResultRanges(pageRange db.PageRange, pages []ocr.PageResult) []db.PageRange {
	present := make(map[int]bool, len(pages))
	for _, page := range pages {
		present[page.PageIndex] = true
	}
	var ranges []db.PageRange
	start := -1
	for index := pageRange.Start; index < pageRange.End; index++ {
		if !present[index] && start < 0 {
			start = index
		}
		if present[index] && start >= 0 {
			ranges = append(ranges, db.PageRange{Start: start, End: index})
			start = -1
		}
	}
	if start >= 0 {
		ranges = append(ranges, db.PageRange{Start: start, End: pageRange.End})
	}
	return ranges
}

func responseModel(pages []ocr.PageResult, fallback string) string {
	if len(pages) == 0 || pages[0].Model == "" {
		return fallback
	}
	model := pages[0].Model
	for _, page := range pages[1:] {
		if page.Model != model {
			return fallback
		}
	}
	return model
}

func formatOCRPageRange(pageRange db.PageRange) string {
	if pageRange.End-pageRange.Start == 1 {
		return fmt.Sprintf("page %d", pageRange.Start+1)
	}
	return fmt.Sprintf("pages %d-%d", pageRange.Start+1, pageRange.End)
}

func printOCRRunSummary(
	out io.Writer,
	completed int,
	remainingContent int,
	remainingPages int,
	totals *ocrRunTotals,
) {
	totals.mu.Lock()
	defer totals.mu.Unlock()
	fmt.Fprintf(out, "Completed this run: %d documents\n", completed)
	fmt.Fprintln(out, "Pages completed this run:")
	pageModels := sortedKeys(totals.pagesByModel)
	if len(pageModels) == 0 {
		fmt.Fprintln(out, "  (none): 0")
	} else {
		for _, model := range pageModels {
			fmt.Fprintf(out, "  %s: %d\n", model, totals.pagesByModel[model])
		}
	}
	fmt.Fprintf(out, "Still pending: %d documents, %d pages\n", remainingContent, remainingPages)

	billingModels := sortedKeys(totals.billing)
	if len(billingModels) == 0 {
		return
	}
	fmt.Fprintln(out, "Actual OCR cost:")
	var total ocr.BillingReport
	for _, model := range billingModels {
		report := totals.billing[model]
		total.Add(report)
		if report.Indeterminate {
			fmt.Fprintf(out, "  %s: %s known (actual may be higher)\n", model, ocr.FormatCurrency(report.KnownCost))
		} else {
			fmt.Fprintf(out, "  %s: %s\n", model, ocr.FormatCurrency(report.KnownCost))
		}
	}
	if total.Indeterminate {
		fmt.Fprintf(out, "  Total: %s known (actual may be higher)\n", ocr.FormatCurrency(total.KnownCost))
	} else {
		fmt.Fprintf(out, "  Total: %s\n", ocr.FormatCurrency(total.KnownCost))
	}
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
