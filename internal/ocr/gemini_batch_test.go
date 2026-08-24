package ocr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pdfutil "github.com/maxim/ringbinder/internal/pdf"
)

func TestWalkGeminiFileRequestsUsesOriginalCompletePDF(t *testing.T) {
	original := []byte("original batch PDF bytes")
	path := filepath.Join(t.TempDir(), "twenty-pages.pdf")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	client := NewGeminiClient("", time.Now().UTC())
	client.pageCount = func(context.Context, io.ReadSeeker) (int, error) { return geminiMaxPDFPages, nil }
	client.extractRange = func(context.Context, io.ReadSeeker, int, int, int) ([]byte, error) {
		t.Fatal("eligible fresh batch PDF was extracted")
		return nil, nil
	}
	var requests []GeminiPreparedRequest
	err := client.WalkFileRequests(
		context.Background(), path, "pdf",
		func(request GeminiPreparedRequest) error {
			requests = append(requests, request)
			return nil
		},
		func(sizeErr *GeminiRangeSizeError) error {
			t.Fatalf("eligible PDF rejected: %v", sizeErr)
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 || requests[0].PageStart != 0 || requests[0].PageEnd != geminiMaxPDFPages {
		t.Fatalf("requests = %+v, want one complete-document range", requests)
	}
	if got := geminiRequestData(t, requests[0].Body); !bytes.Equal(got, original) {
		t.Fatalf("inline PDF = %q, want original bytes %q", got, original)
	}
}

func TestWalkGeminiFileRequestsContinuesAfterOversizedMiddlePage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "three-pages.pdf")
	if err := os.WriteFile(path, []byte("pdf source"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	client := NewGeminiClient("", time.Now().UTC())
	client.pageLimit = 1
	client.pageCount = func(context.Context, io.ReadSeeker) (int, error) { return 3, nil }
	client.extractRange = func(_ context.Context, _ io.ReadSeeker, start, end, _ int) ([]byte, error) {
		if start == 1 {
			return nil, pdfutil.ErrRangeTooLarge
		}
		return []byte(fmt.Sprintf("page-%d-%d", start, end)), nil
	}
	var yielded, rejected []string
	err := client.WalkFileRequests(
		context.Background(), path, "pdf",
		func(request GeminiPreparedRequest) error {
			yielded = append(yielded, fmt.Sprintf("%d-%d", request.PageStart, request.PageEnd))
			return nil
		},
		func(sizeErr *GeminiRangeSizeError) error {
			rejected = append(rejected, fmt.Sprintf("%d-%d", sizeErr.PageStart, sizeErr.PageEnd))
			return nil
		},
	)
	if err != nil {
		t.Fatalf("WalkFileRequests() error = %v", err)
	}
	if got := strings.Join(yielded, ","); got != "0-1,2-3" {
		t.Fatalf("yielded = %s, want valid prefix and suffix", got)
	}
	if got := strings.Join(rejected, ","); got != "1-2" {
		t.Fatalf("rejected = %s, want only oversized middle page", got)
	}
}

func TestWalkGeminiFileRequestsContinuesAfterSerializedOversizedMiddlePage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "three-pages.pdf")
	if err := os.WriteFile(path, []byte("pdf source"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	client := NewGeminiClient("", time.Now().UTC())
	client.pageLimit = 1
	client.pageCount = func(context.Context, io.ReadSeeker) (int, error) { return 3, nil }
	limit, err := client.requestBodySize(1, "pdf", 1)
	if err != nil {
		t.Fatalf("requestBodySize() error = %v", err)
	}
	client.requestByteLimit = limit
	client.decodedByteLimit = 32
	client.extractRange = func(_ context.Context, _ io.ReadSeeker, start, end, max int) ([]byte, error) {
		if start == 1 {
			return bytes.Repeat([]byte("x"), max+1), nil
		}
		return []byte("x"), nil
	}
	var yielded, rejected []string
	err = client.WalkFileRequests(
		context.Background(), path, "pdf",
		func(request GeminiPreparedRequest) error {
			yielded = append(yielded, fmt.Sprintf("%d-%d", request.PageStart, request.PageEnd))
			return nil
		},
		func(sizeErr *GeminiRangeSizeError) error {
			rejected = append(rejected, fmt.Sprintf("%d-%d", sizeErr.PageStart, sizeErr.PageEnd))
			return nil
		},
	)
	if err != nil {
		t.Fatalf("WalkFileRequests() error = %v", err)
	}
	if got := strings.Join(yielded, ","); got != "0-1,2-3" {
		t.Fatalf("yielded = %s, want valid prefix and suffix", got)
	}
	if got := strings.Join(rejected, ","); got != "1-2" {
		t.Fatalf("rejected = %s, want only serialized oversized middle page", got)
	}
}

func TestGeminiBatchPricesAreHalfDirectPrices(t *testing.T) {
	for _, at := range []time.Time{
		time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
	} {
		direct := GeminiPrices(at)
		batch := GeminiBatchPrices(at)
		if batch.Input != direct.Input/2 || batch.Output != direct.Output/2 {
			t.Fatalf("GeminiBatchPrices(%v) = %+v, direct %+v", at, batch, direct)
		}
	}
}

func TestGeminiBatchUploadUsesResumableProtocol(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/upload/v1beta/files":
			if got := r.Header.Get("x-goog-api-key"); got != "test-key" {
				t.Errorf("API key = %q, want test-key", got)
			}
			if got := r.Header.Get("X-Goog-Upload-Protocol"); got != "resumable" {
				t.Errorf("upload protocol = %q", got)
			}
			if got := r.Header.Get("X-Goog-Upload-Header-Content-Type"); got != "application/jsonl" {
				t.Errorf("upload content type = %q", got)
			}
			w.Header().Set("X-Goog-Upload-URL", server.URL+"/upload-session")
			w.WriteHeader(http.StatusOK)
		case "/upload-session":
			if got := r.Header.Get("X-Goog-Upload-Command"); got != "upload, finalize" {
				t.Errorf("upload command = %q", got)
			}
			if got, want := r.ContentLength, int64(len("{\"key\":1}\n")); got != want {
				t.Errorf("upload ContentLength = %d, want %d", got, want)
			}
			body, _ := io.ReadAll(r.Body)
			if got, want := string(body), "{\"key\":1}\n"; got != want {
				t.Errorf("uploaded body = %q, want %q", got, want)
			}
			fmt.Fprint(w, `{"file":{"name":"files/input-1","displayName":"display"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	transport := NewGeminiBatchTransport("test-key")
	transport.apiBase = server.URL
	transport.uploadBase = server.URL
	source := bytes.NewReader([]byte("{\"key\":1}\n"))
	file, err := transport.UploadJSONL(context.Background(), "display", source, int64(source.Len()))
	if err != nil {
		t.Fatalf("UploadJSONL() error = %v", err)
	}
	if file.Name != "files/input-1" {
		t.Fatalf("file name = %q, want files/input-1", file.Name)
	}
}

func TestGeminiBatchCreateAndGetDecodeOperationEnvelope(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-goog-api-key"); got != "test-key" {
			t.Errorf("API key = %q, want test-key", got)
		}
		switch {
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, ":batchGenerateContent"):
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode create request: %v", err)
			}
			batch := body["batch"].(map[string]any)
			if batch["displayName"] != "display" {
				t.Errorf("displayName = %v", batch["displayName"])
			}
			fmt.Fprint(w, `{"name":"batches/17","metadata":{"displayName":"display","state":"JOB_STATE_PENDING"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1beta/batches/17":
			fmt.Fprint(w, `{"name":"batches/17","displayName":"display","state":"BATCH_STATE_SUCCEEDED","output":{"responsesFile":"files/output-17"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	transport := NewGeminiBatchTransport("test-key")
	transport.apiBase = server.URL

	created, err := transport.CreateBatch(context.Background(), "gemini-test", "display", "files/input")
	if err != nil {
		t.Fatalf("CreateBatch() error = %v", err)
	}
	if created.Name != "batches/17" || created.State != "JOB_STATE_PENDING" || created.DisplayName != "display" {
		t.Fatalf("created = %+v", created)
	}
	got, err := transport.GetBatch(context.Background(), created.Name)
	if err != nil {
		t.Fatalf("GetBatch() error = %v", err)
	}
	if got.State != "BATCH_STATE_SUCCEEDED" || got.OutputFileName != "files/output-17" {
		t.Fatalf("GetBatch() = %+v", got)
	}
	state, err := NormalizeGeminiBatchState(got.State)
	if err != nil || state != "succeeded" {
		t.Fatalf("NormalizeGeminiBatchState() = %q, %v", state, err)
	}
}

func TestGeminiBatchCancelDeleteAndDownloadEndpoints(t *testing.T) {
	seen := make(map[string]bool)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		seen[key] = true
		switch key {
		case "POST /v1beta/batches/1:cancel", "DELETE /v1beta/batches/1":
			w.WriteHeader(http.StatusNoContent)
		case "DELETE /v1beta/files/missing":
			http.NotFound(w, r)
		case "GET /download/v1beta/files/output:download":
			if r.URL.Query().Get("alt") != "media" {
				t.Errorf("download alt = %q", r.URL.Query().Get("alt"))
			}
			fmt.Fprint(w, "result bytes")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	transport := NewGeminiBatchTransport("test-key")
	transport.apiBase = server.URL
	if err := transport.CancelBatch(context.Background(), "batches/1"); err != nil {
		t.Fatalf("CancelBatch() error = %v", err)
	}
	if err := transport.DeleteBatch(context.Background(), "batches/1"); err != nil {
		t.Fatalf("DeleteBatch() error = %v", err)
	}
	if err := transport.DeleteFile(context.Background(), "files/missing"); !IsGeminiBatchNotFound(err) {
		t.Fatalf("DeleteFile() error = %v, want not found", err)
	}
	body, err := transport.DownloadFile(context.Background(), "files/output")
	if err != nil {
		t.Fatalf("DownloadFile() error = %v", err)
	}
	data, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil || string(data) != "result bytes" {
		t.Fatalf("download = %q, %v", data, err)
	}
	for _, key := range []string{
		"POST /v1beta/batches/1:cancel", "DELETE /v1beta/batches/1",
		"DELETE /v1beta/files/missing", "GET /download/v1beta/files/output:download",
	} {
		if !seen[key] {
			t.Errorf("endpoint %s was not called", key)
		}
	}
}

func TestIsGeminiDeleteInvalidArgument(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{
			name: "observed file name limit",
			err: fmt.Errorf("clean up output: %w", &GeminiBatchAPIError{
				StatusCode: http.StatusBadRequest,
				Body:       []byte(`{"error":{"code":400,"message":"DeleteFileRequest.name exceeds 40 characters","status":"INVALID_ARGUMENT"}}`),
			}),
		},
		{
			name: "reworded file validation",
			err: &GeminiBatchAPIError{
				StatusCode: http.StatusBadRequest,
				Body:       []byte(`{"error":{"code":400,"message":"file resource cannot be deleted","status":"INVALID_ARGUMENT"}}`),
			},
		},
		{
			name: "batch validation",
			err: &GeminiBatchAPIError{
				StatusCode: http.StatusBadRequest,
				Body:       []byte(`{"error":{"code":3,"message":"invalid batch resource","status":"INVALID_ARGUMENT"}}`),
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if !IsGeminiDeleteInvalidArgument(test.err) {
				t.Fatalf("IsGeminiDeleteInvalidArgument(%v) = false, want true", test.err)
			}
		})
	}

	invalidBody := []byte(`{"error":{"code":400,"message":"bad state","status":"FAILED_PRECONDITION"}}`)
	for _, test := range []struct {
		name string
		err  error
	}{
		{
			name: "different body status",
			err:  &GeminiBatchAPIError{StatusCode: http.StatusBadRequest, Body: invalidBody},
		},
		{
			name: "different HTTP status",
			err: &GeminiBatchAPIError{
				StatusCode: http.StatusInternalServerError,
				Body:       []byte(`{"error":{"code":500,"status":"INVALID_ARGUMENT"}}`),
			},
		},
		{
			name: "malformed body",
			err:  &GeminiBatchAPIError{StatusCode: http.StatusBadRequest, Body: []byte("not json")},
		},
		{name: "non-API error", err: errors.New("offline")},
	} {
		t.Run(test.name, func(t *testing.T) {
			if IsGeminiDeleteInvalidArgument(test.err) {
				t.Fatalf("IsGeminiDeleteInvalidArgument(%v) = true, want false", test.err)
			}
		})
	}
}

func TestGeminiBatchNonIdempotentServerErrorsAreAmbiguous(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "accepted before failure", http.StatusInternalServerError)
	}))
	defer server.Close()
	transport := NewGeminiBatchTransport("test-key")
	transport.apiBase = server.URL
	transport.uploadBase = server.URL
	if _, err := transport.CreateBatch(context.Background(), "gemini-test", "display", "files/input"); err == nil || !IsGeminiAmbiguousOperation(err) {
		t.Fatalf("CreateBatch() error = %v, want ambiguous", err)
	}
	source := bytes.NewReader([]byte("{}\n"))
	if _, err := transport.UploadJSONL(context.Background(), "display", source, int64(source.Len())); err == nil || !IsGeminiAmbiguousOperation(err) {
		t.Fatalf("UploadJSONL() error = %v, want ambiguous", err)
	}
}

func TestGeminiBatchRequestTimeoutsAreAmbiguous(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, ":batchGenerateContent"):
			http.Error(w, "deadline exceeded", http.StatusRequestTimeout)
		case r.URL.Path == "/upload/v1beta/files":
			w.Header().Set("X-Goog-Upload-URL", server.URL+"/upload-session")
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/upload-session":
			http.Error(w, "deadline exceeded", http.StatusRequestTimeout)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	transport := NewGeminiBatchTransport("test-key")
	transport.apiBase = server.URL
	transport.uploadBase = server.URL
	if _, err := transport.CreateBatch(context.Background(), "gemini-test", "display", "files/input"); err == nil || !IsGeminiAmbiguousOperation(err) {
		t.Fatalf("CreateBatch() error = %v, want ambiguous HTTP 408", err)
	}
	source := bytes.NewReader([]byte("{}\n"))
	if _, err := transport.UploadJSONL(context.Background(), "display", source, int64(source.Len())); err == nil || !IsGeminiAmbiguousOperation(err) {
		t.Fatalf("UploadJSONL() error = %v, want ambiguous finalize HTTP 408", err)
	}
}

func TestGeminiBatchClientErrorsAreNotAmbiguous(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, ":batchGenerateContent"):
			http.Error(w, "bad request", http.StatusBadRequest)
		case r.URL.Path == "/upload/v1beta/files":
			w.Header().Set("X-Goog-Upload-URL", server.URL+"/upload-session")
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/upload-session":
			http.Error(w, "bad request", http.StatusBadRequest)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	transport := NewGeminiBatchTransport("test-key")
	transport.apiBase = server.URL
	transport.uploadBase = server.URL
	_, createErr := transport.CreateBatch(context.Background(), "gemini-test", "display", "files/input")
	var createAPIErr *GeminiBatchAPIError
	if !errors.As(createErr, &createAPIErr) || createAPIErr.StatusCode != http.StatusBadRequest || IsGeminiAmbiguousOperation(createErr) {
		t.Fatalf("CreateBatch() error = %v, want deterministic Gemini HTTP 400", createErr)
	}
	source := bytes.NewReader([]byte("{}\n"))
	_, uploadErr := transport.UploadJSONL(context.Background(), "display", source, int64(source.Len()))
	var uploadAPIErr *GeminiBatchAPIError
	if !errors.As(uploadErr, &uploadAPIErr) || uploadAPIErr.StatusCode != http.StatusBadRequest || IsGeminiAmbiguousOperation(uploadErr) {
		t.Fatalf("UploadJSONL() error = %v, want deterministic Gemini finalize HTTP 400", uploadErr)
	}
}

func TestGeminiBatchDecodesCanonicalJobErrorCode(t *testing.T) {
	batch, err := decodeGeminiRemoteBatch(strings.NewReader(
		`{"name":"batches/failed","state":"BATCH_STATE_FAILED","error":{"code":3,"message":"bad request"}}`,
	))
	if err != nil {
		t.Fatalf("decodeGeminiRemoteBatch() error = %v", err)
	}
	if batch.ErrorMessage != "INVALID_ARGUMENT: bad request" {
		t.Fatalf("error message = %q", batch.ErrorMessage)
	}
}

func TestGeminiBatchListsEveryPage(t *testing.T) {
	var fileCalls, batchCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1beta/files":
			fileCalls++
			if got := r.URL.Query().Get("pageSize"); got != "100" {
				t.Errorf("files pageSize = %q, want 100", got)
			}
			if r.URL.Query().Get("pageToken") == "" {
				fmt.Fprint(w, `{"files":[{"name":"files/1","displayName":"one"}],"nextPageToken":"next"}`)
			} else {
				fmt.Fprint(w, `{"files":[{"name":"files/2","displayName":"two"}]}`)
			}
		case "/v1beta/batches":
			batchCalls++
			if r.URL.Query().Get("pageToken") == "" {
				fmt.Fprint(w, `{"operations":[{"name":"batches/1","metadata":{"displayName":"one","state":"JOB_STATE_RUNNING"}}],"nextPageToken":"next"}`)
			} else {
				fmt.Fprint(w, `{"operations":[{"name":"batches/2","metadata":{"displayName":"two","state":"JOB_STATE_PENDING"}}]}`)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	transport := NewGeminiBatchTransport("test-key")
	transport.apiBase = server.URL
	files, err := transport.ListFiles(context.Background())
	if err != nil {
		t.Fatalf("ListFiles() error = %v", err)
	}
	batches, err := transport.ListBatches(context.Background())
	if err != nil {
		t.Fatalf("ListBatches() error = %v", err)
	}
	if len(files) != 2 || fileCalls != 2 || len(batches) != 2 || batchCalls != 2 {
		t.Fatalf("files=%d calls=%d batches=%d calls=%d", len(files), fileCalls, len(batches), batchCalls)
	}
}

func TestDecodeGeminiBatchResultUsesFrozenPrices(t *testing.T) {
	candidateJSON := `{"pages":[{"page_index":0,"transcription":"text","page_description":"page","visual_elements":[]}]}`
	body := []byte(fmt.Sprintf(
		`{"candidates":[{"content":{"parts":[{"text":%q}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":20,"thoughtsTokenCount":5}}`,
		candidateJSON,
	))
	result, err := DecodeGeminiBatchResult(body, 1, GeminiTokenPrices{Input: 3, Output: 7})
	if err != nil {
		t.Fatalf("DecodeGeminiBatchResult() error = %v", err)
	}
	if result.Billing.KnownCost != 205 || result.Billing.Indeterminate {
		t.Fatalf("billing = %+v, want 205 known", result.Billing)
	}
	if result.InputTokens == nil || *result.InputTokens != 10 || result.OutputTokens == nil || *result.OutputTokens != 25 {
		t.Fatalf("tokens = input %v output %v", result.InputTokens, result.OutputTokens)
	}
}
