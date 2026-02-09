package ocr

import (
	"context"
	"encoding/json"
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

func TestOCRFile_SendsBBoxAnnotationFormat(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()

		var req mistralRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}

		if req.BBoxAnnotationFormat.Type != "json_schema" {
			t.Fatalf("bbox_annotation_format.type = %q, want %q", req.BBoxAnnotationFormat.Type, "json_schema")
		}

		schema := req.BBoxAnnotationFormat.JSONSchema.Schema
		if _, ok := schema.Properties["image_type"]; !ok {
			t.Fatalf("schema missing image_type property")
		}
		if _, ok := schema.Properties["description"]; !ok {
			t.Fatalf("schema missing description property")
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"pages":[]}`))
	}))
	defer server.Close()

	input := writeTempOCRFile(t, "input.pdf", []byte("test"))

	client := NewMistralClient("test-key")
	client.endpoint = server.URL

	if _, err := client.OCRFile(context.Background(), input, "pdf"); err != nil {
		t.Fatalf("OCRFile() error = %v", err)
	}
}

func TestOCRFile_ParsesImageAnnotations(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
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

	var annotations pageAnnotations
	if err := json.Unmarshal(result[0].Annotations, &annotations); err != nil {
		t.Fatalf("unmarshal annotations: %v", err)
	}
	if annotations.Dimensions == nil {
		t.Fatalf("dimensions = nil, want non-nil")
	}
	if got, want := annotations.Dimensions.DPI, 200; got != want {
		t.Fatalf("dimensions.dpi = %d, want %d", got, want)
	}
	if got, want := len(annotations.Images), 1; got != want {
		t.Fatalf("len(images) = %d, want %d", got, want)
	}

	image := annotations.Images[0]
	if got, want := image.ID, "img-0.jpeg"; got != want {
		t.Fatalf("image id = %q, want %q", got, want)
	}
	if got, want := image.ImageType, "scatter plot"; got != want {
		t.Fatalf("image_type = %q, want %q", got, want)
	}
	if got, want := image.Description, "A scatter plot comparing model performance vs cost"; got != want {
		t.Fatalf("description = %q, want %q", got, want)
	}
	if got, want := image.BoundingBox.TopLeftX, 100; got != want {
		t.Fatalf("top_left_x = %d, want %d", got, want)
	}
	if got, want := image.BoundingBox.TopLeftY, 50; got != want {
		t.Fatalf("top_left_y = %d, want %d", got, want)
	}
	if got, want := image.BoundingBox.BottomRightX, 400; got != want {
		t.Fatalf("bottom_right_x = %d, want %d", got, want)
	}
	if got, want := image.BoundingBox.BottomRightY, 300; got != want {
		t.Fatalf("bottom_right_y = %d, want %d", got, want)
	}
}

func TestOCRFile_ParsesStringImageAnnotation(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
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

	var annotations pageAnnotations
	if err := json.Unmarshal(result[0].Annotations, &annotations); err != nil {
		t.Fatalf("unmarshal annotations: %v", err)
	}
	if got, want := len(annotations.Images), 1; got != want {
		t.Fatalf("len(images) = %d, want %d", got, want)
	}
	if got, want := annotations.Images[0].ImageType, ""; got != want {
		t.Fatalf("image_type = %q, want %q", got, want)
	}
	if got, want := annotations.Images[0].Description, "A scatter plot comparing model performance vs cost"; got != want {
		t.Fatalf("description = %q, want %q", got, want)
	}
}

func TestOCRFile_NoImages(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
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

	var annotations pageAnnotations
	if err := json.Unmarshal(result[0].Annotations, &annotations); err != nil {
		t.Fatalf("unmarshal annotations: %v", err)
	}
	if annotations.Dimensions == nil {
		t.Fatalf("dimensions = nil, want non-nil")
	}
	if got, want := annotations.Dimensions.Width, 1700; got != want {
		t.Fatalf("width = %d, want %d", got, want)
	}
	if len(annotations.Images) != 0 {
		t.Fatalf("len(images) = %d, want 0", len(annotations.Images))
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
