package ocr

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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

func TestOCRFile_SendsOCR4AnnotatedDataURLRequests(t *testing.T) {
	t.Parallel()

	const encodedInput = "dGVzdA=="

	tests := []struct {
		name         string
		fileName     string
		fileType     string
		documentType string
		documentURL  string
		imageURL     string
	}{
		{
			name:         "pdf",
			fileName:     "input.pdf",
			fileType:     "pdf",
			documentType: "document_url",
			documentURL:  "data:application/pdf;base64," + encodedInput,
		},
		{
			name:         "jpeg",
			fileName:     "input.jpeg",
			fileType:     "jpeg",
			documentType: "image_url",
			imageURL:     "data:image/jpeg;base64," + encodedInput,
		},
		{
			name:         "png",
			fileName:     "input.png",
			fileType:     "png",
			documentType: "image_url",
			imageURL:     "data:image/png;base64," + encodedInput,
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

				if got, want := req.Model, "mistral-ocr-4-0"; got != want {
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
				_, _ = w.Write([]byte(`{"pages":[]}`))
			}))
			defer server.Close()

			input := writeTempOCRFile(t, tt.fileName, []byte("test"))

			client := NewMistralClient("test-key")
			client.endpoint = server.URL

			if _, err := client.OCRFile(context.Background(), input, tt.fileType); err != nil {
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
			"model":"mistral-ocr-4-0",
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

	input := writeTempOCRFile(t, "input.pdf", []byte("test"))

	client := NewMistralClient("test-key")
	client.endpoint = server.URL

	result, err := client.OCRFile(context.Background(), input, "pdf")
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
			"model":"mistral-ocr-4-0",
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

	input := writeTempOCRFile(t, "input.pdf", []byte("test"))

	client := NewMistralClient("test-key")
	client.endpoint = server.URL

	result, err := client.OCRFile(context.Background(), input, "pdf")
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
			"model":"mistral-ocr-4-0",
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

	input := writeTempOCRFile(t, "input.pdf", []byte("test"))

	client := NewMistralClient("test-key")
	client.endpoint = server.URL

	result, err := client.OCRFile(context.Background(), input, "pdf")
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
			"model":"mistral-ocr-4-0",
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

	input := writeTempOCRFile(t, "input.pdf", []byte("test"))

	client := NewMistralClient("test-key")
	client.endpoint = server.URL

	result, err := client.OCRFile(context.Background(), input, "pdf")
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

func writeTempOCRFile(t *testing.T, name string, content []byte) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	return path
}
