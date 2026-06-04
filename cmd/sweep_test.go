package cmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/maxim/ringbinder/internal/config"
	"github.com/maxim/ringbinder/internal/db"
	"github.com/spf13/cobra"
)

func TestSweep_OCRPendingRequiresMTimeAndChecksumChange(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgFile = ""

	scanDir := t.TempDir()
	docPath := filepath.Join(scanDir, "a.png")

	t1 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.WriteFile(docPath, []byte("v1"), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := os.Chtimes(docPath, t1, t1); err != nil {
		t.Fatalf("Chtimes(t1) error = %v", err)
	}

	cmd := &cobra.Command{}
	cmd.Flags().Bool("redo", false, "")
	cmd.Flags().StringSlice("exclude", nil, "")
	if err := runSweep(cmd, []string{scanDir}); err != nil {
		t.Fatalf("runSweep(initial) error = %v", err)
	}

	withDB(t, func(database *db.DB) {
		doc, err := database.GetDocumentByPath(docPath)
		if err != nil {
			t.Fatalf("GetDocumentByPath() error = %v", err)
		}
		if doc == nil {
			t.Fatalf("GetDocumentByPath() = nil, want document")
		}
		if !doc.OCRPending {
			t.Fatalf("initial OCRPending = false, want true")
		}
		if err := database.MarkContentOCRDone(doc.ContentID); err != nil {
			t.Fatalf("MarkContentOCRDone() error = %v", err)
		}
	})

	// Touch only: mtime changes, checksum stays the same => should not become pending.
	t2 := t1.Add(2 * time.Hour)
	if err := os.Chtimes(docPath, t2, t2); err != nil {
		t.Fatalf("Chtimes(t2) error = %v", err)
	}
	if err := runSweep(cmd, []string{scanDir}); err != nil {
		t.Fatalf("runSweep(touch-only) error = %v", err)
	}

	var checksumV1 string
	withDB(t, func(database *db.DB) {
		doc, err := database.GetDocumentByPath(docPath)
		if err != nil {
			t.Fatalf("GetDocumentByPath() error = %v", err)
		}
		if doc == nil {
			t.Fatalf("GetDocumentByPath() = nil, want document")
		}
		if doc.OCRPending {
			t.Fatalf("touch-only OCRPending = true, want false")
		}
		if !doc.ModifiedAt.Equal(t2) {
			t.Fatalf("touch-only ModifiedAt = %s, want %s", doc.ModifiedAt.Format(time.RFC3339Nano), t2.Format(time.RFC3339Nano))
		}
		checksumV1 = doc.Checksum
	})

	// Content change with mtime change => should become pending.
	t3 := t2.Add(2 * time.Hour)
	if err := os.WriteFile(docPath, []byte("v2"), 0644); err != nil {
		t.Fatalf("WriteFile(v2) error = %v", err)
	}
	if err := os.Chtimes(docPath, t3, t3); err != nil {
		t.Fatalf("Chtimes(t3) error = %v", err)
	}
	if err := runSweep(cmd, []string{scanDir}); err != nil {
		t.Fatalf("runSweep(content+mtime) error = %v", err)
	}

	var checksumV2 string
	withDB(t, func(database *db.DB) {
		doc, err := database.GetDocumentByPath(docPath)
		if err != nil {
			t.Fatalf("GetDocumentByPath() error = %v", err)
		}
		if doc == nil {
			t.Fatalf("GetDocumentByPath() = nil, want document")
		}
		if !doc.OCRPending {
			t.Fatalf("content+mtime OCRPending = false, want true")
		}
		if doc.Checksum == checksumV1 {
			t.Fatalf("content+mtime checksum unchanged, want changed")
		}
		if !doc.ModifiedAt.Equal(t3) {
			t.Fatalf("content+mtime ModifiedAt = %s, want %s", doc.ModifiedAt.Format(time.RFC3339Nano), t3.Format(time.RFC3339Nano))
		}
		checksumV2 = doc.Checksum
		if err := database.MarkContentOCRDone(doc.ContentID); err != nil {
			t.Fatalf("MarkContentOCRDone() error = %v", err)
		}
	})

	// Content change but mtime unchanged => checksum updates, but should not become pending.
	if err := os.WriteFile(docPath, []byte("v3"), 0644); err != nil {
		t.Fatalf("WriteFile(v3) error = %v", err)
	}
	if err := os.Chtimes(docPath, t3, t3); err != nil {
		t.Fatalf("Chtimes(t3 again) error = %v", err)
	}
	if err := runSweep(cmd, []string{scanDir}); err != nil {
		t.Fatalf("runSweep(content-only) error = %v", err)
	}

	withDB(t, func(database *db.DB) {
		doc, err := database.GetDocumentByPath(docPath)
		if err != nil {
			t.Fatalf("GetDocumentByPath() error = %v", err)
		}
		if doc == nil {
			t.Fatalf("GetDocumentByPath() = nil, want document")
		}
		if doc.OCRPending {
			t.Fatalf("content-only OCRPending = true, want false")
		}
		if doc.Checksum == checksumV2 {
			t.Fatalf("content-only checksum unchanged, want changed")
		}
	})
}

func TestSweep_DoesNotClearExistingPendingOnTouch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgFile = ""

	scanDir := t.TempDir()
	docPath := filepath.Join(scanDir, "a.png")

	t1 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.WriteFile(docPath, []byte("v1"), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := os.Chtimes(docPath, t1, t1); err != nil {
		t.Fatalf("Chtimes(t1) error = %v", err)
	}

	cmd := &cobra.Command{}
	cmd.Flags().Bool("redo", false, "")
	cmd.Flags().StringSlice("exclude", nil, "")
	if err := runSweep(cmd, []string{scanDir}); err != nil {
		t.Fatalf("runSweep(initial) error = %v", err)
	}

	// Touch only; document is still pending and should remain pending.
	t2 := t1.Add(2 * time.Hour)
	if err := os.Chtimes(docPath, t2, t2); err != nil {
		t.Fatalf("Chtimes(t2) error = %v", err)
	}
	if err := runSweep(cmd, []string{scanDir}); err != nil {
		t.Fatalf("runSweep(touch-only) error = %v", err)
	}

	withDB(t, func(database *db.DB) {
		doc, err := database.GetDocumentByPath(docPath)
		if err != nil {
			t.Fatalf("GetDocumentByPath() error = %v", err)
		}
		if doc == nil {
			t.Fatalf("GetDocumentByPath() = nil, want document")
		}
		if !doc.OCRPending {
			t.Fatalf("touch-only OCRPending = false, want true")
		}
	})
}

func TestSweep_DuplicateFileSharesContent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgFile = ""

	scanDir := t.TempDir()
	pathA := filepath.Join(scanDir, "a.png")
	pathB := filepath.Join(scanDir, "b.png")

	content := []byte("same-bytes")
	if err := os.WriteFile(pathA, content, 0644); err != nil {
		t.Fatalf("WriteFile(a) error = %v", err)
	}
	if err := os.WriteFile(pathB, content, 0644); err != nil {
		t.Fatalf("WriteFile(b) error = %v", err)
	}

	cmd := &cobra.Command{}
	cmd.Flags().Bool("redo", false, "")
	cmd.Flags().StringSlice("exclude", nil, "")
	if err := runSweep(cmd, []string{scanDir}); err != nil {
		t.Fatalf("runSweep() error = %v", err)
	}

	withDB(t, func(database *db.DB) {
		docA, err := database.GetDocumentByPath(pathA)
		if err != nil {
			t.Fatalf("GetDocumentByPath(a) error = %v", err)
		}
		docB, err := database.GetDocumentByPath(pathB)
		if err != nil {
			t.Fatalf("GetDocumentByPath(b) error = %v", err)
		}
		if docA == nil || docB == nil {
			t.Fatalf("expected both documents to exist")
		}
		if docA.ContentID != docB.ContentID {
			t.Fatalf("content_id mismatch: %d vs %d", docA.ContentID, docB.ContentID)
		}

		var contentRows int
		if err := database.QueryRow("SELECT COUNT(*) FROM contents").Scan(&contentRows); err != nil {
			t.Fatalf("count contents error = %v", err)
		}
		if contentRows != 1 {
			t.Fatalf("content rows = %d, want 1", contentRows)
		}
	})
}

func TestSweep_ExcludeSkipsFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgFile = ""

	scanDir := t.TempDir()
	includedPath := filepath.Join(scanDir, "included.png")
	excludedPath := filepath.Join(scanDir, "excluded.png")

	if err := os.WriteFile(includedPath, []byte("included"), 0644); err != nil {
		t.Fatalf("WriteFile(included) error = %v", err)
	}
	if err := os.WriteFile(excludedPath, []byte("excluded"), 0644); err != nil {
		t.Fatalf("WriteFile(excluded) error = %v", err)
	}

	cmd := &cobra.Command{}
	cmd.Flags().Bool("redo", false, "")
	cmd.Flags().StringSlice("exclude", nil, "")
	if err := cmd.Flags().Set("exclude", excludedPath); err != nil {
		t.Fatalf("Set(exclude) error = %v", err)
	}
	if err := runSweep(cmd, []string{scanDir}); err != nil {
		t.Fatalf("runSweep() error = %v", err)
	}

	withDB(t, func(database *db.DB) {
		included, err := database.GetDocumentByPath(includedPath)
		if err != nil {
			t.Fatalf("GetDocumentByPath(included) error = %v", err)
		}
		if included == nil {
			t.Fatalf("included doc = nil, want indexed document")
		}

		excluded, err := database.GetDocumentByPath(excludedPath)
		if err != nil {
			t.Fatalf("GetDocumentByPath(excluded) error = %v", err)
		}
		if excluded != nil {
			t.Fatalf("excluded doc = %+v, want nil", excluded)
		}
	})
}

func TestSweep_ExcludeSoftDeletesPreviouslySwept(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgFile = ""

	scanDir := t.TempDir()
	docPath := filepath.Join(scanDir, "a.png")
	keepPath := filepath.Join(scanDir, "b.png")

	content := []byte("shared-content")
	if err := os.WriteFile(docPath, content, 0644); err != nil {
		t.Fatalf("WriteFile(a) error = %v", err)
	}
	if err := os.WriteFile(keepPath, content, 0644); err != nil {
		t.Fatalf("WriteFile(b) error = %v", err)
	}

	initialCmd := &cobra.Command{}
	initialCmd.Flags().Bool("redo", false, "")
	initialCmd.Flags().StringSlice("exclude", nil, "")
	if err := runSweep(initialCmd, []string{scanDir}); err != nil {
		t.Fatalf("runSweep(initial) error = %v", err)
	}

	withDB(t, func(database *db.DB) {
		doc, err := database.GetDocumentByPath(docPath)
		if err != nil {
			t.Fatalf("GetDocumentByPath(initial) error = %v", err)
		}
		if doc == nil {
			t.Fatalf("initial doc = nil, want indexed document")
		}
		if doc.Deleted {
			t.Fatalf("initial doc.Deleted = true, want false")
		}

		keep, err := database.GetDocumentByPath(keepPath)
		if err != nil {
			t.Fatalf("GetDocumentByPath(initial keep) error = %v", err)
		}
		if keep == nil {
			t.Fatalf("initial keep doc = nil, want indexed document")
		}
		if keep.Deleted {
			t.Fatalf("initial keep doc.Deleted = true, want false")
		}
	})

	excludeCmd := &cobra.Command{}
	excludeCmd.Flags().Bool("redo", false, "")
	excludeCmd.Flags().StringSlice("exclude", nil, "")
	if err := excludeCmd.Flags().Set("exclude", docPath); err != nil {
		t.Fatalf("Set(exclude) error = %v", err)
	}
	if err := runSweep(excludeCmd, []string{scanDir}); err != nil {
		t.Fatalf("runSweep(exclude) error = %v", err)
	}

	withDB(t, func(database *db.DB) {
		doc, err := database.GetDocumentByPath(docPath)
		if err != nil {
			t.Fatalf("GetDocumentByPath(exclude) error = %v", err)
		}
		if doc == nil {
			t.Fatalf("excluded doc = nil, want soft-deleted document")
		}
		if !doc.Deleted {
			t.Fatalf("excluded doc.Deleted = false, want true")
		}

		keep, err := database.GetDocumentByPath(keepPath)
		if err != nil {
			t.Fatalf("GetDocumentByPath(exclude keep) error = %v", err)
		}
		if keep == nil {
			t.Fatalf("keep doc = nil, want indexed document")
		}
		if keep.Deleted {
			t.Fatalf("keep doc.Deleted = true, want false")
		}
	})
}

func TestSweep_GlobPathFiltersAndSoftDeletesWithinRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgFile = ""

	scanDir := t.TempDir()
	nestedDir := filepath.Join(scanDir, "nested")
	if err := os.MkdirAll(nestedDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	keepPath := filepath.Join(nestedDir, "keep.png")
	dropPath := filepath.Join(nestedDir, "drop.jpg")
	content := []byte("shared-content")
	if err := os.WriteFile(keepPath, content, 0644); err != nil {
		t.Fatalf("WriteFile(keep) error = %v", err)
	}
	if err := os.WriteFile(dropPath, content, 0644); err != nil {
		t.Fatalf("WriteFile(drop) error = %v", err)
	}

	initialCmd := &cobra.Command{}
	initialCmd.Flags().Bool("redo", false, "")
	initialCmd.Flags().StringSlice("exclude", nil, "")
	if err := runSweep(initialCmd, []string{scanDir}); err != nil {
		t.Fatalf("runSweep(initial) error = %v", err)
	}

	globCmd := &cobra.Command{}
	globCmd.Flags().Bool("redo", false, "")
	globCmd.Flags().StringSlice("exclude", nil, "")
	if err := runSweep(globCmd, []string{filepath.Join(scanDir, "**", "*.png")}); err != nil {
		t.Fatalf("runSweep(glob) error = %v", err)
	}

	withDB(t, func(database *db.DB) {
		keep, err := database.GetDocumentByPath(keepPath)
		if err != nil {
			t.Fatalf("GetDocumentByPath(keep) error = %v", err)
		}
		if keep == nil {
			t.Fatalf("keep doc = nil, want indexed document")
		}
		if keep.Deleted {
			t.Fatalf("keep doc.Deleted = true, want false")
		}

		drop, err := database.GetDocumentByPath(dropPath)
		if err != nil {
			t.Fatalf("GetDocumentByPath(drop) error = %v", err)
		}
		if drop == nil {
			t.Fatalf("drop doc = nil, want soft-deleted document")
		}
		if !drop.Deleted {
			t.Fatalf("drop doc.Deleted = false, want true")
		}
	})
}

func TestSweep_GlobExcludeSkipsAndSoftDeletes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgFile = ""

	scanDir := t.TempDir()
	nestedDir := filepath.Join(scanDir, "nested")
	if err := os.MkdirAll(nestedDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	keepPath := filepath.Join(scanDir, "keep.png")
	excludedPath := filepath.Join(nestedDir, "excluded.png")
	content := []byte("shared-content")
	if err := os.WriteFile(keepPath, content, 0644); err != nil {
		t.Fatalf("WriteFile(keep) error = %v", err)
	}
	if err := os.WriteFile(excludedPath, content, 0644); err != nil {
		t.Fatalf("WriteFile(excluded) error = %v", err)
	}

	initialCmd := &cobra.Command{}
	initialCmd.Flags().Bool("redo", false, "")
	initialCmd.Flags().StringSlice("exclude", nil, "")
	if err := runSweep(initialCmd, []string{scanDir}); err != nil {
		t.Fatalf("runSweep(initial) error = %v", err)
	}

	excludeCmd := &cobra.Command{}
	excludeCmd.Flags().Bool("redo", false, "")
	excludeCmd.Flags().StringSlice("exclude", nil, "")
	if err := excludeCmd.Flags().Set("exclude", "*excluded*.png"); err != nil {
		t.Fatalf("Set(exclude) error = %v", err)
	}
	if err := runSweep(excludeCmd, []string{scanDir}); err != nil {
		t.Fatalf("runSweep(exclude glob) error = %v", err)
	}

	withDB(t, func(database *db.DB) {
		keep, err := database.GetDocumentByPath(keepPath)
		if err != nil {
			t.Fatalf("GetDocumentByPath(keep) error = %v", err)
		}
		if keep == nil {
			t.Fatalf("keep doc = nil, want indexed document")
		}
		if keep.Deleted {
			t.Fatalf("keep doc.Deleted = true, want false")
		}

		excluded, err := database.GetDocumentByPath(excludedPath)
		if err != nil {
			t.Fatalf("GetDocumentByPath(excluded) error = %v", err)
		}
		if excluded == nil {
			t.Fatalf("excluded doc = nil, want soft-deleted document")
		}
		if !excluded.Deleted {
			t.Fatalf("excluded doc.Deleted = false, want true")
		}
	})
}

func withDB(t *testing.T, fn func(database *db.DB)) {
	t.Helper()

	database, err := db.Open(config.DefaultDatabasePath())
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	defer func() { _ = database.Close() }()

	fn(database)
}
