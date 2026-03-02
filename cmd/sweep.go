package cmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mattn/go-isatty"
	"github.com/maxim/ringbinder/internal/checksum"
	"github.com/maxim/ringbinder/internal/config"
	"github.com/maxim/ringbinder/internal/db"
	"github.com/maxim/ringbinder/internal/pathutil"
	"github.com/maxim/ringbinder/internal/pdf"
	"github.com/maxim/ringbinder/internal/progress"
	"github.com/maxim/ringbinder/internal/scanner"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(sweepCmd)
	sweepCmd.Flags().Bool("redo", false, "Delete all data and re-sweep from scratch")
	sweepCmd.Flags().IntP("concurrency", "j", 4, "Number of concurrent file processing workers")
	sweepCmd.Flags().StringSlice("exclude", nil, "File path or glob patterns to exclude from sweep")
}

var sweepCmd = &cobra.Command{
	Use:   "sweep [paths...]",
	Short: "Scan filesystem paths for PDFs and images",
	Long:  "Scans the given paths (or paths from config) for PNG, JPEG, and PDF files and indexes them in the database.",
	RunE:  runSweep,
}

type sweepResult struct {
	fi        scanner.FileInfo
	checksum  string
	pageCount int
	err       error
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

	resolvedPaths, sweepRoots, pathWarnings, err := pathutil.ResolveGlobs(paths)
	if err != nil {
		return fmt.Errorf("resolve sweep paths: %w", err)
	}
	for _, warning := range pathWarnings {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", warning)
	}
	if len(resolvedPaths) == 0 {
		return fmt.Errorf("no files or directories matched provided sweep paths")
	}

	excludePatterns, err := cmd.Flags().GetStringSlice("exclude")
	if err != nil {
		return fmt.Errorf("read exclude flag: %w", err)
	}
	for i, pattern := range excludePatterns {
		pattern = config.ExpandHome(pattern)
		if !containsGlobMeta(pattern) {
			abs, err := filepath.Abs(pattern)
			if err != nil {
				return fmt.Errorf("resolve exclude path %q: %w", pattern, err)
			}
			info, err := os.Stat(abs)
			if err != nil {
				return fmt.Errorf("stat exclude path %q: %w", pattern, err)
			}
			if info.IsDir() {
				return fmt.Errorf("exclude path %q is a directory; only files are supported", pattern)
			}
			excludePatterns[i] = abs
			continue
		}
		if containsPathSeparator(pattern) && !filepath.IsAbs(pattern) {
			abs, err := filepath.Abs(pattern)
			if err != nil {
				return fmt.Errorf("resolve exclude glob %q: %w", pattern, err)
			}
			pattern = abs
		}
		excludePatterns[i] = pattern
	}
	if len(excludePatterns) > 0 {
		if _, err := pathutil.MatchesAny(resolvedPaths[0], excludePatterns); err != nil {
			return fmt.Errorf("validate exclude patterns: %w", err)
		}
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
	concurrency := 4
	if cmd.Flags().Lookup("concurrency") != nil {
		concurrency, err = cmd.Flags().GetInt("concurrency")
		if err != nil {
			return fmt.Errorf("read concurrency flag: %w", err)
		}
	}
	if concurrency < 1 {
		return fmt.Errorf("--concurrency must be >= 1")
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

	baseCtx := cmd.Context()
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	ctx, cancel := context.WithCancel(baseCtx)
	defer cancel()

	s := scanner.NewScanner()
	results := make(chan scanner.FileInfo, 100)
	processed := make(chan sweepResult, 100)

	// Run scan in background
	scanErr := make(chan error, 1)
	go func() {
		scanErr <- s.Scan(ctx, resolvedPaths, results)
	}()

	isTTY := isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd())
	var scanned atomic.Int64
	stopSweepSpinner := func() {}
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
		var stopOnce sync.Once
		stopSweepSpinner = func() {
			stopOnce.Do(func() {
				sweepSpinner.Stop()
				fmt.Fprintf(os.Stdout, "\r                                                                                \r")
			})
		}
		defer stopSweepSpinner()
	}

	var newCount, updatedCount, restoredCount, unchangedCount int
	seenPaths := make(map[string]bool)

	var workerWg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		workerWg.Add(1)
		go func() {
			defer workerWg.Done()

			for fi := range results {
				scanned.Add(1)

				if len(excludePatterns) > 0 {
					excluded, err := pathutil.MatchesAny(fi.Path, excludePatterns)
					if err != nil {
						processed <- sweepResult{
							fi:  fi,
							err: fmt.Errorf("exclude matching failed for %s: %w", fi.Path, err),
						}
						continue
					}
					if excluded {
						continue
					}
				}

				// If canceled, keep draining scanner output to avoid blocking scanner goroutine.
				if ctx.Err() != nil {
					continue
				}

				res := sweepResult{
					fi:        fi,
					pageCount: 1,
				}

				cs, err := checksum.SHA256File(fi.Path)
				if err != nil {
					res.err = fmt.Errorf("checksum failed for %s: %w", fi.Path, err)
					processed <- res
					continue
				}
				res.checksum = cs

				if fi.ContentType == "pdf" {
					pc, err := pdf.PageCount(fi.Path)
					if err != nil {
						res.err = fmt.Errorf("page count failed for %s: %w", fi.Path, err)
						processed <- res
						continue
					}
					res.pageCount = pc
				}

				processed <- res
			}
		}()
	}

	go func() {
		workerWg.Wait()
		close(processed)
	}()

	var dbErr error

	for res := range processed {
		if dbErr != nil {
			continue
		}

		seenPaths[res.fi.Path] = true

		if res.err != nil {
			fmt.Printf("  warning: %v\n", res.err)
			continue
		}

		// Check existing record
		existing, err := database.GetDocumentByPath(res.fi.Path)
		if err != nil {
			dbErr = fmt.Errorf("query document: %w", err)
			cancel()
			continue
		}

		desiredPending := true
		checksumChanged := false
		mtimeChanged := false
		if existing != nil {
			checksumChanged = existing.Checksum != res.checksum
			mtimeChanged = !existing.ModifiedAt.Equal(res.fi.ModTime)
			desiredPending = existing.OCRPending || (checksumChanged && mtimeChanged)
		}

		content, err := database.GetContentByChecksum(res.checksum)
		if err != nil {
			dbErr = fmt.Errorf("query content: %w", err)
			cancel()
			continue
		}

		if content == nil {
			contentID, err := database.InsertContent(res.checksum, res.pageCount)
			if err != nil {
				dbErr = fmt.Errorf("insert content: %w", err)
				cancel()
				continue
			}
			content = &db.Content{
				ID:         contentID,
				Checksum:   res.checksum,
				PageCount:  res.pageCount,
				OCRPending: true,
			}

			if !desiredPending {
				if err := database.MarkContentOCRDone(contentID); err != nil {
					dbErr = fmt.Errorf("mark content OCR done: %w", err)
					cancel()
					continue
				}
				content.OCRPending = false
			}
		}

		if existing == nil {
			// New file
			if _, err := database.InsertDocument(res.fi.Path, content.ID, res.fi.ModTime, res.fi.ModTime); err != nil {
				dbErr = fmt.Errorf("insert document: %w", err)
				cancel()
				continue
			}
			newCount++
		} else if existing.Deleted {
			// Was soft-deleted, now reappeared
			if err := database.RestoreDocument(existing.ID, content.ID, res.fi.ModTime); err != nil {
				dbErr = fmt.Errorf("restore document: %w", err)
				cancel()
				continue
			}
			restoredCount++
		} else {
			contentChanged := existing.ContentID != content.ID
			if checksumChanged || mtimeChanged || contentChanged {
				if err := database.UpdateDocument(existing.ID, content.ID, res.fi.ModTime); err != nil {
					dbErr = fmt.Errorf("update document: %w", err)
					cancel()
					continue
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

	if err := <-scanErr; err != nil && dbErr == nil {
		return fmt.Errorf("scan: %w", err)
	}

	if dbErr != nil {
		return dbErr
	}
	stopSweepSpinner()

	// Soft-delete files no longer present
	deletedCount, err := database.SoftDeleteMissing(seenPaths, sweepRoots)
	if err != nil {
		return fmt.Errorf("soft delete: %w", err)
	}
	if _, err := database.CleanupOrphanContents(); err != nil {
		return fmt.Errorf("cleanup orphan contents: %w", err)
	}

	fmt.Printf("Sweep complete: %d scanned, %d new, %d updated, %d restored, %d deleted, %d unchanged\n",
		scanned.Load(), newCount, updatedCount, restoredCount, deletedCount, unchangedCount)

	return nil
}

func containsGlobMeta(path string) bool {
	return strings.ContainsAny(path, "*?[{")
}

func containsPathSeparator(path string) bool {
	return strings.Contains(path, "/") || strings.Contains(path, `\`)
}
