package ocr

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFormatCurrency_RoundsOnlyAtDisplayBoundary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		cost Currency
		want string
	}{
		{cost: 0, want: "$0.0000"},
		{cost: 49_999, want: "$0.0000"},
		{cost: 50_000, want: "$0.0001"},
		{cost: 1_380_000, want: "$0.0014"},
		{cost: 1_000_000_000, want: "$1.0000"},
	}
	for _, tt := range tests {
		if got := FormatCurrency(tt.cost); got != tt.want {
			t.Fatalf("FormatCurrency(%d) = %q, want %q", tt.cost, got, tt.want)
		}
	}
}

func TestGeminiPrices_Boundary(t *testing.T) {
	t.Parallel()

	before := time.Date(2026, 12, 31, 23, 59, 59, 999_999_999, time.UTC)
	if got, want := GeminiCost(before, 1, 1), Currency(4_500); got != want {
		t.Fatalf("GeminiCost(before) = %d, want %d", got, want)
	}
	after := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	if got, want := GeminiCost(after, 1, 1), Currency(9_000); got != want {
		t.Fatalf("GeminiCost(after) = %d, want %d", got, want)
	}
}

func TestBillingExtraction_IgnoresMalformedOutputFields(t *testing.T) {
	t.Parallel()

	mistral := mistralBillingIfPresent([]byte(`{"pages":{},"usage_info":{"pages_processed":2}}`))
	if mistral == nil || mistral.KnownCost != MistralCost(2) {
		t.Fatalf("Mistral report = %#v, want authoritative usage", mistral)
	}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	gemini := geminiBillingIfPresent([]byte(`{"candidates":{},"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3,"thoughtsTokenCount":4}}`), at)
	if gemini == nil || gemini.KnownCost != GeminiCost(at, 2, 7) {
		t.Fatalf("Gemini report = %#v, want authoritative usage", gemini)
	}
}

func TestMistralBilling_SurvivesValidationFailure(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"pages":[],"usage_info":{"pages_processed":1}}`))
	}))
	defer server.Close()

	client := NewMistralClient("test-key")
	client.endpoint = server.URL
	input := writeTempOCRFile(t, "input.png", []byte("image"))

	_, report, err := client.OCRFile(context.Background(), input, "png")
	if err == nil {
		t.Fatalf("OCRFile() error = nil, want validation error")
	}
	if report.KnownCost != MistralCost(1) || report.Indeterminate {
		t.Fatalf("report = %+v, want one known page", report)
	}
}

func TestMistralBilling_KeepsAuthoritativeUsageFromRetriedError(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"usage_info":{"pages_processed":1}}`))
			return
		}
		_, _ = w.Write([]byte(`{"pages":[{"index":0,"markdown":"text"}],"usage_info":{"pages_processed":1}}`))
	}))
	defer server.Close()

	client := NewMistralClient("test-key")
	client.endpoint = server.URL
	client.sleep = func(context.Context, time.Duration) error { return nil }
	input := writeTempOCRFile(t, "input.png", []byte("image"))

	_, report, err := client.OCRFile(context.Background(), input, "png")
	if err != nil {
		t.Fatalf("OCRFile() error = %v", err)
	}
	if report.KnownCost != MistralCost(2) || report.Indeterminate {
		t.Fatalf("report = %+v, want two authoritative billed pages", report)
	}
}

func TestMistralBilling_MissingUsageIsIndeterminate(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"pages":[{"index":0,"markdown":"text"}]}`))
	}))
	defer server.Close()

	client := NewMistralClient("test-key")
	client.endpoint = server.URL
	input := writeTempOCRFile(t, "input.png", []byte("image"))

	pages, report, err := client.OCRFile(context.Background(), input, "png")
	if err != nil {
		t.Fatalf("OCRFile() error = %v", err)
	}
	if len(pages) != 1 {
		t.Fatalf("pages = %+v, want one", pages)
	}
	if !report.Indeterminate || report.KnownCost != 0 {
		t.Fatalf("report = %+v, want indeterminate zero known cost", report)
	}
}
