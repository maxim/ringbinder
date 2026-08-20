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
	client := ocr.NewGeminiClient(command.apiKey, runAt)
	pages, report, ocrErr := client.OCRRange(
		cmd.Context(), path, fileType, request.PageStart, request.PageEnd,
	)
	printBatchBilling("Direct retry", report)
	if ocrErr != nil {
		return ocrErr
	}
	if err := verifyContentPath(path, request.Checksum); err != nil {
		return err
	}
	staged := make([]db.GeminiStagedPage, len(pages))
	for i, page := range pages {
		staged[i] = db.GeminiStagedPage{PageIndex: page.PageIndex, Markdown: page.Markdown}
	}
	if err := command.database.StageGeminiDirectRequest(
		request.ID, staged, int64(report.KnownCost), report.Indeterminate, time.Now().UTC(),
	); err != nil {
		return fmt.Errorf("stage direct Gemini retry: %w", err)
	}
	promoted, err := command.database.PromoteReadyGeminiContents()
	if err != nil {
		return fmt.Errorf("promote completed Gemini content: %w", err)
	}
	fmt.Printf("Retried Gemini request %d directly; %d content item(s) promoted.\n", request.ID, promoted)
	return nil
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
		return fmt.Errorf("%s changed while preparing Gemini batch input; run ringbinder sweep", path)
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
