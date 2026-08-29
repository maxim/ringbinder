package cmd

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maxim/ringbinder/internal/db"
)

func TestDocAndReadCommandsExposeCoverageAndPerPageProvenance(t *testing.T) {
	resetCommandState(t)
	databasePath := filepath.Join(t.TempDir(), "output.db")
	database, err := db.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	contentID, err := database.InsertContent("output-provenance", 2)
	if err != nil {
		t.Fatal(err)
	}
	documentPath := "/docs/output.pdf"
	now := time.Now().UTC()
	if _, err := database.InsertDocument(documentPath, contentID, now, now); err != nil {
		t.Fatal(err)
	}
	exact := "provider-response-v1"
	if err := database.UpsertContentPages(contentID, []db.PageInput{
		{PageIndex: 0, Markdown: "historical", Model: nil},
		{PageIndex: 1, Markdown: "current", Model: &exact},
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	oldDocPath, oldDocJSON := docGetPath, docGetJSON
	oldDocListJSON, oldDocListLimit, oldDocListOffset := docListJSON, docListLimit, docListOffset
	oldDocListAfter, oldDocListBefore := docListAfter, docListBefore
	oldReadPath, oldReadPage := readPath, readPage
	oldReadContext, oldReadStart, oldReadEnd, oldReadJSON := readContext, readStart, readEnd, readJSON
	t.Cleanup(func() {
		docGetPath, docGetJSON = oldDocPath, oldDocJSON
		docListJSON, docListLimit, docListOffset = oldDocListJSON, oldDocListLimit, oldDocListOffset
		docListAfter, docListBefore = oldDocListAfter, oldDocListBefore
		readPath, readPage = oldReadPath, oldReadPage
		readContext, readStart, readEnd, readJSON = oldReadContext, oldReadStart, oldReadEnd, oldReadJSON
	})
	cmd := commandWithDatabaseFlag(t, databasePath)
	docGetPath, docGetJSON = documentPath, true
	docOutput := captureStdout(t, func() {
		if err := runDocGet(cmd, nil); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{
		`"ocr_pages_completed":2`,
		`"models":[{"model":null,"page_count":1},{"model":"provider-response-v1","page_count":1}]`,
		`"ocr_pending":false`,
	} {
		if !strings.Contains(docOutput, want) {
			t.Fatalf("doc output = %q, want %q", docOutput, want)
		}
	}

	docListJSON, docListLimit, docListOffset = true, 50, 0
	docListAfter, docListBefore = "", ""
	listOutput := captureStdout(t, func() {
		if err := runDocList(cmd, nil); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(listOutput, `"ocr_pages_completed":2`) ||
		!strings.Contains(listOutput, `"model":null`) ||
		!strings.Contains(listOutput, `"model":"provider-response-v1"`) {
		t.Fatalf("doc list output = %q", listOutput)
	}
	docListJSON = false
	humanList := captureStdout(t, func() {
		if err := runDocList(cmd, nil); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(humanList, "(2/2 OCR; unknown=1, provider-response-v1=1)") {
		t.Fatalf("human doc list output = %q", humanList)
	}

	readPath, readPage, readContext = documentPath, -1, 0
	readStart, readEnd, readJSON = 0, 1, true
	readOutput := captureStdout(t, func() {
		if err := runRead(cmd, nil); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{`"model":null`, `"model":"provider-response-v1"`} {
		if !strings.Contains(readOutput, want) {
			t.Fatalf("read output = %q, want %q", readOutput, want)
		}
	}
	readJSON = false
	humanRead := captureStdout(t, func() {
		if err := runRead(cmd, nil); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{"--- page 1 (OCR: unknown) ---", "--- page 2 (OCR: provider-response-v1) ---"} {
		if !strings.Contains(humanRead, want) {
			t.Fatalf("human read output = %q, want %q", humanRead, want)
		}
	}
}
