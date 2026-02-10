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

func withDB(t *testing.T, fn func(database *db.DB)) {
	t.Helper()

	database, err := db.Open(config.DefaultDir())
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	defer func() { _ = database.Close() }()

	fn(database)
}
