package db

import (
	"path/filepath"
	"testing"
	"time"
)

func TestOpenMigratesV2ToCurrentGeminiBatchSchema(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "migration.db")
	database, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	for _, table := range []string{
		"gemini_batch_pages", "gemini_batch_requests", "gemini_batch_contents",
		"gemini_batch_cleanup", "gemini_batches",
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
		"gemini_batches", "gemini_batch_contents", "gemini_batch_requests",
		"gemini_batch_pages", "gemini_batch_cleanup",
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

func TestOpenMigratesV3BatchContentProvenance(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "migration-v3.db")
	database, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	now := time.Now().UTC()
	currentID := insertGeminiBatchTestContent(t, database, "migration-current", 1, "/docs/current.png")
	retryID := insertGeminiBatchTestContent(t, database, "migration-retry", 1, "/docs/retry.png")
	stagedID := insertGeminiBatchTestContent(t, database, "migration-staged", 1, "/docs/staged.png")
	promotedID := insertGeminiBatchTestContent(t, database, "migration-promoted", 1, "/docs/promoted.png")
	promotedSurvivorID := insertGeminiBatchTestContent(
		t, database, "migration-promoted-survivor", 1, "/docs/promoted-survivor.png",
	)
	currentBatchID, err := database.CreateGeminiBatch(
		"migration-current", "gemini", 1, 1, nil,
		[]GeminiRequestPlan{{
			ContentID: currentID, RequestKey: "migration-current-key", FileType: "png", PageStart: 0, PageEnd: 1,
		}},
		now,
	)
	if err != nil {
		t.Fatalf("CreateGeminiBatch(current) error = %v", err)
	}
	retryBatchID, err := database.CreateGeminiBatch(
		"migration-retry", "gemini", 1, 1, nil,
		[]GeminiRequestPlan{{
			ContentID: retryID, RequestKey: "migration-retry-key", FileType: "png", PageStart: 0, PageEnd: 1,
		}},
		now,
	)
	if err != nil {
		t.Fatalf("CreateGeminiBatch(retry) error = %v", err)
	}
	stagedBatchID, err := database.CreateGeminiBatch(
		"migration-staged", "gemini", 1, 1, nil,
		[]GeminiRequestPlan{{
			ContentID: stagedID, RequestKey: "migration-staged-key", FileType: "png", PageStart: 0, PageEnd: 1,
		}},
		now,
	)
	if err != nil {
		t.Fatalf("CreateGeminiBatch(staged) error = %v", err)
	}
	promotedBatchID, err := database.CreateGeminiBatch(
		"migration-promoted", "gemini", 1, 1, nil,
		[]GeminiRequestPlan{
			{
				ContentID: promotedID, RequestKey: "migration-promoted-key",
				FileType: "png", PageStart: 0, PageEnd: 1,
			},
			{
				ContentID: promotedSurvivorID, RequestKey: "migration-promoted-survivor-key",
				FileType: "png", PageStart: 0, PageEnd: 1,
			},
		},
		now,
	)
	if err != nil {
		t.Fatalf("CreateGeminiBatch(promoted) error = %v", err)
	}
	retryRequests, err := database.GeminiRequestsForBatch(retryBatchID)
	if err != nil || len(retryRequests) != 1 {
		t.Fatalf("GeminiRequestsForBatch(retry) = %+v, %v", retryRequests, err)
	}
	if retryable, err := database.RetryGeminiRequest(retryRequests[0].ID, "retry", now); err != nil || !retryable {
		t.Fatalf("RetryGeminiRequest() = %t, %v; want true, nil", retryable, err)
	}
	stagedRequests, err := database.GeminiRequestsForBatch(stagedBatchID)
	if err != nil || len(stagedRequests) != 1 {
		t.Fatalf("GeminiRequestsForBatch(staged) = %+v, %v", stagedRequests, err)
	}
	if err := database.StageGeminiRequest(
		stagedRequests[0].ID,
		[]GeminiStagedPage{{PageIndex: 0, Markdown: "staged"}},
		nil,
		nil,
		0,
		false,
		now,
	); err != nil {
		t.Fatalf("StageGeminiRequest() error = %v", err)
	}
	if _, err := database.Exec(
		`INSERT INTO pages (content_id, page_index, markdown, search_text)
		 VALUES (?, 0, 'promoted', 'promoted')`, promotedID,
	); err != nil {
		t.Fatalf("insert legacy promoted page: %v", err)
	}
	if _, err := database.Exec(`UPDATE contents SET ocr_pending = 0 WHERE id = ?`, promotedID); err != nil {
		t.Fatalf("mark legacy promoted content complete: %v", err)
	}
	if _, err := database.Exec(`DELETE FROM gemini_batch_requests WHERE content_id = ?`, promotedID); err != nil {
		t.Fatalf("delete legacy promoted request: %v", err)
	}
	if _, err := database.Exec(`DROP TABLE gemini_batch_contents`); err != nil {
		t.Fatalf("drop v4 provenance: %v", err)
	}
	if _, err := database.Exec(
		`ALTER TABLE gemini_batches DROP COLUMN content_provenance_complete`,
	); err != nil {
		t.Fatalf("drop v4 provenance status: %v", err)
	}
	if _, err := database.Exec(`PRAGMA user_version = 3`); err != nil {
		t.Fatalf("set schema v3: %v", err)
	}
	if err := database.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	database, err = Open(dbPath)
	if err != nil {
		t.Fatalf("Open() migration error = %v", err)
	}
	defer database.Close()
	rows, err := database.Query(
		`SELECT batch_id, content_id FROM gemini_batch_contents ORDER BY batch_id, content_id`,
	)
	if err != nil {
		t.Fatalf("query migrated provenance: %v", err)
	}
	defer rows.Close()
	var got [][2]int64
	for rows.Next() {
		var pair [2]int64
		if err := rows.Scan(&pair[0], &pair[1]); err != nil {
			t.Fatalf("scan migrated provenance: %v", err)
		}
		got = append(got, pair)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("migrated provenance rows: %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close migrated provenance rows: %v", err)
	}
	want := [][2]int64{
		{currentBatchID, currentID},
		{retryBatchID, retryID},
		{stagedBatchID, stagedID},
		{promotedBatchID, promotedSurvivorID},
	}
	if len(got) != len(want) {
		t.Fatalf("provenance = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("provenance = %v, want %v", got, want)
		}
	}
	var recoverable int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM gemini_batches
		 WHERE id IN (?, ?, ?) AND content_provenance_complete = 1`,
		currentBatchID, retryBatchID, stagedBatchID,
	).Scan(&recoverable); err != nil {
		t.Fatalf("count recoverable migrated batches: %v", err)
	}
	if recoverable != 3 {
		t.Fatalf("recoverable migrated batches = %d, want 3", recoverable)
	}
	if _, err := database.ForgetGeminiBatch(stagedBatchID, now); err != nil {
		t.Fatalf("ForgetGeminiBatch(migrated staged) error = %v", err)
	}
	stagedContent, err := database.GetContentByID(stagedID)
	if err != nil {
		t.Fatalf("GetContentByID(migrated staged) error = %v", err)
	}
	if stagedContent == nil || !stagedContent.OCRPending {
		t.Fatalf("migrated staged content = %+v, want fresh pending content", stagedContent)
	}
	var promotedComplete int
	if err := database.QueryRow(
		`SELECT content_provenance_complete FROM gemini_batches WHERE id = ?`, promotedBatchID,
	).Scan(&promotedComplete); err != nil {
		t.Fatalf("read legacy promoted provenance status: %v", err)
	}
	if promotedComplete != 0 {
		t.Fatalf("legacy promoted provenance status = %d, want incomplete", promotedComplete)
	}
	if _, err := database.ForgetGeminiBatch(promotedBatchID, now); err == nil {
		t.Fatal("ForgetGeminiBatch(legacy promoted) error = nil, want safety rejection")
	}
	promotedBatch, err := database.GetGeminiBatch(promotedBatchID)
	if err != nil {
		t.Fatalf("GetGeminiBatch(legacy promoted) error = %v", err)
	}
	var promotedPages int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM pages WHERE content_id = ?`, promotedID,
	).Scan(&promotedPages); err != nil {
		t.Fatalf("count legacy promoted pages: %v", err)
	}
	var promotedSurvivorRequests int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM gemini_batch_requests WHERE content_id = ?`, promotedSurvivorID,
	).Scan(&promotedSurvivorRequests); err != nil {
		t.Fatalf("count legacy promoted survivor requests: %v", err)
	}
	if promotedBatch == nil || promotedPages != 1 || promotedSurvivorRequests != 1 {
		t.Fatalf(
			"legacy promoted batch = %+v, pages = %d, survivor requests = %d; want preserved",
			promotedBatch, promotedPages, promotedSurvivorRequests,
		)
	}
	if _, err := database.Exec(`DELETE FROM gemini_batch_requests WHERE batch_id = ?`, currentBatchID); err != nil {
		t.Fatalf("delete migrated batch request: %v", err)
	}
	if _, err := database.Exec(`DELETE FROM gemini_batches WHERE id = ?`, currentBatchID); err != nil {
		t.Fatalf("delete migrated batch: %v", err)
	}
	if _, err := database.Exec(`DELETE FROM documents WHERE content_id = ?`, retryID); err != nil {
		t.Fatalf("delete migrated document: %v", err)
	}
	if _, err := database.Exec(`DELETE FROM contents WHERE id = ?`, retryID); err != nil {
		t.Fatalf("delete migrated content: %v", err)
	}
	var remaining int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM gemini_batch_contents
		 WHERE batch_id = ? OR content_id = ?`,
		currentBatchID, retryID,
	).Scan(&remaining); err != nil {
		t.Fatalf("count cascaded provenance: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("cascaded provenance count = %d, want 0", remaining)
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
	var provenance int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM gemini_batch_contents WHERE batch_id = ? AND content_id = ?`,
		batchID, contentID,
	).Scan(&provenance); err != nil {
		t.Fatalf("count promoted provenance: %v", err)
	}
	if provenance != 1 {
		t.Fatalf("promoted provenance = %d, want 1", provenance)
	}

	// Promotion and finalization are separate durable steps. Forget must still
	// erase promoted OCR if the process stopped between them.
	if _, err := database.ForgetGeminiBatch(batchID, now); err != nil {
		t.Fatalf("ForgetGeminiBatch() after promotion error = %v", err)
	}
	content, err = database.GetContentByID(contentID)
	if err != nil {
		t.Fatalf("GetContentByID() after forget error = %v", err)
	}
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM pages WHERE content_id = ?`, contentID,
	).Scan(&pageCount); err != nil {
		t.Fatalf("count pages after forget: %v", err)
	}
	if content == nil || !content.OCRPending || pageCount != 0 {
		t.Fatalf("content = %+v, pages = %d; want fresh pending content after forget", content, pageCount)
	}
}

func TestForgetGeminiBatchErasesTouchedContentWork(t *testing.T) {
	database := openGeminiBatchTestDB(t)
	now := time.Now().UTC()
	targetID := insertGeminiBatchTestContent(t, database, "forget-checksum", 7, "/docs/forget.pdf")
	secondTargetID := insertGeminiBatchTestContent(t, database, "forget-second", 1, "/docs/forget-second.png")
	controlID := insertGeminiBatchTestContent(t, database, "forget-control", 1, "/docs/control.png")
	untouchedID := insertGeminiBatchTestContent(t, database, "forget-untouched", 1, "/docs/untouched.png")
	if err := database.ReplaceContentPagesDirect(
		untouchedID, []PageInput{{PageIndex: 0, Markdown: "untouched page"}}, now,
	); err != nil {
		t.Fatalf("ReplaceContentPagesDirect(untouched) error = %v", err)
	}
	batchID, err := database.CreateGeminiBatch(
		"batch-forget", "gemini", 375, 1875, nil,
		[]GeminiRequestPlan{
			{ContentID: targetID, RequestKey: "forget-staged", FileType: "pdf", PageStart: 0, PageEnd: 1},
			{ContentID: targetID, RequestKey: "forget-retryable", FileType: "pdf", PageStart: 1, PageEnd: 2},
			{ContentID: targetID, RequestKey: "forget-blocked", FileType: "pdf", PageStart: 2, PageEnd: 3},
			{ContentID: targetID, RequestKey: "forget-assigned", FileType: "pdf", PageStart: 3, PageEnd: 4},
			{ContentID: secondTargetID, RequestKey: "forget-second", FileType: "png", PageStart: 0, PageEnd: 1},
		},
		now,
	)
	if err != nil {
		t.Fatalf("CreateGeminiBatch() error = %v", err)
	}
	otherBatchID, err := database.CreateGeminiBatch(
		"batch-control", "gemini", 375, 1875, nil,
		[]GeminiRequestPlan{
			{ContentID: targetID, RequestKey: "forget-other-batch", FileType: "pdf", PageStart: 4, PageEnd: 5},
			{ContentID: controlID, RequestKey: "control-request", FileType: "png", PageStart: 0, PageEnd: 1},
		},
		now,
	)
	if err != nil {
		t.Fatalf("CreateGeminiBatch(control) error = %v", err)
	}
	emptyPreparedID, err := database.CreateGeminiBatch(
		"batch-empty-after-forget", "gemini", 375, 1875, nil,
		[]GeminiRequestPlan{{
			ContentID: targetID, RequestKey: "forget-empty-prepared", FileType: "pdf", PageStart: 5, PageEnd: 6,
		}},
		now,
	)
	if err != nil {
		t.Fatalf("CreateGeminiBatch(empty prepared) error = %v", err)
	}
	runningBatchID, err := database.CreateGeminiBatch(
		"batch-running-after-forget", "gemini", 375, 1875, nil,
		[]GeminiRequestPlan{{
			ContentID: targetID, RequestKey: "forget-running", FileType: "pdf", PageStart: 6, PageEnd: 7,
		}},
		now,
	)
	if err != nil {
		t.Fatalf("CreateGeminiBatch(running) error = %v", err)
	}
	if err := database.SetGeminiBatchRemote(
		runningBatchID, "batches/running-after-forget", GeminiBatchRunning, "", "", now,
	); err != nil {
		t.Fatalf("SetGeminiBatchRemote(running) error = %v", err)
	}
	preparedRequests, err := database.GeminiRequestsForBatch(otherBatchID)
	if err != nil || len(preparedRequests) != 2 {
		t.Fatalf("GeminiRequestsForBatch(control before forget) = %+v, %v", preparedRequests, err)
	}
	if retryable, err := database.RetryGeminiRequest(
		preparedRequests[0].ID, "detached before forget", now,
	); err != nil || !retryable {
		t.Fatalf("RetryGeminiRequest(control target) = %t, %v; want true, nil", retryable, err)
	}
	requests, err := database.GeminiRequestsForBatch(batchID)
	if err != nil || len(requests) != 5 {
		t.Fatalf("GeminiRequestsForBatch() = %+v, %v", requests, err)
	}
	if err := database.StageGeminiRequest(
		requests[0].ID,
		[]GeminiStagedPage{{PageIndex: 0, Markdown: "discarded staging"}},
		nil,
		nil,
		0,
		false,
		now,
	); err != nil {
		t.Fatalf("StageGeminiRequest() error = %v", err)
	}
	if retryable, err := database.RetryGeminiRequest(requests[1].ID, "retry", now); err != nil || !retryable {
		t.Fatalf("RetryGeminiRequest() = %t, %v; want true, nil", retryable, err)
	}
	if err := database.BlockGeminiRequest(requests[2].ID, "blocked", now); err != nil {
		t.Fatalf("BlockGeminiRequest() error = %v", err)
	}
	if err := database.StageGeminiRequest(
		requests[4].ID,
		[]GeminiStagedPage{{PageIndex: 0, Markdown: "second discarded staging"}},
		nil,
		nil,
		0,
		false,
		now,
	); err != nil {
		t.Fatalf("StageGeminiRequest(second target) error = %v", err)
	}
	if err := database.SetGeminiBatchUploaded(batchID, "files/forget", now); err != nil {
		t.Fatalf("SetGeminiBatchUploaded() error = %v", err)
	}
	if err := database.SetGeminiBatchRemote(
		batchID, "batches/forget", GeminiBatchPending, "", "", now,
	); err != nil {
		t.Fatalf("SetGeminiBatchRemote() error = %v", err)
	}
	stamp := now.Format(time.RFC3339Nano)
	for _, resource := range []struct{ kind, name string }{
		{kind: "batch", name: "batches/forget"},
		{kind: "file", name: "files/forget"},
		{kind: "file", name: "files/control"},
	} {
		if _, err := database.Exec(
			`INSERT INTO gemini_batch_cleanup
			 (resource_kind, resource_name, created_at, updated_at)
			 VALUES (?, ?, ?, ?)`,
			resource.kind, resource.name, stamp, stamp,
		); err != nil {
			t.Fatalf("insert cleanup %s: %v", resource.name, err)
		}
	}
	if _, err := database.Exec(
		`UPDATE contents SET ocr_pending = 0 WHERE id IN (?, ?)`, targetID, secondTargetID,
	); err != nil {
		t.Fatalf("mark targets complete: %v", err)
	}

	if _, err := database.ForgetGeminiBatch(batchID, now); err != nil {
		t.Fatalf("ForgetGeminiBatch() error = %v", err)
	}
	batch, err := database.GetGeminiBatch(batchID)
	if err != nil {
		t.Fatalf("GetGeminiBatch() error = %v", err)
	}
	if batch != nil {
		t.Fatalf("forgotten batch = %+v, want nil", batch)
	}
	otherBatch, err := database.GetGeminiBatch(otherBatchID)
	if err != nil || otherBatch == nil {
		t.Fatalf("control batch = %+v, %v; want retained batch", otherBatch, err)
	}
	if len(otherBatch.RequestKeys) != 1 || otherBatch.RequestKeys[0] != "control-request" {
		t.Fatalf("control manifest = %v, want only control-request", otherBatch.RequestKeys)
	}
	otherRequests, err := database.GeminiRequestsForBatch(otherBatchID)
	if err != nil {
		t.Fatalf("GeminiRequestsForBatch(control) error = %v", err)
	}
	if len(otherRequests) != 1 || otherRequests[0].ContentID != controlID {
		t.Fatalf("control requests = %+v, want only unrelated content", otherRequests)
	}
	emptyPrepared, err := database.GetGeminiBatch(emptyPreparedID)
	if err != nil {
		t.Fatalf("GetGeminiBatch(empty prepared) error = %v", err)
	}
	if emptyPrepared != nil {
		t.Fatalf("empty prepared batch = %+v, want deleted", emptyPrepared)
	}
	runningBatch, err := database.GetGeminiBatch(runningBatchID)
	if err != nil || runningBatch == nil {
		t.Fatalf("running batch = %+v, %v; want retained", runningBatch, err)
	}
	if len(runningBatch.RequestKeys) != 1 || runningBatch.RequestKeys[0] != "forget-running" {
		t.Fatalf("running manifest = %v, want immutable original key", runningBatch.RequestKeys)
	}
	runningRequests, err := database.GeminiRequestsForBatch(runningBatchID)
	if err != nil {
		t.Fatalf("GeminiRequestsForBatch(running) error = %v", err)
	}
	if len(runningRequests) != 0 {
		t.Fatalf("running requests = %+v, want forgotten content removed", runningRequests)
	}
	var requestCount, stagedPageCount int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM gemini_batch_requests WHERE content_id = ?`, targetID,
	).Scan(&requestCount); err != nil {
		t.Fatalf("count forgotten requests: %v", err)
	}
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM gemini_batch_pages WHERE content_id = ?`, targetID,
	).Scan(&stagedPageCount); err != nil {
		t.Fatalf("count forgotten staging: %v", err)
	}
	if requestCount != 0 || stagedPageCount != 0 {
		t.Fatalf("requests = %d, staged pages = %d; want 0 and 0", requestCount, stagedPageCount)
	}
	var secondRequests, secondStaged int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM gemini_batch_requests WHERE content_id = ?`, secondTargetID,
	).Scan(&secondRequests); err != nil {
		t.Fatalf("count second forgotten requests: %v", err)
	}
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM gemini_batch_pages WHERE content_id = ?`, secondTargetID,
	).Scan(&secondStaged); err != nil {
		t.Fatalf("count second forgotten staging: %v", err)
	}
	if secondRequests != 0 || secondStaged != 0 {
		t.Fatalf("second requests = %d, staged pages = %d; want 0 and 0", secondRequests, secondStaged)
	}
	var provenance int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM gemini_batch_contents WHERE content_id IN (?, ?)`,
		targetID, secondTargetID,
	).Scan(&provenance); err != nil {
		t.Fatalf("count forgotten provenance: %v", err)
	}
	if provenance != 0 {
		t.Fatalf("forgotten provenance = %d, want 0", provenance)
	}
	pending, err := database.PendingContentsForGeminiBatch()
	if err != nil {
		t.Fatalf("PendingContentsForGeminiBatch() error = %v", err)
	}
	if len(pending) != 2 || pending[0].ID != targetID || pending[1].ID != secondTargetID ||
		!pending[0].OCRPending || !pending[1].OCRPending {
		t.Fatalf("pending = %+v, want both forgotten contents ready for a fresh batch", pending)
	}
	untouched, err := database.GetContentByID(untouchedID)
	if err != nil {
		t.Fatalf("GetContentByID(untouched) error = %v", err)
	}
	var untouchedPages int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM pages WHERE content_id = ?`, untouchedID,
	).Scan(&untouchedPages); err != nil {
		t.Fatalf("count untouched pages: %v", err)
	}
	if untouched == nil || untouched.OCRPending || untouchedPages != 1 {
		t.Fatalf("untouched content = %+v, pages = %d; want completed content unchanged", untouched, untouchedPages)
	}
	cleanup, err := database.ListGeminiCleanup()
	if err != nil {
		t.Fatalf("ListGeminiCleanup() error = %v", err)
	}
	if len(cleanup) != 1 || cleanup[0].ResourceName != "files/control" {
		t.Fatalf("cleanup = %+v, want only unrelated cleanup", cleanup)
	}
}

func TestForgetGeminiBatchFindsStagedManifestRequest(t *testing.T) {
	database := openGeminiBatchTestDB(t)
	contentID := insertGeminiBatchTestContent(t, database, "forget-staged", 1, "/docs/staged.png")
	now := time.Now().UTC()
	batchID, err := database.CreateGeminiBatch(
		"batch-forget-staged", "gemini", 375, 1875, nil,
		[]GeminiRequestPlan{{
			ContentID: contentID, RequestKey: "forgotten-staged", FileType: "png", PageStart: 0, PageEnd: 1,
		}},
		now,
	)
	if err != nil {
		t.Fatalf("CreateGeminiBatch() error = %v", err)
	}
	requests, err := database.GeminiRequestsForBatch(batchID)
	if err != nil || len(requests) != 1 {
		t.Fatalf("GeminiRequestsForBatch() = %+v, %v", requests, err)
	}
	if err := database.StageGeminiRequest(
		requests[0].ID,
		[]GeminiStagedPage{{PageIndex: 0, Markdown: "staged"}},
		nil,
		nil,
		0,
		false,
		now,
	); err != nil {
		t.Fatalf("StageGeminiRequest() error = %v", err)
	}
	if _, err := database.Exec(`UPDATE contents SET ocr_pending = 0 WHERE id = ?`, contentID); err != nil {
		t.Fatalf("mark staged content complete: %v", err)
	}

	if _, err := database.ForgetGeminiBatch(batchID, now); err != nil {
		t.Fatalf("ForgetGeminiBatch() error = %v", err)
	}
	request, err := database.GeminiRequestByID(requests[0].ID)
	if err != nil {
		t.Fatalf("GeminiRequestByID() error = %v", err)
	}
	content, err := database.GetContentByID(contentID)
	if err != nil {
		t.Fatalf("GetContentByID() error = %v", err)
	}
	var stagedPages int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM gemini_batch_pages WHERE content_id = ?`, contentID,
	).Scan(&stagedPages); err != nil {
		t.Fatalf("count staged pages: %v", err)
	}
	if request != nil || content == nil || !content.OCRPending || stagedPages != 0 {
		t.Fatalf("request = %+v, content = %+v, staged pages = %d; want fresh pending content", request, content, stagedPages)
	}
}

func TestDirectOCRSupersedesBatchProvenanceBeforeForget(t *testing.T) {
	database := openGeminiBatchTestDB(t)
	contentID := insertGeminiBatchTestContent(t, database, "direct-provenance", 3, "/docs/direct.pdf")
	controlID := insertGeminiBatchTestContent(t, database, "direct-control", 1, "/docs/control.png")
	now := time.Now().UTC()
	runningID, err := database.CreateGeminiBatch(
		"batch-direct-provenance", "gemini", 375, 1875, nil,
		[]GeminiRequestPlan{{
			ContentID: contentID, RequestKey: "direct-running", FileType: "pdf", PageStart: 0, PageEnd: 1,
		}},
		now,
	)
	if err != nil {
		t.Fatalf("CreateGeminiBatch(running) error = %v", err)
	}
	if err := database.SetGeminiBatchRemote(
		runningID, "batches/direct-provenance", GeminiBatchRunning, "", "", now,
	); err != nil {
		t.Fatalf("SetGeminiBatchRemote() error = %v", err)
	}
	mixedPreparedID, err := database.CreateGeminiBatch(
		"batch-direct-mixed", "gemini", 375, 1875, nil,
		[]GeminiRequestPlan{
			{ContentID: contentID, RequestKey: "direct-mixed-target", FileType: "pdf", PageStart: 1, PageEnd: 2},
			{ContentID: controlID, RequestKey: "direct-mixed-control", FileType: "png", PageStart: 0, PageEnd: 1},
		},
		now,
	)
	if err != nil {
		t.Fatalf("CreateGeminiBatch(mixed prepared) error = %v", err)
	}
	emptyPreparedID, err := database.CreateGeminiBatch(
		"batch-direct-empty", "gemini", 375, 1875, nil,
		[]GeminiRequestPlan{{
			ContentID: contentID, RequestKey: "direct-empty", FileType: "pdf", PageStart: 2, PageEnd: 3,
		}},
		now,
	)
	if err != nil {
		t.Fatalf("CreateGeminiBatch(empty prepared) error = %v", err)
	}
	for _, preparedID := range []int64{mixedPreparedID, emptyPreparedID} {
		requests, err := database.GeminiRequestsForBatch(preparedID)
		if err != nil || len(requests) == 0 {
			t.Fatalf("GeminiRequestsForBatch(%d) = %+v, %v", preparedID, requests, err)
		}
		if retryable, err := database.RetryGeminiRequest(
			requests[0].ID, "use direct OCR", now,
		); err != nil || !retryable {
			t.Fatalf("RetryGeminiRequest(%d) = %t, %v; want true, nil", preparedID, retryable, err)
		}
	}
	runningRequests, err := database.GeminiRequestsForBatch(runningID)
	if err != nil || len(runningRequests) != 1 {
		t.Fatalf("GeminiRequestsForBatch(running) = %+v, %v", runningRequests, err)
	}
	if err := database.BlockGeminiRequest(runningRequests[0].ID, "use direct OCR", now); err != nil {
		t.Fatalf("BlockGeminiRequest() error = %v", err)
	}
	directPages := []PageInput{
		{PageIndex: 0, Markdown: "new direct page 1"},
		{PageIndex: 1, Markdown: "new direct page 2"},
		{PageIndex: 2, Markdown: "new direct page 3"},
	}
	if err := database.ReplaceContentPagesDirect(contentID, directPages, now); err != nil {
		t.Fatalf("ReplaceContentPagesDirect() error = %v", err)
	}
	var provenance int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM gemini_batch_contents WHERE content_id = ?`, contentID,
	).Scan(&provenance); err != nil {
		t.Fatalf("count direct provenance: %v", err)
	}
	if provenance != 0 {
		t.Fatalf("direct provenance = %d, want 0", provenance)
	}
	mixedPrepared, err := database.GetGeminiBatch(mixedPreparedID)
	if err != nil || mixedPrepared == nil {
		t.Fatalf("mixed prepared batch = %+v, %v; want retained", mixedPrepared, err)
	}
	if len(mixedPrepared.RequestKeys) != 1 || mixedPrepared.RequestKeys[0] != "direct-mixed-control" {
		t.Fatalf("mixed prepared manifest = %v, want only control", mixedPrepared.RequestKeys)
	}
	emptyPrepared, err := database.GetGeminiBatch(emptyPreparedID)
	if err != nil {
		t.Fatalf("GetGeminiBatch(empty prepared) error = %v", err)
	}
	if emptyPrepared != nil {
		t.Fatalf("empty prepared batch = %+v, want deleted", emptyPrepared)
	}

	if _, err := database.ForgetGeminiBatch(runningID, now); err != nil {
		t.Fatalf("ForgetGeminiBatch() error = %v", err)
	}
	content, err := database.GetContentByID(contentID)
	if err != nil {
		t.Fatalf("GetContentByID() error = %v", err)
	}
	rows, err := database.Query(
		`SELECT markdown FROM pages WHERE content_id = ? ORDER BY page_index`, contentID,
	)
	if err != nil {
		t.Fatalf("read direct pages: %v", err)
	}
	defer rows.Close()
	var markdown []string
	for rows.Next() {
		var page string
		if err := rows.Scan(&page); err != nil {
			t.Fatalf("scan direct page: %v", err)
		}
		markdown = append(markdown, page)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("direct pages: %v", err)
	}
	if content == nil || content.OCRPending || len(markdown) != 3 || markdown[0] != "new direct page 1" {
		t.Fatalf("content = %+v, markdown = %q; want superseding direct OCR preserved", content, markdown)
	}
}

func TestForgetGeminiBatchRollsBackAllLocalDeletion(t *testing.T) {
	database := openGeminiBatchTestDB(t)
	contentID := insertGeminiBatchTestContent(t, database, "forget-rollback", 1, "/docs/rollback.png")
	now := time.Now().UTC()
	batchID, err := database.CreateGeminiBatch(
		"batch-forget-rollback", "gemini", 375, 1875, nil,
		[]GeminiRequestPlan{{
			ContentID: contentID, RequestKey: "forgotten-rollback", FileType: "png", PageStart: 0, PageEnd: 1,
		}},
		now,
	)
	if err != nil {
		t.Fatalf("CreateGeminiBatch() error = %v", err)
	}
	requests, err := database.GeminiRequestsForBatch(batchID)
	if err != nil || len(requests) != 1 {
		t.Fatalf("GeminiRequestsForBatch() = %+v, %v", requests, err)
	}
	if err := database.StageGeminiRequest(
		requests[0].ID,
		[]GeminiStagedPage{{PageIndex: 0, Markdown: "preserved"}},
		nil,
		nil,
		0,
		false,
		now,
	); err != nil {
		t.Fatalf("StageGeminiRequest() error = %v", err)
	}
	if err := database.SetGeminiBatchUploaded(batchID, "files/rollback", now); err != nil {
		t.Fatalf("SetGeminiBatchUploaded() error = %v", err)
	}
	if err := database.SetGeminiBatchRemote(
		batchID, "batches/rollback", GeminiBatchPending, "", "", now,
	); err != nil {
		t.Fatalf("SetGeminiBatchRemote() error = %v", err)
	}
	stamp := now.Format(time.RFC3339Nano)
	if _, err := database.Exec(
		`INSERT INTO gemini_batch_cleanup
		 (resource_kind, resource_name, created_at, updated_at)
		 VALUES ('batch', 'batches/rollback', ?, ?)`,
		stamp, stamp,
	); err != nil {
		t.Fatalf("insert rollback cleanup: %v", err)
	}
	if _, err := database.Exec(`UPDATE contents SET ocr_pending = 0 WHERE id = ?`, contentID); err != nil {
		t.Fatalf("mark rollback content complete: %v", err)
	}
	if _, err := database.Exec(
		`INSERT INTO pages (content_id, page_index, markdown, search_text)
		 VALUES (?, 0, 'canonical preserved', 'canonical preserved')`, contentID,
	); err != nil {
		t.Fatalf("insert canonical rollback page: %v", err)
	}
	if _, err := database.Exec(
		`CREATE TEMP TRIGGER abort_forget BEFORE DELETE ON gemini_batches BEGIN
		   SELECT RAISE(ABORT, 'stop forget');
		 END`,
	); err != nil {
		t.Fatalf("create abort trigger: %v", err)
	}

	if _, err := database.ForgetGeminiBatch(batchID, now); err == nil {
		t.Fatal("ForgetGeminiBatch() error = nil, want trigger failure")
	}
	batch, err := database.GetGeminiBatch(batchID)
	if err != nil {
		t.Fatalf("GetGeminiBatch() error = %v", err)
	}
	request, err := database.GeminiRequestByID(requests[0].ID)
	if err != nil {
		t.Fatalf("GeminiRequestByID() error = %v", err)
	}
	content, err := database.GetContentByID(contentID)
	if err != nil {
		t.Fatalf("GetContentByID() error = %v", err)
	}
	cleanup, err := database.ListGeminiCleanup()
	if err != nil {
		t.Fatalf("ListGeminiCleanup() error = %v", err)
	}
	var markdown string
	if err := database.QueryRow(
		`SELECT markdown FROM gemini_batch_pages WHERE content_id = ? AND page_index = 0`, contentID,
	).Scan(&markdown); err != nil {
		t.Fatalf("read staged rollback page: %v", err)
	}
	var provenance int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM gemini_batch_contents WHERE batch_id = ? AND content_id = ?`,
		batchID, contentID,
	).Scan(&provenance); err != nil {
		t.Fatalf("count rollback provenance: %v", err)
	}
	var canonical string
	if err := database.QueryRow(
		`SELECT markdown FROM pages WHERE content_id = ? AND page_index = 0`, contentID,
	).Scan(&canonical); err != nil {
		t.Fatalf("read canonical rollback page: %v", err)
	}
	if batch == nil || request == nil || request.State != GeminiRequestStaged ||
		content == nil || content.OCRPending || len(cleanup) != 1 ||
		markdown != "preserved" || canonical != "canonical preserved" || provenance != 1 {
		t.Fatalf(
			"batch = %+v, request = %+v, content = %+v, cleanup = %+v, staged = %q, canonical = %q, provenance = %d; want full rollback",
			batch, request, content, cleanup, markdown, canonical, provenance,
		)
	}
}

func TestForgetGeminiBatchFindsSplitDescendants(t *testing.T) {
	database := openGeminiBatchTestDB(t)
	contentID := insertGeminiBatchTestContent(t, database, "forget-split", 2, "/docs/split.pdf")
	now := time.Now().UTC()
	batchID, err := database.CreateGeminiBatch(
		"batch-forget-split", "gemini", 375, 1875, nil,
		[]GeminiRequestPlan{{
			ContentID: contentID, RequestKey: "forgotten-parent", FileType: "pdf", PageStart: 0, PageEnd: 2,
		}},
		now,
	)
	if err != nil {
		t.Fatalf("CreateGeminiBatch() error = %v", err)
	}
	requests, err := database.GeminiRequestsForBatch(batchID)
	if err != nil || len(requests) != 1 {
		t.Fatalf("GeminiRequestsForBatch() = %+v, %v", requests, err)
	}
	if _, err := database.SplitGeminiRequest(
		requests[0].ID, "forgotten-left", "forgotten-right", "split", now,
	); err != nil {
		t.Fatalf("SplitGeminiRequest() error = %v", err)
	}

	if _, err := database.ForgetGeminiBatch(batchID, now); err != nil {
		t.Fatalf("ForgetGeminiBatch() error = %v", err)
	}
	var requestCount int
	if err := database.QueryRow(
		`SELECT COUNT(*) FROM gemini_batch_requests WHERE content_id = ?`, contentID,
	).Scan(&requestCount); err != nil {
		t.Fatalf("count split descendants: %v", err)
	}
	if requestCount != 0 {
		t.Fatalf("split descendant count = %d, want 0", requestCount)
	}
}

func TestFinalizeGeminiBatchQueuesDeletableRemoteResources(t *testing.T) {
	database := openGeminiBatchTestDB(t)
	contentID := insertGeminiBatchTestContent(t, database, "cleanup-checksum", 1, "/docs/cleanup.png")
	now := time.Now().UTC()
	batchID, err := database.CreateGeminiBatch(
		"cleanup-batch", "gemini", 375, 1875, nil,
		[]GeminiRequestPlan{{
			ContentID: contentID, RequestKey: "cleanup-key", FileType: "png", PageStart: 0, PageEnd: 1,
		}},
		now,
	)
	if err != nil {
		t.Fatalf("CreateGeminiBatch() error = %v", err)
	}
	if err := database.SetGeminiBatchUploaded(batchID, "files/input", now); err != nil {
		t.Fatalf("SetGeminiBatchUploaded() error = %v", err)
	}
	if err := database.SetGeminiBatchRemote(
		batchID, "batches/remote", GeminiBatchSucceeded, "files/batch-output", "", now,
	); err != nil {
		t.Fatalf("SetGeminiBatchRemote() error = %v", err)
	}
	requests, err := database.GeminiRequestsForBatch(batchID)
	if err != nil || len(requests) != 1 {
		t.Fatalf("GeminiRequestsForBatch() = %+v, %v", requests, err)
	}
	if err := database.StageGeminiRequest(
		requests[0].ID, []GeminiStagedPage{{PageIndex: 0, Markdown: "page"}}, nil, nil, 0, false, now,
	); err != nil {
		t.Fatalf("StageGeminiRequest() error = %v", err)
	}

	cleanup, err := database.FinalizeGeminiBatch(batchID, now)
	if err != nil {
		t.Fatalf("FinalizeGeminiBatch() error = %v", err)
	}
	if len(cleanup) != 2 {
		t.Fatalf("cleanup = %+v, want remote batch and uploaded input", cleanup)
	}
	if cleanup[0].ResourceKind != "batch" || cleanup[0].ResourceName != "batches/remote" {
		t.Fatalf("first cleanup = %+v, want remote batch", cleanup[0])
	}
	if cleanup[1].ResourceKind != "file" || cleanup[1].ResourceName != "files/input" {
		t.Fatalf("second cleanup = %+v, want uploaded input", cleanup[1])
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
