package cmd

import (
	"fmt"
	"time"

	"github.com/maxim/ringbinder/internal/db"
	"github.com/maxim/ringbinder/internal/ocr"
	"github.com/spf13/cobra"
)

// These intentionally approximate medium-resolution PDF input, high-resolution
// image input, and average candidate-plus-thinking output. Request overhead uses
// nominal 20-page chunks; byte-driven splits and retries remain actual-cost
// variability rather than false precision in an offline estimate.
const (
	geminiPDFMediaTokens   = 560
	geminiImageMediaTokens = 1_120
	geminiOutputTokens     = 1_200
	geminiRequestOverhead  = 250
	geminiPDFPagesPerChunk = 20
)

func init() {
	costCmd.Flags().String("model", "", "OCR provider: mistral or gemini")
	costCmd.Flags().Int("limit", 0, "Maximum number of pending content items to estimate")
	rootCmd.AddCommand(costCmd)
}

var costCmd = &cobra.Command{
	Use:   "cost",
	Short: "Estimate OCR cost for documents",
	Long:  "Shows the number of pending content items and pages, and estimates the selected OCR API cost.",
	RunE:  runCost,
}

type costEstimate struct {
	items      int
	totalItems int
	pages      int
	cost       ocr.Currency
	truncated  bool
}

func runCost(cmd *cobra.Command, args []string) error {
	cfg, err := loadCommandConfig(cmd, "model")
	if err != nil {
		return err
	}
	model, err := resolveModel(cmd, cfg)
	if err != nil {
		return err
	}
	limit, err := readOCRLimit(cmd)
	if err != nil {
		return err
	}
	database, err := openDatabaseWithConfig(cmd, cfg)
	if err != nil {
		return err
	}
	defer database.Close()

	estimate, err := estimateOCRCost(database, model, limit, time.Now().UTC())
	if err != nil {
		return err
	}
	if estimate.items == 0 {
		fmt.Println("No documents pending OCR.")
		return nil
	}

	if estimate.truncated {
		fmt.Printf("Pending OCR batch: %d of %d content item(s), %d pages\n",
			estimate.items, estimate.totalItems, estimate.pages)
	} else {
		fmt.Printf("Pending OCR: %d content item(s), %d pages\n", estimate.items, estimate.pages)
	}
	if model == modelGemini {
		fmt.Printf("Estimated cost: ~%s (actual cost may vary)\n", formatApproxCurrency(estimate.cost))
	} else {
		fmt.Printf("Estimated cost: %s (at $0.0050/page)\n", ocr.FormatCurrency(estimate.cost))
	}
	return nil
}

func estimateOCRCost(database *db.DB, model string, limit int, at time.Time) (costEstimate, error) {
	batch, err := pendingContentBatch(database, limit)
	if err != nil {
		return costEstimate{}, fmt.Errorf("query contents: %w", err)
	}

	estimate := costEstimate{
		items:      len(batch.contents),
		totalItems: batch.total,
		truncated:  batch.truncated,
	}
	var inputTokens, outputTokens int64
	for _, content := range batch.contents {
		path, err := database.GetDocumentPathForContent(content.ID)
		if err != nil {
			return costEstimate{}, fmt.Errorf("query document path for content %d: %w", content.ID, err)
		}
		fileType := classifyPath(path)
		if fileType == "" {
			continue
		}

		estimate.pages += content.PageCount
		if model != modelGemini {
			continue
		}
		pages := int64(content.PageCount)
		outputTokens += pages * geminiOutputTokens
		if fileType == "pdf" {
			requests := (pages + geminiPDFPagesPerChunk - 1) / geminiPDFPagesPerChunk
			inputTokens += pages*geminiPDFMediaTokens + requests*geminiRequestOverhead
		} else {
			inputTokens += geminiImageMediaTokens + geminiRequestOverhead
		}
	}

	if model == modelGemini {
		estimate.cost = ocr.GeminiCost(at, inputTokens, outputTokens)
	} else {
		estimate.cost = ocr.MistralCost(estimate.pages)
	}
	return estimate, nil
}

func formatApproxCurrency(cost ocr.Currency) string {
	cents := (cost + 5_000_000) / 10_000_000
	return fmt.Sprintf("$%d.%02d", cents/100, cents%100)
}
