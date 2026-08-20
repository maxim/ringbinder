package cmd

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maxim/ringbinder/internal/db"
	"github.com/maxim/ringbinder/internal/ocr"
	"github.com/spf13/cobra"
)

func TestUploadedBatchWithOrphanedMembershipIsNeverSubmitted(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "orphan.db"))
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	defer database.Close()
	now := time.Now().UTC()
	firstID, err := database.InsertContent("orphan-first", 1)
	if err != nil {
		t.Fatalf("InsertContent(first) error = %v", err)
	}
	firstDoc, err := database.InsertDocument("/docs/orphan.png", firstID, now, now)
	if err != nil {
		t.Fatalf("InsertDocument(first) error = %v", err)
	}
	secondID, err := database.InsertContent("orphan-second", 1)
	if err != nil {
		t.Fatalf("InsertContent(second) error = %v", err)
	}
	if _, err := database.InsertDocument("/docs/live.png", secondID, now, now); err != nil {
		t.Fatalf("InsertDocument(second) error = %v", err)
	}
	prices := ocr.GeminiBatchPrices(now)
	batchID, err := database.CreateGeminiBatch(
		"orphan-batch", ocr.GeminiBatchModel, int64(prices.Input), int64(prices.Output), nil,
		[]db.GeminiRequestPlan{
			{ContentID: firstID, RequestKey: "orphan-key", FileType: "png", PageStart: 0, PageEnd: 1},
			{ContentID: secondID, RequestKey: "live-key", FileType: "png", PageStart: 0, PageEnd: 1},
		}, now,
	)
	if err != nil {
		t.Fatalf("CreateGeminiBatch() error = %v", err)
	}
	if err := database.SetGeminiBatchUploaded(batchID, "files/uploaded", now); err != nil {
		t.Fatalf("SetGeminiBatchUploaded() error = %v", err)
	}
	if _, err := database.Exec("DELETE FROM documents WHERE id = ?", firstDoc); err != nil {
		t.Fatalf("delete orphan document: %v", err)
	}
	if _, err := database.Exec("DELETE FROM contents WHERE id = ?", firstID); err != nil {
		t.Fatalf("delete orphan content: %v", err)
	}

	api := &fakeGeminiBatchAPI{}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	err = submitUploadedGeminiBatch(cmd, database, api, batchID)
	if err == nil || !strings.Contains(err.Error(), "refusing non-idempotent submission") {
		t.Fatalf("submitUploadedGeminiBatch() error = %v", err)
	}
	if api.createCalls != 0 {
		t.Fatalf("CreateBatch calls = %d, want 0", api.createCalls)
	}
	batch, err := database.GetGeminiBatch(batchID)
	if err != nil {
		t.Fatalf("GetGeminiBatch() error = %v", err)
	}
	if batch != nil {
		t.Fatalf("batch remains tracked: %+v", batch)
	}
	retryable, err := database.RetryableGeminiRequests()
	if err != nil {
		t.Fatalf("RetryableGeminiRequests() error = %v", err)
	}
	if len(retryable) != 1 || retryable[0].ContentID != secondID {
		t.Fatalf("retryable = %+v, want surviving request detached for regeneration", retryable)
	}
	cleanup, err := database.ListGeminiCleanup()
	if err != nil {
		t.Fatalf("ListGeminiCleanup() error = %v", err)
	}
	if len(cleanup) != 1 || cleanup[0].ResourceName != "files/uploaded" {
		t.Fatalf("cleanup = %+v, want uploaded input cleanup", cleanup)
	}
}
