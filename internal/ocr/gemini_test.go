package ocr

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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

func TestGeminiProductionDefaults(t *testing.T) {
	const model = "gemini-3.8-flash"
	if GeminiDirectModel != model {
		t.Fatalf("GeminiDirectModel = %q, want %q", GeminiDirectModel, model)
	}
	if GeminiBatchModel != model {
		t.Fatalf("GeminiBatchModel = %q, want %q", GeminiBatchModel, model)
	}
	client := NewGeminiClient("test-key", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if client.endpoint != "https://generativelanguage.googleapis.com/v1beta/models/gemini-3.8-flash:generateContent" {
		t.Fatalf("default endpoint = %q", client.endpoint)
	}
}

func TestGeminiOCRFileRequestAndMarkdown(t *testing.T) {
	input := filepath.Join(t.TempDir(), "scan.jpg")
	if err := os.WriteFile(input, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1beta/models/gemini-3.8-flash:generateContent" {
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
	client.endpoint, client.sleep, client.randFloat64 = server.URL+"/v1beta/models/gemini-3.8-flash:generateContent", func(context.Context, time.Duration) error { return nil }, func() float64 { return 0 }
	pages, report, err := client.OCRFile(context.Background(), input, "jpeg")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := pages, []PageResult{{
		PageIndex: 0,
		Markdown:  "# Receipt\n\n[Page: A receipt page]\n\n[Image: logo — Store mark]",
		Model:     geminiModel,
	}}; !equalPages(got, want) {
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

func TestGeminiPDFCompleteRequestUsesOriginalBytes(t *testing.T) {
	for _, totalPages := range []int{1, geminiMaxPDFPages} {
		t.Run(strconv.Itoa(totalPages)+" pages", func(t *testing.T) {
			original := []byte("original-pdf-" + strconv.Itoa(totalPages))
			input := filepath.Join(t.TempDir(), "scan.pdf")
			if err := os.WriteFile(input, original, 0o600); err != nil {
				t.Fatal(err)
			}
			var calls int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&calls, 1)
				var request geminiRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatal(err)
				}
				data, err := base64.StdEncoding.DecodeString(request.Contents[0].Parts[0].InlineData.Data)
				if err != nil || !bytes.Equal(data, original) {
					t.Fatalf("inline PDF = %q, %v; want original bytes %q", data, err, original)
				}
				if got := *request.GenerationConfig.ResponseJSONSchema.Properties["pages"].MaxItems; got != totalPages {
					t.Fatalf("schema pages = %d, want %d", got, totalPages)
				}
				_, _ = w.Write([]byte(geminiResponseJSON("STOP", geminiPagesPayload(totalPages), 1, 1, 0)))
			}))
			defer server.Close()

			client := NewGeminiClient("key", time.Now())
			client.endpoint = server.URL
			client.pageCount = func(context.Context, io.ReadSeeker) (int, error) { return totalPages, nil }
			client.extractRange = func(context.Context, io.ReadSeeker, int, int, int) ([]byte, error) {
				t.Fatal("eligible complete PDF was extracted")
				return nil, nil
			}
			pages, _, err := client.OCRFile(context.Background(), input, "pdf")
			if err != nil {
				t.Fatal(err)
			}
			if len(pages) != totalPages || pages[0].PageIndex != 0 || pages[len(pages)-1].PageIndex != totalPages-1 {
				t.Fatalf("pages = %+v, want complete zero-based indexes", pages)
			}
			if got := atomic.LoadInt32(&calls); got != 1 {
				t.Fatalf("requests = %d, want 1", got)
			}
		})
	}
}

func TestGeminiPDFUsesAlreadyOpenDescriptorAfterPathReplacement(t *testing.T) {
	original := []byte("already-open-original")
	replacement := []byte("replacement-path-data")
	input := filepath.Join(t.TempDir(), "scan.pdf")
	if err := os.WriteFile(input, original, 0o600); err != nil {
		t.Fatal(err)
	}
	replacementPath := filepath.Join(t.TempDir(), "replacement.pdf")
	if err := os.WriteFile(replacementPath, replacement, 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request geminiRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		data, err := base64.StdEncoding.DecodeString(request.Contents[0].Parts[0].InlineData.Data)
		if err != nil || !bytes.Equal(data, original) {
			t.Fatalf("inline PDF = %q, %v; want bytes from already-open descriptor", data, err)
		}
		_, _ = w.Write([]byte(geminiResponseJSON("STOP", geminiPagesPayload(1), 1, 1, 0)))
	}))
	defer server.Close()

	client := NewGeminiClient("key", time.Now())
	client.endpoint = server.URL
	client.pageCount = func(context.Context, io.ReadSeeker) (int, error) {
		if err := os.Rename(replacementPath, input); err != nil {
			t.Fatal(err)
		}
		return 1, nil
	}
	client.extractRange = func(context.Context, io.ReadSeeker, int, int, int) ([]byte, error) {
		t.Fatal("eligible already-open PDF was extracted")
		return nil, nil
	}
	if _, _, err := client.OCRFile(context.Background(), input, "pdf"); err != nil {
		t.Fatal(err)
	}
}

func TestGeminiPDFOriginalEligibilityFallsBackToExtraction(t *testing.T) {
	t.Run("over page cap without source read", func(t *testing.T) {
		client := NewGeminiClient("", time.Now())
		reader := &trackedReadSeeker{ReadSeeker: bytes.NewReader([]byte("raw"))}
		var extracts int
		client.extractRange = func(_ context.Context, _ io.ReadSeeker, start, end, _ int) ([]byte, error) {
			extracts++
			return []byte("extracted"), nil
		}
		chunk, err := client.planPDFChunk(context.Background(), reader, 3, 21, 0, 21)
		if err != nil {
			t.Fatal(err)
		}
		if chunk.start != 0 || chunk.end != geminiMaxPDFPages || string(chunk.data) != "extracted" {
			t.Fatalf("chunk = %+v, want extracted pages 0-%d", chunk, geminiMaxPDFPages)
		}
		if reader.reads != 0 || extracts == 0 {
			t.Fatalf("source reads = %d, extracts = %d; want no original read and extraction", reader.reads, extracts)
		}
	})

	t.Run("source size precheck exceeds decoded limit", func(t *testing.T) {
		client := NewGeminiClient("", time.Now())
		client.decodedByteLimit = 2
		reader := &trackedReadSeeker{ReadSeeker: bytes.NewReader([]byte("raw"))}
		client.extractRange = func(_ context.Context, _ io.ReadSeeker, start, end, _ int) ([]byte, error) {
			return []byte("x"), nil
		}
		chunk, err := client.planPDFChunk(context.Background(), reader, 3, 1, 0, 1)
		if err != nil {
			t.Fatal(err)
		}
		if string(chunk.data) != "x" || reader.reads != 0 {
			t.Fatalf("chunk data = %q, source reads = %d; want extraction without original read", chunk.data, reader.reads)
		}
	})

	t.Run("exact decoded and serialized limits", func(t *testing.T) {
		client := NewGeminiClient("", time.Now())
		limit, err := client.requestBodySize(1, "pdf", 1)
		if err != nil {
			t.Fatal(err)
		}
		client.decodedByteLimit = 1
		client.requestByteLimit = limit
		client.extractRange = func(context.Context, io.ReadSeeker, int, int, int) ([]byte, error) {
			t.Fatal("original at exact limits was extracted")
			return nil, nil
		}
		chunk, err := client.planPDFChunk(context.Background(), bytes.NewReader([]byte("x")), 1, 1, 0, 1)
		if err != nil {
			t.Fatal(err)
		}
		if chunk.start != 0 || chunk.end != 1 || string(chunk.data) != "x" {
			t.Fatalf("chunk = %+v, want original at exact limits", chunk)
		}
	})

	t.Run("bounded read exceeds decoded limit", func(t *testing.T) {
		client := NewGeminiClient("", time.Now())
		client.decodedByteLimit = 2
		reader := &trackedReadSeeker{ReadSeeker: bytes.NewReader([]byte("raw"))}
		client.extractRange = func(_ context.Context, source io.ReadSeeker, start, end, _ int) ([]byte, error) {
			position, err := source.Seek(0, io.SeekCurrent)
			if err != nil || position != 0 {
				t.Fatalf("fallback source position = %d, %v; want rewind", position, err)
			}
			return []byte("x"), nil
		}
		chunk, err := client.planPDFChunk(context.Background(), reader, 1, 1, 0, 1)
		if err != nil {
			t.Fatal(err)
		}
		if string(chunk.data) != "x" || reader.reads == 0 {
			t.Fatalf("chunk data = %q, source reads = %d; want extracted fallback after bounded read", chunk.data, reader.reads)
		}
	})
}

func TestGeminiPDFSerializedOriginalMissRewindsBeforeExtraction(t *testing.T) {
	client := NewGeminiClient("", time.Now())
	onePageLimit, err := client.requestBodySize(1, "pdf", 1)
	if err != nil {
		t.Fatal(err)
	}
	twentyPageSize, err := client.requestBodySize(1, "pdf", geminiMaxPDFPages)
	if err != nil {
		t.Fatal(err)
	}
	if twentyPageSize <= onePageLimit {
		t.Fatalf("20-page body size = %d, want greater than one-page size %d", twentyPageSize, onePageLimit)
	}
	client.requestByteLimit = onePageLimit
	client.decodedByteLimit = 1
	reader := &trackedReadSeeker{ReadSeeker: bytes.NewReader([]byte("x"))}
	client.extractRange = func(_ context.Context, source io.ReadSeeker, start, end, _ int) ([]byte, error) {
		position, err := source.Seek(0, io.SeekCurrent)
		if err != nil || position != 0 {
			t.Fatalf("fallback source position = %d, %v; want rewind", position, err)
		}
		return []byte("e"), nil
	}
	chunk, err := client.planPDFChunk(context.Background(), reader, 1, geminiMaxPDFPages, 0, geminiMaxPDFPages)
	if err != nil {
		t.Fatal(err)
	}
	if chunk.start != 0 || chunk.end >= geminiMaxPDFPages || string(chunk.data) != "e" {
		t.Fatalf("chunk = %+v, want a strict extracted subset", chunk)
	}
	if reader.reads == 0 {
		t.Fatal("serialized-size gate did not read the original")
	}
}

func TestGeminiPDFOriginalReadCancellationStopsBeforeExtraction(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reader := &cancelOnReadSeeker{ReadSeeker: bytes.NewReader([]byte("original")), cancel: cancel}
	client := NewGeminiClient("", time.Now())
	var extracts int
	client.extractRange = func(context.Context, io.ReadSeeker, int, int, int) ([]byte, error) {
		extracts++
		return []byte("extracted"), nil
	}
	_, err := client.planPDFChunk(ctx, reader, int64(len("original")), 1, 0, 1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("planPDFChunk() error = %v, want cancellation", err)
	}
	if extracts != 0 {
		t.Fatalf("extract calls = %d, want none after cancellation", extracts)
	}
}

func TestGeminiRangeEntryPoints(t *testing.T) {
	input := filepath.Join(t.TempDir(), "scan.pdf")
	if err := os.WriteFile(input, []byte("pdf"), 0o600); err != nil {
		t.Fatal(err)
	}
	var requestCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := atomic.AddInt32(&requestCalls, 1)
		var request geminiRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		data, err := base64.StdEncoding.DecodeString(request.Contents[0].Parts[0].InlineData.Data)
		if err != nil {
			t.Fatal(err)
		}
		wantData := "pdf"
		if call == 2 {
			wantData = "range-data"
		}
		if string(data) != wantData {
			t.Fatalf("request %d data = %q, want %q", call, data, wantData)
		}
		count := *request.GenerationConfig.ResponseJSONSchema.Properties["pages"].MaxItems
		_, _ = w.Write([]byte(geminiResponseJSON("STOP", geminiPagesPayload(count), 2, 2, 0)))
	}))
	defer server.Close()

	client := NewGeminiClient("key", time.Now())
	client.endpoint = server.URL
	client.pageCount = func(context.Context, io.ReadSeeker) (int, error) { return 3, nil }
	var extractCalls int32
	client.extractRange = func(_ context.Context, _ io.ReadSeeker, start, end, _ int) ([]byte, error) {
		atomic.AddInt32(&extractCalls, 1)
		if start != 1 || end != 3 {
			t.Fatalf("extract range = %d-%d, want 1-3", start, end)
		}
		return []byte("range-data"), nil
	}

	full, err := client.PrepareRangeRequest(context.Background(), input, "pdf", 0, 3)
	if err != nil {
		t.Fatalf("PrepareRangeRequest(full) error = %v", err)
	}
	if got := geminiRequestData(t, full.Body); string(got) != "pdf" {
		t.Fatalf("full prepared data = %q, want original", got)
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

	pages, _, err := client.OCRRange(context.Background(), input, "pdf", 0, 3)
	if err != nil {
		t.Fatalf("OCRRange(full) error = %v", err)
	}
	if len(pages) != 3 || pages[0].PageIndex != 0 || pages[2].PageIndex != 2 {
		t.Fatalf("OCRRange(full) pages = %+v, want absolute indexes 0 through 2", pages)
	}
	pages, _, err = client.OCRRange(context.Background(), input, "pdf", 1, 3)
	if err != nil {
		t.Fatalf("OCRRange(partial) error = %v", err)
	}
	if len(pages) != 2 || pages[0].PageIndex != 1 || pages[1].PageIndex != 2 {
		t.Fatalf("OCRRange(partial) pages = %+v, want absolute indexes 1 and 2", pages)
	}
	if got := atomic.LoadInt32(&extractCalls); got != 2 {
		t.Fatalf("extract calls = %d, want only prepared and direct partial ranges", got)
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
	if got, want := pages, []PageResult{
		{PageIndex: 0, Markdown: "[Page: Page]", Model: geminiModel},
		{PageIndex: 1, Markdown: "[Page: Page]", Model: geminiModel},
		{PageIndex: 2, Markdown: "[Page: Page]", Model: geminiModel},
	}; !equalPages(got, want) {
		t.Fatalf("pages = %#v, want %#v", got, want)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("calls = %d, want 2", got)
	}
}

func TestGeminiPDFRecursivelySplits413AndKeepsBilling(t *testing.T) {
	input := filepath.Join(t.TempDir(), "scan.pdf")
	original := []byte("pdf")
	if err := os.WriteFile(input, original, 0o600); err != nil {
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
		if bytes.Equal(rangeData, original) {
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
	var extracts int32
	client.extractRange = func(_ context.Context, _ io.ReadSeeker, start, end, _ int) ([]byte, error) {
		atomic.AddInt32(&extracts, 1)
		if start == 0 && end == 4 {
			t.Fatal("adaptive fallback repeated the complete original range")
		}
		return []byte(strconv.Itoa(start) + "-" + strconv.Itoa(end)), nil
	}
	pages, report, err := client.OCRFile(context.Background(), input, "pdf")
	if err != nil {
		t.Fatal(err)
	}
	wantPages := []PageResult{
		{PageIndex: 0, Markdown: "[Page: Page]", Model: geminiModel},
		{PageIndex: 1, Markdown: "[Page: Page]", Model: geminiModel},
		{PageIndex: 2, Markdown: "[Page: Page]", Model: geminiModel},
		{PageIndex: 3, Markdown: "[Page: Page]", Model: geminiModel},
	}
	if !equalPages(pages, wantPages) {
		t.Fatalf("pages = %#v, want %#v", pages, wantPages)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("calls = %d, want initial request plus two halves", got)
	}
	if got := atomic.LoadInt32(&extracts); got != 2 {
		t.Fatalf("extract calls = %d, want two strict child ranges", got)
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
		data, err := base64.StdEncoding.DecodeString(request.Contents[0].Parts[0].InlineData.Data)
		if err != nil {
			t.Fatal(err)
		}
		if call == 1 {
			if string(data) != "pdf" {
				t.Fatalf("initial data = %q, want original PDF", data)
			}
			response := strings.Replace(geminiResponseJSON("MAX_TOKENS", `{}`, 2, 1, 1), `"index":0,`, "", 1)
			_, _ = w.Write([]byte(response))
			return
		}
		_, _ = w.Write([]byte(geminiResponseJSON("STOP", geminiPagesPayload(count), 1, 1, 0)))
	}))
	defer server.Close()

	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	client := NewGeminiClient("key", at)
	client.endpoint = server.URL
	client.pageCount = func(context.Context, io.ReadSeeker) (int, error) { return 2, nil }
	var extracts int32
	client.extractRange = func(_ context.Context, _ io.ReadSeeker, start, end, _ int) ([]byte, error) {
		atomic.AddInt32(&extracts, 1)
		if start == 0 && end == 2 {
			t.Fatal("MAX_TOKENS fallback repeated the complete original range")
		}
		return []byte(strings.Repeat("x", end-start)), nil
	}
	pages, report, err := client.OCRFile(context.Background(), input, "pdf")
	if err != nil {
		t.Fatal(err)
	}
	wantPages := []PageResult{
		{PageIndex: 0, Markdown: "[Page: Page]", Model: geminiModel},
		{PageIndex: 1, Markdown: "[Page: Page]", Model: geminiModel},
	}
	if !equalPages(pages, wantPages) || atomic.LoadInt32(&calls) != 3 {
		t.Fatalf("pages = %#v, calls = %d; want split with absolute indexes", pages, calls)
	}
	if got := atomic.LoadInt32(&extracts); got != 2 {
		t.Fatalf("extract calls = %d, want two strict child ranges", got)
	}
	if got, want := report.KnownCost, GeminiCost(at, 4, 4); got != want || report.Indeterminate {
		t.Fatalf("report = %+v, want failed original plus both children at cost %d", report, want)
	}
}

func TestGeminiPDFAdaptiveSplitRetainsIndeterminateBilling(t *testing.T) {
	input := filepath.Join(t.TempDir(), "scan.pdf")
	original := []byte("pdf")
	if err := os.WriteFile(input, original, 0o600); err != nil {
		t.Fatal(err)
	}
	var originalCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request geminiRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		data, err := base64.StdEncoding.DecodeString(request.Contents[0].Parts[0].InlineData.Data)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Equal(data, original) {
			if atomic.AddInt32(&originalCalls, 1) == 1 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		count := *request.GenerationConfig.ResponseJSONSchema.Properties["pages"].MaxItems
		_, _ = w.Write([]byte(geminiResponseJSON("STOP", geminiPagesPayload(count), 1, 1, 0)))
	}))
	defer server.Close()

	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	client := NewGeminiClient("key", at)
	client.endpoint = server.URL
	client.sleep = func(context.Context, time.Duration) error { return nil }
	client.randFloat64 = func() float64 { return 0 }
	client.pageCount = func(context.Context, io.ReadSeeker) (int, error) { return 2, nil }
	client.extractRange = func(_ context.Context, _ io.ReadSeeker, start, end, _ int) ([]byte, error) {
		return []byte(strconv.Itoa(start) + "-" + strconv.Itoa(end)), nil
	}
	pages, report, err := client.OCRFile(context.Background(), input, "pdf")
	if err != nil {
		t.Fatal(err)
	}
	wantPages := []PageResult{
		{PageIndex: 0, Markdown: "[Page: Page]", Model: geminiModel},
		{PageIndex: 1, Markdown: "[Page: Page]", Model: geminiModel},
	}
	if !equalPages(pages, wantPages) {
		t.Fatalf("pages = %#v, want %#v", pages, wantPages)
	}
	if got, want := report.KnownCost, GeminiCost(at, 2, 2); got != want || !report.Indeterminate {
		t.Fatalf("report = %+v, want known child cost %d plus indeterminate original", report, want)
	}
}

func TestGeminiSinglePageOriginalAdaptiveFailuresDoNotExtract(t *testing.T) {
	for _, test := range []struct {
		name      string
		response  func(http.ResponseWriter)
		wantCalls int32
	}{
		{
			name: "HTTP 413",
			response: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusRequestEntityTooLarge)
				_, _ = w.Write([]byte(`{"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1}}`))
			},
			wantCalls: 1,
		},
		{
			name: "MAX_TOKENS",
			response: func(w http.ResponseWriter) {
				_, _ = w.Write([]byte(geminiResponseJSON("MAX_TOKENS", `{}`, 2, 1, 1)))
			},
			wantCalls: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := filepath.Join(t.TempDir(), "scan.pdf")
			original := []byte("one-page-original")
			if err := os.WriteFile(input, original, 0o600); err != nil {
				t.Fatal(err)
			}
			var calls int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&calls, 1)
				var request geminiRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatal(err)
				}
				data, err := base64.StdEncoding.DecodeString(request.Contents[0].Parts[0].InlineData.Data)
				if err != nil || !bytes.Equal(data, original) {
					t.Fatalf("request data = %q, %v; want unchanged original", data, err)
				}
				test.response(w)
			}))
			defer server.Close()

			client := NewGeminiClient("key", time.Now())
			client.endpoint = server.URL
			client.pageCount = func(context.Context, io.ReadSeeker) (int, error) { return 1, nil }
			client.extractRange = func(context.Context, io.ReadSeeker, int, int, int) ([]byte, error) {
				t.Fatal("single-page adaptive failure retried normalized bytes")
				return nil, nil
			}
			_, report, err := client.OCRFile(context.Background(), input, "pdf")
			if err == nil {
				t.Fatal("OCRFile() error = nil, want terminal adaptive failure")
			}
			if got := atomic.LoadInt32(&calls); got != test.wantCalls {
				t.Fatalf("calls = %d, want %d unchanged raw request(s)", got, test.wantCalls)
			}
			if report.KnownCost == 0 || report.Indeterminate {
				t.Fatalf("report = %+v, want known failed-attempt billing", report)
			}
		})
	}
}

func TestGeminiOriginalPDFInvalidArgumentDoesNotExtract(t *testing.T) {
	input := filepath.Join(t.TempDir(), "scan.pdf")
	original := []byte("original")
	if err := os.WriteFile(input, original, 0o600); err != nil {
		t.Fatal(err)
	}
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		var request geminiRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		data, err := base64.StdEncoding.DecodeString(request.Contents[0].Parts[0].InlineData.Data)
		if err != nil || !bytes.Equal(data, original) {
			t.Fatalf("request data = %q, %v; want unchanged original", data, err)
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"status":"INVALID_ARGUMENT"},"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1}}`))
	}))
	defer server.Close()

	client := NewGeminiClient("key", time.Now())
	client.endpoint = server.URL
	client.pageCount = func(context.Context, io.ReadSeeker) (int, error) { return 2, nil }
	client.extractRange = func(context.Context, io.ReadSeeker, int, int, int) ([]byte, error) {
		t.Fatal("non-adaptive error retried normalized bytes")
		return nil, nil
	}
	_, report, err := client.OCRFile(context.Background(), input, "pdf")
	if err == nil || !strings.Contains(err.Error(), "API error 400") {
		t.Fatalf("OCRFile() error = %v, want API error 400", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("calls = %d, want one raw request", got)
	}
	if report.KnownCost == 0 || report.Indeterminate {
		t.Fatalf("report = %+v, want known failed-attempt billing", report)
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

type trackedReadSeeker struct {
	io.ReadSeeker
	reads int
}

func (reader *trackedReadSeeker) Read(buffer []byte) (int, error) {
	reader.reads++
	return reader.ReadSeeker.Read(buffer)
}

type cancelOnReadSeeker struct {
	io.ReadSeeker
	cancel context.CancelFunc
}

func (reader *cancelOnReadSeeker) Read(buffer []byte) (int, error) {
	count, err := reader.ReadSeeker.Read(buffer)
	reader.cancel()
	return count, err
}

func geminiRequestData(t *testing.T, body []byte) []byte {
	t.Helper()
	var request geminiRequest
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatalf("decode Gemini request: %v", err)
	}
	data, err := base64.StdEncoding.DecodeString(request.Contents[0].Parts[0].InlineData.Data)
	if err != nil {
		t.Fatalf("decode Gemini inline data: %v", err)
	}
	return data
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
