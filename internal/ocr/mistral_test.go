package ocr

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pdfutil "github.com/maxim/ringbinder/internal/pdf"
)

func TestMistralPricePerPage(t *testing.T) {
	t.Parallel()

	if got, want := MistralPricePerPage(), 0.005; got != want {
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

func TestRetry_TransportErrorRetried(t *testing.T) {
	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Fatalf("response writer does not support hijacking")
		}
		conn, _, err := hijacker.Hijack()
		if err != nil {
			t.Fatalf("hijack connection: %v", err)
		}
		_ = conn.Close()
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

func TestRetry_TransportErrorRecovery(t *testing.T) {
	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&requests, 1)
		if count <= 2 {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Fatalf("response writer does not support hijacking")
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Fatalf("hijack connection: %v", err)
			}
			_ = conn.Close()
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

func TestRetry_TransportErrorContextCancelled(t *testing.T) {
	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"pages":[]}`))
	}))
	defer server.Close()

	client := NewMistralClient("test-key")
	client.endpoint = server.URL
	client.sleep = func(ctx context.Context, d time.Duration) error { return nil }
	client.randFloat64 = func() float64 { return 0 }

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := client.doWithRetry(ctx, mistralRequest{
		Model: mistralModel,
		Document: mistralDocument{
			Type:        "document_url",
			DocumentURL: "data:application/pdf;base64,AA==",
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("doWithRetry() error = %v, want context.Canceled", err)
	}
	if got := atomic.LoadInt32(&requests); got != 0 {
		t.Fatalf("request attempts = %d, want 0", got)
	}
}

func TestOCRFile_SendsOCR41AnnotatedDataURLRequests(t *testing.T) {
	t.Parallel()

	pdfInput := testPDF("page 0")
	imageInput := []byte("test")
	tests := []struct {
		name         string
		fileName     string
		fileType     string
		content      []byte
		documentType string
		documentURL  string
		imageURL     string
	}{
		{
			name:         "pdf",
			fileName:     "input.pdf",
			fileType:     "pdf",
			content:      pdfInput,
			documentType: "document_url",
			documentURL:  "data:application/pdf;base64," + base64.StdEncoding.EncodeToString(pdfInput),
		},
		{
			name:         "jpeg",
			fileName:     "input.jpeg",
			fileType:     "jpeg",
			content:      imageInput,
			documentType: "image_url",
			imageURL:     "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(imageInput),
		},
		{
			name:         "png",
			fileName:     "input.png",
			fileType:     "png",
			content:      imageInput,
			documentType: "image_url",
			imageURL:     "data:image/png;base64," + base64.StdEncoding.EncodeToString(imageInput),
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer r.Body.Close()

				var req mistralRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Fatalf("decode request: %v", err)
				}

				if got, want := req.Model, "mistral-ocr-4-1"; got != want {
					t.Fatalf("model = %q, want %q", got, want)
				}
				if got, want := req.Document.Type, tt.documentType; got != want {
					t.Fatalf("document.type = %q, want %q", got, want)
				}
				if got, want := req.Document.DocumentURL, tt.documentURL; got != want {
					t.Fatalf("document.document_url = %q, want %q", got, want)
				}
				if got, want := req.Document.ImageURL, tt.imageURL; got != want {
					t.Fatalf("document.image_url = %q, want %q", got, want)
				}

				format := req.BBoxAnnotationFormat
				if got, want := format.Type, "json_schema"; got != want {
					t.Fatalf("bbox_annotation_format.type = %q, want %q", got, want)
				}
				if got, want := format.JSONSchema.Name, "image_annotation"; got != want {
					t.Fatalf("bbox_annotation_format.json_schema.name = %q, want %q", got, want)
				}
				if !format.JSONSchema.Strict {
					t.Fatalf("bbox_annotation_format.json_schema.strict = false, want true")
				}

				schema := format.JSONSchema.Schema
				if got, want := schema.Type, "object"; got != want {
					t.Fatalf("schema.type = %q, want %q", got, want)
				}
				if _, ok := schema.Properties["image_type"]; !ok {
					t.Fatalf("schema missing image_type property")
				}
				if _, ok := schema.Properties["description"]; !ok {
					t.Fatalf("schema missing description property")
				}
				if schema.AdditionalProperties {
					t.Fatalf("schema.additionalProperties = true, want false")
				}

				required := make(map[string]bool, len(schema.Required))
				for _, name := range schema.Required {
					required[name] = true
				}
				if !required["image_type"] || !required["description"] {
					t.Fatalf("schema.required = %v, want image_type and description", schema.Required)
				}

				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"pages":[{"index":0,"markdown":"page"}]}`))
			}))
			defer server.Close()

			input := writeTempOCRFile(t, tt.fileName, tt.content)

			client := NewMistralClient("test-key")
			client.endpoint = server.URL

			if _, _, err := client.OCRFile(context.Background(), input, tt.fileType); err != nil {
				t.Fatalf("OCRFile() error = %v", err)
			}
		})
	}
}

func TestOCRFile_ParsesImageAnnotations(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"model":"mistral-ocr-4-1",
			"usage_info":{"pages_processed":1,"doc_size_bytes":4},
			"pages":[
				{
					"index":0,
					"markdown":"Page text",
					"dimensions":{"dpi":200,"width":1700,"height":2200},
					"images":[
						{
							"id":"img-0.jpeg",
							"top_left_x":100,
							"top_left_y":50,
							"bottom_right_x":400,
							"bottom_right_y":300,
							"image_annotation":{
								"image_type":"scatter plot",
								"description":"A scatter plot comparing model performance vs cost"
							}
						}
					]
				}
			]
		}`))
	}))
	defer server.Close()

	input := writeTempOCRFile(t, "input.pdf", testPDF("page 0"))

	client := NewMistralClient("test-key")
	client.endpoint = server.URL

	result, _, err := client.OCRFile(context.Background(), input, "pdf")
	if err != nil {
		t.Fatalf("OCRFile() error = %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}

	if got, want := result[0].Markdown, "Page text\n\n[Image: scatter plot — A scatter plot comparing model performance vs cost]"; got != want {
		t.Fatalf("markdown = %q, want %q", got, want)
	}
}

func TestOCRFile_ParsesStringImageAnnotation(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"model":"mistral-ocr-4-1",
			"usage_info":{"pages_processed":1,"doc_size_bytes":4},
			"pages":[
				{
					"index":0,
					"markdown":"Page text",
					"dimensions":{"dpi":200,"width":1700,"height":2200},
					"images":[
						{
							"id":"img-0.jpeg",
							"top_left_x":100,
							"top_left_y":50,
							"bottom_right_x":400,
							"bottom_right_y":300,
							"image_annotation":"A scatter plot comparing model performance vs cost"
						}
					]
				}
			]
		}`))
	}))
	defer server.Close()

	input := writeTempOCRFile(t, "input.pdf", testPDF("page 0"))

	client := NewMistralClient("test-key")
	client.endpoint = server.URL

	result, _, err := client.OCRFile(context.Background(), input, "pdf")
	if err != nil {
		t.Fatalf("OCRFile() error = %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}

	if got, want := result[0].Markdown, "Page text\n\n[Image: image — A scatter plot comparing model performance vs cost]"; got != want {
		t.Fatalf("markdown = %q, want %q", got, want)
	}
}

func TestOCRFile_ParsesEscapedJSONStringImageAnnotation(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"model":"mistral-ocr-4-1",
			"usage_info":{"pages_processed":1,"doc_size_bytes":4},
			"pages":[
				{
					"index":0,
					"markdown":"Page text",
					"dimensions":{"dpi":200,"width":1700,"height":2200},
					"images":[
						{
							"id":"img-0.jpeg",
							"top_left_x":100,
							"top_left_y":50,
							"bottom_right_x":400,
							"bottom_right_y":300,
							"image_annotation":"{\"image_type\":\"diagram\",\"description\":\"A diagram showing how scanned documents flow into searchable Markdown\"}"
						}
					],
					"tables":[],
					"hyperlinks":[],
					"header":"",
					"footer":"",
					"confidence_scores":{"page":0.99},
					"blocks":[{"type":"text","text":"Page text"}]
				}
			]
		}`))
	}))
	defer server.Close()

	input := writeTempOCRFile(t, "input.pdf", testPDF("page 0"))

	client := NewMistralClient("test-key")
	client.endpoint = server.URL

	result, _, err := client.OCRFile(context.Background(), input, "pdf")
	if err != nil {
		t.Fatalf("OCRFile() error = %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}

	want := "Page text\n\n[Image: diagram — A diagram showing how scanned documents flow into searchable Markdown]"
	if got := result[0].Markdown; got != want {
		t.Fatalf("markdown = %q, want %q", got, want)
	}
}

func TestOCRFile_NoImages(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"model":"mistral-ocr-4-1",
			"usage_info":{"pages_processed":1,"doc_size_bytes":4},
			"pages":[
				{
					"index":0,
					"markdown":"Page text",
					"dimensions":{"dpi":200,"width":1700,"height":2200}
				}
			]
		}`))
	}))
	defer server.Close()

	input := writeTempOCRFile(t, "input.pdf", testPDF("page 0"))

	client := NewMistralClient("test-key")
	client.endpoint = server.URL

	result, _, err := client.OCRFile(context.Background(), input, "pdf")
	if err != nil {
		t.Fatalf("OCRFile() error = %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("len(result) = %d, want 1", len(result))
	}
	if got, want := result[0].Markdown, "Page text"; got != want {
		t.Fatalf("markdown = %q, want %q", got, want)
	}
}

func TestMistralProductionLimits(t *testing.T) {
	t.Parallel()

	client := NewMistralClient("test-key")
	if got, want := client.requestByteLimit, 45*1024*1024; got != want {
		t.Fatalf("request limit = %d, want %d", got, want)
	}
	if got, want := client.decodedByteLimit, 45*1024*1024; got != want {
		t.Fatalf("decoded limit = %d, want %d", got, want)
	}
	if got, want := client.pageLimit, 1000; got != want {
		t.Fatalf("page limit = %d, want %d", got, want)
	}
	if got, want := client.httpClient.Timeout, 15*time.Minute; got != want {
		t.Fatalf("HTTP timeout = %v, want %v", got, want)
	}
}

func TestRequestBodySizeMatchesCanonicalBody(t *testing.T) {
	t.Parallel()

	client := NewMistralClient("test-key")
	for _, fileType := range []string{"pdf", "jpeg", "png"} {
		for _, size := range []int{0, 1, 2, 3, 4, 1024} {
			data := bytes.Repeat([]byte{0x5a}, size)
			body, err := client.buildRequestBody(data, fileType)
			if err != nil {
				t.Fatalf("buildRequestBody(%s, %d) error = %v", fileType, size, err)
			}
			calculated, err := client.requestBodySize(size, fileType)
			if err != nil {
				t.Fatalf("requestBodySize(%s, %d) error = %v", fileType, size, err)
			}
			if len(body) != calculated {
				t.Fatalf("%s body size = %d, calculated %d", fileType, len(body), calculated)
			}
		}
	}
}

func TestRequestAndDecodedLimitBoundaries(t *testing.T) {
	t.Parallel()

	client := NewMistralClient("test-key")
	capAtThree, err := client.requestBodySize(3, "pdf")
	if err != nil {
		t.Fatalf("requestBodySize() error = %v", err)
	}
	client.requestByteLimit = capAtThree
	client.decodedByteLimit = 100

	limit, err := client.effectiveDecodedLimit("pdf")
	if err != nil {
		t.Fatalf("effectiveDecodedLimit() error = %v", err)
	}
	if limit != 3 {
		t.Fatalf("effective decoded limit = %d, want 3", limit)
	}
	if _, err := client.buildRequestBody([]byte{1, 2, 3}, "pdf"); err != nil {
		t.Fatalf("build at cap error = %v", err)
	}
	if _, err := client.buildRequestBody([]byte{1, 2, 3, 4}, "pdf"); err == nil {
		t.Fatalf("build over cap error = nil")
	}

	client.requestByteLimit = maxRequestBytes
	client.decodedByteLimit = 10
	limit, err = client.effectiveDecodedLimit("pdf")
	if err != nil {
		t.Fatalf("effectiveDecodedLimit() error = %v", err)
	}
	if limit != 10 {
		t.Fatalf("decoded guard limit = %d, want 10", limit)
	}
}

func TestDecodeResultsValidatesAndOrdersIndexes(t *testing.T) {
	t.Parallel()

	results, err := decodeResults([]byte(`{"pages":[{"index":2,"markdown":"two"},{"index":0,"markdown":"zero"},{"index":1,"markdown":"one"}]}`), 3)
	if err != nil {
		t.Fatalf("decodeResults() error = %v", err)
	}
	for i, result := range results {
		if result.PageIndex != i || result.Markdown != []string{"zero", "one", "two"}[i] {
			t.Fatalf("result[%d] = %+v", i, result)
		}
	}

	tests := []struct {
		name string
		body string
	}{
		{name: "null page", body: `{"pages":[null]}`},
		{name: "missing index", body: `{"pages":[{"markdown":"x"}]}`},
		{name: "missing page", body: `{"pages":[]}`},
		{name: "duplicate", body: `{"pages":[{"index":0},{"index":0}]}`},
		{name: "negative", body: `{"pages":[{"index":-1}]}`},
		{name: "out of range", body: `{"pages":[{"index":1}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := decodeResults([]byte(tt.body), 1)
			if err == nil {
				t.Fatalf("decodeResults() error = nil")
			}
			if !strings.Contains(err.Error(), "expected [0]") || !strings.Contains(err.Error(), "actual") {
				t.Fatalf("error = %q, want expected and actual index sets", err)
			}
		})
	}
}

func TestPlanPDFChunkFindsLargestMeasuredFit(t *testing.T) {
	t.Parallel()

	client := NewMistralClient("test-key")
	client.decodedByteLimit = 10
	var candidates []int
	client.extractRange = func(_ context.Context, _ io.ReadSeeker, start, end, maxBytes int) ([]byte, error) {
		if maxBytes != 10 {
			t.Fatalf("extract maxBytes = %d, want 10", maxBytes)
		}
		candidates = append(candidates, end)
		size := (end - start) * 3
		if size > maxBytes {
			return nil, pdfutil.ErrRangeTooLarge
		}
		return bytes.Repeat([]byte{1}, size), nil
	}

	chunk, err := client.planPDFChunk(context.Background(), bytes.NewReader([]byte("source")), 100, 10, 0, 10)
	if err != nil {
		t.Fatalf("planPDFChunk() error = %v", err)
	}
	if chunk.start != 0 || chunk.end != 3 || len(chunk.data) != 9 {
		t.Fatalf("chunk = [%d,%d) %d bytes, want [0,3) 9 bytes", chunk.start, chunk.end, len(chunk.data))
	}
	if len(candidates) == 0 || candidates[0] != 1 {
		t.Fatalf("candidate sequence = %v, want average-seeded first range ending at 1", candidates)
	}
}

func TestOCRFileRejectsLocallyOversizedSinglePDFPageBeforeHTTP(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()

	input := writeTempOCRFile(t, "oversized-page.pdf", []byte("source"))
	client := NewMistralClient("test-key")
	client.endpoint = server.URL
	client.pageLimit = 1
	client.pageCount = func(context.Context, io.ReadSeeker) (int, error) { return 2, nil }
	client.extractRange = func(context.Context, io.ReadSeeker, int, int, int) ([]byte, error) {
		return nil, pdfutil.ErrRangeTooLarge
	}

	results, _, err := client.OCRFile(context.Background(), input, "pdf")
	if err == nil || !strings.Contains(err.Error(), "source page 1 exceeds") {
		t.Fatalf("OCRFile() error = %v, want page-specific local size error", err)
	}
	if results != nil {
		t.Fatalf("results = %v, want nil", results)
	}
	if requests.Load() != 0 {
		t.Fatalf("HTTP requests = %d, want 0", requests.Load())
	}
}

func TestOCRFileStopsAfterLateChunkValidationFailure(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, end := requestRange(t, r)
		call := requests.Add(1)
		if call == 2 {
			_, _ = w.Write([]byte(`{"pages":[null]}`))
			return
		}
		writeRangeResponse(w, start, end)
	}))
	defer server.Close()

	input := writeTempOCRFile(t, "late-failure.pdf", []byte("source"))
	client := NewMistralClient("test-key")
	client.endpoint = server.URL
	client.pageLimit = 2
	client.pageCount = func(context.Context, io.ReadSeeker) (int, error) { return 5, nil }
	client.extractRange = func(_ context.Context, _ io.ReadSeeker, start, end, _ int) ([]byte, error) {
		return []byte{byte(start), byte(end)}, nil
	}

	results, _, err := client.OCRFile(context.Background(), input, "pdf")
	if err == nil {
		t.Fatalf("OCRFile() error = nil")
	}
	wantResults := []PageResult{
		{PageIndex: 0, Markdown: "source-0", Model: mistralModel},
		{PageIndex: 1, Markdown: "source-1", Model: mistralModel},
	}
	if !equalPages(results, wantResults) {
		t.Fatalf("results = %v, want retained successful chunk %v", results, wantResults)
	}
	for _, want := range []string{"late-failure.pdf", "source pages 3-4", "expected [0 1]", "actual [null]"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want contains %q", err, want)
		}
	}
	if requests.Load() != 2 {
		t.Fatalf("HTTP requests = %d, want stop before third chunk", requests.Load())
	}
}

func TestOCRFileTerminalSinglePage413(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = w.Write([]byte(`{"error":"too large"}`))
	}))
	defer server.Close()

	input := writeTempOCRFile(t, "single.pdf", []byte("source"))
	client := NewMistralClient("test-key")
	client.endpoint = server.URL
	client.pageCount = func(context.Context, io.ReadSeeker) (int, error) { return 1, nil }

	results, _, err := client.OCRFile(context.Background(), input, "pdf")
	if err == nil || !strings.Contains(err.Error(), "source page 1 was rejected as too large") || !strings.Contains(err.Error(), "API error 413") {
		t.Fatalf("OCRFile() error = %v, want terminal page-specific 413", err)
	}
	if results != nil {
		t.Fatalf("results = %v, want nil", results)
	}
	if requests.Load() != 1 {
		t.Fatalf("HTTP requests = %d, want 1", requests.Load())
	}
}

func TestOCRFileChunksPDFSequentiallyAndOffsetsPages(t *testing.T) {
	var mu sync.Mutex
	var sent [][2]int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, end := requestRange(t, r)
		mu.Lock()
		sent = append(sent, [2]int{start, end})
		mu.Unlock()
		if end-start == 1 {
			writeRangeResponse(w, start, end)
			return
		}
		fmt.Fprintf(w, `{"pages":[{"index":%d,"markdown":"source-%d"},{"index":0,"markdown":"source-%d"}]}`, end-start-1, end-1, start)
	}))
	defer server.Close()

	input := writeTempOCRFile(t, "oversized.pdf", []byte("source"))
	client := NewMistralClient("test-key")
	client.endpoint = server.URL
	client.pageLimit = 2
	client.pageCount = func(context.Context, io.ReadSeeker) (int, error) { return 5, nil }
	client.extractRange = func(_ context.Context, _ io.ReadSeeker, start, end, _ int) ([]byte, error) {
		return []byte{byte(start), byte(end)}, nil
	}

	results, _, err := client.OCRFile(context.Background(), input, "pdf")
	if err != nil {
		t.Fatalf("OCRFile() error = %v", err)
	}
	if got, want := sent, [][2]int{{0, 2}, {2, 4}, {4, 5}}; !rangesEqual(got, want) {
		t.Fatalf("sent ranges = %v, want %v", got, want)
	}
	if len(results) != 5 {
		t.Fatalf("results = %d, want 5", len(results))
	}
	for i, result := range results {
		if result.PageIndex != i || result.Markdown != fmt.Sprintf("source-%d", i) {
			t.Fatalf("result[%d] = %+v", i, result)
		}
	}
}

func TestOCRFileRecursivelyBisectsOnly413(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data := requestData(t, r)
		if string(data) == "source" {
			mu.Lock()
			calls = append(calls, "direct")
			mu.Unlock()
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = w.Write([]byte(`{"error":"too large"}`))
			return
		}

		start, end := int(data[0]), int(data[1])
		mu.Lock()
		calls = append(calls, fmt.Sprintf("%d-%d", start, end))
		mu.Unlock()
		if start == 0 && end == 2 {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = w.Write([]byte(`{"error":"too large"}`))
			return
		}
		writeRangeResponse(w, start, end)
	}))
	defer server.Close()

	input := writeTempOCRFile(t, "fallback.pdf", []byte("source"))
	client := NewMistralClient("test-key")
	client.endpoint = server.URL
	client.pageCount = func(context.Context, io.ReadSeeker) (int, error) { return 4, nil }
	client.extractRange = func(_ context.Context, _ io.ReadSeeker, start, end, _ int) ([]byte, error) {
		return []byte{byte(start), byte(end)}, nil
	}

	results, _, err := client.OCRFile(context.Background(), input, "pdf")
	if err != nil {
		t.Fatalf("OCRFile() error = %v", err)
	}
	wantCalls := []string{"direct", "0-2", "0-1", "1-2", "2-4"}
	if strings.Join(calls, ",") != strings.Join(wantCalls, ",") {
		t.Fatalf("calls = %v, want %v", calls, wantCalls)
	}
	if len(results) != 4 {
		t.Fatalf("results = %d, want 4", len(results))
	}
	for i, result := range results {
		if result.PageIndex != i {
			t.Fatalf("result[%d].PageIndex = %d", i, result.PageIndex)
		}
	}
}

func TestOCRFileDoesNotSplitGeneric422(t *testing.T) {
	var extracts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"error":"document size is too large"}`))
	}))
	defer server.Close()

	input := writeTempOCRFile(t, "input.pdf", []byte("source"))
	client := NewMistralClient("test-key")
	client.endpoint = server.URL
	client.pageCount = func(context.Context, io.ReadSeeker) (int, error) { return 2, nil }
	client.extractRange = func(_ context.Context, _ io.ReadSeeker, start, end, max int) ([]byte, error) {
		extracts.Add(1)
		return []byte{byte(start), byte(end)}, nil
	}

	_, _, err := client.OCRFile(context.Background(), input, "pdf")
	if err == nil || !strings.Contains(err.Error(), "API error 422") {
		t.Fatalf("OCRFile() error = %v, want API error 422", err)
	}
	if extracts.Load() != 0 {
		t.Fatalf("extract calls = %d, want 0", extracts.Load())
	}
}

func TestOCRFileRejectsOversizedImageBeforeHTTP(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()

	input := writeTempOCRFile(t, "input.png", []byte("too large"))
	client := NewMistralClient("test-key")
	client.endpoint = server.URL
	framing, err := client.requestBodySize(0, "png")
	if err != nil {
		t.Fatalf("requestBodySize() error = %v", err)
	}
	client.requestByteLimit = framing + base64.StdEncoding.EncodedLen(1)

	_, _, err = client.OCRFile(context.Background(), input, "png")
	if err == nil || !strings.Contains(err.Error(), "oversized png image cannot be transformed") {
		t.Fatalf("OCRFile() error = %v", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("HTTP requests = %d, want 0", requests.Load())
	}
}

func TestOCRFileCallerDeadlineEndsBeforeClientTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		writeRangeResponse(w, 0, 1)
	}))
	defer server.Close()

	input := writeTempOCRFile(t, "input.png", []byte("image"))
	client := NewMistralClient("test-key")
	client.endpoint = server.URL
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, _, err := client.OCRFile(ctx, input, "png")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("OCRFile() error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("OCRFile() took %v, want caller deadline precedence", elapsed)
	}
}

func requestData(t *testing.T, r *http.Request) []byte {
	t.Helper()
	defer r.Body.Close()
	var req mistralRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	url := req.Document.DocumentURL
	encoded := strings.TrimPrefix(url, "data:application/pdf;base64,")
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode data URL: %v", err)
	}
	return data
}

func requestRange(t *testing.T, r *http.Request) (int, int) {
	t.Helper()
	data := requestData(t, r)
	if len(data) != 2 {
		t.Fatalf("request chunk data = %v, want two range bytes", data)
	}
	return int(data[0]), int(data[1])
}

func writeRangeResponse(w http.ResponseWriter, start, end int) {
	pages := make([]string, end-start)
	for i := range pages {
		pages[i] = fmt.Sprintf(`{"index":%d,"markdown":"source-%d"}`, i, start+i)
	}
	fmt.Fprintf(w, `{"pages":[%s]}`, strings.Join(pages, ","))
}

func rangesEqual(a, b [][2]int) bool {
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

func writeTempOCRFile(t *testing.T, name string, content []byte) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	return path
}

func testPDF(labels ...string) []byte {
	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")
	offsets := []int{0}
	writeObject := func(number int, body string) {
		offsets = append(offsets, out.Len())
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", number, body)
	}

	kids := make([]string, len(labels))
	for i := range labels {
		kids[i] = fmt.Sprintf("%d 0 R", 4+i*2)
	}
	writeObject(1, "<< /Type /Catalog /Pages 2 0 R >>")
	writeObject(2, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(labels)))
	writeObject(3, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	for i, label := range labels {
		pageObject := 4 + i*2
		contentObject := pageObject + 1
		writeObject(pageObject, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 3 0 R >> >> /Contents %d 0 R >>", contentObject))
		stream := fmt.Sprintf("BT /F1 12 Tf 72 720 Td (%s) Tj ET", label)
		writeObject(contentObject, fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream))
	}

	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n", len(offsets))
	out.WriteString("0000000000 65535 f \n")
	for _, offset := range offsets[1:] {
		fmt.Fprintf(&out, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets), xref)
	return out.Bytes()
}
