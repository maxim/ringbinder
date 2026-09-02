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

func TestUnknownSubmissionAdoptsExactlyOneProvenanceMatch(t *testing.T) {
	tests := []struct {
		name    string
		remotes func(db.GeminiBatch) []ocr.GeminiRemoteBatch
		adopts  bool
	}{
		{name: "zero"},
		{
			name: "one",
			remotes: func(batch db.GeminiBatch) []ocr.GeminiRemoteBatch {
				return []ocr.GeminiRemoteBatch{matchingRemoteBatch(batch, "batches/remote")}
			},
			adopts: true,
		},
		{
			name: "multiple",
			remotes: func(batch db.GeminiBatch) []ocr.GeminiRemoteBatch {
				return []ocr.GeminiRemoteBatch{
					matchingRemoteBatch(batch, "batches/one"),
					matchingRemoteBatch(batch, "batches/two"),
				}
			},
		},
		{
			name: "one among same-name decoys",
			remotes: func(batch db.GeminiBatch) []ocr.GeminiRemoteBatch {
				wrongModel := matchingRemoteBatch(batch, "batches/wrong-model")
				wrongModel.Model = "models/gemini-3.8-flash"
				wrongInput := matchingRemoteBatch(batch, "batches/wrong-input")
				wrongInput.InputFileName = "files/other"
				return []ocr.GeminiRemoteBatch{
					wrongModel,
					matchingRemoteBatch(batch, "batches/remote"),
					wrongInput,
				}
			},
			adopts: true,
		},
		{
			name: "wrong model",
			remotes: func(batch db.GeminiBatch) []ocr.GeminiRemoteBatch {
				remote := matchingRemoteBatch(batch, "batches/wrong-model")
				remote.Model = "models/gemini-3.8-flash"
				return []ocr.GeminiRemoteBatch{remote}
			},
		},
		{
			name: "wrong input",
			remotes: func(batch db.GeminiBatch) []ocr.GeminiRemoteBatch {
				remote := matchingRemoteBatch(batch, "batches/wrong-input")
				remote.InputFileName = "files/other"
				return []ocr.GeminiRemoteBatch{remote}
			},
		},
		{
			name: "blank provenance",
			remotes: func(batch db.GeminiBatch) []ocr.GeminiRemoteBatch {
				remote := matchingRemoteBatch(batch, "batches/blank-model")
				remote.Model = ""
				return []ocr.GeminiRemoteBatch{remote}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			database, batch := createAdoptionTestBatch(t, "submission-"+test.name)
			batch.Model = "gemini-3.7-flash"
			if _, err := database.Exec(`UPDATE gemini_batches SET model = ? WHERE id = ?`, batch.Model, batch.ID); err != nil {
				t.Fatal(err)
			}
			if err := database.SetGeminiBatchUploaded(batch.ID, "files/input", time.Now().UTC()); err != nil {
				t.Fatalf("SetGeminiBatchUploaded() error = %v", err)
			}
			now := time.Now().UTC()
			prices := ocr.GeminiBatchPrices(now)
			if err := database.ClaimGeminiBatchSubmission(
				batch.ID, batch.Model, int64(prices.Input), int64(prices.Output), now,
			); err != nil {
				t.Fatalf("ClaimGeminiBatchSubmission() error = %v", err)
			}
			stored, err := database.GetGeminiBatch(batch.ID)
			if err != nil || stored == nil {
				t.Fatalf("GetGeminiBatch() = %+v, %v", stored, err)
			}
			batch = *stored
			api := &fakeGeminiBatchAPI{}
			if test.remotes != nil {
				api.batches = test.remotes(batch)
			}
			cmd := &cobra.Command{}
			cmd.SetContext(context.Background())
			err = adoptUnknownGeminiSubmission(cmd, database, api, batch)
			if test.adopts {
				if err != nil {
					t.Fatalf("adoptUnknownGeminiSubmission() error = %v", err)
				}
				stored, err = database.GetGeminiBatch(batch.ID)
				if err != nil || stored == nil || stored.State != db.GeminiBatchSucceeded ||
					stored.RemoteName != "batches/remote" || stored.OutputFileName != "files/output" ||
					stored.Model != "gemini-3.7-flash" {
					t.Fatalf("adopted batch = %+v, %v", stored, err)
				}
				return
			}
			if err == nil {
				t.Fatal("adoptUnknownGeminiSubmission() error = nil")
			}
			stored, queryErr := database.GetGeminiBatch(batch.ID)
			if queryErr != nil || stored == nil || stored.State != db.GeminiBatchSubmissionUnknown ||
				stored.RemoteName != "" || stored.Model != "gemini-3.7-flash" {
				t.Fatalf("unadopted batch = %+v, %v", stored, queryErr)
			}
			if api.createCalls != 0 {
				t.Fatalf("CreateBatch calls = %d, want 0", api.createCalls)
			}
		})
	}
}

func matchingRemoteBatch(batch db.GeminiBatch, name string) ocr.GeminiRemoteBatch {
	return ocr.GeminiRemoteBatch{
		Name: name, DisplayName: batch.DisplayName, Model: "models/" + batch.Model,
		InputFileName: batch.InputFileName, State: "BATCH_STATE_SUCCEEDED",
		OutputFileName: "files/output",
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
