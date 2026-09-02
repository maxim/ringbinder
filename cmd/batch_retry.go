package cmd

import (
	"fmt"
	"io"
	"time"

	"github.com/maxim/ringbinder/internal/checksum"
	"github.com/maxim/ringbinder/internal/db"
	"github.com/maxim/ringbinder/internal/ocr"
	"github.com/maxim/ringbinder/internal/progress"
	"github.com/spf13/cobra"
)

var newGeminiDirectProvider = func(apiKey string, runAt time.Time) ocr.Provider {
	return ocr.NewGeminiClient(apiKey, runAt)
}

func runBatchRetry(cmd *cobra.Command, args []string) error {
	ensureCommandContext(cmd)
	requestID, err := parsePositiveID(args[0], "request")
	if err != nil {
		return err
	}
	mode, err := cmd.Flags().GetString("mode")
	if err != nil {
		return fmt.Errorf("read --mode flag: %w", err)
	}
	if mode != "direct" {
		return fmt.Errorf("--mode must be direct; blocked requests cannot return to discounted batching")
	}
	command, err := openGeminiBatchCommand(cmd, true, true)
	if err != nil {
		return err
	}
	defer command.Close()

	out := commandStdout(cmd)
	coordinator := newCommandProgress(cmd)
	defer func() { coordinator.Finish(cmd.Context().Err() != nil) }()

	request, err := command.database.GeminiRequestByID(requestID)
	if err != nil {
		return err
	}
	if request == nil {
		return fmt.Errorf("Gemini request %d not found", requestID)
	}
	if request.State != db.GeminiRequestBlocked {
		return fmt.Errorf("Gemini request %d is not blocked", requestID)
	}

	preparing := coordinator.StartPhase(progress.PhaseOptions{Label: "Preparing direct retry"})
	path, err := matchingContentPath(command.database, request.ContentID, request.Checksum)
	if err != nil {
		coordinator.ClosePhase(preparing)
		return err
	}
	preparing.SetCurrent(path)
	fileType := classifyPath(path)
	if fileType == "" {
		coordinator.ClosePhase(preparing)
		return fmt.Errorf("unsupported OCR file type: %s", path)
	}

	runAt := time.Now().UTC()
	provider := newGeminiDirectProvider(command.apiKey, runAt)
	missing, err := missingRequestRanges(command.database, *request)
	if err != nil {
		coordinator.ClosePhase(preparing)
		return err
	}
	if contextErr := cmd.Context().Err(); contextErr != nil {
		coordinator.StopPhase(preparing)
		return contextErr
	}
	coordinator.CompletePhase(preparing)

	initialMissing := 0
	for _, pageRange := range missing {
		initialMissing += pageRange.End - pageRange.Start
	}
	var retrying *progress.Reporter
	if len(missing) > 0 {
		retrying = coordinator.StartPhase(progress.PhaseOptions{
			Label: "Direct retry OCR", Total: len(missing), Unit: "page ranges",
			Detail: fmt.Sprintf("0/%d pages saved", initialMissing),
		})
	}
	finishRetrying := func(stopped bool) {
		if retrying == nil {
			return
		}
		if stopped {
			coordinator.StopPhase(retrying)
		} else {
			coordinator.ClosePhase(retrying)
		}
		retrying = nil
	}

	var billing ocr.BillingReport
	var finalPages []db.PageInput
	pagesSaved := 0
	fail := func(runErr error) error {
		finishRetrying(cmd.Context().Err() != nil)
		printBatchBilling(out, "Direct retry", billing)
		return runErr
	}
	for rangeIndex, pageRange := range missing {
		result, ocrErr := provider.OCRRangeResult(
			cmd.Context(), path, fileType, pageRange.Start, pageRange.End,
		)
		billing.Add(result.Billing)
		if contextErr := cmd.Context().Err(); contextErr != nil {
			return fail(contextErr)
		}
		if retrying != nil {
			retrying.Advance()
		}
		if len(result.Pages) > 0 {
			if err := validateOCRRangePages(pageRange, result.Pages, ocrErr == nil); err != nil {
				return fail(err)
			}
			if err := verifyContentPath(path, request.Checksum); err != nil {
				return fail(err)
			}
			inputs := make([]db.PageInput, len(result.Pages))
			for i, page := range result.Pages {
				model := page.Model
				inputs[i] = db.PageInput{
					PageIndex: page.PageIndex,
					Markdown:  page.Markdown,
					Model:     &model,
				}
			}
			// Keep earlier partial progress immediately, but commit the final pages
			// with the blocked-to-staged transition so a crash cannot separate them.
			if rangeIndex == len(missing)-1 && ocrErr == nil {
				finalPages = inputs
			} else {
				// A provider may return after cancellation. Do not let that late
				// result cross the direct partial-page write boundary.
				if contextErr := cmd.Context().Err(); contextErr != nil {
					return fail(contextErr)
				}
				if err := command.database.UpsertContentPages(request.ContentID, inputs); err != nil {
					return fail(fmt.Errorf("store direct Gemini retry pages: %w", err))
				}
				pagesSaved += len(inputs)
				if retrying != nil {
					retrying.SetDetail(fmt.Sprintf("%d/%d pages saved", pagesSaved, initialMissing))
				}
			}
		}
		if ocrErr != nil {
			return fail(ocrErr)
		}
	}
	// Check immediately before the final blocked-to-staged transaction for the
	// same late-provider cancellation race as partial writes above.
	if contextErr := cmd.Context().Err(); contextErr != nil {
		return fail(contextErr)
	}
	contentComplete, err := command.database.CompleteGeminiDirectRequest(
		request.ID, finalPages, int64(billing.KnownCost), billing.Indeterminate, time.Now().UTC(),
	)
	if err != nil {
		return fail(fmt.Errorf("complete direct Gemini retry: %w", err))
	}
	pagesSaved += len(finalPages)
	if retrying != nil {
		retrying.SetDetail(fmt.Sprintf("%d/%d pages saved", pagesSaved, initialMissing))
	}
	finishRetrying(false)
	printBatchBilling(out, "Direct retry", billing)
	if _, err := command.database.RetireCompletedGeminiRequests(); err != nil {
		return fmt.Errorf("retire completed Gemini requests: %w", err)
	}
	status := "document remains pending"
	if contentComplete {
		status = "OCR completed for its document"
	}
	fmt.Fprintf(out, "Retried Gemini request %d directly; %s.\n", request.ID, status)
	return nil
}

func missingRequestRanges(database *db.DB, request db.GeminiBatchRequest) ([]db.PageRange, error) {
	ranges, err := database.MissingPageRangesWithin(
		request.ContentID, request.PageStart, request.PageEnd,
	)
	if err != nil {
		return nil, fmt.Errorf("query missing pages for Gemini request %d: %w", request.ID, err)
	}
	return ranges, nil
}

// Function variables let cancellation tests model a non-interruptible source
// check completing after the command context has been cancelled.
var matchingContentPath = func(database *db.DB, contentID int64, expectedChecksum string) (string, error) {
	paths, err := database.GetDocumentPathsForContent(contentID)
	if err != nil {
		return "", fmt.Errorf("query live paths for content %d: %w", contentID, err)
	}
	for _, path := range paths {
		actual, checksumErr := checksum.SHA256File(path)
		if checksumErr != nil {
			continue
		}
		if actual == expectedChecksum {
			return path, nil
		}
	}
	return "", fmt.Errorf("no live path for content %d matches its planned checksum; run ringbinder sweep", contentID)
}

var verifyContentPath = func(path, expectedChecksum string) error {
	actual, err := checksum.SHA256File(path)
	if err != nil {
		return fmt.Errorf("recheck %s: %w", path, err)
	}
	if actual != expectedChecksum {
		return fmt.Errorf("%s changed during OCR input processing; run ringbinder sweep", path)
	}
	return nil
}

func printBatchBilling(out io.Writer, label string, report ocr.BillingReport) {
	if report.KnownCost == 0 && !report.Indeterminate {
		return
	}
	if report.Indeterminate {
		fmt.Fprintf(out, "%s known cost: %s (actual cost may be higher)\n", label, ocr.FormatCurrency(report.KnownCost))
		return
	}
	fmt.Fprintf(out, "%s cost: %s\n", label, ocr.FormatCurrency(report.KnownCost))
}
