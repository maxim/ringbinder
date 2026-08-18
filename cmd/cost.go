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
	costCmd.Flags().Bool("redo", false, "Estimate cost for all documents, not just pending ones")
	costCmd.Flags().String("model", "", "OCR provider: mistral or gemini")
	rootCmd.AddCommand(costCmd)
}

var costCmd = &cobra.Command{
	Use:   "cost",
	Short: "Estimate OCR cost for documents",
	Long:  "Shows the number of pending content items and pages, and estimates the selected OCR API cost.\nUse --redo to estimate the cost of processing all content.",
	RunE:  runCost,
}

type costEstimate struct {
	items int
	pages int
	cost  ocr.Currency
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
	database, err := openDatabaseWithConfig(cmd, cfg)
	if err != nil {
		return err
	}
	defer database.Close()

	redo, err := cmd.Flags().GetBool("redo")
	if err != nil {
		return fmt.Errorf("read --redo flag: %w", err)
	}
	estimate, err := estimateOCRCost(database, model, redo, time.Now().UTC())
	if err != nil {
		return err
	}
	if estimate.items == 0 {
		if redo {
			fmt.Println("No documents found.")
		} else {
			fmt.Println("No documents pending OCR.")
		}
		return nil
	}

	label := "Pending OCR"
	if redo {
		label = "All content"
	}
	fmt.Printf("%s: %d content item(s), %d pages\n", label, estimate.items, estimate.pages)
	if model == modelGemini {
		fmt.Printf("Estimated cost: ~%s (actual cost may vary)\n", formatApproxCurrency(estimate.cost))
	} else {
		fmt.Printf("Estimated cost: %s (at $0.0050/page)\n", ocr.FormatCurrency(estimate.cost))
	}
	return nil
}

func estimateOCRCost(database *db.DB, model string, redo bool, at time.Time) (costEstimate, error) {
	var contents []db.Content
	var err error
	if redo {
		contents, err = database.LiveContents()
	} else {
		contents, err = database.PendingContents()
	}
	if err != nil {
		return costEstimate{}, fmt.Errorf("query contents: %w", err)
	}

	var estimate costEstimate
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

		estimate.items++
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
