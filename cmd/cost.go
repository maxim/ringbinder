package cmd

import (
	"fmt"

	"github.com/maxim/ringbinder/internal/config"
	"github.com/maxim/ringbinder/internal/db"
	"github.com/maxim/ringbinder/internal/ocr"
	"github.com/spf13/cobra"
)

func init() {
	costCmd.Flags().Bool("redo", false, "Estimate cost for all documents, not just pending ones")
	rootCmd.AddCommand(costCmd)
}

var costCmd = &cobra.Command{
	Use:   "cost",
	Short: "Estimate OCR cost for documents",
	Long:  "Shows the number of pending documents and pages, and estimates the Mistral OCR API cost.\nUse --redo to estimate the cost of processing all documents.",
	RunE:  runCost,
}

func runCost(cmd *cobra.Command, args []string) error {
	database, err := db.Open(config.DefaultDir())
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	redo, err := cmd.Flags().GetBool("redo")
	if err != nil {
		return fmt.Errorf("read --redo flag: %w", err)
	}

	var docCount, totalPages int
	if redo {
		docCount, totalPages, err = database.AllStats()
	} else {
		docCount, totalPages, err = database.PendingStats()
	}
	if err != nil {
		return fmt.Errorf("query stats: %w", err)
	}

	if docCount == 0 {
		if redo {
			fmt.Println("No documents found.")
		} else {
			fmt.Println("No documents pending OCR.")
		}
		return nil
	}

	price := ocr.MistralPricePerPage()
	cost := float64(totalPages) * price

	label := "Pending OCR"
	if redo {
		label = "All documents"
	}
	fmt.Printf("%s: %d documents, %d pages\n", label, docCount, totalPages)
	fmt.Printf("Estimated cost: $%.2f (at $%.4f/page)\n", cost, price)

	return nil
}
