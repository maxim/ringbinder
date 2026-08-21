package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maxim/ringbinder/internal/checksum"
	"github.com/maxim/ringbinder/internal/db"
	"github.com/maxim/ringbinder/internal/ocr"
)

type fakeGeminiBatchAPI struct {
	requestKey            string
	state                 string
	output                []byte
	deleted               []string
	cancelRequested       bool
	uploadCalls           int
	uploadError           error
	uploadReadBeforeError int64
	createCalls           int
	createError           error
	remoteError           string
	deleteErrors          map[string]error
	downloadCalls         int
	downloadError         error
	getError              error
	files                 []ocr.GeminiRemoteFile
	batches               []ocr.GeminiRemoteBatch
}

func (api *fakeGeminiBatchAPI) UploadJSONL(_ context.Context, _ string, source io.ReadSeeker, _ int64) (ocr.GeminiRemoteFile, error) {
	api.uploadCalls++
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return ocr.GeminiRemoteFile{}, err
	}
	if api.uploadError != nil {
		if api.uploadReadBeforeError > 0 {
			_, _ = io.CopyN(io.Discard, source, api.uploadReadBeforeError)
		}
		return ocr.GeminiRemoteFile{}, api.uploadError
	}
	body, err := io.ReadAll(source)
	if err != nil {
		return ocr.GeminiRemoteFile{}, err
	}
	var line struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(body))), &line); err != nil {
		return ocr.GeminiRemoteFile{}, err
	}
	api.requestKey = line.Key
	return ocr.GeminiRemoteFile{Name: "files/input", DisplayName: "input"}, nil
}

func (api *fakeGeminiBatchAPI) CreateBatch(context.Context, string, string, string) (ocr.GeminiRemoteBatch, error) {
	api.createCalls++
	if api.createError != nil {
		return ocr.GeminiRemoteBatch{}, api.createError
	}
	return ocr.GeminiRemoteBatch{
		Name: fmt.Sprintf("batches/%d", api.createCalls), State: "JOB_STATE_PENDING",
	}, nil
}

func (api *fakeGeminiBatchAPI) GetBatch(context.Context, string) (ocr.GeminiRemoteBatch, error) {
	if api.getError != nil {
		return ocr.GeminiRemoteBatch{}, api.getError
	}
	outputFile := "files/output"
	if api.state == "JOB_STATE_FAILED" || api.state == "BATCH_STATE_FAILED" ||
		api.state == "JOB_STATE_CANCELLED" || api.state == "BATCH_STATE_CANCELLED" {
		outputFile = ""
	}
	return ocr.GeminiRemoteBatch{
		Name: "batches/1", State: api.state, OutputFileName: outputFile,
		ErrorMessage: api.remoteError,
	}, nil
}

func (api *fakeGeminiBatchAPI) ListBatches(context.Context) ([]ocr.GeminiRemoteBatch, error) {
	return api.batches, nil
}

func (api *fakeGeminiBatchAPI) ListFiles(context.Context) ([]ocr.GeminiRemoteFile, error) {
	return api.files, nil
}

func (api *fakeGeminiBatchAPI) CancelBatch(context.Context, string) error {
	api.cancelRequested = true
	return nil
}

func (api *fakeGeminiBatchAPI) DeleteBatch(_ context.Context, name string) error {
	api.deleted = append(api.deleted, name)
	return api.deleteErrors[name]
}

func (api *fakeGeminiBatchAPI) DeleteFile(_ context.Context, name string) error {
	api.deleted = append(api.deleted, name)
	return api.deleteErrors[name]
}

func (api *fakeGeminiBatchAPI) DownloadFile(context.Context, string) (io.ReadCloser, error) {
	api.downloadCalls++
	if api.downloadError != nil {
		return nil, api.downloadError
	}
	return io.NopCloser(strings.NewReader(string(api.output))), nil
}

func TestBatchStartAndContinueFakeEndToEnd(t *testing.T) {
	resetCommandState(t)
	t.Setenv("GEMINI_API_KEY", "test-key")
	api := &fakeGeminiBatchAPI{state: "JOB_STATE_SUCCEEDED"}
	oldFactory := newGeminiBatchAPI
	newGeminiBatchAPI = func(string) geminiBatchAPI { return api }
	t.Cleanup(func() { newGeminiBatchAPI = oldFactory })

	imagePath := filepath.Join(t.TempDir(), "scan.png")
	if err := os.WriteFile(imagePath, []byte("not-a-real-png-but-valid-transport-bytes"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	sum, err := checksum.SHA256File(imagePath)
	if err != nil {
		t.Fatalf("SHA256File() error = %v", err)
	}
	dbPath := filepath.Join(t.TempDir(), "batch.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	contentID, err := database.InsertContent(sum, 1)
	if err != nil {
		t.Fatalf("InsertContent() error = %v", err)
	}
	if _, err := database.InsertDocument(imagePath, contentID, time.Now().UTC(), time.Now().UTC()); err != nil {
		t.Fatalf("InsertDocument() error = %v", err)
	}
	_ = database.Close()

	startCmd := commandWithDatabaseFlag(t, dbPath)
	startCmd.SetContext(context.Background())
	startCmd.Flags().String("model", "", "")
	startCmd.Flags().Int("limit", 0, "")
	if err := startCmd.Flags().Set("model", modelGemini); err != nil {
		t.Fatalf("set start model: %v", err)
	}
	var startErr error
	startOutput := captureStdout(t, func() {
		startErr = runBatchStart(startCmd, nil)
	})
	if startErr != nil {
		t.Fatalf("runBatchStart() error = %v", startErr)
	}
	assertInOrder(t, startOutput,
		"Gemini batch 1 prepared with 1 request(s).",
		"Uploading Gemini batch 1:",
		"Gemini batch 1 upload complete:",
		"Gemini batch 1 submitted.",
	)
	if api.requestKey == "" {
		t.Fatal("batch input did not include a request key")
	}
	cancelCmd := commandWithDatabaseFlag(t, dbPath)
	cancelCmd.SetContext(context.Background())
	cancelCmd.Flags().String("model", "", "")
	if err := cancelCmd.Flags().Set("model", modelGemini); err != nil {
		t.Fatalf("set cancel model: %v", err)
	}
	if err := runBatchCancel(cancelCmd, []string{"1"}); err != nil {
		t.Fatalf("runBatchCancel() error = %v", err)
	}
	if !api.cancelRequested {
		t.Fatal("cancel request was not sent")
	}

	// The remote success fixture pins cancellation losing its race: continue
	// must import the completed output rather than discarding it as cancelled.
	api.output = successfulGeminiOutputLine(t, api.requestKey, 0, 10, 20, 5)
	continueCmd := commandWithDatabaseFlag(t, dbPath)
	continueCmd.SetContext(context.Background())
	continueCmd.Flags().String("model", "", "")
	if err := continueCmd.Flags().Set("model", modelGemini); err != nil {
		t.Fatalf("set continue model: %v", err)
	}
	if err := runBatchContinue(continueCmd, nil); err != nil {
		t.Fatalf("runBatchContinue() error = %v", err)
	}

	database, err = db.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen DB error = %v", err)
	}
	defer database.Close()
	content, err := database.GetContentByID(contentID)
	if err != nil {
		t.Fatalf("GetContentByID() error = %v", err)
	}
	if content == nil || content.OCRPending {
		t.Fatalf("content = %+v, want promoted OCR", content)
	}
	batches, err := database.CountTrackedGeminiBatches()
	if err != nil {
		t.Fatalf("CountTrackedGeminiBatches() error = %v", err)
	}
	cleanup, err := database.CountGeminiCleanup()
	if err != nil {
		t.Fatalf("CountGeminiCleanup() error = %v", err)
	}
	if batches != 0 || cleanup != 0 {
		t.Fatalf("batches = %d cleanup = %d, want handled and cleaned", batches, cleanup)
	}
	if got := strings.Join(api.deleted, ","); got != "batches/1,files/input" {
		t.Fatalf("deleted resources = %v, want batch and uploaded input", api.deleted)
	}
}
