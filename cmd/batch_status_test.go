package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/maxim/ringbinder/internal/db"
	"github.com/maxim/ringbinder/internal/ocr"
)

func TestBatchForgetErasesLocalWorkWithoutAPIActivityOrWarning(t *testing.T) {
	resetCommandState(t)
	t.Setenv("GEMINI_API_KEY", "")
	dbPath := filepath.Join(t.TempDir(), "forget.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	contentID := addCostContent(t, database, "forget-command", 1, true, "/docs/forget.png")
	batchID, err := database.CreateGeminiBatch(
		"forget-command", ocr.GeminiBatchModel, 375, 1875, nil,
		[]db.GeminiRequestPlan{{
			ContentID: contentID, RequestKey: "forget-command-key",
			FileType: "png", PageStart: 0, PageEnd: 1,
		}},
		time.Now().UTC(),
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := database.SetGeminiBatchUploaded(batchID, "files/forget-command", now); err != nil {
		t.Fatal(err)
	}
	if err := database.SetGeminiBatchRemote(
		batchID, "batches/forget-command", db.GeminiBatchRunning, "", "", now,
	); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	oldFactory := newGeminiBatchAPI
	newGeminiBatchAPI = func(string) geminiBatchAPI {
		t.Fatal("batch forget constructed a Gemini API transport")
		return nil
	}
	t.Cleanup(func() { newGeminiBatchAPI = oldFactory })
	cmd := commandWithDatabaseFlag(t, dbPath)
	cmd.Flags().String("model", "", "")
	if err := cmd.Flags().Set("model", modelGemini); err != nil {
		t.Fatal(err)
	}
	var output string
	var runErr error
	stderr := captureStderr(t, func() {
		output = captureStdout(t, func() {
			runErr = runBatchForget(cmd, []string{strconv.FormatInt(batchID, 10)})
		})
	})
	if runErr != nil {
		t.Fatalf("runBatchForget() error = %v", runErr)
	}
	wantOutput := "Forgot Gemini batch " + strconv.FormatInt(batchID, 10) + "; completed OCR pages were retained.\n"
	if output != wantOutput || stderr != "" {
		t.Fatalf("stdout = %q, stderr = %q; want %q and no warning", output, stderr, wantOutput)
	}

	output = ""
	stderr = captureStderr(t, func() {
		output = captureStdout(t, func() {
			runErr = runBatchForget(cmd, []string{strconv.FormatInt(batchID, 10)})
		})
	})
	wantError := "Gemini batch " + strconv.FormatInt(batchID, 10) + " not found"
	if runErr == nil || runErr.Error() != wantError || output != "" || stderr != "" {
		t.Fatalf(
			"second forget error = %v, stdout = %q, stderr = %q; want %q and no output",
			runErr, output, stderr, wantError,
		)
	}

	database, err = db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	batches, err := database.CountTrackedGeminiBatches()
	if err != nil {
		t.Fatal(err)
	}
	pending, err := database.PendingContentsForGeminiBatch()
	if err != nil {
		t.Fatal(err)
	}
	if batches != 0 || len(pending) != 1 || pending[0].ID != contentID {
		t.Fatalf("batches = %d, pending = %+v; want forgotten batch and fresh pending content", batches, pending)
	}
}

func TestBatchListJSONReportsBlockedWorkWithoutBatchHistory(t *testing.T) {
	resetCommandState(t)
	t.Setenv("GEMINI_API_KEY", "test-key")
	dbPath := filepath.Join(t.TempDir(), "blocked-list.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	firstID := addCostContent(t, database, "first-blocked", 2, true, "/docs/first.pdf")
	secondID := addCostContent(t, database, "second-blocked", 1, true, "/docs/second.png")
	plans := []db.GeminiRequestPlan{
		{ContentID: firstID, RequestKey: "first-a", FileType: "pdf", PageStart: 0, PageEnd: 1},
		{ContentID: firstID, RequestKey: "first-b", FileType: "pdf", PageStart: 1, PageEnd: 2},
		{ContentID: secondID, RequestKey: "second", FileType: "png", PageStart: 0, PageEnd: 1},
	}
	for _, plan := range plans {
		if _, err := database.CreateBlockedGeminiRequest(plan, "blocked", now); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	cmd := commandWithDatabaseFlag(t, dbPath)
	cmd.Flags().String("model", "", "")
	cmd.Flags().Bool("json", false, "")
	if err := cmd.Flags().Set("model", modelGemini); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("json", "true"); err != nil {
		t.Fatal(err)
	}
	oldTerminalCheck := progressWriterIsTerminal
	progressWriterIsTerminal = func(io.Writer) bool { return true }
	t.Cleanup(func() { progressWriterIsTerminal = oldTerminalCheck })
	var runErr error
	output := captureStdout(t, func() { runErr = runBatchList(cmd, nil) })
	if runErr != nil {
		t.Fatalf("runBatchList() error = %v", runErr)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(output), &fields); err != nil {
		t.Fatalf("decode output %q: %v", output, err)
	}
	for _, key := range []string{
		"batches", "blocked_requests", "blocked_contents", "cleanup_pending", "refresh_errors",
	} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("fields = %v, missing %q", fields, key)
		}
	}
	if len(fields) != 5 {
		t.Fatalf("fields = %v, want exactly five documented keys", fields)
	}
	if strings.Contains(output, "\x1b[") {
		t.Fatalf("JSON output contains terminal progress: %q", output)
	}
	if string(fields["batches"]) != "[]" || string(fields["refresh_errors"]) != "[]" ||
		string(fields["blocked_requests"]) != "3" || string(fields["blocked_contents"]) != "2" {
		t.Fatalf("fields = %v, want empty history and blocked counts 3/2", fields)
	}
}

func TestBatchListHumanOutputShowsOneRefreshLifecycle(t *testing.T) {
	resetCommandState(t)
	t.Setenv("GEMINI_API_KEY", "test-key")
	dbPath := filepath.Join(t.TempDir(), "list-progress.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	batch, _ := addPreparedImageTestBatch(t, database, "list-progress")
	if err := database.SetGeminiBatchRemote(
		batch.ID, "batches/list-progress", db.GeminiBatchRunning, "", "", time.Now().UTC(),
	); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	api := &fakeGeminiBatchAPI{state: "JOB_STATE_RUNNING"}
	oldFactory := newGeminiBatchAPI
	newGeminiBatchAPI = func(string) geminiBatchAPI { return api }
	t.Cleanup(func() { newGeminiBatchAPI = oldFactory })
	cmd := commandWithDatabaseFlag(t, dbPath)
	cmd.SetContext(context.Background())
	cmd.Flags().String("model", "", "")
	cmd.Flags().Bool("json", false, "")
	if err := cmd.Flags().Set("model", modelGemini); err != nil {
		t.Fatal(err)
	}
	var runErr error
	output := captureStdout(t, func() { runErr = runBatchList(cmd, nil) })
	if runErr != nil {
		t.Fatalf("runBatchList() error = %v", runErr)
	}
	if !strings.Contains(output, "Refreshing Gemini batches started: 0/1 batches.") ||
		!strings.Contains(output, strconv.FormatInt(batch.ID, 10)+"\trunning") ||
		strings.Contains(output, "\x1b[") {
		t.Fatalf("human refresh output = %q", output)
	}
}

func TestBatchListDoesNotReconcileCompletedResponseAfterCancellation(t *testing.T) {
	resetCommandState(t)
	t.Setenv("GEMINI_API_KEY", "test-key")
	dbPath := filepath.Join(t.TempDir(), "cancelled-list-refresh.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	batch, _ := addPreparedImageTestBatch(t, database, "cancelled-list-refresh")
	if err := database.SetGeminiBatchRemote(
		batch.ID, "batches/cancelled-list-refresh", db.GeminiBatchRunning, "", "", time.Now().UTC(),
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
			Name: "batches/cancelled-list-refresh", State: "JOB_STATE_SUCCEEDED", OutputFileName: "files/output",
		}, nil
	}}
	oldFactory := newGeminiBatchAPI
	newGeminiBatchAPI = func(string) geminiBatchAPI { return api }
	t.Cleanup(func() { newGeminiBatchAPI = oldFactory })
	cmd := commandWithDatabaseFlag(t, dbPath)
	cmd.SetContext(ctx)
	cmd.Flags().String("model", "", "")
	cmd.Flags().Bool("json", false, "")
	if err := cmd.Flags().Set("model", modelGemini); err != nil {
		t.Fatal(err)
	}
	var runErr error
	_ = captureStdout(t, func() { runErr = runBatchList(cmd, nil) })
	if !errors.Is(runErr, context.Canceled) {
		t.Fatalf("runBatchList() error = %v, want context cancellation", runErr)
	}
	if api.getCalls != 1 {
		t.Fatalf("refresh calls = %d, want one", api.getCalls)
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

func TestBatchCancelProgressCleansUpTTYOnSuccessAndError(t *testing.T) {
	resetCommandState(t)
	t.Setenv("GEMINI_API_KEY", "test-key")
	oldTerminalCheck := progressWriterIsTerminal
	progressWriterIsTerminal = func(io.Writer) bool { return true }
	t.Cleanup(func() { progressWriterIsTerminal = oldTerminalCheck })
	oldFactory := newGeminiBatchAPI
	t.Cleanup(func() { newGeminiBatchAPI = oldFactory })

	tests := []struct {
		name        string
		cancelError error
	}{
		{name: "success"},
		{name: "error", cancelError: errors.New("remote cancellation rejected")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "cancel.db")
			database, err := db.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			batch, _ := addPreparedImageTestBatch(t, database, "cancel-progress")
			if err := database.SetGeminiBatchRemote(
				batch.ID, "batches/cancel-progress", db.GeminiBatchRunning, "", "", time.Now().UTC(),
			); err != nil {
				t.Fatal(err)
			}
			if err := database.Close(); err != nil {
				t.Fatal(err)
			}

			api := &fakeGeminiBatchAPI{cancelError: test.cancelError}
			newGeminiBatchAPI = func(string) geminiBatchAPI { return api }
			cmd := commandWithDatabaseFlag(t, dbPath)
			cmd.SetContext(context.Background())
			cmd.Flags().String("model", "", "")
			if err := cmd.Flags().Set("model", modelGemini); err != nil {
				t.Fatal(err)
			}
			var runErr error
			output := captureStdout(t, func() { runErr = runBatchCancel(cmd, []string{strconv.FormatInt(batch.ID, 10)}) })
			if !errors.Is(runErr, test.cancelError) {
				t.Fatalf("runBatchCancel() error = %v, want %v", runErr, test.cancelError)
			}
			if !api.cancelRequested || !strings.Contains(output, "Requesting cancellation for Gemini batch ") ||
				strings.Index(output, "\x1b[?25l") < 0 ||
				strings.LastIndex(output, "\x1b[?25h") < strings.Index(output, "\x1b[?25l") {
				t.Fatalf("cancel progress did not clean up its terminal display: %q", output)
			}
			if test.cancelError == nil {
				if !strings.Contains(output, "Cancellation requested for Gemini batch ") {
					t.Fatalf("success output = %q, want confirmation", output)
				}
			} else if strings.Contains(output, "Cancellation requested for Gemini batch ") {
				t.Fatalf("error output = %q, must not confirm cancellation", output)
			}

			database, err = db.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			stored, err := database.GetGeminiBatch(batch.ID)
			_ = database.Close()
			if err != nil || stored == nil || stored.State != db.GeminiBatchCancelling {
				t.Fatalf("batch after cancellation attempt = %+v, %v; want durable cancelling intent", stored, err)
			}
		})
	}
}

func TestBatchListJSONEmitsCachedStaleEnvelopeOnGlobalRefreshFailure(t *testing.T) {
	resetCommandState(t)
	t.Setenv("GEMINI_API_KEY", "test-key")
	dbPath := filepath.Join(t.TempDir(), "list.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	contentID := addCostContent(t, database, "list-content", 1, true, "/docs/list.png")
	now := time.Now().UTC()
	batchID, err := database.CreateGeminiBatch(
		"list-batch", ocr.GeminiBatchModel, 375, 1875, nil,
		[]db.GeminiRequestPlan{{ContentID: contentID, RequestKey: "list-key", FileType: "png", PageStart: 0, PageEnd: 1}},
		now,
	)
	if err != nil {
		t.Fatalf("CreateGeminiBatch() error = %v", err)
	}
	if err := database.SetGeminiBatchRemote(batchID, "batches/1", db.GeminiBatchPending, "", "", now); err != nil {
		t.Fatalf("SetGeminiBatchRemote() error = %v", err)
	}
	blockedContentID := addCostContent(t, database, "blocked-content", 1, true, "/docs/blocked.png")
	if _, err := database.CreateBlockedGeminiRequest(
		db.GeminiRequestPlan{
			ContentID: blockedContentID, RequestKey: "blocked-key",
			FileType: "png", PageStart: 0, PageEnd: 1,
		},
		"blocked",
		now,
	); err != nil {
		t.Fatalf("CreateBlockedGeminiRequest() error = %v", err)
	}
	_ = database.Close()

	api := &fakeGeminiBatchAPI{getError: &ocr.GeminiBatchAPIError{StatusCode: 401, Body: []byte("bad key")}}
	oldFactory := newGeminiBatchAPI
	newGeminiBatchAPI = func(string) geminiBatchAPI { return api }
	t.Cleanup(func() { newGeminiBatchAPI = oldFactory })
	cmd := commandWithDatabaseFlag(t, dbPath)
	cmd.Flags().String("model", "", "")
	cmd.Flags().Bool("json", false, "")
	if err := cmd.Flags().Set("model", modelGemini); err != nil {
		t.Fatalf("set model: %v", err)
	}
	if err := cmd.Flags().Set("json", "true"); err != nil {
		t.Fatalf("set json: %v", err)
	}
	oldTerminalCheck := progressWriterIsTerminal
	progressWriterIsTerminal = func(io.Writer) bool { return true }
	t.Cleanup(func() { progressWriterIsTerminal = oldTerminalCheck })
	var runErr error
	var output string
	stderr := captureStderr(t, func() {
		output = captureStdout(t, func() { runErr = runBatchList(cmd, nil) })
	})
	if runErr == nil {
		t.Fatal("runBatchList() error = nil, want partial refresh failure")
	}
	var envelope batchListEnvelope
	if err := json.Unmarshal([]byte(output), &envelope); err != nil {
		t.Fatalf("decode list JSON %q: %v", output, err)
	}
	if len(envelope.Batches) != 1 || envelope.Batches[0].ID != batchID ||
		envelope.Batches[0].State != db.GeminiBatchPending || !envelope.Batches[0].Stale {
		t.Fatalf("batches = %+v, want one cached stale row", envelope.Batches)
	}
	if envelope.BlockedRequests != 1 || envelope.BlockedContents != 1 ||
		envelope.CleanupPending != 0 || len(envelope.RefreshErrors) != 1 ||
		envelope.RefreshErrors[0].BatchID != nil {
		t.Fatalf("envelope = %+v, want blocked counts and nullable global refresh error", envelope)
	}
	if api.getCalls != 1 || api.downloadCalls != 0 || len(api.deleted) != 0 {
		t.Fatalf("list refresh calls = %d, downloads=%d, deletes=%v; want one refresh and no lifecycle side effects", api.getCalls, api.downloadCalls, api.deleted)
	}
	if stderr != "warning: refresh Gemini batch "+strconv.FormatInt(batchID, 10)+": Gemini Batch API returned HTTP 401: bad key\n" {
		t.Fatalf("stderr = %q, want the refresh warning only", stderr)
	}
	if strings.Contains(output, "warning:") || strings.Contains(output, "\x1b[") {
		t.Fatalf("JSON stdout contains warning or terminal output: %q", output)
	}
}
