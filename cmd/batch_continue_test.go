package cmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maxim/ringbinder/internal/checksum"
	"github.com/maxim/ringbinder/internal/db"
	"github.com/maxim/ringbinder/internal/ocr"
	pdfutil "github.com/maxim/ringbinder/internal/pdf"
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

func TestBatchContinueRetiresUndeletableLegacyOutput(t *testing.T) {
	resetCommandState(t)
	t.Setenv("GEMINI_API_KEY", "test-key")
	dbPath := filepath.Join(t.TempDir(), "cleanup.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	outputName := "files/batch-123456789012345678901234567890123456"
	inputName := "files/input"
	insertGeminiCleanup(t, database, "file", outputName)
	insertGeminiCleanup(t, database, "file", inputName)
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	api := &fakeGeminiBatchAPI{deleteErrors: map[string]error{
		outputName: geminiDeleteInvalidArgumentError("generated output name is too long"),
	}}
	oldFactory := newGeminiBatchAPI
	newGeminiBatchAPI = func(string) geminiBatchAPI { return api }
	t.Cleanup(func() { newGeminiBatchAPI = oldFactory })
	run := func() error {
		cmd := commandWithDatabaseFlag(t, dbPath)
		cmd.SetContext(context.Background())
		cmd.Flags().String("model", "", "")
		if err := cmd.Flags().Set("model", modelGemini); err != nil {
			t.Fatal(err)
		}
		return runBatchContinue(cmd, nil)
	}

	var continueErr error
	warning := captureStderr(t, func() { continueErr = run() })
	if continueErr != nil {
		t.Fatalf("runBatchContinue() error = %v, want terminal cleanup success", continueErr)
	}
	wantWarning := fmt.Sprintf(
		"warning: Gemini permanently rejected cleanup of file %s; Ringbinder will not retry it\n",
		outputName,
	)
	if warning != wantWarning {
		t.Fatalf("warning = %q, want %q", warning, wantWarning)
	}
	if got := strings.Join(api.deleted, ","); got != outputName+","+inputName {
		t.Fatalf("deleted resources = %v, want legacy output attempt followed by input cleanup", api.deleted)
	}
	database, err = db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if count, err := database.CountGeminiCleanup(); err != nil || count != 0 {
		t.Fatalf("cleanup count = %d, error = %v; want none", count, err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	warning = captureStderr(t, func() { continueErr = run() })
	if continueErr != nil || warning != "" || len(api.deleted) != 2 {
		t.Fatalf("second continue error = %v, warning = %q, deletes = %v; want no repeated work", continueErr, warning, api.deleted)
	}
}

func TestGeminiCleanupSeparatesTerminalAndRetryableFailures(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "cleanup.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	outputName := "files/batch-123456789012345678901234567890123456"
	inputName := "files/input"
	batchName := "batches/remote"
	retryName := "files/rate-limited"
	insertGeminiCleanup(t, database, "file", outputName)
	insertGeminiCleanup(t, database, "file", inputName)
	insertGeminiCleanup(t, database, "batch", batchName)
	insertGeminiCleanup(t, database, "file", retryName)
	api := &fakeGeminiBatchAPI{deleteErrors: map[string]error{
		outputName: geminiDeleteInvalidArgumentError("generated output name is too long"),
		inputName:  geminiDeleteInvalidArgumentError("file resource is invalid"),
		batchName:  geminiDeleteInvalidArgumentError("batch resource is invalid"),
		retryName: &ocr.GeminiBatchAPIError{
			StatusCode: 429,
			Body:       []byte(`{"error":{"code":429,"message":"try later","status":"RESOURCE_EXHAUSTED"}}`),
		},
	}}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())

	var cleanupErrors []error
	var cleanupFound bool
	warning := captureStderr(t, func() {
		cleanupFound, cleanupErrors = retryGeminiCleanup(cmd, database, api)
	})
	if !cleanupFound {
		t.Fatal("retryGeminiCleanup() found no work, want cleanup attempts")
	}
	if len(cleanupErrors) != 1 {
		t.Fatalf("retryGeminiCleanup() errors = %v, want one retryable failure", cleanupErrors)
	}
	if strings.Count(warning, "\n") != 3 || !strings.Contains(warning, outputName) ||
		!strings.Contains(warning, inputName) || !strings.Contains(warning, batchName) ||
		strings.Contains(warning, retryName) {
		t.Fatalf("warning = %q, want exactly the three permanent failures", warning)
	}
	cleanup, err := database.ListGeminiCleanup()
	if err != nil {
		t.Fatal(err)
	}
	if len(cleanup) != 1 || cleanup[0].ResourceName != retryName || cleanup[0].LastError == "" {
		t.Fatalf("cleanup = %+v, want only retryable row retained with error", cleanup)
	}
	firstPass := strings.Join([]string{outputName, inputName, batchName, retryName}, ",")
	if got := strings.Join(api.deleted, ","); got != firstPass {
		t.Fatalf("deleted resources = %v, want all first-pass attempts", api.deleted)
	}

	warning = captureStderr(t, func() {
		cleanupFound, cleanupErrors = retryGeminiCleanup(cmd, database, api)
	})
	if !cleanupFound || len(cleanupErrors) != 1 || warning != "" {
		t.Fatalf("second cleanup errors = %v, warning = %q; want only retryable failure", cleanupErrors, warning)
	}
	if got := strings.Join(api.deleted, ","); got != firstPass+","+retryName {
		t.Fatalf("deleted resources = %v, want only retryable row reattempted", api.deleted)
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
	found, errs := submitRetryableGeminiRequests(
		cmd, database, &fakeGeminiBatchAPI{}, ocr.NewGeminiClient("", time.Now().UTC()),
	)
	if !found || len(errs) != 1 || !errors.Is(errs[0], context.Canceled) {
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
	found, errs := submitRetryableGeminiRequests(cmd, database, api, ocr.NewGeminiClient("", time.Now().UTC()))
	if !found || len(errs) != 1 || !ocr.IsGeminiGlobalFailure(errs[0]) {
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
	var canonicalModel string
	if err := database.QueryRow(
		`SELECT model FROM pages WHERE content_id = ? AND page_index = 0`,
		request.ContentID,
	).Scan(&canonicalModel); err != nil {
		t.Fatalf("query canonical page: %v", err)
	}
	content, err := database.GetContentByID(request.ContentID)
	if err != nil || content == nil || content.OCRPending {
		t.Fatalf("content = %+v, %v; want immediately complete canonical coverage", content, err)
	}
	if canonicalModel != "gemini-response-test" {
		t.Fatalf("canonical model = %q, want response model", canonicalModel)
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

func TestAccountGeminiOutputSplitsAdaptiveRequestErrors(t *testing.T) {
	for _, test := range []struct {
		name    string
		code    int
		status  string
		message string
	}{
		{name: "HTTP 413", code: 413, status: "INVALID_ARGUMENT", message: "payload too large"},
		{name: "MAX_TOKENS", code: 400, status: "INVALID_ARGUMENT", message: "MAX_TOKENS"},
	} {
		t.Run(test.name, func(t *testing.T) {
			database, batch, request := createOutputTestBatch(t, 0, 4)
			line := []byte(fmt.Sprintf(
				`{"key":%q,"error":{"code":%d,"status":%q,"message":%q}}`+"\n",
				request.RequestKey, test.code, test.status, test.message,
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
			if !totals.accounted || !totals.billing.Indeterminate {
				t.Fatalf("totals = %+v, want indeterminate failed request", totals)
			}
		})
	}
}

func TestGeminiAdaptiveBatchFailureRegeneratesExtractedChildren(t *testing.T) {
	ctx := context.Background()
	labels := []string{"one", "two", "three", "four"}
	original := commandTestPDF(labels...)
	path := filepath.Join(t.TempDir(), "four-pages.pdf")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	digest, err := checksum.SHA256File(path)
	if err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(filepath.Join(t.TempDir(), "adaptive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	contentID, err := database.InsertContent(digest, 4)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := database.InsertDocument(path, contentID, now, now); err != nil {
		t.Fatal(err)
	}
	const inputPrice, outputPrice = int64(3), int64(7)
	batchID, err := database.CreateGeminiBatch(
		"original-backed", ocr.GeminiBatchModel, inputPrice, outputPrice, nil,
		[]db.GeminiRequestPlan{{
			ContentID: contentID, RequestKey: "original-key", FileType: "pdf", PageStart: 0, PageEnd: 4,
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

	planner := ocr.NewGeminiClient("", now)
	prepared, err := planner.PrepareRangeRequest(ctx, path, "pdf", 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	initialLine, err := marshalGeminiBatchLine(requests[0].RequestKey, prepared.Body)
	if err != nil {
		t.Fatal(err)
	}
	initial := decodeUploadedGeminiInputs(t, initialLine)[requests[0].RequestKey]
	if !bytes.Equal(initial.data, original) || initial.pages != 4 {
		t.Fatalf("initial input = %d bytes/%d pages, want unchanged %d-byte four-page PDF", len(initial.data), initial.pages, len(original))
	}

	output := geminiMaxTokensOutputLine(t, requests[0].RequestKey, 10, 20, 5)
	var totals batchContinueTotals
	if err := accountGeminiOutput(bytes.NewReader(output), database, *batch, &totals); err != nil {
		t.Fatal(err)
	}
	wantCost := ocr.Currency(10*inputPrice + 25*outputPrice)
	if !totals.accounted || totals.billing.Indeterminate || totals.billing.KnownCost != wantCost {
		t.Fatalf("failed-attempt billing = %+v, want frozen known cost %d", totals, wantCost)
	}
	retryable, err := database.RetryableGeminiRequests()
	if err != nil {
		t.Fatal(err)
	}
	if len(retryable) != 2 || retryable[0].PageStart != 0 || retryable[0].PageEnd != 2 ||
		retryable[1].PageStart != 2 || retryable[1].PageEnd != 4 {
		t.Fatalf("durable split ranges = %+v, want [0,2) and [2,4)", retryable)
	}
	if _, err := database.FinalizeGeminiBatch(batchID, now); err != nil {
		t.Fatal(err)
	}

	api := &fakeGeminiBatchAPI{}
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	var found bool
	var submitErrors []error
	_ = captureStdout(t, func() {
		found, submitErrors = submitRetryableGeminiRequests(cmd, database, api, planner)
	})
	if !found || len(submitErrors) != 0 {
		t.Fatalf("submitRetryableGeminiRequests() = (%t, %v)", found, submitErrors)
	}
	if len(api.uploadBodies) != 1 {
		t.Fatalf("uploads = %d, want one replacement input", len(api.uploadBodies))
	}
	batches, err := database.ListGeminiBatches()
	if err != nil || len(batches) != 1 {
		t.Fatalf("ListGeminiBatches() = %+v, %v", batches, err)
	}
	children, err := database.GeminiRequestsForBatch(batches[0].ID)
	if err != nil || len(children) != 2 {
		t.Fatalf("replacement requests = %+v, %v", children, err)
	}
	uploaded := decodeUploadedGeminiInputs(t, api.uploadBodies[0])
	for _, child := range children {
		if child.ContentID != contentID || child.Checksum != digest || child.Path != path || child.PageCount != 4 {
			t.Fatalf("child identity = %+v, want original content, checksum, path, and page count", child)
		}
		if child.AttemptCount != 0 || child.InputTokens != nil || child.OutputTokens != nil ||
			child.KnownCost != 0 || child.CostIndeterminate {
			t.Fatalf("child billing/retry state = %+v, want fresh split child", child)
		}
		got, exists := uploaded[child.RequestKey]
		if !exists || got.pages != child.PageEnd-child.PageStart {
			t.Fatalf("uploaded child %q = %+v, want matching schema", child.RequestKey, got)
		}
		pageCount, err := pdfutil.PageCountContext(ctx, bytes.NewReader(got.data))
		if err != nil || pageCount != child.PageEnd-child.PageStart || bytes.Equal(got.data, original) {
			t.Fatalf("uploaded child %d-%d is not a valid extracted subset: pages %d, error %v", child.PageStart, child.PageEnd, pageCount, err)
		}
		for index, label := range labels {
			present := bytes.Contains(got.data, []byte("("+label+") Tj"))
			wantPresent := index >= child.PageStart && index < child.PageEnd
			if present != wantPresent {
				t.Fatalf("uploaded child %d-%d label %q present = %t, want %t", child.PageStart, child.PageEnd, label, present, wantPresent)
			}
		}
	}

	if _, err := database.DetachGeminiBatchForReplacement(batches[0].ID, "checksum check", now); err != nil {
		t.Fatal(err)
	}
	if _, err := database.FinalizeGeminiBatch(batches[0].ID, now); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(bytes.Clone(original), 'x'), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = captureStdout(t, func() {
		found, submitErrors = submitRetryableGeminiRequests(cmd, database, api, planner)
	})
	if !found || len(submitErrors) != 0 || len(api.uploadBodies) != 1 {
		t.Fatalf("checksum-mismatch submission = (%t, %v), uploads %d; want blocked without upload", found, submitErrors, len(api.uploadBodies))
	}
	for _, child := range children {
		stored, err := database.GeminiRequestByID(child.ID)
		if err != nil || stored == nil || stored.State != db.GeminiRequestBlocked {
			t.Fatalf("checksum-mismatched child = %+v, %v; want blocked", stored, err)
		}
	}
}

func TestGeminiOnePageBatchMaxTokensRetriesSameOriginalThenBlocks(t *testing.T) {
	ctx := context.Background()
	original := commandTestPDF("only")
	path := filepath.Join(t.TempDir(), "one-page.pdf")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	digest, err := checksum.SHA256File(path)
	if err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(filepath.Join(t.TempDir(), "one-page.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	contentID, err := database.InsertContent(digest, 1)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := database.InsertDocument(path, contentID, now, now); err != nil {
		t.Fatal(err)
	}
	batchID, err := database.CreateGeminiBatch(
		"one-page", ocr.GeminiBatchModel, 3, 7, nil,
		[]db.GeminiRequestPlan{{
			ContentID: contentID, RequestKey: "one-page-key", FileType: "pdf", PageStart: 0, PageEnd: 1,
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
	firstOutput := geminiMaxTokensOutputLine(t, requests[0].RequestKey, 2, 1, 1)
	var totals batchContinueTotals
	if err := accountGeminiOutput(bytes.NewReader(firstOutput), database, *batch, &totals); err != nil {
		t.Fatal(err)
	}
	retryable, err := database.GeminiRequestByID(requests[0].ID)
	if err != nil || retryable == nil || retryable.State != db.GeminiRequestRetryable || retryable.AttemptCount != 1 {
		t.Fatalf("first MAX_TOKENS request = %+v, %v; want one retry", retryable, err)
	}
	if _, err := database.FinalizeGeminiBatch(batchID, now); err != nil {
		t.Fatal(err)
	}

	api := &fakeGeminiBatchAPI{}
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	planner := ocr.NewGeminiClient("", now)
	var found bool
	var submitErrors []error
	_ = captureStdout(t, func() {
		found, submitErrors = submitRetryableGeminiRequests(cmd, database, api, planner)
	})
	if !found || len(submitErrors) != 0 || len(api.uploadBodies) != 1 {
		t.Fatalf("retry submission = (%t, %v), uploads %d", found, submitErrors, len(api.uploadBodies))
	}
	uploaded := decodeUploadedGeminiInputs(t, api.uploadBodies[0])[requests[0].RequestKey]
	if !bytes.Equal(uploaded.data, original) || uploaded.pages != 1 {
		t.Fatalf("retry input = %d bytes/%d pages, want unchanged one-page original", len(uploaded.data), uploaded.pages)
	}
	batches, err := database.ListGeminiBatches()
	if err != nil || len(batches) != 1 {
		t.Fatalf("ListGeminiBatches() = %+v, %v", batches, err)
	}
	assigned, err := database.GeminiRequestsForBatch(batches[0].ID)
	if err != nil || len(assigned) != 1 || assigned[0].AttemptCount != 1 {
		t.Fatalf("replacement request = %+v, %v", assigned, err)
	}
	secondOutput := geminiMaxTokensOutputLine(t, assigned[0].RequestKey, 2, 1, 1)
	if err := accountGeminiOutput(bytes.NewReader(secondOutput), database, batches[0], &totals); err != nil {
		t.Fatal(err)
	}
	blocked, err := database.GeminiRequestByID(assigned[0].ID)
	if err != nil || blocked == nil || blocked.State != db.GeminiRequestBlocked || blocked.BatchID != nil {
		t.Fatalf("second MAX_TOKENS request = %+v, %v; want detached block", blocked, err)
	}
}

func TestGeminiOnePageBatch413BlocksWithoutRetry(t *testing.T) {
	database, batch, request := createOutputTestBatch(t, 0, 1)
	line := []byte(fmt.Sprintf(
		`{"key":%q,"error":{"code":413,"status":"INVALID_ARGUMENT","message":"payload too large"}}`+"\n",
		request.RequestKey,
	))
	var totals batchContinueTotals
	if err := accountGeminiOutput(bytes.NewReader(line), database, batch, &totals); err != nil {
		t.Fatal(err)
	}
	blocked, err := database.GeminiRequestByID(request.ID)
	if err != nil || blocked == nil || blocked.State != db.GeminiRequestBlocked || blocked.BatchID != nil {
		t.Fatalf("one-page 413 request = %+v, %v; want detached block", blocked, err)
	}
	if !totals.accounted || !totals.billing.Indeterminate {
		t.Fatalf("totals = %+v, want indeterminate failed request", totals)
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
	disposition, err := refreshAndHandleGeminiBatch(cmd, database, api, batch, &totals)
	if err != nil {
		t.Fatalf("refreshAndHandleGeminiBatch() error = %v", err)
	}
	if disposition != batchAdvanceDidWork {
		t.Fatalf("refresh disposition = %v, want handled", disposition)
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
	disposition, err := refreshAndHandleGeminiBatch(cmd, database, api, batch, &totals)
	if err != nil {
		t.Fatalf("refreshAndHandleGeminiBatch() error = %v", err)
	}
	if disposition != batchAdvanceDidWork {
		t.Fatalf("refresh disposition = %v, want handled", disposition)
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
	disposition, err := refreshAndHandleGeminiBatch(cmd, database, api, batch, &totals)
	if err != nil {
		t.Fatalf("refreshAndHandleGeminiBatch() error = %v", err)
	}
	if disposition != batchAdvanceDidWork {
		t.Fatalf("refresh disposition = %v, want handled", disposition)
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

func insertGeminiCleanup(t *testing.T, database *db.DB, kind, name string) {
	t.Helper()
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := database.Exec(
		`INSERT INTO gemini_batch_cleanup
		 (resource_kind, resource_name, created_at, updated_at)
		 VALUES (?, ?, ?, ?)`,
		kind, name, stamp, stamp,
	); err != nil {
		t.Fatal(err)
	}
}

func geminiDeleteInvalidArgumentError(message string) error {
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"code": 400, "message": message, "status": "INVALID_ARGUMENT",
		},
	})
	return &ocr.GeminiBatchAPIError{StatusCode: 400, Body: body}
}

// captureStderr replaces a process global and must not be used by parallel tests.
func captureStderr(t *testing.T, run func()) string {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "stderr-")
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stderr
	os.Stderr = file
	func() {
		defer func() { os.Stderr = original }()
		run()
	}()
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	output, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(output)
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

type uploadedGeminiInput struct {
	data  []byte
	pages int
}

func decodeUploadedGeminiInputs(t *testing.T, body []byte) map[string]uploadedGeminiInput {
	t.Helper()
	inputs := make(map[string]uploadedGeminiInput)
	for _, raw := range bytes.Split(bytes.TrimSpace(body), []byte{'\n'}) {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var line struct {
			Key     string `json:"key"`
			Request struct {
				Contents []struct {
					Parts []struct {
						InlineData *struct {
							Data string `json:"data"`
						} `json:"inlineData"`
					} `json:"parts"`
				} `json:"contents"`
				GenerationConfig struct {
					ResponseJSONSchema struct {
						Properties map[string]struct {
							MaxItems *int `json:"maxItems"`
						} `json:"properties"`
					} `json:"responseJsonSchema"`
				} `json:"generationConfig"`
			} `json:"request"`
		}
		if err := json.Unmarshal(raw, &line); err != nil {
			t.Fatalf("decode uploaded Gemini line: %v", err)
		}
		var encoded string
		for _, content := range line.Request.Contents {
			for _, part := range content.Parts {
				if part.InlineData != nil {
					encoded = part.InlineData.Data
				}
			}
		}
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			t.Fatalf("decode uploaded Gemini data: %v", err)
		}
		pageSchema, ok := line.Request.GenerationConfig.ResponseJSONSchema.Properties["pages"]
		if line.Key == "" || encoded == "" || !ok || pageSchema.MaxItems == nil {
			t.Fatalf("incomplete uploaded Gemini line: %s", raw)
		}
		inputs[line.Key] = uploadedGeminiInput{data: data, pages: *pageSchema.MaxItems}
	}
	return inputs
}

// commandTestPDF keeps labeled page streams uncompressed so the adaptive batch
// test can prove each regenerated body contains the requested page subset.
func commandTestPDF(labels ...string) []byte {
	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")
	offsets := []int{0}
	writeObject := func(number int, body string) {
		offsets = append(offsets, out.Len())
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", number, body)
	}
	kids := make([]string, len(labels))
	for i := range labels {
		kids[i] = fmt.Sprintf("%d 0 R", 4+i*2)
	}
	writeObject(1, "<< /Type /Catalog /Pages 2 0 R >>")
	writeObject(2, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(labels)))
	writeObject(3, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	for i, label := range labels {
		pageObject := 4 + i*2
		contentObject := pageObject + 1
		writeObject(pageObject, fmt.Sprintf(
			"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 3 0 R >> >> /Contents %d 0 R >>",
			contentObject,
		))
		stream := fmt.Sprintf("BT /F1 12 Tf 72 720 Td (%s) Tj ET", label)
		writeObject(contentObject, fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream))
	}
	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n", len(offsets))
	out.WriteString("0000000000 65535 f \n")
	for _, offset := range offsets[1:] {
		fmt.Fprintf(&out, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets), xref)
	return out.Bytes()
}

func geminiMaxTokensOutputLine(t *testing.T, key string, input, candidate, thinking int64) []byte {
	t.Helper()
	response := map[string]any{
		"candidates": []any{map[string]any{
			"finishReason": "MAX_TOKENS",
			"content":      map[string]any{"parts": []any{map[string]any{"text": "{}"}}},
		}},
		"usageMetadata": map[string]any{
			"promptTokenCount": input, "candidatesTokenCount": candidate, "thoughtsTokenCount": thinking,
		},
	}
	line, err := json.Marshal(map[string]any{"key": key, "response": response})
	if err != nil {
		t.Fatalf("marshal MAX_TOKENS output line: %v", err)
	}
	return append(line, '\n')
}

func successfulGeminiOutputLine(t *testing.T, key string, pageIndex int, input, candidate, thinking int64) []byte {
	t.Helper()
	payload := fmt.Sprintf(
		`{"pages":[{"page_index":%d,"transcription":"text","page_description":"page","visual_elements":[]}]}`,
		pageIndex,
	)
	response := map[string]any{
		"modelVersion": "gemini-response-test",
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
