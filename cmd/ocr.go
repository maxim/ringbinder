package cmd

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/maxim/ringbinder/internal/config"
	"github.com/maxim/ringbinder/internal/db"
	"github.com/maxim/ringbinder/internal/ocr"
	"github.com/spf13/cobra"
)

func init() {
	ocrCmd.Flags().Bool("redo", false, "Re-OCR all documents, not just pending ones")
	ocrCmd.Flags().IntP("concurrency", "j", 4, "Number of concurrent OCR workers")
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
	concurrency, err := cmd.Flags().GetInt("concurrency")
	if err != nil {
		return fmt.Errorf("read --concurrency flag: %w", err)
	}
	if concurrency < 1 {
		return fmt.Errorf("--concurrency must be >= 1")
	}

	return processOCR(cmd.Context(), database, client, redo, concurrency)
}

func processOCR(ctx context.Context, database *db.DB, provider ocr.Provider, redo bool, concurrency int) error {
	if concurrency < 1 {
		return fmt.Errorf("concurrency must be >= 1")
	}

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
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	type ocrJob struct {
		index   int
		content db.Content
		path    string
		fileTyp string
	}

	var outMu sync.Mutex
	var writeMu sync.Mutex
	var totalPages, processedOK, skipped, failed atomic.Int64
	jobs := make([]ocrJob, 0, len(contents))

	for i, content := range contents {
		if err := ctx.Err(); err != nil {
			break
		}

		path, err := database.GetDocumentPathForContent(content.ID)
		if err != nil {
			return fmt.Errorf("query document path for content %d: %w", content.ID, err)
		}
		if path == "" {
			skipped.Add(1)
			continue
		}

		fileType := classifyPath(path)
		if fileType == "" {
			outMu.Lock()
			fmt.Printf("Processing %d/%d: %s...\n", i+1, len(contents), filepath.Base(path))
			fmt.Printf("  skipping: unknown file type\n")
			outMu.Unlock()
			skipped.Add(1)
			continue
		}

		jobs = append(jobs, ocrJob{
			index:   i,
			content: content,
			path:    path,
			fileTyp: fileType,
		})
	}

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, job := range jobs {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(job ocrJob) {
			defer wg.Done()

			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()

			outMu.Lock()
			fmt.Printf("Processing %d/%d: %s...\n", job.index+1, len(contents), filepath.Base(job.path))
			outMu.Unlock()

			pages, err := provider.OCRFile(ctx, job.path, job.fileTyp)
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					cancel(err)
				}
				outMu.Lock()
				fmt.Printf("  error: %v\n", err)
				outMu.Unlock()
				failed.Add(1)
				return
			}

			pageInputs := make([]db.PageInput, len(pages))
			for i, page := range pages {
				pageInputs[i] = db.PageInput{
					PageIndex: page.PageIndex,
					Markdown:  page.Markdown,
				}
			}

			writeMu.Lock()
			err = database.ReplaceContentPages(job.content.ID, pageInputs)
			writeMu.Unlock()
			if err != nil {
				outMu.Lock()
				fmt.Printf("  error: %v\n", err)
				outMu.Unlock()
				failed.Add(1)
				return
			}

			totalPages.Add(int64(len(pages)))
			processedOK.Add(1)
		}(job)
	}
	wg.Wait()

	fmt.Printf(
		"OCR complete: %d succeeded, %d skipped, %d failed, %d pages processed.\n",
		processedOK.Load(),
		skipped.Load(),
		failed.Load(),
		totalPages.Load(),
	)
	if err := context.Cause(ctx); err != nil {
		return err
	}
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
