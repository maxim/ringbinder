package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/maxim/ringbinder/internal/checksum"
	"github.com/maxim/ringbinder/internal/db"
	"github.com/maxim/ringbinder/internal/ocr"
)

func TestBatchRetryReportsWhenContentRemainsPending(t *testing.T) {
	resetCommandState(t)
	t.Setenv("GEMINI_API_KEY", "test-key")
	databasePath := filepath.Join(t.TempDir(), "pending-retry.db")
	documentPath := filepath.Join(t.TempDir(), "pending-retry.pdf")
	if err := os.WriteFile(documentPath, []byte("stable pending source"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest, err := checksum.SHA256File(documentPath)
	if err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	contentID, err := database.InsertContent(digest, 3)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := database.InsertDocument(documentPath, contentID, now, now); err != nil {
		t.Fatal(err)
	}
	requestID, err := database.CreateBlockedGeminiRequest(db.GeminiRequestPlan{
		ContentID: contentID, RequestKey: "pending-direct-retry", FileType: "pdf",
		PageStart: 0, PageEnd: 1,
	}, "blocked", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	provider := &fakeRangeProvider{fn: func(_ context.Context, _ string, start, end int) (ocr.RangeResult, error) {
		return successfulRange("gemini-pending", start, end, 10), nil
	}}
	oldFactory := newGeminiDirectProvider
	newGeminiDirectProvider = func(string, time.Time) ocr.Provider { return provider }
	t.Cleanup(func() { newGeminiDirectProvider = oldFactory })
	cmd := commandWithDatabaseFlag(t, databasePath)
	cmd.Flags().String("mode", "", "")
	if err := cmd.Flags().Set("mode", "direct"); err != nil {
		t.Fatal(err)
	}
	output := captureStdout(t, func() {
		if err := runBatchRetry(cmd, []string{strconv.FormatInt(requestID, 10)}); err != nil {
			t.Fatalf("runBatchRetry() error = %v", err)
		}
	})
	if !strings.Contains(output, "content item remains pending") {
		t.Fatalf("retry output = %q, want pending status", output)
	}

	database, err = db.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	content, err := database.GetContentByID(contentID)
	if err != nil || content == nil || !content.OCRPending {
		t.Fatalf("content after partial request = %+v, %v; want pending", content, err)
	}
	request, err := database.GeminiRequestByID(requestID)
	if err != nil || request == nil || request.State != db.GeminiRequestStaged {
		t.Fatalf("request after partial completion = %+v, %v; want staged", request, err)
	}
}

func TestBatchRetryRetainsPartialPagesAndRetriesOnlyMissingRange(t *testing.T) {
	resetCommandState(t)
	t.Setenv("GEMINI_API_KEY", "test-key")
	databasePath := filepath.Join(t.TempDir(), "retry.db")
	documentPath := filepath.Join(t.TempDir(), "retry.pdf")
	if err := os.WriteFile(documentPath, []byte("stable retry source"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest, err := checksum.SHA256File(documentPath)
	if err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	contentID, err := database.InsertContent(digest, 3)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := database.InsertDocument(documentPath, contentID, now, now); err != nil {
		t.Fatal(err)
	}
	historical := "historical"
	if err := database.UpsertContentPages(contentID, []db.PageInput{{
		PageIndex: 0, Markdown: "existing", Model: &historical,
	}}); err != nil {
		t.Fatal(err)
	}
	requestID, err := database.CreateBlockedGeminiRequest(db.GeminiRequestPlan{
		ContentID: contentID, RequestKey: "direct-retry", FileType: "pdf",
		PageStart: 1, PageEnd: 3,
	}, "blocked", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	provider := &fakeRangeProvider{fn: func(_ context.Context, _ string, start, end int) (ocr.RangeResult, error) {
		if start == 1 && end == 3 {
			return successfulRange("gemini-partial", 1, 2, 10),
				ocr.TemporaryFailure(errors.New("later chunk timed out"))
		}
		return successfulRange("gemini-final", start, end, 20), nil
	}}
	oldFactory := newGeminiDirectProvider
	newGeminiDirectProvider = func(string, time.Time) ocr.Provider { return provider }
	t.Cleanup(func() { newGeminiDirectProvider = oldFactory })
	cmd := commandWithDatabaseFlag(t, databasePath)
	cmd.Flags().String("mode", "", "")
	if err := cmd.Flags().Set("mode", "direct"); err != nil {
		t.Fatal(err)
	}
	requestArg := strconv.FormatInt(requestID, 10)
	_ = captureStdout(t, func() {
		if err := runBatchRetry(cmd, []string{requestArg}); err == nil {
			t.Fatal("first runBatchRetry() error = nil")
		}
	})

	database, err = db.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	var partialModel string
	if err := database.QueryRow(
		`SELECT model FROM pages WHERE content_id = ? AND page_index = 1`, contentID,
	).Scan(&partialModel); err != nil {
		t.Fatal(err)
	}
	request, err := database.GeminiRequestByID(requestID)
	if err != nil || request == nil || request.State != db.GeminiRequestBlocked {
		t.Fatalf("request after partial retry = %+v, %v", request, err)
	}
	_ = database.Close()
	if partialModel != "gemini-partial" {
		t.Fatalf("partial model = %q", partialModel)
	}

	output := captureStdout(t, func() {
		if err := runBatchRetry(cmd, []string{requestArg}); err != nil {
			t.Fatalf("second runBatchRetry() error = %v", err)
		}
	})
	if !strings.Contains(output, "OCR completed for its content item") {
		t.Fatalf("second retry output = %q, want request-scoped completion", output)
	}
	provider.mu.Lock()
	calls := append([]rangeCall(nil), provider.calls...)
	provider.mu.Unlock()
	if len(calls) != 2 || calls[0] != (rangeCall{start: 1, end: 3}) ||
		calls[1] != (rangeCall{start: 2, end: 3}) {
		t.Fatalf("retry calls = %+v", calls)
	}
	database, err = db.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	content, err := database.GetContentByID(contentID)
	if err != nil || content == nil || content.OCRPending {
		t.Fatalf("content after retry = %+v, %v", content, err)
	}
	request, err = database.GeminiRequestByID(requestID)
	if err != nil || request != nil {
		t.Fatalf("request after completion = %+v, %v", request, err)
	}
}
