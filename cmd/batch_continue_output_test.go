package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maxim/ringbinder/internal/checksum"
	"github.com/maxim/ringbinder/internal/db"
	"github.com/maxim/ringbinder/internal/ocr"
	"github.com/spf13/cobra"
)

func TestBatchContinueReportsNoTrackedWork(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "empty.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	output, err := runBatchContinueWithFake(t, dbPath, &fakeGeminiBatchAPI{})
	if err != nil {
		t.Fatalf("runBatchContinue() error = %v", err)
	}
	if output != "No tracked Gemini batch work to continue.\n" {
		t.Fatalf("output = %q", output)
	}
}

func TestBatchContinueReportsBlockedWorkWhenNoBatchesRemain(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "blocked.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	contentID := addCostContent(t, database, "blocked-content", 1, true, "/docs/blocked.png")
	if _, err := database.CreateBlockedGeminiRequest(
		db.GeminiRequestPlan{
			ContentID: contentID, RequestKey: "blocked-key",
			FileType: "png", PageStart: 0, PageEnd: 1,
		},
		"failed",
		time.Now().UTC(),
	); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	output, err := runBatchContinueWithFake(t, dbPath, &fakeGeminiBatchAPI{})
	if err != nil {
		t.Fatalf("runBatchContinue() error = %v", err)
	}
	want := "1 blocked Gemini batch OCR page range across 1 content item requires attention.\n" +
		"Run `ringbinder batch failures` for details and recovery commands.\n"
	if output != want {
		t.Fatalf("output = %q, want %q", output, want)
	}
}

func TestBatchContinueReportsPollOnlyWork(t *testing.T) {
	tests := []struct {
		name        string
		localState  string
		remoteState string
	}{
		{name: "pending", localState: db.GeminiBatchPending, remoteState: "JOB_STATE_PENDING"},
		{name: "running", localState: db.GeminiBatchRunning, remoteState: "JOB_STATE_RUNNING"},
		{name: "cancelling", localState: db.GeminiBatchCancelling, remoteState: "JOB_STATE_CANCELLING"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "poll-only.db")
			database, err := db.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			batch, _ := addPreparedImageTestBatch(t, database, "poll-"+test.name)
			if err := database.SetGeminiBatchRemote(
				batch.ID, "batches/1", test.localState, "", "", time.Now().UTC(),
			); err != nil {
				t.Fatal(err)
			}
			if err := database.Close(); err != nil {
				t.Fatal(err)
			}

			output, err := runBatchContinueWithFake(
				t, dbPath, &fakeGeminiBatchAPI{state: test.remoteState},
			)
			if err != nil {
				t.Fatalf("runBatchContinue() error = %v", err)
			}
			for _, want := range []string{
				"Checking Gemini batches started: 0/1 batches.",
				"Checking Gemini batches complete: 1/1 batches ·",
				"Checked 1 Gemini batch; nothing ready to process.",
			} {
				if !strings.Contains(output, want) {
					t.Fatalf("output = %q, want %q", output, want)
				}
			}
		})
	}
}

func TestBatchContinueDoesNotReportIdleAfterSubmittingDetachedRetry(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "retry.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	batch, request := addPreparedImageTestBatch(t, database, "retry-progress")
	if _, err := database.Exec(
		`UPDATE gemini_batches SET model = ? WHERE id = ?`, "gemini-3.7-flash", batch.ID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := database.RetryGeminiRequest(request.ID, "retry", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := database.FinalizeGeminiBatch(batch.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	api := &fakeGeminiBatchAPI{}
	output, err := runBatchContinueWithFake(t, dbPath, api)
	if err != nil {
		t.Fatalf("runBatchContinue() error = %v", err)
	}
	assertInOrder(t, output,
		"Gemini retry batch 2 prepared with 1 request(s).",
		"Uploading Gemini batch 2:",
		"Gemini batch 2 upload complete:",
	)
	if strings.Contains(output, "No tracked Gemini batch work") || strings.Contains(output, "nothing ready") {
		t.Fatalf("retry submission incorrectly reported idle: %q", output)
	}
	if api.uploadCalls != 1 || len(api.createModels) != 1 || api.createModels[0] != "gemini-3.8-flash" {
		t.Fatalf("remote calls = %d uploads with models %v", api.uploadCalls, api.createModels)
	}
	database, err = db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var model string
	var replacementOf int64
	if err := database.QueryRow(
		`SELECT model, replacement_of FROM gemini_batches ORDER BY id DESC LIMIT 1`,
	).Scan(&model, &replacementOf); err != nil {
		t.Fatal(err)
	}
	if model != "gemini-3.8-flash" || replacementOf != batch.ID {
		t.Fatalf("retry batch model=%q replacement_of=%d, want gemini-3.8-flash and %d", model, replacementOf, batch.ID)
	}
}

func TestBatchContinueBlankAttemptModelRetriesWithoutStagingAndAccountsUsage(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "blank-attempt-model.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	batch, request := addPreparedImageTestBatch(t, database, "blank-attempt-model")
	if _, err := database.Exec(`UPDATE gemini_batches SET model = '' WHERE id = ?`, batch.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.SetGeminiBatchRemote(
		batch.ID, "batches/completed", db.GeminiBatchSucceeded, "files/output", "", time.Now().UTC(),
	); err != nil {
		t.Fatal(err)
	}
	outputLine := successfulGeminiOutputLineWithModel(t, request.RequestKey, 0, 10, 20, 5, "")
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	api := &fakeGeminiBatchAPI{output: outputLine}
	output, runErr := runBatchContinueWithFake(t, dbPath, api)
	if runErr != nil {
		t.Fatalf("runBatchContinue() error = %v", runErr)
	}
	if !strings.Contains(output, "Batch OCR cost: $0.0001") {
		t.Fatalf("output = %q, want usage-based billing", output)
	}
	if api.uploadCalls != 1 || api.createCalls != 1 {
		t.Fatalf("retry remote calls = %d uploads and %d submissions, want one each", api.uploadCalls, api.createCalls)
	}

	database, err = db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	stored, err := database.GeminiRequestByID(request.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored == nil || stored.State != db.GeminiRequestAssigned || stored.AttemptCount != 1 || stored.BatchID == nil ||
		!strings.Contains(stored.LastError, "no fallback model") {
		t.Fatalf("retried request = %+v, want one assigned retry with the decode failure", stored)
	}
	batches, err := database.ListGeminiBatches()
	if err != nil || len(batches) != 1 || batches[0].ID != *stored.BatchID {
		t.Fatalf("batches = %+v, %v; want exactly the retry batch", batches, err)
	}
	var pages int
	if err := database.QueryRow(`SELECT COUNT(*) FROM pages WHERE content_id = ?`, request.ContentID).Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if pages != 0 {
		t.Fatalf("canonical pages = %d, want none", pages)
	}
}

func TestBatchContinueDoesNotReportIdleAfterPromotingStagedContent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "promote.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	batch, request := addPreparedImageTestBatch(t, database, "promote")
	if err := database.StageGeminiRequest(
		request.ID,
		[]db.GeminiStagedPage{{PageIndex: 0, Markdown: "ready", Model: "gemini-test"}},
		nil,
		nil,
		0,
		false,
		time.Now().UTC(),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := database.FinalizeGeminiBatch(batch.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	output, err := runBatchContinueWithFake(t, dbPath, &fakeGeminiBatchAPI{})
	if err != nil {
		t.Fatalf("runBatchContinue() error = %v", err)
	}
	if strings.Contains(output, "No tracked Gemini batch work") || strings.Contains(output, "nothing ready") {
		t.Fatalf("promotion invocation incorrectly reported idle: %q", output)
	}
	database, err = db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	content, err := database.GetContentByID(request.ContentID)
	if err != nil {
		t.Fatal(err)
	}
	if content == nil || content.OCRPending {
		t.Fatalf("content = %+v, want promoted OCR", content)
	}
}

func TestBatchContinueDoesNotReportIdleForCleanupWorkOrError(t *testing.T) {
	tests := []struct {
		name      string
		deleteErr error
		wantErr   bool
	}{
		{name: "cleaned"},
		{name: "failed", deleteErr: errors.New("delete failed"), wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "cleanup.db")
			database, err := db.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			insertGeminiCleanup(t, database, "file", "files/input")
			if err := database.Close(); err != nil {
				t.Fatal(err)
			}
			api := &fakeGeminiBatchAPI{deleteErrors: map[string]error{"files/input": test.deleteErr}}
			var output string
			var runErr error
			_ = captureStderr(t, func() {
				output, runErr = runBatchContinueWithFake(t, dbPath, api)
			})
			if (runErr != nil) != test.wantErr {
				t.Fatalf("runBatchContinue() error = %v, wantErr %t", runErr, test.wantErr)
			}
			if strings.Contains(output, "No tracked Gemini batch work") || strings.Contains(output, "nothing ready") {
				t.Fatalf("cleanup invocation incorrectly reported idle: %q", output)
			}
		})
	}
}

func TestBatchContinueSuppressesIdleWhenPhaseListingFails(t *testing.T) {
	tests := []struct {
		name      string
		statement string
	}{
		{name: "retry requests", statement: "ALTER TABLE gemini_batch_requests RENAME TO hidden_requests"},
		{name: "cleanup", statement: "ALTER TABLE gemini_batch_cleanup RENAME TO hidden_cleanup"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "listing-error.db")
			database, err := db.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := database.Exec(test.statement); err != nil {
				t.Fatal(err)
			}
			if err := database.Close(); err != nil {
				t.Fatal(err)
			}
			var output string
			var runErr error
			_ = captureStderr(t, func() {
				output, runErr = runBatchContinueWithFake(t, dbPath, &fakeGeminiBatchAPI{})
			})
			if runErr == nil {
				t.Fatal("runBatchContinue() error = nil, want listing failure")
			}
			if strings.Contains(output, "No tracked Gemini batch work") || strings.Contains(output, "nothing ready") {
				t.Fatalf("failed invocation incorrectly reported idle: %q", output)
			}
		})
	}
}

func TestRetryGeminiCleanupReturnsListErrorWithoutWork(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "closed.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())

	found, cleanupErrors := retryGeminiCleanup(cmd, database, &fakeGeminiBatchAPI{})
	if found || len(cleanupErrors) != 1 {
		t.Fatalf("retryGeminiCleanup() = (%t, %v), want false and one list error", found, cleanupErrors)
	}
}

func TestBatchContinueNestedCancellationReportsOneStoppedOutcome(t *testing.T) {
	resetCommandState(t)
	t.Setenv("GEMINI_API_KEY", "test-key")
	dbPath := filepath.Join(t.TempDir(), "nested-cancellation.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	addPreparedImageTestBatch(t, database, "nested-cancellation")
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	api := &fakeGeminiBatchAPI{}
	oldFactory := newGeminiBatchAPI
	newGeminiBatchAPI = func(string) geminiBatchAPI { return api }
	t.Cleanup(func() { newGeminiBatchAPI = oldFactory })
	ctx, cancel := context.WithCancel(context.Background())
	oldMatch := matchingContentPath
	matchingContentPath = func(database *db.DB, contentID int64, checksum string) (string, error) {
		path, matchErr := oldMatch(database, contentID, checksum)
		cancel()
		return path, matchErr
	}
	t.Cleanup(func() { matchingContentPath = oldMatch })
	cmd := commandWithDatabaseFlag(t, dbPath)
	cmd.SetContext(ctx)
	cmd.Flags().String("model", "", "")
	if err := cmd.Flags().Set("model", modelGemini); err != nil {
		t.Fatal(err)
	}

	var runErr error
	output := captureStdout(t, func() { runErr = runBatchContinue(cmd, nil) })
	if !errors.Is(runErr, context.Canceled) {
		t.Fatalf("runBatchContinue() error = %v, want context cancellation", runErr)
	}
	if strings.Count(output, "Stopped") != 1 ||
		!strings.Contains(output, "Stopped at 0/1 requests: Regenerating Gemini batch 1 input") ||
		strings.Contains(output, "Stopped at 0/1 batches: Checking Gemini batches") {
		t.Fatalf("nested cancellation output = %q, want only the child stopped outcome", output)
	}
}

func TestBatchContinueDoesNotReconcileCompletedResponseAfterCancellation(t *testing.T) {
	resetCommandState(t)
	t.Setenv("GEMINI_API_KEY", "test-key")
	dbPath := filepath.Join(t.TempDir(), "cancelled-refresh.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	batch, _ := addPreparedImageTestBatch(t, database, "cancelled-refresh")
	if err := database.SetGeminiBatchRemote(
		batch.ID, "batches/cancelled-refresh", db.GeminiBatchRunning, "", "", time.Now().UTC(),
	); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	api := &fakeGeminiBatchAPI{getFunc: func(context.Context, string) (ocr.GeminiRemoteBatch, error) {
		cancel()
		return ocr.GeminiRemoteBatch{
			Name: "batches/cancelled-refresh", State: "JOB_STATE_SUCCEEDED", OutputFileName: "files/output",
		}, nil
	}}
	oldFactory := newGeminiBatchAPI
	newGeminiBatchAPI = func(string) geminiBatchAPI { return api }
	t.Cleanup(func() { newGeminiBatchAPI = oldFactory })
	cmd := commandWithDatabaseFlag(t, dbPath)
	cmd.SetContext(ctx)
	cmd.Flags().String("model", "", "")
	if err := cmd.Flags().Set("model", modelGemini); err != nil {
		t.Fatal(err)
	}
	var runErr error
	output := captureStdout(t, func() { runErr = runBatchContinue(cmd, nil) })
	if !errors.Is(runErr, context.Canceled) {
		t.Fatalf("runBatchContinue() error = %v, want context cancellation", runErr)
	}
	if api.getCalls != 1 || api.downloadCalls != 0 {
		t.Fatalf("refresh calls = %d, downloads = %d; want one refresh and no import", api.getCalls, api.downloadCalls)
	}
	if strings.Count(output, "Stopped") != 1 ||
		!strings.Contains(output, "Stopped at 0/1 batches: Checking Gemini batches") {
		t.Fatalf("refresh cancellation output = %q, want one unadvanced checking outcome", output)
	}

	database, err = db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	stored, err := database.GetGeminiBatch(batch.ID)
	if err != nil || stored == nil || stored.State != db.GeminiBatchRunning || stored.OutputFileName != "" {
		t.Fatalf("batch after cancelled refresh = %+v, %v; want unchanged running batch", stored, err)
	}
}

type recordingGeminiBatchAPI struct {
	*fakeGeminiBatchAPI
	events []string
}

func (api *recordingGeminiBatchAPI) GetBatch(ctx context.Context, name string) (ocr.GeminiRemoteBatch, error) {
	api.events = append(api.events, name)
	return api.fakeGeminiBatchAPI.GetBatch(ctx, name)
}

func TestBatchContinueAdvancesRemoteBatchesOldestFirst(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "oldest-first.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	oldest, _ := addPreparedImageTestBatch(t, database, "oldest")
	if err := database.SetGeminiBatchRemote(
		oldest.ID, "batches/oldest", db.GeminiBatchRunning, "", "", time.Now().UTC(),
	); err != nil {
		t.Fatal(err)
	}
	newest, _ := addPreparedImageTestBatch(t, database, "newest")
	if err := database.SetGeminiBatchRemote(
		newest.ID, "batches/newest", db.GeminiBatchRunning, "", "", time.Now().UTC(),
	); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	api := &recordingGeminiBatchAPI{fakeGeminiBatchAPI: &fakeGeminiBatchAPI{state: "JOB_STATE_RUNNING"}}
	if _, err := runBatchContinueWithFake(t, dbPath, api); err != nil {
		t.Fatalf("runBatchContinue() error = %v", err)
	}
	want := []string{"batches/oldest", "batches/newest"}
	if fmt.Sprint(api.events) != fmt.Sprint(want) {
		t.Fatalf("remote refresh order = %v, want %v", api.events, want)
	}
}

func TestBatchContinueCapsAutomaticRetriesAtOneHundred(t *testing.T) {
	const retryCount = maxAutomaticRequestsPerContinue + 1
	dbPath := filepath.Join(t.TempDir(), "retry-cap.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	plans := make([]db.GeminiRequestPlan, 0, retryCount)
	for index := 0; index < retryCount; index++ {
		path := filepath.Join(t.TempDir(), fmt.Sprintf("retry-cap-%03d.png", index))
		if err := os.WriteFile(path, []byte(fmt.Sprintf("retry cap %d", index)), 0o600); err != nil {
			t.Fatal(err)
		}
		digest, err := checksum.SHA256File(path)
		if err != nil {
			t.Fatal(err)
		}
		contentID, err := database.InsertContent(digest, 1)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC()
		if _, err := database.InsertDocument(path, contentID, now, now); err != nil {
			t.Fatal(err)
		}
		plans = append(plans, db.GeminiRequestPlan{
			ContentID: contentID, RequestKey: fmt.Sprintf("retry-cap-key-%03d", index),
			FileType: "png", PageStart: 0, PageEnd: 1,
		})
	}
	now := time.Now().UTC()
	prices := ocr.GeminiBatchPrices(now)
	originalBatchID, err := database.CreateGeminiBatch(
		"retry-cap-original", ocr.GeminiBatchModel, int64(prices.Input), int64(prices.Output), nil, plans, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	requests, err := database.GeminiRequestsForBatch(originalBatchID)
	if err != nil || len(requests) != retryCount {
		t.Fatalf("GeminiRequestsForBatch() = %d requests, %v; want %d", len(requests), err, retryCount)
	}
	for _, request := range requests {
		if _, err := database.RetryGeminiRequest(request.ID, "retry", now); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := database.FinalizeGeminiBatch(originalBatchID, now); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	api := &fakeGeminiBatchAPI{}
	output, err := runBatchContinueWithFake(t, dbPath, api)
	if err != nil {
		t.Fatalf("runBatchContinue() error = %v", err)
	}
	if !strings.Contains(output, "Preparing Gemini retry input started: 0/100 selected requests.") ||
		!strings.Contains(output, "prepared with 100 request(s).") {
		t.Fatalf("retry-cap output = %q, want exactly 100 selected and prepared", output)
	}
	guidance := "More retryable Gemini requests remain; run `ringbinder batch continue` again."
	if strings.Count(output, guidance) != 1 {
		t.Fatalf("retry-cap guidance count = %d, output = %q; want one", strings.Count(output, guidance), output)
	}
	if api.uploadCalls != 1 || api.createCalls != 1 {
		t.Fatalf("remote retry calls = uploads %d, submissions %d; want one each", api.uploadCalls, api.createCalls)
	}

	database, err = db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	batches, err := database.ListGeminiBatches()
	if err != nil || len(batches) != 1 {
		t.Fatalf("ListGeminiBatches() = %+v, %v; want one submitted retry batch", batches, err)
	}
	selected, err := database.GeminiRequestsForBatch(batches[0].ID)
	if err != nil || len(selected) != maxAutomaticRequestsPerContinue {
		t.Fatalf("submitted retry requests = %d, %v; want %d", len(selected), err, maxAutomaticRequestsPerContinue)
	}
	retryable, err := database.RetryableGeminiRequests()
	if err != nil || len(retryable) != 1 {
		t.Fatalf("remaining retryable requests = %+v, %v; want one", retryable, err)
	}
}

func runBatchContinueWithFake(
	t *testing.T,
	dbPath string,
	api geminiBatchAPI,
) (string, error) {
	t.Helper()
	resetCommandState(t)
	t.Setenv("GEMINI_API_KEY", "test-key")
	oldFactory := newGeminiBatchAPI
	newGeminiBatchAPI = func(string) geminiBatchAPI { return api }
	t.Cleanup(func() { newGeminiBatchAPI = oldFactory })

	cmd := commandWithDatabaseFlag(t, dbPath)
	cmd.SetContext(context.Background())
	cmd.Flags().String("model", "", "")
	if err := cmd.Flags().Set("model", modelGemini); err != nil {
		t.Fatal(err)
	}
	var runErr error
	output := captureStdout(t, func() {
		runErr = runBatchContinue(cmd, nil)
	})
	return output, runErr
}
