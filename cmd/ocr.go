package cmd

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/maxim/ringbinder/internal/config"
	"github.com/maxim/ringbinder/internal/db"
	"github.com/maxim/ringbinder/internal/ocr"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(ocrCmd)
}

var ocrCmd = &cobra.Command{
	Use:   "ocr",
	Short: "Run OCR on all pending documents",
	Long:  "Processes all documents marked as OCR-pending through the Mistral OCR API and stores extracted text.",
	RunE:  runOCR,
}

func runOCR(cmd *cobra.Command, args []string) error {
	database, err := db.Open(config.DefaultDir())
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	client, err := ocr.NewMistralClientFromEnv()
	if err != nil {
		return err
	}

	return processOCR(cmd.Context(), database, client)
}

func processOCR(ctx context.Context, database *db.DB, provider ocr.Provider) error {
	docs, err := database.PendingDocuments()
	if err != nil {
		return fmt.Errorf("query pending documents: %w", err)
	}

	if len(docs) == 0 {
		fmt.Println("No documents pending OCR.")
		return nil
	}

	fmt.Printf("Processing %d documents...\n", len(docs))
	var totalPages int

	for i, doc := range docs {
		fmt.Printf("Processing %d/%d: %s...\n", i+1, len(docs), filepath.Base(doc.Path))

		fileType := classifyPath(doc.Path)
		if fileType == "" {
			fmt.Printf("  skipping: unknown file type\n")
			continue
		}

		pages, err := provider.OCRFile(ctx, doc.Path, fileType)
		if err != nil {
			fmt.Printf("  error: %v\n", err)
			continue
		}

		pageInputs := make([]db.PageInput, len(pages))
		for j, page := range pages {
			pageInputs[j] = db.PageInput{
				PageIndex:   page.PageIndex,
				Markdown:    page.Markdown,
				Annotations: page.Annotations,
			}
		}

		if err := database.ReplaceDocumentPages(doc.ID, pageInputs); err != nil {
			return fmt.Errorf("replace document pages: %w", err)
		}

		totalPages += len(pages)
	}

	fmt.Printf("OCR complete: %d documents, %d pages processed.\n", len(docs), totalPages)
	return nil
}

func classifyPath(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".pdf":
		return "pdf"
	case ".jpg", ".jpeg":
		return "jpeg"
	case ".png":
		return "png"
	default:
		return ""
	}
}
