package ocr

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestMistralGenericSinglePage413RemainsUnclassified(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid request"}}`))
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "one.pdf")
	if err := os.WriteFile(path, []byte("pdf"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := NewMistralClient("key")
	client.endpoint = server.URL
	client.pageCount = func(context.Context, io.ReadSeeker) (int, error) { return 1, nil }
	client.extractRange = func(context.Context, io.ReadSeeker, int, int, int) ([]byte, error) {
		return []byte("one page"), nil
	}
	_, err := client.OCRRangeResult(context.Background(), path, "pdf", 0, 1)
	if err == nil {
		t.Fatal("OCRRangeResult() error = nil")
	}
	if got := ClassifyFailure(err); got != FailureUnclassified {
		t.Fatalf("classification = %q, want %q; error = %v", got, FailureUnclassified, err)
	}
}
