package ocr

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGeminiOCRFileRequestAndMarkdown(t *testing.T) {
	input := filepath.Join(t.TempDir(), "scan.jpg")
	if err := os.WriteFile(input, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta/models/gemini-3.7-flash:generateContent" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("x-goog-api-key"); got != "test-key" {
			t.Fatalf("key = %q", got)
		}
		var request geminiRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		parts := request.Contents[0].Parts
		if got := parts[0].InlineData.MIMEType; got != "image/jpeg" {
			t.Fatalf("MIME type = %q", got)
		}
		if got := parts[0].MediaResolution.Level; got != "MEDIA_RESOLUTION_HIGH" {
			t.Fatalf("resolution = %q", got)
		}
		if got, _ := base64.StdEncoding.DecodeString(parts[0].InlineData.Data); string(got) != "image" {
			t.Fatalf("inline data = %q", got)
		}
		if request.GenerationConfig.ThinkingConfig.ThinkingLevel != "LOW" || request.GenerationConfig.ResponseMIMEType != "application/json" || request.GenerationConfig.MaxOutputTokens != 65_536 {
			t.Fatalf("generation config = %#v", request.GenerationConfig)
		}
		schema := request.GenerationConfig.ResponseJSONSchema
		pageArray := schema.Properties["pages"]
		if schema.Type != "object" || pageArray.Type != "array" || pageArray.Items == nil || pageArray.Items.Type != "object" {
			t.Fatalf("responseJsonSchema uses invalid JSON Schema types: %#v", schema)
		}
		_, _ = w.Write([]byte(geminiResponseJSON("STOP", `{"pages":[{"page_index":0,"transcription":"# Receipt","page_description":"A receipt page","visual_elements":[{"type":"logo","description":"Store mark"}]}]}`, 10, 2, 3)))
	}))
	defer server.Close()
	client := NewGeminiClient("test-key", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	client.endpoint, client.sleep, client.randFloat64 = server.URL+"/v1beta/models/gemini-3.7-flash:generateContent", func(context.Context, time.Duration) error { return nil }, func() float64 { return 0 }
	pages, report, err := client.OCRFile(context.Background(), input, "jpeg")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pages, []PageResult{{PageIndex: 0, Markdown: "# Receipt\n\n[Page: A receipt page]\n\n[Image: logo — Store mark]"}}; !equalPages(got, want) {
		t.Fatalf("pages = %#v, want %#v", got, want)
	}
	if got, want := report.KnownCost, GeminiCost(client.runAt, 10, 5); got != want || report.Indeterminate {
		t.Fatalf("report = %#v", report)
	}
}

func TestGeminiSemanticRetryBillsBothResponses(t *testing.T) {
	input := filepath.Join(t.TempDir(), "scan.png")
	if err := os.WriteFile(input, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := atomic.AddInt32(&calls, 1)
		payload := `{"pages":[{"page_index":0,"transcription":"","page_description":"Page","visual_elements":[]}]}`
		if call == 1 {
			_, _ = w.Write([]byte(geminiResponseJSON("STOP", `{"pages":[]}`, 2, 1, 0)))
			return
		}
		_, _ = w.Write([]byte(geminiResponseJSON("STOP", payload, 3, 2, 1)))
	}))
	defer server.Close()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	client := NewGeminiClient("key", at)
	client.endpoint = server.URL
	client.sleep = func(context.Context, time.Duration) error { return nil }
	_, report, err := client.OCRFile(context.Background(), input, "png")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := atomic.LoadInt32(&calls), int32(2); got != want {
		t.Fatalf("calls = %d", got)
	}
	if got, want := report.KnownCost, GeminiCost(at, 2, 1)+GeminiCost(at, 3, 3); got != want || report.Indeterminate {
		t.Fatalf("report = %#v", report)
	}
}

func TestGeminiRejectsMalformedCandidateAndMarksMissingUsageIndeterminate(t *testing.T) {
	input := filepath.Join(t.TempDir(), "scan.png")
	if err := os.WriteFile(input, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"candidates":[{"index":0,"finishReason":"STOP","content":{"parts":[{"text":"{}"}]}}]}`))
	}))
	defer server.Close()
	client := NewGeminiClient("key", time.Now())
	client.endpoint = server.URL
	client.sleep = func(context.Context, time.Duration) error { return nil }
	_, report, err := client.OCRFile(context.Background(), input, "png")
	if err == nil || !strings.Contains(err.Error(), "invalid response") {
		t.Fatalf("error = %v", err)
	}
	if !report.Indeterminate {
		t.Fatalf("report = %#v, want indeterminate", report)
	}
}

func TestGeminiRetriesTransientResponses(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(geminiResponseJSON("STOP", `{"pages":[{"page_index":0,"transcription":"","page_description":"Page","visual_elements":[]}]}`, 1, 1, 0)))
	}))
	defer server.Close()
	client := NewGeminiClient("key", time.Now())
	client.endpoint = server.URL
	client.sleep = func(context.Context, time.Duration) error { return nil }
	client.randFloat64 = func() float64 { return 0 }
	body, err := client.buildRequestBody([]byte("x"), "png", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.doWithRetry(context.Background(), body); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("calls = %d", got)
	}
}

func TestGeminiRetryHonorsRetryAfter(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(geminiResponseJSON("STOP", `{"pages":[{"page_index":0,"transcription":"","page_description":"Page","visual_elements":[]}]}`, 1, 1, 0)))
	}))
	defer server.Close()

	client := NewGeminiClient("key", time.Now())
	client.endpoint = server.URL
	client.randFloat64 = func() float64 { return 0 }
	var sleeps []time.Duration
	client.sleep = func(_ context.Context, delay time.Duration) error {
		sleeps = append(sleeps, delay)
		return nil
	}
	body, err := client.buildRequestBody([]byte("x"), "png", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.doWithRetry(context.Background(), body); err != nil {
		t.Fatal(err)
	}
	if len(sleeps) != 1 || sleeps[0] != 30*time.Second {
		t.Fatalf("sleeps = %v, want [30s]", sleeps)
	}
	now := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	header := now.Add(45 * time.Second).Format(http.TimeFormat)
	if got := geminiNextBackoff(time.Second, header, now, func() float64 { return 0 }); got != 45*time.Second {
		t.Fatalf("HTTP-date backoff = %v, want 45s", got)
	}
}

func TestGeminiRangeEntryPoints(t *testing.T) {
	input := filepath.Join(t.TempDir(), "scan.pdf")
	if err := os.WriteFile(input, []byte("pdf"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(geminiResponseJSON("STOP", geminiPagesPayload(2), 2, 2, 0)))
	}))
	defer server.Close()

	client := NewGeminiClient("key", time.Now())
	client.endpoint = server.URL
	client.pageCount = func(context.Context, io.ReadSeeker) (int, error) { return 3, nil }
	client.extractRange = func(_ context.Context, _ io.ReadSeeker, start, end, _ int) ([]byte, error) {
		if start != 1 || end != 3 {
			t.Fatalf("extract range = %d-%d, want 1-3", start, end)
		}
		return []byte("range-data"), nil
	}

	prepared, err := client.PrepareRangeRequest(context.Background(), input, "pdf", 1, 3)
	if err != nil {
		t.Fatalf("PrepareRangeRequest() error = %v", err)
	}
	if prepared.PageStart != 1 || prepared.PageEnd != 3 {
		t.Fatalf("prepared range = %d-%d, want 1-3", prepared.PageStart, prepared.PageEnd)
	}
	var request geminiRequest
	if err := json.Unmarshal(prepared.Body, &request); err != nil {
		t.Fatalf("decode prepared body: %v", err)
	}
	data, err := base64.StdEncoding.DecodeString(request.Contents[0].Parts[0].InlineData.Data)
	if err != nil || string(data) != "range-data" {
		t.Fatalf("prepared data = %q, %v", data, err)
	}

	pages, _, err := client.OCRRange(context.Background(), input, "pdf", 1, 3)
	if err != nil {
		t.Fatalf("OCRRange() error = %v", err)
	}
	if len(pages) != 2 || pages[0].PageIndex != 1 || pages[1].PageIndex != 2 {
		t.Fatalf("OCRRange() pages = %+v, want absolute indexes 1 and 2", pages)
	}
}

func TestGeminiPrepareRangeClassifiesSerializedSizeError(t *testing.T) {
	input := filepath.Join(t.TempDir(), "scan.pdf")
	if err := os.WriteFile(input, []byte("pdf"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := NewGeminiClient("", time.Now())
	limit, err := client.requestBodySize(1, "pdf", 1)
	if err != nil {
		t.Fatal(err)
	}
	client.requestByteLimit = limit
	client.decodedByteLimit = 1
	client.pageCount = func(context.Context, io.ReadSeeker) (int, error) { return 10, nil }
	client.extractRange = func(context.Context, io.ReadSeeker, int, int, int) ([]byte, error) {
		return []byte("x"), nil
	}

	_, err = client.PrepareRangeRequest(context.Background(), input, "pdf", 0, 10)
	if !IsGeminiRangeSizeError(err) {
		t.Fatalf("PrepareRangeRequest() error = %v, want GeminiRangeSizeError", err)
	}
}

func TestGeminiPDFChunksUseRelativeIndexesAndAbsoluteOffsets(t *testing.T) {
	input := filepath.Join(t.TempDir(), "scan.pdf")
	if err := os.WriteFile(input, []byte("pdf"), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request geminiRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if got := request.Contents[0].Parts[0].MediaResolution.Level; got != "MEDIA_RESOLUTION_MEDIUM" {
			t.Fatalf("resolution = %q", got)
		}
		count := *request.GenerationConfig.ResponseJSONSchema.Properties["pages"].MaxItems
		atomic.AddInt32(&calls, 1)
		_, _ = w.Write([]byte(geminiResponseJSON("STOP", geminiPagesPayload(count), 1, 1, 0)))
	}))
	defer server.Close()
	client := NewGeminiClient("key", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	client.endpoint = server.URL
	client.pageLimit = 2
	client.pageCount = func(context.Context, io.ReadSeeker) (int, error) { return 3, nil }
	client.extractRange = func(_ context.Context, _ io.ReadSeeker, start, end, _ int) ([]byte, error) {
		return []byte(strings.Repeat("x", end-start)), nil
	}
	pages, _, err := client.OCRFile(context.Background(), input, "pdf")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pages, []PageResult{{0, "[Page: Page]"}, {1, "[Page: Page]"}, {2, "[Page: Page]"}}; !equalPages(got, want) {
		t.Fatalf("pages = %#v, want %#v", got, want)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("calls = %d, want 2", got)
	}
}

func TestGeminiPDFRecursivelySplits413AndKeepsBilling(t *testing.T) {
	input := filepath.Join(t.TempDir(), "scan.pdf")
	if err := os.WriteFile(input, []byte("pdf"), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request geminiRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		rangeData, err := base64.StdEncoding.DecodeString(request.Contents[0].Parts[0].InlineData.Data)
		if err != nil {
			t.Fatal(err)
		}
		atomic.AddInt32(&calls, 1)
		if string(rangeData) == "0-4" {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = w.Write([]byte(`{"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":1,"thoughtsTokenCount":0}}`))
			return
		}
		count := *request.GenerationConfig.ResponseJSONSchema.Properties["pages"].MaxItems
		_, _ = w.Write([]byte(geminiResponseJSON("STOP", geminiPagesPayload(count), 2, 1, 0)))
	}))
	defer server.Close()

	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	client := NewGeminiClient("key", at)
	client.endpoint = server.URL
	client.pageCount = func(context.Context, io.ReadSeeker) (int, error) { return 4, nil }
	client.extractRange = func(_ context.Context, _ io.ReadSeeker, start, end, _ int) ([]byte, error) {
		return []byte(strconv.Itoa(start) + "-" + strconv.Itoa(end)), nil
	}
	pages, report, err := client.OCRFile(context.Background(), input, "pdf")
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 4 || pages[3].PageIndex != 3 {
		t.Fatalf("pages = %#v, want four absolute indexes", pages)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("calls = %d, want initial request plus two halves", got)
	}
	if got, want := report.KnownCost, GeminiCost(at, 8, 3); got != want || report.Indeterminate {
		t.Fatalf("report = %#v, want %d", report, want)
	}
}

func TestGeminiPDFMaxTokensWithoutCandidateIndexSplitsImmediately(t *testing.T) {
	input := filepath.Join(t.TempDir(), "scan.pdf")
	if err := os.WriteFile(input, []byte("pdf"), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := atomic.AddInt32(&calls, 1)
		var request geminiRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		count := *request.GenerationConfig.ResponseJSONSchema.Properties["pages"].MaxItems
		if call == 1 {
			response := strings.Replace(geminiResponseJSON("MAX_TOKENS", `{}`, 2, 1, 1), `"index":0,`, "", 1)
			_, _ = w.Write([]byte(response))
			return
		}
		_, _ = w.Write([]byte(geminiResponseJSON("STOP", geminiPagesPayload(count), 1, 1, 0)))
	}))
	defer server.Close()

	client := NewGeminiClient("key", time.Now())
	client.endpoint = server.URL
	client.pageCount = func(context.Context, io.ReadSeeker) (int, error) { return 2, nil }
	client.extractRange = func(_ context.Context, _ io.ReadSeeker, start, end, _ int) ([]byte, error) {
		return []byte(strings.Repeat("x", end-start)), nil
	}
	pages, _, err := client.OCRFile(context.Background(), input, "pdf")
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 2 || atomic.LoadInt32(&calls) != 3 {
		t.Fatalf("pages = %#v, calls = %d; want split without unchanged retry", pages, calls)
	}
}

func TestGeminiDecodeAcceptsMissingCandidateIndex(t *testing.T) {
	response := strings.Replace(geminiResponseJSON("STOP", geminiPagesPayload(1), 1, 1, 0), `"index":0,`, "", 1)
	pages, err := decodeGeminiResults([]byte(response), 1)
	if err != nil || len(pages) != 1 {
		t.Fatalf("decodeGeminiResults() pages = %#v, error = %v", pages, err)
	}
}

func TestGeminiDecodeRejectsCandidateFinishAndVisuals(t *testing.T) {
	for _, response := range []string{
		geminiResponseJSON("MAX_TOKENS", `{"pages":[]}`, 1, 1, 0),
		geminiResponseJSON("STOP", `{"pages":[{"page_index":0,"transcription":"","page_description":"Page","visual_elements":[{"type":"","description":"x"}]}]}`, 1, 1, 0),
	} {
		if _, err := decodeGeminiResults([]byte(response), 1); err == nil {
			t.Fatalf("response unexpectedly accepted: %s", response)
		}
	}
}

func TestGeminiDecodeExplainsRecitationFinishReason(t *testing.T) {
	response := geminiResponseJSON("RECITATION", `{"pages":[]}`, 1, 0, 0)
	_, err := decodeGeminiResults([]byte(response), 1)
	want := "Gemini stopped generation for potential recitation (RECITATION)"
	if err == nil || err.Error() != want {
		t.Fatalf("decodeGeminiResults() error = %v, want %q", err, want)
	}
}

func geminiPagesPayload(count int) string {
	pages := make([]map[string]any, count)
	for index := range pages {
		pages[index] = map[string]any{"page_index": index, "transcription": "", "page_description": "Page", "visual_elements": []any{}}
	}
	payload, _ := json.Marshal(map[string]any{"pages": pages})
	return string(payload)
}

func geminiResponseJSON(finish, payload string, prompt, candidate, thoughts int) string {
	return `{"candidates":[{"index":0,"finishReason":"` + finish + `","content":{"parts":[{"text":` + strconvQuote(payload) + `}]}}],"usageMetadata":{"promptTokenCount":` + itoa(prompt) + `,"candidatesTokenCount":` + itoa(candidate) + `,"thoughtsTokenCount":` + itoa(thoughts) + `}}`
}
func strconvQuote(s string) string { encoded, _ := json.Marshal(s); return string(encoded) }
func itoa(n int) string            { return strconv.Itoa(n) }
func equalPages(a, b []PageResult) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
