package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/mattn/go-isatty"
	"github.com/maxim/ringbinder/internal/config"
	"github.com/maxim/ringbinder/internal/db"
	"github.com/maxim/ringbinder/internal/ocr"
	"github.com/maxim/ringbinder/internal/progress"
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

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	type ocrJob struct {
		content db.Content
		path    string
		name    string
		fileTyp string
	}

	var writeMu sync.Mutex
	jobs := make([]ocrJob, 0, len(contents))
	isTTY := isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd())
	tracker := progress.New(os.Stdout, isTTY, len(contents), concurrency)

	for _, content := range contents {
		if err := ctx.Err(); err != nil {
			break
		}

		path, err := database.GetDocumentPathForContent(content.ID)
		if err != nil {
			return fmt.Errorf("query document path for content %d: %w", content.ID, err)
		}
		if path == "" {
			tracker.Skip(fmt.Sprintf("content-%d", content.ID))
			continue
		}

		fileType := classifyPath(path)
		if fileType == "" {
			tracker.Skip(filepath.Base(path))
			continue
		}

		jobs = append(jobs, ocrJob{
			content: content,
			path:    path,
			name:    filepath.Base(path),
			fileTyp: fileType,
		})
	}

	slots := make(chan int, concurrency)
	for i := 0; i < concurrency; i++ {
		slots <- i
	}

	var wg sync.WaitGroup
	for _, job := range jobs {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(job ocrJob) {
			defer wg.Done()

			var slotID int
			select {
			case slotID = <-slots:
			case <-ctx.Done():
				return
			}
			defer func() { slots <- slotID }()
			tracker.WorkerStart(slotID, job.name)

			pages, err := provider.OCRFile(ctx, job.path, job.fileTyp)
			if err != nil {
				if ctx.Err() != nil {
					cancel(err)
				}
				tracker.WorkerError(slotID, err)
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
				tracker.WorkerError(slotID, err)
				return
			}

			tracker.WorkerDone(slotID)
		}(job)
	}
	wg.Wait()
	tracker.Finish()
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
