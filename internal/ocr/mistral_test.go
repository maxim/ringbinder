package ocr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMistralPricePerPage(t *testing.T) {
	t.Parallel()

	if got, want := MistralPricePerPage(), 0.002; got != want {
		t.Fatalf("MistralPricePerPage() = %v, want %v", got, want)
	}
}

func TestRetry_MaxAttempts(t *testing.T) {
	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate limited"}`))
	}))
	defer server.Close()

	client := NewMistralClient("test-key")
	client.endpoint = server.URL
	client.sleep = func(ctx context.Context, d time.Duration) error { return nil }
	client.randFloat64 = func() float64 { return 0 }

	_, err := client.doWithRetry(context.Background(), mistralRequest{
		Model: mistralModel,
		Document: mistralDocument{
			Type:        "document_url",
			DocumentURL: "data:application/pdf;base64,AA==",
		},
	})
	if err == nil {
		t.Fatalf("doWithRetry() error = nil, want non-nil")
	}
	if got := atomic.LoadInt32(&requests); got != 5 {
		t.Fatalf("request attempts = %d, want 5", got)
	}
}

func TestRetry_5xxRetried(t *testing.T) {
	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&requests, 1)
		if count <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"unavailable"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"pages":[]}`))
	}))
	defer server.Close()

	client := NewMistralClient("test-key")
	client.endpoint = server.URL
	client.sleep = func(ctx context.Context, d time.Duration) error { return nil }
	client.randFloat64 = func() float64 { return 0 }

	if _, err := client.doWithRetry(context.Background(), mistralRequest{
		Model: mistralModel,
		Document: mistralDocument{
			Type:        "document_url",
			DocumentURL: "data:application/pdf;base64,AA==",
		},
	}); err != nil {
		t.Fatalf("doWithRetry() error = %v", err)
	}
	if got := atomic.LoadInt32(&requests); got != 3 {
		t.Fatalf("request attempts = %d, want 3", got)
	}
}

func TestRetry_4xxNotRetried(t *testing.T) {
	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad request"}`))
	}))
	defer server.Close()

	client := NewMistralClient("test-key")
	client.endpoint = server.URL
	client.sleep = func(ctx context.Context, d time.Duration) error { return nil }
	client.randFloat64 = func() float64 { return 0 }

	_, err := client.doWithRetry(context.Background(), mistralRequest{
		Model: mistralModel,
		Document: mistralDocument{
			Type:        "document_url",
			DocumentURL: "data:application/pdf;base64,AA==",
		},
	})
	if err == nil {
		t.Fatalf("doWithRetry() error = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "API error 400") {
		t.Fatalf("error = %q, want contains %q", err.Error(), "API error 400")
	}
	if got := atomic.LoadInt32(&requests); got != 1 {
		t.Fatalf("request attempts = %d, want 1", got)
	}
}
