package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/maxim/ringbinder/internal/checksum"
	"github.com/maxim/ringbinder/internal/db"
	"github.com/maxim/ringbinder/internal/ocr"
	"github.com/spf13/cobra"
)

type readThenError struct {
	data []byte
	done bool
}

func (reader *readThenError) Read(buffer []byte) (int, error) {
	if !reader.done {
		reader.done = true
		return copy(buffer, reader.data), nil
	}
	return 0, errors.New("download interrupted")
}

func TestInterruptedGeminiOutputDownloadDoesNotMutateRequests(t *testing.T) {
	database, batch, request := createOutputTestBatch(t, 0, 1)
	line := successfulGeminiOutputLine(t, request.RequestKey, 0, 10, 20, 5)
	var totals batchContinueTotals
	err := accountGeminiOutput(&readThenError{data: line}, database, batch, &totals)
	var incomplete *incompleteGeminiOutputError
	if !errors.As(err, &incomplete) {
		t.Fatalf("accountGeminiOutput() error = %v, want incomplete download", err)
	}
	stored, err := database.GeminiRequestByID(request.ID)
	if err != nil {
		t.Fatalf("GeminiRequestByID() error = %v", err)
	}
	if stored == nil || stored.State != db.GeminiRequestAssigned || stored.BatchID == nil {
		t.Fatalf("stored request = %+v, want original assignment retained", stored)
	}
}

func TestOversizedGeminiOutputBlocksAndFinalizesBatch(t *testing.T) {
	database, batch, request := createOutputTestBatch(t, 0, 1)
	batch.OutputFileName = "files/output"
	api := &fakeGeminiBatchAPI{output: append(
		bytes.Repeat([]byte("x"), ocr.GeminiBatchMaxResponseBytes+1), '\n',
	)}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var totals batchContinueTotals
	err := accountSucceededGeminiBatch(cmd, database, api, batch, &totals)
	var incomplete *incompleteGeminiOutputError
	if err == nil || errors.As(err, &incomplete) {
		t.Fatalf("accountSucceededGeminiBatch() error = %v, want terminal oversized-line error", err)
	}
	if stored, lookupErr := database.GeminiRequestByID(request.ID); lookupErr != nil || stored == nil || stored.State != db.GeminiRequestBlocked {
		t.Fatalf("stored request = %+v, error = %v, want blocked", stored, lookupErr)
	}
	if storedBatch, lookupErr := database.GetGeminiBatch(batch.ID); lookupErr != nil || storedBatch != nil {
		t.Fatalf("stored batch = %+v, error = %v, want finalized", storedBatch, lookupErr)
	}
	if !totals.accounted || !totals.billing.Indeterminate {
		t.Fatalf("totals = %+v, want accounted indeterminate billing", totals)
	}
}

func TestMaximumSizedGeminiOutputLineIsAccepted(t *testing.T) {
	database, batch, request := createOutputTestBatch(t, 0, 1)
	line := bytes.TrimSuffix(successfulGeminiOutputLine(t, request.RequestKey, 0, 10, 20, 5), []byte{'\n'})
	if len(line) >= ocr.GeminiBatchMaxResponseBytes {
		t.Fatalf("fixture line is already %d bytes", len(line))
	}
	line = append(line, bytes.Repeat([]byte{' '}, ocr.GeminiBatchMaxResponseBytes-len(line))...)
	line = append(line, '\n')
	var totals batchContinueTotals
	if err := accountGeminiOutput(bytes.NewReader(line), database, batch, &totals); err != nil {
		t.Fatalf("accountGeminiOutput() error = %v", err)
	}
	stored, err := database.GeminiRequestByID(request.ID)
	if err != nil || stored == nil || stored.State != db.GeminiRequestStaged {
		t.Fatalf("stored request = %+v, error = %v, want staged", stored, err)
	}
}

func TestBatchContinueCanceledWithoutRetryableWorkReturnsError(t *testing.T) {
	resetCommandState(t)
	t.Setenv("GEMINI_API_KEY", "test-key")
	dbPath := filepath.Join(t.TempDir(), "canceled.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := database.Exec(
		`INSERT INTO gemini_batch_cleanup
		 (resource_kind, resource_name, created_at, updated_at)
		 VALUES ('file', 'files/pending-cleanup', ?, ?)`,
		stamp, stamp,
	); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	api := &fakeGeminiBatchAPI{}
	oldFactory := newGeminiBatchAPI
	newGeminiBatchAPI = func(string) geminiBatchAPI { return api }
	t.Cleanup(func() { newGeminiBatchAPI = oldFactory })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd := commandWithDatabaseFlag(t, dbPath)
	cmd.SetContext(ctx)
	cmd.Flags().String("model", "", "")
	if err := cmd.Flags().Set("model", modelGemini); err != nil {
		t.Fatal(err)
	}
	if err := runBatchContinue(cmd, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("runBatchContinue() error = %v, want context cancellation", err)
	}
	if len(api.deleted) != 0 {
		t.Fatalf("cleanup deletes = %v, want none after cancellation", api.deleted)
	}
	database, err = db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if cleanup, err := database.CountGeminiCleanup(); err != nil || cleanup != 1 {
		t.Fatalf("cleanup count = %d, error = %v; want retained cleanup", cleanup, err)
	}
}

func TestPreparedBatchCancellationPreservesAssignment(t *testing.T) {
	database, batch, request := createPreparedImageTestBatch(t, "prepared-cancel")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	err := resumePreparedGeminiBatch(
		cmd, database, &fakeGeminiBatchAPI{}, ocr.NewGeminiClient("", time.Now().UTC()), batch,
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("resumePreparedGeminiBatch() error = %v, want context cancellation", err)
	}
	stored, err := database.GeminiRequestByID(request.ID)
	if err != nil || stored == nil || stored.State != db.GeminiRequestAssigned || stored.BatchID == nil {
		t.Fatalf("stored request = %+v, error = %v, want original assignment", stored, err)
	}
}

func TestRetryPreparationCancellationPreservesRetryableRequest(t *testing.T) {
	database, _, request := createPreparedImageTestBatch(t, "retry-cancel")
	if _, err := database.RetryGeminiRequest(request.ID, "retry", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	errs := submitRetryableGeminiRequests(
		cmd, database, &fakeGeminiBatchAPI{}, ocr.NewGeminiClient("", time.Now().UTC()),
	)
	if len(errs) != 1 || !errors.Is(errs[0], context.Canceled) {
		t.Fatalf("submitRetryableGeminiRequests() errors = %v, want context cancellation", errs)
	}
	stored, err := database.GeminiRequestByID(request.ID)
	if err != nil || stored == nil || stored.State != db.GeminiRequestRetryable || stored.BatchID != nil {
		t.Fatalf("stored request = %+v, error = %v, want retryable", stored, err)
	}
}

func TestRetrySubmissionStopsAfterGlobalFailure(t *testing.T) {
	database, _, firstRequest := createPreparedImageTestBatch(t, "global-one")
	secondBatch, secondRequest := addPreparedImageTestBatch(t, database, "global-two")
	if _, err := database.RetryGeminiRequest(firstRequest.ID, "retry", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := database.RetryGeminiRequest(secondRequest.ID, "retry", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	api := &fakeGeminiBatchAPI{uploadError: &ocr.GeminiAmbiguousOperationError{
		Operation: "file upload",
		Err:       &net.DNSError{Err: "offline", IsTimeout: true},
	}}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	errs := submitRetryableGeminiRequests(cmd, database, api, ocr.NewGeminiClient("", time.Now().UTC()))
	if len(errs) != 1 || !ocr.IsGeminiGlobalFailure(errs[0]) {
		t.Fatalf("submitRetryableGeminiRequests() errors = %v, want one global failure", errs)
	}
	if api.uploadCalls != 1 {
		t.Fatalf("upload calls = %d, want 1", api.uploadCalls)
	}
	stored, err := database.GeminiRequestByID(secondRequest.ID)
	if err != nil || stored == nil || stored.State != db.GeminiRequestRetryable {
		t.Fatalf("second request = %+v, error = %v, want unsubmitted retryable", stored, err)
	}
	if batch, err := database.GetGeminiBatch(secondBatch.ID); err != nil || batch == nil {
		t.Fatalf("original second batch = %+v, error = %v", batch, err)
	}
}

func TestAccountGeminiOutputStagesValidatedPagesAndBatchBilling(t *testing.T) {
	database, batch, request := createOutputTestBatch(t, 0, 1)
	line := successfulGeminiOutputLine(t, request.RequestKey, 0, 10, 20, 5)
	var totals batchContinueTotals
	if err := accountGeminiOutput(bytes.NewReader(line), database, batch, &totals); err != nil {
		t.Fatalf("accountGeminiOutput() error = %v", err)
	}
	stored, err := database.GeminiRequestByID(request.ID)
	if err != nil {
		t.Fatalf("GeminiRequestByID() error = %v", err)
	}
	if stored == nil || stored.State != db.GeminiRequestStaged || stored.BatchID != nil {
		t.Fatalf("stored request = %+v, want detached staged", stored)
	}
	wantCost := ocr.Currency(10*batch.InputPrice + 25*batch.OutputPrice)
	if !totals.accounted || totals.billing.Indeterminate || totals.billing.KnownCost != wantCost {
		t.Fatalf("totals = %+v, want known cost %d", totals, wantCost)
	}
}

func TestAccountGeminiOutputRejectsDuplicateAndRetriesOnce(t *testing.T) {
	database, batch, request := createOutputTestBatch(t, 0, 1)
	line := successfulGeminiOutputLine(t, request.RequestKey, 0, 10, 20, 5)
	input := append(append([]byte{}, line...), line...)
	var totals batchContinueTotals
	if err := accountGeminiOutput(bytes.NewReader(input), database, batch, &totals); err != nil {
		t.Fatalf("accountGeminiOutput() error = %v", err)
	}
	stored, err := database.GeminiRequestByID(request.ID)
	if err != nil {
		t.Fatalf("GeminiRequestByID() error = %v", err)
	}
	if stored == nil || stored.State != db.GeminiRequestRetryable || stored.AttemptCount != 1 || stored.BatchID != nil {
		t.Fatalf("stored request = %+v, want one detached automatic retry", stored)
	}
	if !totals.billing.Indeterminate {
		t.Fatalf("duplicate output cost should be indeterminate")
	}
}

func TestAccountGeminiOutputSplitsOnlyFailedRange(t *testing.T) {
	database, batch, request := createOutputTestBatch(t, 0, 4)
	line := []byte(fmt.Sprintf(
		`{"key":%q,"error":{"code":400,"status":"INVALID_ARGUMENT","message":"MAX_TOKENS"}}`+"\n",
		request.RequestKey,
	))
	var totals batchContinueTotals
	if err := accountGeminiOutput(bytes.NewReader(line), database, batch, &totals); err != nil {
		t.Fatalf("accountGeminiOutput() error = %v", err)
	}
	if got, err := database.GeminiRequestByID(request.ID); err != nil || got != nil {
		t.Fatalf("parent request = %+v, err %v, want deleted", got, err)
	}
	retryable, err := database.RetryableGeminiRequests()
	if err != nil {
		t.Fatalf("RetryableGeminiRequests() error = %v", err)
	}
	if len(retryable) != 2 || retryable[0].PageStart != 0 || retryable[0].PageEnd != 2 ||
		retryable[1].PageStart != 2 || retryable[1].PageEnd != 4 || retryable[0].AttemptCount != 0 {
		t.Fatalf("split requests = %+v", retryable)
	}
}

func TestMissingRunningBatchReleasesOwnershipForOneReplacement(t *testing.T) {
	database, batch, request := createOutputTestBatch(t, 0, 1)
	if err := database.SetGeminiBatchRemote(
		batch.ID, "batches/missing", db.GeminiBatchRunning, "", "", time.Now().UTC(),
	); err != nil {
		t.Fatalf("SetGeminiBatchRemote() error = %v", err)
	}
	batch.State = db.GeminiBatchRunning
	batch.RemoteName = "batches/missing"
	api := &fakeGeminiBatchAPI{getError: &ocr.GeminiBatchAPIError{StatusCode: 404}}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var totals batchContinueTotals
	if err := refreshAndHandleGeminiBatch(cmd, database, api, batch, &totals); err != nil {
		t.Fatalf("refreshAndHandleGeminiBatch() error = %v", err)
	}
	retryable, err := database.GeminiRequestByID(request.ID)
	if err != nil {
		t.Fatalf("GeminiRequestByID() error = %v", err)
	}
	if retryable == nil || retryable.State != db.GeminiRequestRetryable || retryable.ReplacementCount != 1 {
		t.Fatalf("request = %+v, want one replacement", retryable)
	}
}

func TestMissingSucceededOutputBlocksWithoutDuplicateGeneration(t *testing.T) {
	database, batch, request := createOutputTestBatch(t, 0, 1)
	if err := database.SetGeminiBatchRemote(
		batch.ID, "batches/1", db.GeminiBatchSucceeded, "files/missing", "", time.Now().UTC(),
	); err != nil {
		t.Fatalf("SetGeminiBatchRemote() error = %v", err)
	}
	batch.State = db.GeminiBatchSucceeded
	batch.RemoteName = "batches/1"
	batch.OutputFileName = "files/missing"
	api := &fakeGeminiBatchAPI{downloadError: &ocr.GeminiBatchAPIError{StatusCode: 404}}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var totals batchContinueTotals
	if err := accountSucceededGeminiBatch(cmd, database, api, batch, &totals); err != nil {
		t.Fatalf("accountSucceededGeminiBatch() error = %v", err)
	}
	blocked, err := database.GeminiRequestByID(request.ID)
	if err != nil {
		t.Fatalf("GeminiRequestByID() error = %v", err)
	}
	if blocked == nil || blocked.State != db.GeminiRequestBlocked {
		t.Fatalf("request = %+v, want blocked unavailable output", blocked)
	}
}

func TestConfirmedCancellationBlocksWithoutDownloadingOutput(t *testing.T) {
	database, batch, request := createOutputTestBatch(t, 0, 1)
	if err := database.SetGeminiBatchRemote(
		batch.ID, "batches/1", db.GeminiBatchCancelling, "", "", time.Now().UTC(),
	); err != nil {
		t.Fatalf("SetGeminiBatchRemote() error = %v", err)
	}
	batch.State = db.GeminiBatchCancelling
	batch.RemoteName = "batches/1"
	api := &fakeGeminiBatchAPI{state: "BATCH_STATE_CANCELLED"}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var totals batchContinueTotals
	if err := refreshAndHandleGeminiBatch(cmd, database, api, batch, &totals); err != nil {
		t.Fatalf("refreshAndHandleGeminiBatch() error = %v", err)
	}
	blocked, err := database.GeminiRequestByID(request.ID)
	if err != nil {
		t.Fatalf("GeminiRequestByID() error = %v", err)
	}
	if blocked == nil || blocked.State != db.GeminiRequestBlocked {
		t.Fatalf("request = %+v, want blocked cancellation gap", blocked)
	}
	if api.downloadCalls != 0 {
		t.Fatalf("download calls = %d, want none for cancellation", api.downloadCalls)
	}
	if !totals.accounted || !totals.billing.Indeterminate {
		t.Fatalf("totals = %+v, want indeterminate cancellation spend", totals)
	}
}

func TestFreshDeterministicRemoteFailureBlocksWithoutReplacement(t *testing.T) {
	database, batch, request := createOutputTestBatch(t, 0, 1)
	if err := database.SetGeminiBatchRemote(
		batch.ID, "batches/1", db.GeminiBatchRunning, "", "", time.Now().UTC(),
	); err != nil {
		t.Fatalf("SetGeminiBatchRemote() error = %v", err)
	}
	batch.State = db.GeminiBatchRunning
	batch.RemoteName = "batches/1"
	api := &fakeGeminiBatchAPI{state: "BATCH_STATE_FAILED", remoteError: "INVALID_ARGUMENT: bad request"}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var totals batchContinueTotals
	if err := refreshAndHandleGeminiBatch(cmd, database, api, batch, &totals); err != nil {
		t.Fatalf("refreshAndHandleGeminiBatch() error = %v", err)
	}
	blocked, err := database.GeminiRequestByID(request.ID)
	if err != nil {
		t.Fatalf("GeminiRequestByID() error = %v", err)
	}
	if blocked == nil || blocked.State != db.GeminiRequestBlocked || blocked.ReplacementCount != 0 {
		t.Fatalf("request = %+v, want deterministic block without replacement", blocked)
	}
	if !totals.accounted || !totals.billing.Indeterminate {
		t.Fatalf("totals = %+v, want indeterminate outputless spend", totals)
	}
}

func TestCanonicalUnavailableStatusRetriesOnce(t *testing.T) {
	database, batch, request := createOutputTestBatch(t, 0, 1)
	line := []byte(fmt.Sprintf(
		`{"key":%q,"error":{"code":14,"message":"temporarily unavailable"}}`+"\n",
		request.RequestKey,
	))
	var totals batchContinueTotals
	if err := accountGeminiOutput(bytes.NewReader(line), database, batch, &totals); err != nil {
		t.Fatalf("accountGeminiOutput() error = %v", err)
	}
	retryable, err := database.GeminiRequestByID(request.ID)
	if err != nil {
		t.Fatalf("GeminiRequestByID() error = %v", err)
	}
	if retryable == nil || retryable.State != db.GeminiRequestRetryable || retryable.AttemptCount != 1 {
		t.Fatalf("request = %+v, want one retry for canonical UNAVAILABLE", retryable)
	}
}

func TestInvalidJSONPayloadErrorBlocksWithoutAdaptiveFanout(t *testing.T) {
	database, batch, request := createOutputTestBatch(t, 0, 4)
	line := []byte(fmt.Sprintf(
		`{"key":%q,"error":{"code":400,"status":"INVALID_ARGUMENT","message":"Invalid JSON payload received"}}`+"\n",
		request.RequestKey,
	))
	var totals batchContinueTotals
	if err := accountGeminiOutput(bytes.NewReader(line), database, batch, &totals); err != nil {
		t.Fatalf("accountGeminiOutput() error = %v", err)
	}
	blocked, err := database.GeminiRequestByID(request.ID)
	if err != nil {
		t.Fatalf("GeminiRequestByID() error = %v", err)
	}
	if blocked == nil || blocked.State != db.GeminiRequestBlocked {
		t.Fatalf("request = %+v, want deterministic block", blocked)
	}
	retryable, err := database.RetryableGeminiRequests()
	if err != nil {
		t.Fatalf("RetryableGeminiRequests() error = %v", err)
	}
	if len(retryable) != 0 {
		t.Fatalf("retryable = %+v, want no adaptive children", retryable)
	}
}

func TestOutputlessGeminiBatchReplacementIsCapped(t *testing.T) {
	database, first, request := createOutputTestBatch(t, 0, 1)
	if _, err := database.Exec(
		`UPDATE gemini_batches SET state = 'failed', remote_name = 'batches/first', last_error = 'transient' WHERE id = ?`,
		first.ID,
	); err != nil {
		t.Fatalf("mark first failed: %v", err)
	}
	first.State = db.GeminiBatchFailed
	first.RemoteName = "batches/first"
	first.LastError = "transient"
	totals := &batchContinueTotals{}
	if err := replaceOutputlessGeminiBatch(database, first, totals); err != nil {
		t.Fatalf("first replacement error = %v", err)
	}
	retryable, err := database.GeminiRequestByID(request.ID)
	if err != nil {
		t.Fatalf("GeminiRequestByID() error = %v", err)
	}
	if retryable == nil || retryable.State != db.GeminiRequestRetryable || retryable.ReplacementCount != 1 {
		t.Fatalf("first replacement request = %+v", retryable)
	}
	now := time.Now().UTC()
	prices := ocr.GeminiBatchPrices(now)
	secondID, err := database.CreateGeminiBatchForRequests(
		"replacement", ocr.GeminiBatchModel, int64(prices.Input), int64(prices.Output),
		&first.ID, []int64{request.ID}, now,
	)
	if err != nil {
		t.Fatalf("CreateGeminiBatchForRequests() error = %v", err)
	}
	if _, err := database.Exec(
		`UPDATE gemini_batches SET state = 'failed', remote_name = 'batches/second', last_error = 'transient again' WHERE id = ?`,
		secondID,
	); err != nil {
		t.Fatalf("mark second failed: %v", err)
	}
	second, err := database.GetGeminiBatch(secondID)
	if err != nil {
		t.Fatalf("GetGeminiBatch() error = %v", err)
	}
	if err := replaceOutputlessGeminiBatch(database, *second, totals); err != nil {
		t.Fatalf("second replacement error = %v", err)
	}
	blocked, err := database.GeminiRequestByID(request.ID)
	if err != nil {
		t.Fatalf("GeminiRequestByID() error = %v", err)
	}
	if blocked == nil || blocked.State != db.GeminiRequestBlocked || blocked.ReplacementCount != 1 {
		t.Fatalf("second replacement request = %+v, want blocked", blocked)
	}
}

func createPreparedImageTestBatch(
	t *testing.T,
	name string,
) (*db.DB, db.GeminiBatch, db.GeminiBatchRequest) {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "prepared.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	batch, request := addPreparedImageTestBatch(t, database, name)
	return database, batch, request
}

func addPreparedImageTestBatch(
	t *testing.T,
	database *db.DB,
	name string,
) (db.GeminiBatch, db.GeminiBatchRequest) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name+".png")
	if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
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
	prices := ocr.GeminiBatchPrices(now)
	batchID, err := database.CreateGeminiBatch(
		name, ocr.GeminiBatchModel, int64(prices.Input), int64(prices.Output), nil,
		[]db.GeminiRequestPlan{{
			ContentID: contentID, RequestKey: name + "-key", FileType: "png", PageStart: 0, PageEnd: 1,
		}},
		now,
	)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := database.GetGeminiBatch(batchID)
	if err != nil || batch == nil {
		t.Fatalf("GetGeminiBatch() = %+v, %v", batch, err)
	}
	requests, err := database.GeminiRequestsForBatch(batchID)
	if err != nil || len(requests) != 1 {
		t.Fatalf("GeminiRequestsForBatch() = %+v, %v", requests, err)
	}
	return *batch, requests[0]
}

func createOutputTestBatch(t *testing.T, start, end int) (*db.DB, db.GeminiBatch, db.GeminiBatchRequest) {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "output.db"))
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	contentID, err := database.InsertContent("output-checksum", end)
	if err != nil {
		t.Fatalf("InsertContent() error = %v", err)
	}
	if _, err := database.InsertDocument("/docs/output.pdf", contentID, time.Now().UTC(), time.Now().UTC()); err != nil {
		t.Fatalf("InsertDocument() error = %v", err)
	}
	now := time.Now().UTC()
	prices := ocr.GeminiBatchPrices(now)
	batchID, err := database.CreateGeminiBatch(
		"output-batch", ocr.GeminiBatchModel, int64(prices.Input), int64(prices.Output), nil,
		[]db.GeminiRequestPlan{{ContentID: contentID, RequestKey: "output-key", FileType: "pdf", PageStart: start, PageEnd: end}},
		now,
	)
	if err != nil {
		t.Fatalf("CreateGeminiBatch() error = %v", err)
	}
	batch, err := database.GetGeminiBatch(batchID)
	if err != nil {
		t.Fatalf("GetGeminiBatch() error = %v", err)
	}
	requests, err := database.GeminiRequestsForBatch(batchID)
	if err != nil {
		t.Fatalf("GeminiRequestsForBatch() error = %v", err)
	}
	return database, *batch, requests[0]
}

func successfulGeminiOutputLine(t *testing.T, key string, pageIndex int, input, candidate, thinking int64) []byte {
	t.Helper()
	payload := fmt.Sprintf(
		`{"pages":[{"page_index":%d,"transcription":"text","page_description":"page","visual_elements":[]}]}`,
		pageIndex,
	)
	response := map[string]any{
		"candidates": []any{map[string]any{
			"content":      map[string]any{"parts": []any{map[string]any{"text": payload}}},
			"finishReason": "STOP",
			"index":        0,
		}},
		"usageMetadata": map[string]any{
			"promptTokenCount": input, "candidatesTokenCount": candidate, "thoughtsTokenCount": thinking,
		},
	}
	line, err := json.Marshal(map[string]any{"key": key, "response": response})
	if err != nil {
		t.Fatalf("marshal output line: %v", err)
	}
	return append(line, '\n')
}
