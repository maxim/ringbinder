package cmd

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maxim/ringbinder/internal/db"
)

func TestEstimateOCRCost_GeminiAssumptionsAndPriceBoundary(t *testing.T) {
	t.Parallel()

	database := openCostTestDB(t)
	addCostContent(t, database, "pdf", 21, true, "/docs/report.pdf")
	addCostContent(t, database, "image", 1, true, "/docs/photo.png")

	before, err := estimateOCRCost(database, modelGemini, false, time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC))
	if err != nil {
		t.Fatalf("estimateOCRCost(before) error = %v", err)
	}
	if before.items != 2 || before.pages != 22 {
		t.Fatalf("estimate before counts = %+v, want 2 items and 22 pages", before)
	}
	// PDF: 21*560 media + 2*250 request overhead. Image: 1120+250.
	// Both use 1200 candidate-plus-thinking output tokens per page.
	if got, want := int64(before.cost), geminiExpectedCost(2026, 13_630, 26_400); got != want {
		t.Fatalf("before cost = %d, want %d", got, want)
	}

	after, err := estimateOCRCost(database, modelGemini, false, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("estimateOCRCost(after) error = %v", err)
	}
	if got, want := after.cost, before.cost*2; got != want {
		t.Fatalf("after cost = %d, want %d", got, want)
	}
}

func TestEstimateOCRCost_RedoCountsUniqueLiveContent(t *testing.T) {
	t.Parallel()

	database := openCostTestDB(t)
	addCostContent(t, database, "shared", 2, false, "/docs/a.pdf", "/docs/copy.pdf")
	addCostContent(t, database, "pending", 1, true, "/docs/pending.jpg")

	estimate, err := estimateOCRCost(database, modelMistral, true, time.Now())
	if err != nil {
		t.Fatalf("estimateOCRCost() error = %v", err)
	}
	if estimate.items != 2 || estimate.pages != 3 {
		t.Fatalf("estimate = %+v, want 2 unique items and 3 pages", estimate)
	}
	if got, want := int64(estimate.cost), int64(15_000_000); got != want {
		t.Fatalf("cost = %d, want %d", got, want)
	}
}

func TestRunCost_RedoPrintsUniqueContentAndExactMistralCost(t *testing.T) {
	resetCommandState(t)

	dbPath := filepath.Join(t.TempDir(), "cost.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	addCostContent(t, database, "shared", 2, false, "/docs/a.pdf", "/docs/copy.pdf")
	if err := database.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	cmd := commandWithDatabaseFlag(t, dbPath)
	cmd.Flags().Bool("redo", false, "")
	cmd.Flags().String("model", "", "")
	if err := cmd.Flags().Set("redo", "true"); err != nil {
		t.Fatalf("Set(redo) error = %v", err)
	}
	if err := cmd.Flags().Set("model", modelMistral); err != nil {
		t.Fatalf("Set(model) error = %v", err)
	}

	output := captureStdout(t, func() {
		if err := runCost(cmd, nil); err != nil {
			t.Fatalf("runCost() error = %v", err)
		}
	})
	want := "All content: 1 content item(s), 2 pages\nEstimated cost: $0.0100 (at $0.0050/page)\n"
	if output != want {
		t.Fatalf("output = %q, want %q", output, want)
	}
}

func TestRunCost_GeminiIsOfflineAndPrintsApproximation(t *testing.T) {
	resetCommandState(t)
	t.Setenv("GEMINI_API_KEY", "")

	dbPath := filepath.Join(t.TempDir(), "cost.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	addCostContent(t, database, "image", 1, true, "/docs/page.png")
	if err := database.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	cmd := commandWithDatabaseFlag(t, dbPath)
	cmd.Flags().Bool("redo", false, "")
	cmd.Flags().String("model", "", "")
	if err := cmd.Flags().Set("model", modelGemini); err != nil {
		t.Fatalf("Set(model) error = %v", err)
	}

	output := captureStdout(t, func() {
		if err := runCost(cmd, nil); err != nil {
			t.Fatalf("runCost() error = %v", err)
		}
	})
	if !strings.Contains(output, "Pending OCR: 1 content item(s), 1 pages") {
		t.Fatalf("output = %q, want content count", output)
	}
	if !strings.Contains(output, "Estimated cost: ~$0.01 (actual cost may vary)") {
		t.Fatalf("output = %q, want approximate cost", output)
	}
}

func openCostTestDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func addCostContent(t *testing.T, database *db.DB, checksum string, pages int, pending bool, paths ...string) {
	t.Helper()
	contentID, err := database.InsertContent(checksum, pages)
	if err != nil {
		t.Fatalf("InsertContent() error = %v", err)
	}
	now := time.Now().UTC()
	for _, path := range paths {
		if _, err := database.InsertDocument(path, contentID, now, now); err != nil {
			t.Fatalf("InsertDocument() error = %v", err)
		}
	}
	if !pending {
		if err := database.MarkContentOCRDone(contentID); err != nil {
			t.Fatalf("MarkContentOCRDone() error = %v", err)
		}
	}
}

func geminiExpectedCost(year int, input, output int64) int64 {
	inputRate, outputRate := int64(750), int64(3_750)
	if year >= 2027 {
		inputRate, outputRate = 1_500, 7_500
	}
	return input*inputRate + output*outputRate
}

func captureStdout(t *testing.T, run func()) string {
	t.Helper()
	original := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	os.Stdout = writer
	t.Cleanup(func() { os.Stdout = original })

	run()
	if err := writer.Close(); err != nil {
		t.Fatalf("stdout writer close error = %v", err)
	}
	os.Stdout = original
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read stdout error = %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("stdout reader close error = %v", err)
	}
	return string(output)
}
