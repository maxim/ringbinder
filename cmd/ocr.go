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
	var contents []db.Content
	var err error
	if redo {
		contents, err = allLiveContents(database)
	} else {
		contents, err = database.PendingContents()
	}
	if err != nil {
		return fmt.Errorf("query contents: %w", err)
	}

	if len(contents) == 0 {
		if redo {
			fmt.Println("No documents found.")
		} else {
			fmt.Println("No documents pending OCR.")
		}
		return nil
	}

	fmt.Printf("Processing %d content item(s)...\n", len(contents))
	var totalPages, processedOK, skipped, failed int

	for i, content := range contents {
		path, err := database.GetDocumentPathForContent(content.ID)
		if err != nil {
			return fmt.Errorf("query document path for content %d: %w", content.ID, err)
		}
		if path == "" {
			skipped++
			continue
		}

		fmt.Printf("Processing %d/%d: %s...\n", i+1, len(contents), filepath.Base(path))

		fileType := classifyPath(path)
		if fileType == "" {
			fmt.Printf("  skipping: unknown file type\n")
			skipped++
			continue
		}

		pages, err := provider.OCRFile(ctx, path, fileType)
		if err != nil {
			fmt.Printf("  error: %v\n", err)
			failed++
			continue
		}

		pageInputs := make([]db.PageInput, len(pages))
		for j, page := range pages {
			pageInputs[j] = db.PageInput{
				PageIndex: page.PageIndex,
				Markdown:  page.Markdown,
			}
		}

		if err := database.ReplaceContentPages(content.ID, pageInputs); err != nil {
			return fmt.Errorf("replace content pages for %s: %w", path, err)
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

func allLiveContents(database *db.DB) ([]db.Content, error) {
	docs, err := database.AllDocuments()
	if err != nil {
		return nil, err
	}

	seen := make(map[int64]bool)
	contents := make([]db.Content, 0, len(docs))
	for _, doc := range docs {
		if seen[doc.ContentID] {
			continue
		}
		seen[doc.ContentID] = true
		contents = append(contents, db.Content{
			ID:         doc.ContentID,
			Checksum:   doc.Checksum,
			PageCount:  doc.PageCount,
			OCRPending: doc.OCRPending,
		})
	}
	return contents, nil
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
