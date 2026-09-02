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

	tests := []struct {
		name   string
		at     time.Time
		direct GeminiTokenPrices
		batch  GeminiTokenPrices
	}{
		{
			name:   "before cutover",
			at:     time.Date(2026, 12, 31, 23, 59, 59, 999_999_999, time.UTC),
			direct: GeminiTokenPrices{Input: 750, Output: 3_750},
			batch:  GeminiTokenPrices{Input: 375, Output: 1_875},
		},
		{
			name:   "at cutover",
			at:     time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
			direct: GeminiTokenPrices{Input: 1_500, Output: 7_500},
			batch:  GeminiTokenPrices{Input: 750, Output: 3_750},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := GeminiPrices(test.at); got != test.direct {
				t.Fatalf("GeminiPrices(%v) = %+v, want %+v", test.at, got, test.direct)
			}
			if got := GeminiBatchPrices(test.at); got != test.batch {
				t.Fatalf("GeminiBatchPrices(%v) = %+v, want %+v", test.at, got, test.batch)
			}
		})
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

func TestGeminiBillingTreatsOmittedOutputCountsAsZero(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name string
		body string
		want Currency
	}{
		{
			name: "thoughts",
			body: `{"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":3}}`,
			want: GeminiCost(at, 2, 3),
		},
		{
			name: "candidates",
			body: `{"usageMetadata":{"promptTokenCount":2,"thoughtsTokenCount":4}}`,
			want: GeminiCost(at, 2, 4),
		},
		{
			name: "both output counts",
			body: `{"usageMetadata":{"promptTokenCount":2}}`,
			want: GeminiCost(at, 2, 0),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			report := geminiBillingIfPresent([]byte(test.body), at)
			if report == nil || report.KnownCost != test.want || report.Indeterminate {
				t.Fatalf("report = %#v, want known cost %d", report, test.want)
			}
		})
	}
	if report := geminiBillingIfPresent([]byte(`{"usageMetadata":{"candidatesTokenCount":3}}`), at); report != nil {
		t.Fatalf("report = %#v, want missing prompt usage rejected", report)
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
