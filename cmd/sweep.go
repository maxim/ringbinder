package cmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/maxim/ringbinder/internal/checksum"
	"github.com/maxim/ringbinder/internal/config"
	"github.com/maxim/ringbinder/internal/db"
	"github.com/maxim/ringbinder/internal/pdf"
	"github.com/maxim/ringbinder/internal/progress"
	"github.com/maxim/ringbinder/internal/scanner"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(sweepCmd)
	sweepCmd.Flags().Bool("redo", false, "Delete all data and re-sweep from scratch")
}

var sweepCmd = &cobra.Command{
	Use:   "sweep [paths...]",
	Short: "Scan filesystem paths for PDFs and images",
	Long:  "Scans the given paths (or paths from config) for PNG, JPEG, and PDF files and indexes them in the database.",
	RunE:  runSweep,
}

func runSweep(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	paths := args
	if len(paths) == 0 {
		paths = cfg.Paths
	}
	if len(paths) == 0 {
		return fmt.Errorf("no paths provided and none configured in %s", config.DefaultPath())
	}

	// Resolve to absolute paths
	for i, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return fmt.Errorf("resolve path %q: %w", p, err)
		}
		paths[i] = abs
	}

	database, err := db.Open(config.DefaultDir())
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	redo, err := cmd.Flags().GetBool("redo")
	if err != nil {
		return fmt.Errorf("read redo flag: %w", err)
	}
	if redo {
		var documentCount int
		if err := database.QueryRow("SELECT COUNT(*) FROM documents").Scan(&documentCount); err != nil {
			return fmt.Errorf("count documents: %w", err)
		}

		fmt.Printf("This will delete all %d documents and their OCR data. Continue? [y/N] ", documentCount)
		reader := bufio.NewReader(cmd.InOrStdin())
		response, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			return fmt.Errorf("read confirmation: %w", err)
		}
		response = strings.TrimSpace(response)
		if response != "y" && response != "Y" {
			fmt.Println("Aborted.")
			return nil
		}

		deletedCount, err := database.ResetAllDocuments()
		if err != nil {
			return fmt.Errorf("reset documents: %w", err)
		}
		fmt.Printf("Deleted %d documents.\n", deletedCount)
	}

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	s := scanner.NewScanner()
	results := make(chan scanner.FileInfo, 100)

	// Run scan in background
	scanErr := make(chan error, 1)
	go func() {
		scanErr <- s.Scan(ctx, paths, results)
	}()

	isTTY := isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd())
	var scanned atomic.Int64
	if isTTY {
		var spinnerPtr atomic.Pointer[progress.Spinner]
		sweepSpinner := progress.NewSpinner(80*time.Millisecond, func() {
			spinner := spinnerPtr.Load()
			frame := ' '
			if spinner != nil {
				frame = spinner.Frame()
			}
			fmt.Fprintf(os.Stdout, "\r%c %d files scanned   ", frame, scanned.Load())
		})
		spinnerPtr.Store(sweepSpinner)
		defer func() {
			sweepSpinner.Stop()
			fmt.Fprintf(os.Stdout, "\r                                                                                \r")
		}()
	}

	var newCount, updatedCount, restoredCount, unchangedCount int
	seenPaths := make(map[string]bool)

	for fi := range results {
		scanned.Add(1)

		seenPaths[fi.Path] = true

		// Compute checksum
		cs, err := checksum.SHA256File(fi.Path)
		if err != nil {
			fmt.Printf("  warning: checksum failed for %s: %v\n", fi.Path, err)
			continue
		}

		// Count pages
		pageCount := 1
		if fi.ContentType == "pdf" {
			pc, err := pdf.PageCount(fi.Path)
			if err != nil {
				fmt.Printf("  warning: page count failed for %s: %v\n", fi.Path, err)
				continue
			}
			pageCount = pc
		}

		// Check existing record
		existing, err := database.GetDocumentByPath(fi.Path)
		if err != nil {
			return fmt.Errorf("query document: %w", err)
		}

		desiredPending := true
		checksumChanged := false
		mtimeChanged := false
		if existing != nil {
			checksumChanged = existing.Checksum != cs
			mtimeChanged = !existing.ModifiedAt.Equal(fi.ModTime)
			desiredPending = existing.OCRPending || (checksumChanged && mtimeChanged)
		}

		content, err := database.GetContentByChecksum(cs)
		if err != nil {
			return fmt.Errorf("query content: %w", err)
		}

		if content == nil {
			contentID, err := database.InsertContent(cs, pageCount)
			if err != nil {
				return fmt.Errorf("insert content: %w", err)
			}
			content = &db.Content{
				ID:         contentID,
				Checksum:   cs,
				PageCount:  pageCount,
				OCRPending: true,
			}

			if !desiredPending {
				if err := database.MarkContentOCRDone(contentID); err != nil {
					return fmt.Errorf("mark content OCR done: %w", err)
				}
				content.OCRPending = false
			}
		}

		if existing == nil {
			// New file
			if _, err := database.InsertDocument(fi.Path, content.ID, fi.ModTime, fi.ModTime); err != nil {
				return fmt.Errorf("insert document: %w", err)
			}
			newCount++
		} else if existing.Deleted {
			// Was soft-deleted, now reappeared
			if err := database.RestoreDocument(existing.ID, content.ID, fi.ModTime); err != nil {
				return fmt.Errorf("restore document: %w", err)
			}
			restoredCount++
		} else {
			contentChanged := existing.ContentID != content.ID
			if checksumChanged || mtimeChanged || contentChanged {
				if err := database.UpdateDocument(existing.ID, content.ID, fi.ModTime); err != nil {
					return fmt.Errorf("update document: %w", err)
				}
				if checksumChanged {
					updatedCount++
				} else {
					unchangedCount++
				}
			} else {
				unchangedCount++
			}
		}
	}

	if err := <-scanErr; err != nil {
		return fmt.Errorf("scan: %w", err)
	}

	// Soft-delete files no longer present
	deletedCount, err := database.SoftDeleteMissing(seenPaths, paths)
	if err != nil {
		return fmt.Errorf("soft delete: %w", err)
	}
	if _, err := database.CleanupOrphanContents(); err != nil {
		return fmt.Errorf("cleanup orphan contents: %w", err)
	}

	fmt.Printf("Sweep complete: %d new, %d updated, %d restored, %d deleted, %d unchanged\n",
		newCount, updatedCount, restoredCount, deletedCount, unchangedCount)

	return nil
}
