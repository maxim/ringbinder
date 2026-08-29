package cmd

import (
	"fmt"
	"os"
	"time"

	"github.com/maxim/ringbinder/internal/checksum"
	"github.com/maxim/ringbinder/internal/db"
	"github.com/maxim/ringbinder/internal/ocr"
	"github.com/spf13/cobra"
)

var newGeminiDirectProvider = func(apiKey string, runAt time.Time) ocr.Provider {
	return ocr.NewGeminiClient(apiKey, runAt)
}

func runBatchRetry(cmd *cobra.Command, args []string) error {
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
	path, err := matchingContentPath(command.database, request.ContentID, request.Checksum)
	if err != nil {
		return err
	}
	fileType := classifyPath(path)
	if fileType == "" {
		return fmt.Errorf("unsupported OCR file type: %s", path)
	}

	runAt := time.Now().UTC()
	provider := newGeminiDirectProvider(command.apiKey, runAt)
	missing, err := missingRequestRanges(command.database, *request)
	if err != nil {
		return err
	}
	var billing ocr.BillingReport
	var finalPages []db.PageInput
	for rangeIndex, pageRange := range missing {
		result, ocrErr := provider.OCRRangeResult(
			cmd.Context(), path, fileType, pageRange.Start, pageRange.End,
		)
		billing.Add(result.Billing)
		if len(result.Pages) > 0 {
			if err := validateOCRRangePages(pageRange, result.Pages, ocrErr == nil); err != nil {
				printBatchBilling("Direct retry", billing)
				return err
			}
			if err := verifyContentPath(path, request.Checksum); err != nil {
				printBatchBilling("Direct retry", billing)
				return err
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
			} else if err := command.database.UpsertContentPages(request.ContentID, inputs); err != nil {
				printBatchBilling("Direct retry", billing)
				return fmt.Errorf("store direct Gemini retry pages: %w", err)
			}
		}
		if ocrErr != nil {
			printBatchBilling("Direct retry", billing)
			return ocrErr
		}
	}
	printBatchBilling("Direct retry", billing)
	contentComplete, err := command.database.CompleteGeminiDirectRequest(
		request.ID, finalPages, int64(billing.KnownCost), billing.Indeterminate, time.Now().UTC(),
	)
	if err != nil {
		return fmt.Errorf("complete direct Gemini retry: %w", err)
	}
	if _, err := command.database.RetireCompletedGeminiRequests(); err != nil {
		return fmt.Errorf("retire completed Gemini requests: %w", err)
	}
	status := "content item remains pending"
	if contentComplete {
		status = "OCR completed for its content item"
	}
	fmt.Printf("Retried Gemini request %d directly; %s.\n", request.ID, status)
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

func matchingContentPath(database *db.DB, contentID int64, expectedChecksum string) (string, error) {
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

func verifyContentPath(path, expectedChecksum string) error {
	actual, err := checksum.SHA256File(path)
	if err != nil {
		return fmt.Errorf("recheck %s: %w", path, err)
	}
	if actual != expectedChecksum {
		return fmt.Errorf("%s changed during OCR input processing; run ringbinder sweep", path)
	}
	return nil
}

func printBatchBilling(label string, report ocr.BillingReport) {
	if report.KnownCost == 0 && !report.Indeterminate {
		return
	}
	if report.Indeterminate {
		fmt.Fprintf(os.Stdout, "%s known cost: %s (actual cost may be higher)\n", label, ocr.FormatCurrency(report.KnownCost))
		return
	}
	fmt.Fprintf(os.Stdout, "%s cost: %s\n", label, ocr.FormatCurrency(report.KnownCost))
}
