package db

import (
	"testing"
	"time"
)

func TestForgetGeminiBatchRollsBackWithoutErasingCanonicalPages(t *testing.T) {
	database := openGeminiBatchTestDB(t)
	now := time.Now().UTC()
	contentID := insertGeminiBatchTestContent(t, database, "forget-rollback-v5", 2, "/docs/rollback.pdf")
	model := "successful-model"
	if err := database.UpsertContentPages(contentID, []PageInput{{
		PageIndex: 0, Markdown: "retained", Model: &model,
	}}); err != nil {
		t.Fatal(err)
	}
	batchID, err := database.CreateGeminiBatch(
		"forget-rollback-v5", "gemini", 1, 1, nil,
		[]GeminiRequestPlan{{
			ContentID: contentID, RequestKey: "unfinished", FileType: "pdf",
			PageStart: 1, PageEnd: 2,
		}},
		now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`
		CREATE TRIGGER fail_v5_forget BEFORE DELETE ON gemini_batches
		BEGIN SELECT RAISE(ABORT, 'forced forget failure'); END;
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ForgetGeminiBatch(batchID, now); err == nil {
		t.Fatal("ForgetGeminiBatch() error = nil")
	}
	var batches, requests, pages int
	if err := database.QueryRow(`SELECT COUNT(*) FROM gemini_batches WHERE id = ?`, batchID).Scan(&batches); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM gemini_batch_requests WHERE batch_id = ?`, batchID).Scan(&requests); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM pages WHERE content_id = ?`, contentID).Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if batches != 1 || requests != 1 || pages != 1 {
		t.Fatalf("after rollback: batches=%d requests=%d pages=%d", batches, requests, pages)
	}
	if _, err := database.Exec(`DROP TRIGGER fail_v5_forget`); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ForgetGeminiBatch(batchID, now); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM pages WHERE content_id = ?`, contentID).Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if pages != 1 {
		t.Fatalf("canonical pages after successful forget = %d, want 1", pages)
	}
}

func TestGeminiPagesAreCanonicalImmediatelyAndForgetPreservesLineage(t *testing.T) {
	database := openGeminiBatchTestDB(t)
	now := time.Now().UTC()
	contentID := insertGeminiBatchTestContent(t, database, "partial-batch", 3, "/docs/partial.pdf")
	batchID, err := database.CreateGeminiBatch(
		"partial-batch", "gemini", 1, 1, nil,
		[]GeminiRequestPlan{
			{ContentID: contentID, RequestKey: "first", FileType: "pdf", PageStart: 0, PageEnd: 1},
			{ContentID: contentID, RequestKey: "replacement", FileType: "pdf", PageStart: 1, PageEnd: 2},
			{ContentID: contentID, RequestKey: "unfinished", FileType: "pdf", PageStart: 2, PageEnd: 3},
		},
		now,
	)
	if err != nil {
		t.Fatal(err)
	}
	requests, err := database.GeminiRequestsForBatch(batchID)
	if err != nil || len(requests) != 3 {
		t.Fatalf("requests = %+v, %v", requests, err)
	}
	if err := database.StageGeminiRequest(
		requests[0].ID,
		[]GeminiStagedPage{{PageIndex: 0, Markdown: "first page", Model: "gemini-exact-v1"}},
		nil, nil, 0, false, now,
	); err != nil {
		t.Fatal(err)
	}
	var canonical int
	var model string
	if err := database.QueryRow(
		`SELECT COUNT(*), MAX(model) FROM pages WHERE content_id = ?`, contentID,
	).Scan(&canonical, &model); err != nil {
		t.Fatal(err)
	}
	if canonical != 1 || model != "gemini-exact-v1" {
		t.Fatalf("canonical pages = %d, model = %q", canonical, model)
	}
	content, err := database.GetContentByID(contentID)
	if err != nil || content == nil || !content.OCRPending {
		t.Fatalf("content = %+v, %v; want partial pending", content, err)
	}

	if retryable, err := database.RetryGeminiRequest(requests[1].ID, "replace", now); err != nil || !retryable {
		t.Fatalf("RetryGeminiRequest() = %t, %v", retryable, err)
	}
	replacementID, err := database.CreateGeminiBatchForRequests(
		"replacement-batch", "gemini", 1, 1, &batchID, []int64{requests[1].ID}, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.ForgetGeminiBatch(batchID, now); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM pages WHERE content_id = ?`, contentID,
	).Scan(&canonical); err != nil {
		t.Fatal(err)
	}
	if canonical != 1 {
		t.Fatalf("canonical pages after forget = %d, want 1", canonical)
	}
	var replacementRequests, oldOwned int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM gemini_batch_requests WHERE batch_id = ?`, replacementID,
	).Scan(&replacementRequests); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM gemini_batch_requests WHERE batch_id = ?`, batchID,
	).Scan(&oldOwned); err != nil {
		t.Fatal(err)
	}
	if replacementRequests != 1 || oldOwned != 0 {
		t.Fatalf("replacement requests = %d, old owned = %d", replacementRequests, oldOwned)
	}
	ranges, err := database.MissingUnownedPageRanges(contentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ranges) != 1 || ranges[0] != (PageRange{Start: 2, End: 3}) {
		t.Fatalf("unowned missing ranges = %+v, want page 3", ranges)
	}
}
