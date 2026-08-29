package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/maxim/ringbinder/internal/checksum"
	"github.com/maxim/ringbinder/internal/db"
	"github.com/maxim/ringbinder/internal/ocr"
)

func TestWalkGeminiMissingRangeUsesMaximalPageLimitChunks(t *testing.T) {
	labels := make([]string, 45)
	for i := range labels {
		labels[i] = fmt.Sprintf("page-%d", i)
	}
	path := filepath.Join(t.TempDir(), "chunks.pdf")
	if err := os.WriteFile(path, commandTestPDF(labels...), 0o600); err != nil {
		t.Fatal(err)
	}
	planner := ocr.NewGeminiClient("", time.Now().UTC())
	var ranges []db.PageRange
	err := walkGeminiMissingRange(
		context.Background(), planner, path, "pdf", 3, len(labels),
		func(request ocr.GeminiPreparedRequest) error {
			ranges = append(ranges, db.PageRange{Start: request.PageStart, End: request.PageEnd})
			return nil
		},
		func(sizeErr *ocr.GeminiRangeSizeError) error {
			t.Fatalf("ordinary page-limit chunk rejected: %v", sizeErr)
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []db.PageRange{{Start: 3, End: 23}, {Start: 23, End: 43}, {Start: 43, End: 45}}
	if len(ranges) != len(want) || ranges[0] != want[0] || ranges[1] != want[1] || ranges[2] != want[2] {
		t.Fatalf("planned ranges = %+v, want %+v", ranges, want)
	}
}

func TestBatchStartPersistsExactSparseRangeForPlanningFailure(t *testing.T) {
	resetCommandState(t)
	t.Setenv("GEMINI_API_KEY", "test-key")
	api := &fakeGeminiBatchAPI{}
	oldFactory := newGeminiBatchAPI
	newGeminiBatchAPI = func(string) geminiBatchAPI { return api }
	t.Cleanup(func() { newGeminiBatchAPI = oldFactory })

	documentPath := filepath.Join(t.TempDir(), "malformed.pdf")
	if err := os.WriteFile(documentPath, []byte("not a PDF"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest, err := checksum.SHA256File(documentPath)
	if err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(t.TempDir(), "planning.db")
	database, err := db.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	contentID, err := database.InsertContent(digest, 5)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := database.InsertDocument(documentPath, contentID, now, now); err != nil {
		t.Fatal(err)
	}
	model := "existing"
	if err := database.UpsertContentPages(contentID, []db.PageInput{
		{PageIndex: 0, Markdown: "done", Model: &model},
		{PageIndex: 3, Markdown: "done", Model: &model},
		{PageIndex: 4, Markdown: "done", Model: &model},
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	cmd := commandWithDatabaseFlag(t, databasePath)
	cmd.SetContext(context.Background())
	cmd.Flags().Int("limit", 0, "")
	_ = captureStdout(t, func() {
		if err := runBatchStart(cmd, nil); err != nil {
			t.Fatalf("runBatchStart() error = %v", err)
		}
	})
	if api.uploadCalls != 0 {
		t.Fatalf("upload calls = %d, want no upload for planning failure", api.uploadCalls)
	}
	database, err = db.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	blocked, err := database.BlockedGeminiRequests()
	if err != nil {
		t.Fatal(err)
	}
	if len(blocked) != 1 || blocked[0].ContentID != contentID ||
		blocked[0].PageStart != 1 || blocked[0].PageEnd != 3 {
		t.Fatalf("blocked planning ranges = %+v, want content %d range [1,3)", blocked, contentID)
	}
}

func TestBatchStartSubmitsOnlySparseUnownedMissingRanges(t *testing.T) {
	resetCommandState(t)
	t.Setenv("GEMINI_API_KEY", "test-key")
	api := &fakeGeminiBatchAPI{}
	oldFactory := newGeminiBatchAPI
	newGeminiBatchAPI = func(string) geminiBatchAPI { return api }
	t.Cleanup(func() { newGeminiBatchAPI = oldFactory })

	documentPath := filepath.Join(t.TempDir(), "sparse.pdf")
	if err := os.WriteFile(
		documentPath,
		commandTestPDF("one", "two", "three", "four", "five"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	digest, err := checksum.SHA256File(documentPath)
	if err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(t.TempDir(), "batch.db")
	database, err := db.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	contentID, err := database.InsertContent(digest, 5)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := database.InsertDocument(documentPath, contentID, now, now); err != nil {
		t.Fatal(err)
	}
	model := "existing"
	if err := database.UpsertContentPages(contentID, []db.PageInput{
		{PageIndex: 0, Markdown: "done", Model: &model},
		{PageIndex: 2, Markdown: "done", Model: &model},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.CreateGeminiBatch(
		"already-owned", "gemini", 1, 1, nil,
		[]db.GeminiRequestPlan{{
			ContentID: contentID, RequestKey: "owned-page-four", FileType: "pdf",
			PageStart: 3, PageEnd: 4,
		}},
		now,
	); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	cmd := commandWithDatabaseFlag(t, databasePath)
	cmd.SetContext(context.Background())
	cmd.Flags().Int("limit", 0, "")
	if err := runBatchStart(cmd, nil); err != nil {
		t.Fatalf("runBatchStart() error = %v", err)
	}
	if api.uploadCalls != 1 {
		t.Fatalf("upload calls = %d, want 1", api.uploadCalls)
	}
	database, err = db.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	rows, err := database.Query(
		`SELECT page_start, page_end FROM gemini_batch_requests
		 WHERE content_id = ? AND request_key != 'owned-page-four'
		 ORDER BY page_start`,
		contentID,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ranges []db.PageRange
	for rows.Next() {
		var pageRange db.PageRange
		if err := rows.Scan(&pageRange.Start, &pageRange.End); err != nil {
			t.Fatal(err)
		}
		ranges = append(ranges, pageRange)
	}
	want := []db.PageRange{{Start: 1, End: 2}, {Start: 4, End: 5}}
	if len(ranges) != len(want) || ranges[0] != want[0] || ranges[1] != want[1] {
		t.Fatalf("submitted ranges = %+v, want %+v", ranges, want)
	}
}
