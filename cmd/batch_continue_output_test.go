package cmd

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maxim/ringbinder/internal/db"
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
			if output != "Checked 1 Gemini batch; nothing ready to process.\n" {
				t.Fatalf("output = %q", output)
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
	if api.uploadCalls != 1 {
		t.Fatalf("upload calls = %d, want 1", api.uploadCalls)
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
		[]db.GeminiStagedPage{{PageIndex: 0, Markdown: "ready"}},
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
