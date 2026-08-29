package cmd

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maxim/ringbinder/internal/db"
	"github.com/maxim/ringbinder/internal/ocr"
)

func TestEstimateOCRChainCostUsesOnlyMissingPagesAndFivePercentHighScenario(t *testing.T) {
	t.Parallel()
	database := openCostTestDB(t)
	contentID := addCostContent(t, database, "partial", 5, true, "/docs/partial.pdf")
	model := "historical"
	if err := database.UpsertContentPages(contentID, []db.PageInput{
		{PageIndex: 0, Markdown: "done", Model: &model},
		{PageIndex: 2, Markdown: "done", Model: &model},
	}); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	estimate, err := estimateOCRChainCost(
		database, []string{modelMistral, modelGemini}, 0, at,
	)
	if err != nil {
		t.Fatal(err)
	}
	if estimate.items != 1 || estimate.pages != 3 {
		t.Fatalf("estimate = %+v, want 1 item and 3 missing pages", estimate)
	}
	mistral := ocr.MistralCost(3)
	// Missing ranges are page 2 and pages 4-5: 3 media pages and 2 requests.
	gemini := ocr.GeminiCost(
		at,
		3*geminiPDFMediaTokens+2*geminiRequestOverhead,
		3*geminiOutputTokens,
	)
	wantHigh := ((mistral+gemini)*105 + 99) / 100
	if estimate.low != mistral || estimate.high != wantHigh {
		t.Fatalf("range = %d..%d, want %d..%d", estimate.low, estimate.high, mistral, wantHigh)
	}
}

func TestEstimateOCRCost_GeminiAssumptionsAndPriceBoundary(t *testing.T) {
	t.Parallel()

	database := openCostTestDB(t)
	addCostContent(t, database, "pdf", 21, true, "/docs/report.pdf")
	addCostContent(t, database, "image", 1, true, "/docs/photo.png")

	before, err := estimateOCRChainCost(database, []string{modelGemini}, 0, time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC))
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

	after, err := estimateOCRChainCost(database, []string{modelGemini}, 0, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("estimateOCRCost(after) error = %v", err)
	}
	if got, want := after.cost, before.cost*2; got != want {
		t.Fatalf("after cost = %d, want %d", got, want)
	}
}

func TestEstimateOCRCost_LimitUsesStablePendingPrefix(t *testing.T) {
	t.Parallel()

	database := openCostTestDB(t)
	addCostContent(t, database, "first", 2, true, "/docs/first.pdf", "/docs/first-copy.pdf")
	addCostContent(t, database, "second", 1, true, "/docs/second.jpg")
	addCostContent(t, database, "third", 7, true, "/docs/third.pdf")
	addCostContent(t, database, "completed", 20, false, "/docs/completed.pdf")

	estimate, err := estimateOCRChainCost(database, []string{modelMistral}, 2, time.Now())
	if err != nil {
		t.Fatalf("estimateOCRCost() error = %v", err)
	}
	if estimate.items != 2 || estimate.totalItems != 3 || estimate.pages != 3 || !estimate.truncated {
		t.Fatalf("estimate = %+v, want first 2 of 3 pending items and 3 pages", estimate)
	}
	if got, want := int64(estimate.cost), int64(15_000_000); got != want {
		t.Fatalf("cost = %d, want %d", got, want)
	}
}

func TestRunCost_LimitedBatchPrintsSelectedAndTotal(t *testing.T) {
	resetCommandState(t)

	dbPath := filepath.Join(t.TempDir(), "cost.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	addCostContent(t, database, "first", 2, true, "/docs/first.pdf", "/docs/first-copy.pdf")
	addCostContent(t, database, "second", 3, true, "/docs/second.pdf")
	addCostContent(t, database, "third", 5, true, "/docs/third.pdf")
	if err := database.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	cmd := commandWithDatabaseFlag(t, dbPath)
	cmd.Flags().StringArray("model", nil, "")
	cmd.Flags().Int("limit", 0, "")
	if err := cmd.Flags().Set("model", modelMistral); err != nil {
		t.Fatalf("Set(model) error = %v", err)
	}
	if err := cmd.Flags().Set("limit", "2"); err != nil {
		t.Fatalf("Set(limit) error = %v", err)
	}

	output := captureStdout(t, func() {
		if err := runCost(cmd, nil); err != nil {
			t.Fatalf("runCost() error = %v", err)
		}
	})
	want := "OCR models: mistral\n" +
		"Selected: 2 of 3 pending content item(s), 5 missing pages\n" +
		"Estimated cost range: $0.03–$0.03\n" +
		"High estimate assumes every model processes every missing page and adds 5% for retries; actual cost may be higher.\n"
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
	cmd.Flags().StringArray("model", nil, "")
	cmd.Flags().Int("limit", 0, "")
	if err := cmd.Flags().Set("model", modelGemini); err != nil {
		t.Fatalf("Set(model) error = %v", err)
	}
	if err := cmd.Flags().Set("limit", "5"); err != nil {
		t.Fatalf("Set(limit) error = %v", err)
	}

	output := captureStdout(t, func() {
		if err := runCost(cmd, nil); err != nil {
			t.Fatalf("runCost() error = %v", err)
		}
	})
	if !strings.Contains(output, "Selected: 1 pending content item(s), 1 missing pages") {
		t.Fatalf("output = %q, want content count", output)
	}
	if !strings.Contains(output, "Estimated cost range: $0.01–$0.01") {
		t.Fatalf("output = %q, want approximate cost", output)
	}
}

func TestRunCost_RejectsNonPositiveExplicitLimit(t *testing.T) {
	for _, limit := range []string{"0", "-1"} {
		t.Run(limit, func(t *testing.T) {
			resetCommandState(t)
			cmd := commandWithDatabaseFlag(t, filepath.Join(t.TempDir(), "cost.db"))
			cmd.Flags().StringArray("model", nil, "")
			cmd.Flags().Int("limit", 0, "")
			if err := cmd.Flags().Set("model", modelMistral); err != nil {
				t.Fatalf("Set(model) error = %v", err)
			}
			if err := cmd.Flags().Set("limit", limit); err != nil {
				t.Fatalf("Set(limit) error = %v", err)
			}

			err := runCost(cmd, nil)
			if err == nil || err.Error() != "--limit must be >= 1" {
				t.Fatalf("runCost() error = %v, want --limit validation error", err)
			}
		})
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

func addCostContent(t *testing.T, database *db.DB, checksum string, pages int, pending bool, paths ...string) int64 {
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
		for pageIndex := 0; pageIndex < pages; pageIndex++ {
			if err := database.UpsertPage(contentID, pageIndex, "complete"); err != nil {
				t.Fatalf("UpsertPage(%d) error = %v", pageIndex, err)
			}
		}
	}
	return contentID
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
