package cmd

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/maxim/ringbinder/internal/checksum"
	"github.com/maxim/ringbinder/internal/config"
	"github.com/maxim/ringbinder/internal/db"
	"github.com/maxim/ringbinder/internal/pdf"
	"github.com/maxim/ringbinder/internal/scanner"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(sweepCmd)
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

	var newCount, updatedCount, restoredCount, unchangedCount int
	seenPaths := make(map[string]bool)

	for fi := range results {
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

		if existing == nil {
			// New file
			if _, err := database.InsertDocument(fi.Path, cs, fi.ModTime, fi.ModTime, pageCount); err != nil {
				return fmt.Errorf("insert document: %w", err)
			}
			newCount++
		} else if existing.Deleted {
			// Was soft-deleted, now reappeared
			checksumChanged := existing.Checksum != cs
			mtimeChanged := !existing.ModifiedAt.Equal(fi.ModTime)
			ocrPending := existing.OCRPending || (checksumChanged && mtimeChanged)
			if err := database.RestoreDocument(existing.ID, cs, fi.ModTime, pageCount, ocrPending); err != nil {
				return fmt.Errorf("restore document: %w", err)
			}
			restoredCount++
		} else {
			checksumChanged := existing.Checksum != cs
			mtimeChanged := !existing.ModifiedAt.Equal(fi.ModTime)
			ocrPending := existing.OCRPending || (checksumChanged && mtimeChanged)

			if checksumChanged || mtimeChanged {
				if err := database.UpdateDocument(existing.ID, cs, fi.ModTime, pageCount, ocrPending); err != nil {
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

	fmt.Printf("Sweep complete: %d new, %d updated, %d restored, %d deleted, %d unchanged\n",
		newCount, updatedCount, restoredCount, deletedCount, unchangedCount)

	return nil
}
