package cmd

import (
	"fmt"
	"strings"
	"time"

	"github.com/maxim/ringbinder/internal/db"
	"github.com/maxim/ringbinder/internal/ocr"
	"github.com/spf13/cobra"
)

// These intentionally approximate medium-resolution PDF input, high-resolution
// image input, and average candidate-plus-thinking output. Request overhead uses
// nominal 20-page chunks; byte-driven splits remain actual-cost variability.
const (
	geminiPDFMediaTokens   = 560
	geminiImageMediaTokens = 1_120
	geminiOutputTokens     = 1_200
	geminiRequestOverhead  = 250
	geminiPDFPagesPerChunk = 20
)

func init() {
	costCmd.Flags().StringArray("model", nil, "OCR model in priority order (repeat: mistral or gemini)")
	costCmd.Flags().Int("limit", 0, "Maximum number of pending content items to estimate")
	rootCmd.AddCommand(costCmd)
}

var costCmd = &cobra.Command{
	Use:   "cost",
	Short: "Estimate OCR cost for documents",
	Long:  "Shows the selected unfinished documents and an estimated cost range for the chosen OCR models.",
	RunE:  runCost,
}

type costEstimate struct {
	items      int
	totalItems int
	pages      int
	excluded   int
	cost       ocr.Currency
	low        ocr.Currency
	high       ocr.Currency
	truncated  bool
}

func runCost(cmd *cobra.Command, args []string) error {
	cfg, err := loadCommandConfig(cmd, "model")
	if err != nil {
		return err
	}
	settings, err := resolveOCRSettings(cmd, cfg)
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

	estimate, err := estimateOCRChainCost(database, settings.models, limit, time.Now().UTC())
	if err != nil {
		return err
	}
	fmt.Printf("OCR models: %s\n", strings.Join(settings.models, " -> "))
	if estimate.excluded > 0 {
		fmt.Printf("Excluded from estimate: %d content item(s) already managed by batch OCR\n", estimate.excluded)
	}
	if estimate.items == 0 {
		fmt.Println("No documents pending OCR.")
		return nil
	}
	if estimate.truncated {
		fmt.Printf("Selected: %d of %d pending content item(s), %d missing pages\n",
			estimate.items, estimate.totalItems, estimate.pages)
	} else {
		fmt.Printf("Selected: %d pending content item(s), %d missing pages\n", estimate.items, estimate.pages)
	}
	fmt.Printf(
		"Estimated cost range: %s–%s\n",
		formatApproxCurrency(estimate.low), formatApproxCurrency(estimate.high),
	)
	fmt.Println("High estimate assumes every model processes every missing page and adds 5% for retries; actual cost may be higher.")
	return nil
}

func estimateOCRChainCost(
	database *db.DB,
	models []string,
	limit int,
	at time.Time,
) (costEstimate, error) {
	batch, err := pendingContentBatch(database, limit)
	if err != nil {
		return costEstimate{}, fmt.Errorf("query contents: %w", err)
	}
	estimate := costEstimate{
		items:      len(batch.contents),
		totalItems: batch.total,
		excluded:   batch.excluded,
		truncated:  batch.truncated,
	}
	for _, content := range batch.contents {
		missing, err := database.MissingPageIndexes(content.ID)
		if err != nil {
			return costEstimate{}, fmt.Errorf("query missing pages for content %d: %w", content.ID, err)
		}
		estimate.pages += len(missing)
	}
	if len(models) == 0 || estimate.items == 0 {
		return estimate, nil
	}

	modelCosts := make([]ocr.Currency, len(models))
	for i, model := range models {
		modelCosts[i], err = estimateModelCost(database, batch.contents, model, at)
		if err != nil {
			return costEstimate{}, err
		}
	}
	estimate.low = modelCosts[0]
	for _, cost := range modelCosts {
		estimate.high += cost
	}
	// Round upward in billionths of a dollar so the documented 5% retry
	// assumption is never understated by integer division.
	estimate.high = (estimate.high*105 + 99) / 100
	estimate.cost = estimate.low
	return estimate, nil
}

func estimateModelCost(
	database *db.DB,
	contents []db.Content,
	model string,
	at time.Time,
) (ocr.Currency, error) {
	var pages int
	var inputTokens, outputTokens int64
	for _, content := range contents {
		missing, err := database.MissingPageIndexes(content.ID)
		if err != nil {
			return 0, fmt.Errorf("query missing pages for content %d: %w", content.ID, err)
		}
		if len(missing) == 0 {
			continue
		}
		pages += len(missing)
		if model != modelGemini {
			continue
		}
		path, err := database.GetDocumentPathForContent(content.ID)
		if err != nil {
			return 0, fmt.Errorf("query document path for content %d: %w", content.ID, err)
		}
		outputTokens += int64(len(missing) * geminiOutputTokens)
		if classifyPath(path) != "pdf" {
			inputTokens += geminiImageMediaTokens + geminiRequestOverhead
			continue
		}
		ranges, err := database.MissingPageRanges(content.ID)
		if err != nil {
			return 0, fmt.Errorf("query missing ranges for content %d: %w", content.ID, err)
		}
		requests := 0
		for _, pageRange := range ranges {
			count := pageRange.End - pageRange.Start
			requests += (count + geminiPDFPagesPerChunk - 1) / geminiPDFPagesPerChunk
		}
		inputTokens += int64(len(missing)*geminiPDFMediaTokens + requests*geminiRequestOverhead)
	}
	if model == modelGemini {
		return ocr.GeminiCost(at, inputTokens, outputTokens), nil
	}
	return ocr.MistralCost(pages), nil
}

func formatApproxCurrency(cost ocr.Currency) string {
	cents := (cost + 5_000_000) / 10_000_000
	return fmt.Sprintf("$%d.%02d", cents/100, cents%100)
}
