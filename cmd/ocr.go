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
	ocrCmd.Flags().Bool("redo", false, "Re-OCR all documents, not just pending ones")
	rootCmd.AddCommand(ocrCmd)
}

var ocrCmd = &cobra.Command{
	Use:   "ocr",
	Short: "Run OCR on documents",
	Long:  "Processes all documents marked as OCR-pending through the Mistral OCR API and stores extracted text.\nUse --redo to re-process all documents regardless of pending status.",
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

	redo, err := cmd.Flags().GetBool("redo")
	if err != nil {
		return fmt.Errorf("read --redo flag: %w", err)
	}
	return processOCR(cmd.Context(), database, client, redo)
}

func processOCR(ctx context.Context, database *db.DB, provider ocr.Provider, redo bool) error {
	var docs []db.Document
	var err error
	if redo {
		docs, err = database.AllDocuments()
	} else {
		docs, err = database.PendingDocuments()
	}
	if err != nil {
		return fmt.Errorf("query documents: %w", err)
	}

	if len(docs) == 0 {
		if redo {
			fmt.Println("No documents found.")
		} else {
			fmt.Println("No documents pending OCR.")
		}
		return nil
	}

	fmt.Printf("Processing %d documents...\n", len(docs))
	var totalPages, processedOK, skipped, failed int

	for i, doc := range docs {
		fmt.Printf("Processing %d/%d: %s...\n", i+1, len(docs), filepath.Base(doc.Path))

		fileType := classifyPath(doc.Path)
		if fileType == "" {
			fmt.Printf("  skipping: unknown file type\n")
			skipped++
			continue
		}

		pages, err := provider.OCRFile(ctx, doc.Path, fileType)
		if err != nil {
			fmt.Printf("  error: %v\n", err)
			failed++
			continue
		}

		pageInputs := make([]db.PageInput, len(pages))
		for j, page := range pages {
			pageInputs[j] = db.PageInput{
				PageIndex: page.PageIndex,
				Markdown:  db.BuildPageMarkdown(doc.Path, page.PageIndex, page.Markdown),
			}
		}

		if err := database.ReplaceDocumentPages(doc.ID, pageInputs); err != nil {
			return fmt.Errorf("replace document pages for %s: %w", doc.Path, err)
		}

		totalPages += len(pages)
		processedOK++
	}

	fmt.Printf(
		"OCR complete: %d succeeded, %d skipped, %d failed, %d pages processed.\n",
		processedOK, skipped, failed, totalPages,
	)
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
