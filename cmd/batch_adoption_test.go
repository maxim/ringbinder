package cmd

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/maxim/ringbinder/internal/db"
	"github.com/maxim/ringbinder/internal/ocr"
	"github.com/spf13/cobra"
)

func TestUnknownUploadAdoptionRequiresExactlyOneDisplayNameMatch(t *testing.T) {
	for _, matchCount := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("matches-%d", matchCount), func(t *testing.T) {
			database, batch := createAdoptionTestBatch(t, fmt.Sprintf("upload-%d", matchCount))
			if err := database.SetGeminiBatchUploadUnknown(batch.ID, time.Now().UTC()); err != nil {
				t.Fatalf("SetGeminiBatchUploadUnknown() error = %v", err)
			}
			api := &fakeGeminiBatchAPI{}
			for i := 0; i < matchCount; i++ {
				api.files = append(api.files, ocr.GeminiRemoteFile{
					Name: fmt.Sprintf("files/%d", i), DisplayName: batch.DisplayName,
				})
			}
			cmd := &cobra.Command{}
			cmd.SetContext(context.Background())
			err := adoptUnknownGeminiUpload(cmd, database, api, batch)
			if matchCount == 1 {
				if err != nil {
					t.Fatalf("adoptUnknownGeminiUpload() error = %v", err)
				}
				if api.createCalls != 1 {
					t.Fatalf("create calls = %d, want 1", api.createCalls)
				}
				stored, err := database.GetGeminiBatch(batch.ID)
				if err != nil {
					t.Fatalf("GetGeminiBatch() error = %v", err)
				}
				if stored == nil || stored.State != db.GeminiBatchPending || stored.RemoteName != "batches/1" {
					t.Fatalf("stored batch = %+v", stored)
				}
				return
			}
			if err == nil {
				t.Fatalf("adoption with %d matches succeeded", matchCount)
			}
			if api.createCalls != 0 {
				t.Fatalf("create calls = %d, want none", api.createCalls)
			}
			stored, queryErr := database.GetGeminiBatch(batch.ID)
			if queryErr != nil {
				t.Fatalf("GetGeminiBatch() error = %v", queryErr)
			}
			if stored == nil || stored.State != db.GeminiBatchUploadUnknown {
				t.Fatalf("stored batch = %+v, want upload_unknown", stored)
			}
		})
	}
}

func TestDeterministicCreateFailureBlocksInsteadOfRepeating(t *testing.T) {
	database, batch := createAdoptionTestBatch(t, "deterministic-create")
	if err := database.SetGeminiBatchUploaded(batch.ID, "files/input", time.Now().UTC()); err != nil {
		t.Fatalf("SetGeminiBatchUploaded() error = %v", err)
	}
	api := &fakeGeminiBatchAPI{createError: &ocr.GeminiBatchAPIError{
		StatusCode: 400, Body: []byte("INVALID_ARGUMENT"),
	}}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := submitUploadedGeminiBatch(cmd, database, api, batch.ID); err == nil {
		t.Fatal("submitUploadedGeminiBatch() error = nil, want deterministic rejection")
	}
	if api.createCalls != 1 {
		t.Fatalf("create calls = %d, want 1", api.createCalls)
	}
	stored, err := database.GetGeminiBatch(batch.ID)
	if err != nil {
		t.Fatalf("GetGeminiBatch() error = %v", err)
	}
	if stored != nil {
		t.Fatalf("deterministically failed batch remains retryable: %+v", stored)
	}
	blocked, err := database.BlockedGeminiRequests()
	if err != nil {
		t.Fatalf("BlockedGeminiRequests() error = %v", err)
	}
	if len(blocked) != 1 || blocked[0].LastError == "" {
		t.Fatalf("blocked requests = %+v", blocked)
	}
}

func TestUnknownSubmissionAdoptsExactlyOneCurrentBatchResource(t *testing.T) {
	database, batch := createAdoptionTestBatch(t, "submission")
	if err := database.SetGeminiBatchUploaded(batch.ID, "files/input", time.Now().UTC()); err != nil {
		t.Fatalf("SetGeminiBatchUploaded() error = %v", err)
	}
	if err := database.SetGeminiBatchSubmissionUnknown(batch.ID, time.Now().UTC()); err != nil {
		t.Fatalf("SetGeminiBatchSubmissionUnknown() error = %v", err)
	}
	api := &fakeGeminiBatchAPI{batches: []ocr.GeminiRemoteBatch{{
		Name: "batches/remote", DisplayName: batch.DisplayName,
		State: "BATCH_STATE_SUCCEEDED", OutputFileName: "files/output",
	}}}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := adoptUnknownGeminiSubmission(cmd, database, api, batch); err != nil {
		t.Fatalf("adoptUnknownGeminiSubmission() error = %v", err)
	}
	stored, err := database.GetGeminiBatch(batch.ID)
	if err != nil {
		t.Fatalf("GetGeminiBatch() error = %v", err)
	}
	if stored == nil || stored.State != db.GeminiBatchSucceeded ||
		stored.RemoteName != "batches/remote" || stored.OutputFileName != "files/output" {
		t.Fatalf("stored batch = %+v", stored)
	}
}

func createAdoptionTestBatch(t *testing.T, suffix string) (*db.DB, db.GeminiBatch) {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "adoption.db"))
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	contentID, err := database.InsertContent("checksum-"+suffix, 1)
	if err != nil {
		t.Fatalf("InsertContent() error = %v", err)
	}
	if _, err := database.InsertDocument("/docs/"+suffix+".png", contentID, time.Now().UTC(), time.Now().UTC()); err != nil {
		t.Fatalf("InsertDocument() error = %v", err)
	}
	now := time.Now().UTC()
	prices := ocr.GeminiBatchPrices(now)
	batchID, err := database.CreateGeminiBatch(
		"batch-"+suffix, ocr.GeminiBatchModel, int64(prices.Input), int64(prices.Output), nil,
		[]db.GeminiRequestPlan{{
			ContentID: contentID, RequestKey: "key-" + suffix,
			FileType: "png", PageStart: 0, PageEnd: 1,
		}}, now,
	)
	if err != nil {
		t.Fatalf("CreateGeminiBatch() error = %v", err)
	}
	batch, err := database.GetGeminiBatch(batchID)
	if err != nil {
		t.Fatalf("GetGeminiBatch() error = %v", err)
	}
	return database, *batch
}
