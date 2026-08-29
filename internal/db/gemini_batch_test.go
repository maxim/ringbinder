package db

import (
	"path/filepath"
	"testing"
	"time"
)

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

func TestCreateGeminiBatchForRequestsTracksReplacementContent(t *testing.T) {
	database := openGeminiBatchTestDB(t)
	contentID := insertGeminiBatchTestContent(t, database, "replacement-provenance", 1, "/docs/replacement.png")
	now := time.Now().UTC()
	originalID, err := database.CreateGeminiBatch(
		"replacement-original", "gemini", 1, 1, nil,
		[]GeminiRequestPlan{{
			ContentID: contentID, RequestKey: "replacement-key", FileType: "png", PageStart: 0, PageEnd: 1,
		}},
		now,
	)
	if err != nil {
		t.Fatalf("CreateGeminiBatch() error = %v", err)
	}
	requests, err := database.GeminiRequestsForBatch(originalID)
	if err != nil || len(requests) != 1 {
		t.Fatalf("GeminiRequestsForBatch() = %+v, %v", requests, err)
	}
	if retryable, err := database.RetryGeminiRequest(requests[0].ID, "retry", now); err != nil || !retryable {
		t.Fatalf("RetryGeminiRequest() = %t, %v; want true, nil", retryable, err)
	}
	if _, err := database.FinalizeGeminiBatch(originalID, now); err != nil {
		t.Fatalf("FinalizeGeminiBatch() error = %v", err)
	}
	replacementID, err := database.CreateGeminiBatchForRequests(
		"replacement-new", "gemini", 1, 1, &originalID, []int64{requests[0].ID}, now,
	)
	if err != nil {
		t.Fatalf("CreateGeminiBatchForRequests() error = %v", err)
	}
	var provenance int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM gemini_batch_contents WHERE batch_id = ? AND content_id = ?`,
		replacementID, contentID,
	).Scan(&provenance); err != nil {
		t.Fatalf("count replacement provenance: %v", err)
	}
	if provenance != 1 {
		t.Fatalf("replacement provenance = %d, want 1", provenance)
	}
}

func TestCompleteGeminiDirectRequestRejectsPagesOutsideRequest(t *testing.T) {
	database := openGeminiBatchTestDB(t)
	contentID := insertGeminiBatchTestContent(t, database, "direct-bounds", 3, "/docs/bounds.pdf")
	now := time.Now().UTC()
	requestID, err := database.CreateBlockedGeminiRequest(GeminiRequestPlan{
		ContentID: contentID, RequestKey: "direct-bounds", FileType: "pdf",
		PageStart: 1, PageEnd: 2,
	}, "blocked", now)
	if err != nil {
		t.Fatal(err)
	}
	model := "gemini-direct"
	if _, err := database.CompleteGeminiDirectRequest(
		requestID,
		[]PageInput{{PageIndex: 0, Markdown: "outside", Model: &model}},
		10,
		false,
		now,
	); err == nil {
		t.Fatal("CompleteGeminiDirectRequest() error = nil, want request-boundary rejection")
	}
	var pages, knownCost int
	var state string
	if err := database.QueryRow(`SELECT COUNT(*) FROM pages WHERE content_id = ?`, contentID).Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(
		`SELECT state, known_cost FROM gemini_batch_requests WHERE id = ?`, requestID,
	).Scan(&state, &knownCost); err != nil {
		t.Fatal(err)
	}
	if pages != 0 || state != GeminiRequestBlocked || knownCost != 0 {
		t.Fatalf("rejected completion left pages=%d state=%q cost=%d", pages, state, knownCost)
	}
}

func TestCompleteGeminiDirectRequestCommitsFinalPagesAndStateAtomically(t *testing.T) {
	database := openGeminiBatchTestDB(t)
	contentID := insertGeminiBatchTestContent(t, database, "direct-atomic", 2, "/docs/direct.pdf")
	now := time.Now().UTC()
	model := "historical"
	if err := database.UpsertContentPages(contentID, []PageInput{{
		PageIndex: 0, Markdown: "first", Model: &model,
	}}); err != nil {
		t.Fatal(err)
	}
	requestID, err := database.CreateBlockedGeminiRequest(GeminiRequestPlan{
		ContentID: contentID, RequestKey: "direct-atomic", FileType: "pdf",
		PageStart: 1, PageEnd: 2,
	}, "blocked", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`
		CREATE TRIGGER fail_direct_completion
		BEFORE UPDATE OF state ON gemini_batch_requests
		WHEN new.state = 'staged'
		BEGIN
			SELECT RAISE(ABORT, 'fail direct completion');
		END;
	`); err != nil {
		t.Fatal(err)
	}
	finalModel := "gemini-direct"
	finalPages := []PageInput{{PageIndex: 1, Markdown: "second", Model: &finalModel}}
	if _, err := database.CompleteGeminiDirectRequest(
		requestID, finalPages, 10, false, now,
	); err == nil {
		t.Fatal("CompleteGeminiDirectRequest() error = nil, want forced rollback")
	}
	missing, err := database.MissingPageIndexes(contentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 1 || missing[0] != 1 {
		t.Fatalf("missing pages after rollback = %v, want [1]", missing)
	}
	request, err := database.GeminiRequestByID(requestID)
	if err != nil || request == nil || request.State != GeminiRequestBlocked {
		t.Fatalf("request after rollback = %+v, %v; want blocked", request, err)
	}
	if _, err := database.Exec(`DROP TRIGGER fail_direct_completion`); err != nil {
		t.Fatal(err)
	}
	complete, err := database.CompleteGeminiDirectRequest(
		requestID, finalPages, 10, false, now,
	)
	if err != nil || !complete {
		t.Fatalf("CompleteGeminiDirectRequest() = %t, %v; want true, nil", complete, err)
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
