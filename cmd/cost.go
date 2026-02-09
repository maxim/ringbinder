package cmd

import (
	"fmt"

	"github.com/maxim/ringbinder/internal/config"
	"github.com/maxim/ringbinder/internal/db"
	"github.com/maxim/ringbinder/internal/ocr"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(costCmd)
}

var costCmd = &cobra.Command{
	Use:   "cost",
	Short: "Estimate OCR cost for pending documents",
	Long:  "Shows the number of pending documents and pages, and estimates the Mistral OCR API cost.",
	RunE:  runCost,
}

func runCost(cmd *cobra.Command, args []string) error {
	database, err := db.Open(config.DefaultDir())
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	docCount, totalPages, err := database.PendingStats()
	if err != nil {
		return fmt.Errorf("query stats: %w", err)
	}

	if docCount == 0 {
		fmt.Println("No documents pending OCR.")
		return nil
	}

	price := ocr.MistralPricePerPage()
	cost := float64(totalPages) * price

	fmt.Printf("Pending OCR: %d documents, %d pages\n", docCount, totalPages)
	fmt.Printf("Estimated cost: $%.2f (at $%.4f/page)\n", cost, price)

	return nil
}
