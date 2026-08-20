package db

import (
	"path/filepath"
	"testing"
	"time"
)

func TestOpenMigratesV2ToV3GeminiBatchSchema(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "migration.db")
	database, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	for _, table := range []string{
		"gemini_batch_pages", "gemini_batch_requests", "gemini_batch_cleanup", "gemini_batches",
	} {
		if _, err := database.Exec("DROP TABLE " + table); err != nil {
			t.Fatalf("drop %s: %v", table, err)
		}
	}
	if _, err := database.Exec("PRAGMA user_version = 2"); err != nil {
		t.Fatalf("set schema v2: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	database, err = Open(dbPath)
	if err != nil {
		t.Fatalf("Open() migration error = %v", err)
	}
	defer database.Close()
	var version int
	if err := database.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read version: %v", err)
	}
	if version != schemaVersion {
		t.Fatalf("schema version = %d, want %d", version, schemaVersion)
	}
	for _, table := range []string{
		"gemini_batches", "gemini_batch_requests", "gemini_batch_pages", "gemini_batch_cleanup",
	} {
		var count int
		if err := database.QueryRow(
			"SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?", table,
		).Scan(&count); err != nil {
			t.Fatalf("find %s: %v", table, err)
		}
		if count != 1 {
			t.Fatalf("table %s count = %d, want 1", table, count)
		}
	}
}

func TestCreateGeminiBatchWorkPersistsAllTransportJobsAtomically(t *testing.T) {
	database := openGeminiBatchTestDB(t)
	contentID := insertGeminiBatchTestContent(t, database, "atomic-checksum", 3, "/docs/atomic.pdf")
	now := time.Now().UTC()
	_, err := database.CreateGeminiBatchWork([]GeminiBatchCreation{
		{
			DisplayName: "atomic-one", Model: "gemini", InputPrice: 1, OutputPrice: 1,
			Requests: []GeminiRequestPlan{{
				ContentID: contentID, RequestKey: "duplicate-key", FileType: "pdf", PageStart: 0, PageEnd: 1,
			}},
		},
		{
			DisplayName: "atomic-two", Model: "gemini", InputPrice: 1, OutputPrice: 1,
			Requests: []GeminiRequestPlan{{
				ContentID: contentID, RequestKey: "duplicate-key", FileType: "pdf", PageStart: 1, PageEnd: 2,
			}},
		},
	}, []GeminiBlockedRequest{{
		Plan: GeminiRequestPlan{
			ContentID: contentID, RequestKey: "blocked-key", FileType: "pdf", PageStart: 2, PageEnd: 3,
		},
		Message: "blocked",
	}}, now)
	if err == nil {
		t.Fatal("CreateGeminiBatchWork() error = nil, want duplicate-key rollback")
	}
	var batches, requests int
	if err := database.QueryRow("SELECT COUNT(*) FROM gemini_batches").Scan(&batches); err != nil {
		t.Fatalf("count batches: %v", err)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM gemini_batch_requests").Scan(&requests); err != nil {
		t.Fatalf("count requests: %v", err)
	}
	if batches != 0 || requests != 0 {
		t.Fatalf("batches = %d requests = %d, want full rollback", batches, requests)
	}
}

func TestGeminiBatchOwnershipStagingAndPromotion(t *testing.T) {
	database := openGeminiBatchTestDB(t)
	contentID := insertGeminiBatchTestContent(t, database, "owned-checksum", 2, "/docs/report.pdf")
	now := time.Now().UTC()
	plans := []GeminiRequestPlan{
		{ContentID: contentID, RequestKey: "request-a", FileType: "pdf", PageStart: 0, PageEnd: 1},
		{ContentID: contentID, RequestKey: "request-b", FileType: "pdf", PageStart: 1, PageEnd: 2},
	}
	batchID, err := database.CreateGeminiBatch("batch-a", "gemini", 375, 1875, nil, plans, now)
	if err != nil {
		t.Fatalf("CreateGeminiBatch() error = %v", err)
	}

	direct, excluded, err := database.PendingContentsForDirect()
	if err != nil {
		t.Fatalf("PendingContentsForDirect() error = %v", err)
	}
	if len(direct) != 0 || excluded != 1 {
		t.Fatalf("direct = %+v, excluded = %d, want no direct content and 1 excluded", direct, excluded)
	}
	untouched, err := database.PendingContentsForGeminiBatch()
	if err != nil {
		t.Fatalf("PendingContentsForGeminiBatch() error = %v", err)
	}
	if len(untouched) != 0 {
		t.Fatalf("untouched = %+v, want none", untouched)
	}

	requests, err := database.GeminiRequestsForBatch(batchID)
	if err != nil {
		t.Fatalf("GeminiRequestsForBatch() error = %v", err)
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(requests))
	}
	input, output := int64(10), int64(20)
	for i, request := range requests {
		page := GeminiStagedPage{PageIndex: i, Markdown: "page text"}
		if err := database.StageGeminiRequest(request.ID, []GeminiStagedPage{page}, &input, &output, 41_250, false, now); err != nil {
			t.Fatalf("StageGeminiRequest(%d) error = %v", request.ID, err)
		}
	}
	promoted, err := database.PromoteReadyGeminiContents()
	if err != nil {
		t.Fatalf("PromoteReadyGeminiContents() error = %v", err)
	}
	if promoted != 1 {
		t.Fatalf("promoted = %d, want 1", promoted)
	}
	content, err := database.GetContentByID(contentID)
	if err != nil {
		t.Fatalf("GetContentByID() error = %v", err)
	}
	if content == nil || content.OCRPending {
		t.Fatalf("content = %+v, want OCR complete", content)
	}
	var pageCount, requestCount int
	if err := database.QueryRow("SELECT COUNT(*) FROM pages WHERE content_id = ?", contentID).Scan(&pageCount); err != nil {
		t.Fatalf("count pages: %v", err)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM gemini_batch_requests WHERE content_id = ?", contentID).Scan(&requestCount); err != nil {
		t.Fatalf("count requests: %v", err)
	}
	if pageCount != 2 || requestCount != 0 {
		t.Fatalf("pages = %d, requests = %d, want 2 and 0", pageCount, requestCount)
	}
}

func TestForgetGeminiBatchBlocksAndReleasesDirectOwnership(t *testing.T) {
	database := openGeminiBatchTestDB(t)
	contentID := insertGeminiBatchTestContent(t, database, "forget-checksum", 1, "/docs/forget.png")
	now := time.Now().UTC()
	batchID, err := database.CreateGeminiBatch(
		"batch-forget", "gemini", 375, 1875, nil,
		[]GeminiRequestPlan{{ContentID: contentID, RequestKey: "forget-key", FileType: "png", PageStart: 0, PageEnd: 1}},
		now,
	)
	if err != nil {
		t.Fatalf("CreateGeminiBatch() error = %v", err)
	}
	if _, err := database.ForgetGeminiBatch(batchID, now); err != nil {
		t.Fatalf("ForgetGeminiBatch() error = %v", err)
	}
	direct, excluded, err := database.PendingContentsForDirect()
	if err != nil {
		t.Fatalf("PendingContentsForDirect() error = %v", err)
	}
	if len(direct) != 1 || excluded != 0 {
		t.Fatalf("direct = %+v, excluded = %d, want released content", direct, excluded)
	}
	blocked, err := database.BlockedGeminiRequests()
	if err != nil {
		t.Fatalf("BlockedGeminiRequests() error = %v", err)
	}
	if len(blocked) != 1 || blocked[0].ContentID != contentID {
		t.Fatalf("blocked = %+v, want one request", blocked)
	}
	cleanup, err := database.CountGeminiCleanup()
	if err != nil {
		t.Fatalf("CountGeminiCleanup() error = %v", err)
	}
	if cleanup != 0 {
		t.Fatalf("cleanup = %d, want no cleanup for forgotten remote resources", cleanup)
	}
}

func TestGeminiBatchStateChecksRejectInvalidRows(t *testing.T) {
	database := openGeminiBatchTestDB(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := database.Exec(
		`INSERT INTO gemini_batches
		 (display_name, model, request_keys, state, input_price, output_price, created_at, updated_at)
		 VALUES ('bad', 'gemini', '[]', 'unknown', 1, 1, ?, ?)`, now, now,
	); err == nil {
		t.Fatal("invalid Gemini batch state was accepted")
	}
	contentID := insertGeminiBatchTestContent(t, database, "check-checksum", 1, "/docs/check.png")
	if _, err := database.Exec(
		`INSERT INTO gemini_batch_requests
		 (content_id, request_key, file_type, page_start, page_end, state, created_at, updated_at)
		 VALUES (?, 'bad-request', 'png', 0, 1, 'assigned', ?, ?)`, contentID, now, now,
	); err == nil {
		t.Fatal("assigned request without a batch was accepted")
	}
}

func openGeminiBatchTestDB(t *testing.T) *DB {
	t.Helper()
	database, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func insertGeminiBatchTestContent(t *testing.T, database *DB, checksum string, pages int, path string) int64 {
	t.Helper()
	contentID, err := database.InsertContent(checksum, pages)
	if err != nil {
		t.Fatalf("InsertContent() error = %v", err)
	}
	now := time.Now().UTC()
	if _, err := database.InsertDocument(path, contentID, now, now); err != nil {
		t.Fatalf("InsertDocument() error = %v", err)
	}
	return contentID
}
