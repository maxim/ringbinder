package cmd

import (
	"context"
	"fmt"
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

const defaultSweepConcurrency = 4

func init() {
	rootCmd.AddCommand(sweepCmd)
	sweepCmd.Flags().IntP("concurrency", "j", defaultSweepConcurrency, "Number of concurrent file processing workers")
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
	// Persistent exclusions are safeguards, so explicit paths and flags never
	// bypass the configured list.
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	paths := args
	if len(paths) == 0 && cfg != nil {
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

	cliExclude, err := cmd.Flags().GetStringSlice("exclude")
	if err != nil {
		return fmt.Errorf("read exclude flag: %w", err)
	}
	excludePatterns := dedupeStrings(append(append([]string(nil), cfg.Exclude...), cliExclude...))
	for i, pattern := range excludePatterns {
		pattern = config.ExpandHome(pattern)
		if !containsGlobMeta(pattern) {
			abs, err := filepath.Abs(pattern)
			if err != nil {
				return fmt.Errorf("resolve exclude path %q: %w", pattern, err)
			}
			info, err := os.Stat(abs)
			if err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("stat exclude path %q: %w", pattern, err)
			}
			if err == nil && info.IsDir() {
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

	concurrency, err := resolveSweepConcurrency(cmd, cfg)
	if err != nil {
		return err
	}

	database, err := openDatabaseWithConfig(cmd, cfg)
	if err != nil {
		return err
	}
	defer database.Close()
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
		existing, err := database.GetDocumentMetadataByPath(res.fi.Path)
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
				// A checksum-only change intentionally does not request OCR again.
				// Copy the prior canonical pages instead of manufacturing a complete
				// content row with no page coverage.
				if err := database.CopyContentPages(existing.ContentID, contentID); err != nil {
					dbErr = fmt.Errorf("carry forward OCR pages: %w", err)
					cancel()
					continue
				}
				carried, err := database.GetContentByChecksum(res.checksum)
				if err != nil {
					dbErr = fmt.Errorf("query carried content: %w", err)
					cancel()
					continue
				}
				content.OCRPending = carried == nil || carried.OCRPending
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

func resolveSweepConcurrency(cmd *cobra.Command, cfg *config.Config) (int, error) {
	if flag := cmd.Flag("concurrency"); flag != nil && flag.Changed {
		concurrency, err := cmd.Flags().GetInt("concurrency")
		if err != nil {
			return 0, fmt.Errorf("read --concurrency flag: %w", err)
		}
		if concurrency < 1 {
			return 0, fmt.Errorf("--concurrency must be >= 1")
		}
		return concurrency, nil
	}

	if cfg != nil && cfg.SweepConcurrency != nil {
		concurrency := *cfg.SweepConcurrency
		if concurrency < 1 {
			return 0, fmt.Errorf("sweep_concurrency must be >= 1")
		}
		return concurrency, nil
	}

	return defaultSweepConcurrency, nil
}

func dedupeStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

func containsGlobMeta(path string) bool {
	return strings.ContainsAny(path, "*?[{")
}

func containsPathSeparator(path string) bool {
	return strings.Contains(path, "/") || strings.Contains(path, `\`)
}
