package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/maxim/ringbinder/internal/db"
	"github.com/maxim/ringbinder/internal/ocr"
	"github.com/maxim/ringbinder/internal/progress"
	"github.com/spf13/cobra"
)

func init() {
	ocrCmd.Flags().Bool("redo", false, "Re-OCR all documents, not just pending ones")
	ocrCmd.Flags().String("model", "", "OCR provider: mistral or gemini")
	ocrCmd.Flags().IntP("concurrency", "j", 0, "Number of concurrent OCR workers")
	rootCmd.AddCommand(ocrCmd)
}

var ocrCmd = &cobra.Command{
	Use:   "ocr",
	Short: "Run OCR on documents",
	Long:  "Processes all documents marked as OCR-pending through the selected OCR API and stores extracted text.\nUse --redo to re-process all documents regardless of pending status.",
	RunE:  runOCR,
}

func runOCR(cmd *cobra.Command, args []string) error {
	cfg, err := loadCommandConfig(cmd, "model", "concurrency")
	if err != nil {
		return err
	}
	settings, err := resolveOCRSettings(cmd, cfg)
	if err != nil {
		return err
	}
	database, err := openDatabaseWithConfig(cmd, cfg)
	if err != nil {
		return err
	}
	defer database.Close()

	runAt := time.Now().UTC()
	provider, err := newOCRProvider(settings.model, runAt)
	if err != nil {
		return err
	}

	redo, err := cmd.Flags().GetBool("redo")
	if err != nil {
		return fmt.Errorf("read --redo flag: %w", err)
	}
	return processOCR(cmd.Context(), database, provider, redo, settings.concurrency)
}

func newOCRProvider(model string, runAt time.Time) (ocr.Provider, error) {
	switch model {
	case modelMistral:
		return ocr.NewMistralClientFromEnv()
	case modelGemini:
		return ocr.NewGeminiClientFromEnv(runAt)
	default:
		return nil, fmt.Errorf("invalid OCR model %q: allowed values are mistral, gemini", model)
	}
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
	var billingMu sync.Mutex
	var billing ocr.BillingReport
	attempted := 0
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

			pages, report, err := provider.OCRFile(ctx, job.path, job.fileTyp)
			billingMu.Lock()
			billing.Add(report)
			attempted++
			billingMu.Unlock()
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

			err = replacePagesWhileActive(ctx, &writeMu, func() error {
				return database.ReplaceContentPages(job.content.ID, pageInputs)
			})
			if err != nil {
				tracker.WorkerError(slotID, err)
				return
			}

			tracker.WorkerDone(slotID)
		}(job)
	}
	wg.Wait()
	tracker.Finish()
	if attempted > 0 {
		if billing.Indeterminate {
			fmt.Printf("Known OCR cost: %s (actual cost may be higher)\n", ocr.FormatCurrency(billing.KnownCost))
		} else {
			fmt.Printf("OCR cost: %s\n", ocr.FormatCurrency(billing.KnownCost))
		}
	}
	if err := context.Cause(ctx); err != nil {
		return err
	}
	return nil
}

func replacePagesWhileActive(ctx context.Context, writeMu *sync.Mutex, replace func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	writeMu.Lock()
	defer writeMu.Unlock()

	// Cancellation cannot interrupt a SQLite transaction already in progress,
	// so the last safe boundary is immediately after acquiring the write lock.
	if err := ctx.Err(); err != nil {
		return err
	}
	return replace()
}

func allLiveContents(database *db.DB) ([]db.Content, error) {
	return database.LiveContents()
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
