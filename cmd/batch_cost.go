package cmd

import (
	"fmt"
	"time"

	"github.com/maxim/ringbinder/internal/db"
	"github.com/maxim/ringbinder/internal/ocr"
	"github.com/spf13/cobra"
)

func runBatchCost(cmd *cobra.Command, args []string) error {
	limit, err := readOCRLimit(cmd)
	if err != nil {
		return err
	}
	command, err := openGeminiBatchCommand(cmd, false, false)
	if err != nil {
		return err
	}
	defer command.Close()

	estimate, err := estimateGeminiBatchCost(command.database, limit, time.Now().UTC())
	if err != nil {
		return err
	}
	if estimate.items == 0 {
		fmt.Println("No documents have unassigned pages pending Gemini batch OCR.")
		return nil
	}
	if estimate.truncated {
		fmt.Printf(
			"Pending Gemini batch: %d of %d content item(s), %d pages\n",
			estimate.items, estimate.totalItems, estimate.pages,
		)
	} else {
		fmt.Printf("Pending Gemini batch: %d content item(s), %d pages\n", estimate.items, estimate.pages)
	}
	fmt.Printf("Estimated discounted cost: ~%s (actual cost may vary)\n", formatApproxCurrency(estimate.cost))
	return nil
}

func estimateGeminiBatchCost(database *db.DB, limit int, at time.Time) (costEstimate, error) {
	contents, err := database.PendingContentsForGeminiBatch()
	if err != nil {
		return costEstimate{}, fmt.Errorf("query pending contents with unassigned pages: %w", err)
	}
	estimate := costEstimate{items: len(contents), totalItems: len(contents)}
	if limit > 0 && limit < len(contents) {
		contents = contents[:limit]
		estimate.items = len(contents)
		estimate.truncated = true
	}
	var inputTokens, outputTokens int64
	for _, content := range contents {
		path, err := database.GetDocumentPathForContent(content.ID)
		if err != nil {
			return costEstimate{}, fmt.Errorf("query document path for content %d: %w", content.ID, err)
		}
		fileType := classifyPath(path)
		if fileType == "" {
			continue
		}
		ranges, err := database.MissingUnownedPageRanges(content.ID)
		if err != nil {
			return costEstimate{}, fmt.Errorf("query missing ranges for content %d: %w", content.ID, err)
		}
		pages := 0
		requests := 0
		for _, pageRange := range ranges {
			count := pageRange.End - pageRange.Start
			pages += count
			requests += (count + geminiPDFPagesPerChunk - 1) / geminiPDFPagesPerChunk
		}
		estimate.pages += pages
		outputTokens += int64(pages * geminiOutputTokens)
		if fileType == "pdf" {
			inputTokens += int64(pages*geminiPDFMediaTokens + requests*geminiRequestOverhead)
		} else if pages > 0 {
			inputTokens += geminiImageMediaTokens + geminiRequestOverhead
		}
	}
	estimate.cost = ocr.GeminiBatchCost(at, inputTokens, outputTokens)
	return estimate, nil
}
