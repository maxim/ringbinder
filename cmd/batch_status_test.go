package cmd

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/maxim/ringbinder/internal/db"
	"github.com/maxim/ringbinder/internal/ocr"
)

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
	var runErr error
	output := captureStdout(t, func() { runErr = runBatchList(cmd, nil) })
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
	if envelope.CleanupPending != 0 || len(envelope.RefreshErrors) != 1 ||
		envelope.RefreshErrors[0].BatchID != nil {
		t.Fatalf("envelope = %+v, want nullable global refresh error", envelope)
	}
	if api.downloadCalls != 0 || len(api.deleted) != 0 {
		t.Fatalf("list performed lifecycle side effects: downloads=%d deletes=%v", api.downloadCalls, api.deleted)
	}
}
