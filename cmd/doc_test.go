package cmd

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/maxim/ringbinder/internal/db"
)

func TestDocumentJSONIncludesCoverageAndNullableModels(t *testing.T) {
	t.Parallel()
	exact := "provider-model-v1"
	payload := documentJSON(db.Document{
		Path:              "/docs/example.pdf",
		CreatedAt:         time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ModifiedAt:        time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		PageCount:         3,
		OCRPagesCompleted: 2,
		Models: []db.ModelCount{
			{Model: nil, PageCount: 1},
			{Model: &exact, PageCount: 1},
		},
		OCRPending: true,
	})
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"path":"/docs/example.pdf","created_at":"2026-01-01T00:00:00Z","modified_at":"2026-01-02T00:00:00Z","page_count":3,"ocr_pages_completed":2,"models":[{"model":null,"page_count":1},{"model":"provider-model-v1","page_count":1}],"ocr_pending":true,"deleted":false}`
	if string(encoded) != want {
		t.Fatalf("JSON = %s, want %s", encoded, want)
	}
}

func TestParseDocListTimeFlag_Empty(t *testing.T) {
	t.Parallel()

	parsed, err := parseDocListTimeFlag("")
	if err != nil {
		t.Fatalf("parseDocListTimeFlag() error = %v", err)
	}
	if parsed != nil {
		t.Fatalf("parseDocListTimeFlag() = %v, want nil", parsed)
	}
}

func TestParseDocListTimeFlag_RFC3339(t *testing.T) {
	t.Parallel()

	parsed, err := parseDocListTimeFlag("2025-01-15T10:30:00Z")
	if err != nil {
		t.Fatalf("parseDocListTimeFlag() error = %v", err)
	}
	if parsed == nil {
		t.Fatalf("parseDocListTimeFlag() = nil, want time")
	}

	want := time.Date(2025, time.January, 15, 10, 30, 0, 0, time.UTC)
	if !parsed.Equal(want) {
		t.Fatalf("parseDocListTimeFlag() = %s, want %s", parsed.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
	}
}

func TestParseDocListTimeFlag_DateOnly(t *testing.T) {
	t.Parallel()

	parsed, err := parseDocListTimeFlag("2025-01-15")
	if err != nil {
		t.Fatalf("parseDocListTimeFlag() error = %v", err)
	}
	if parsed == nil {
		t.Fatalf("parseDocListTimeFlag() = nil, want time")
	}

	want := time.Date(2025, time.January, 15, 0, 0, 0, 0, time.UTC)
	if !parsed.Equal(want) {
		t.Fatalf("parseDocListTimeFlag() = %s, want %s", parsed.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
	}
}

func TestParseDocListTimeFlag_Invalid(t *testing.T) {
	t.Parallel()

	if _, err := parseDocListTimeFlag("01/15/2025"); err == nil {
		t.Fatalf("parseDocListTimeFlag() error = nil, want error")
	}
}
