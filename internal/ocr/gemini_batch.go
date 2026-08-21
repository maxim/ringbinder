package ocr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	geminiAPIBase       = "https://generativelanguage.googleapis.com"
	geminiBatchPageSize = 100
	geminiErrorBodyMax  = 1 << 20
)

type GeminiRemoteFile struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	URI         string `json:"uri"`
	State       string `json:"state"`
	MIMEType    string `json:"mimeType"`
	SizeBytes   string `json:"sizeBytes"`
}

type GeminiRemoteBatch struct {
	Name           string
	DisplayName    string
	State          string
	OutputFileName string
	ErrorMessage   string
	CreateTime     string
	UpdateTime     string
}

type GeminiBatchAPIError struct {
	StatusCode int
	Body       []byte
}

func (e *GeminiBatchAPIError) Error() string {
	body := strings.TrimSpace(string(e.Body))
	if body == "" {
		return fmt.Sprintf("Gemini Batch API returned HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("Gemini Batch API returned HTTP %d: %s", e.StatusCode, body)
}

type GeminiAmbiguousOperationError struct {
	Operation string
	Err       error
}

func (e *GeminiAmbiguousOperationError) Error() string {
	return fmt.Sprintf("Gemini %s outcome is unknown: %v", e.Operation, e.Err)
}

func (e *GeminiAmbiguousOperationError) Unwrap() error { return e.Err }

func IsGeminiAmbiguousOperation(err error) bool {
	var ambiguous *GeminiAmbiguousOperationError
	return errors.As(err, &ambiguous)
}

func IsGeminiBatchNotFound(err error) bool {
	var apiErr *GeminiBatchAPIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

func IsGeminiDeleteInvalidArgument(err error) bool {
	var apiErr *GeminiBatchAPIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
		return false
	}
	var envelope struct {
		Error struct {
			Status string `json:"status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(apiErr.Body, &envelope); err != nil {
		return false
	}
	return envelope.Error.Status == "INVALID_ARGUMENT"
}

func IsGeminiGlobalFailure(err error) bool {
	var apiErr *GeminiBatchAPIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

// GeminiBatchTransport keeps the discounted Batch API's Files and batch-job
// lifecycle explicit instead of hiding non-idempotent calls behind an SDK.
type GeminiBatchTransport struct {
	apiKey     string
	httpClient *http.Client
	apiBase    string
	uploadBase string
}

func NewGeminiBatchTransport(apiKey string) *GeminiBatchTransport {
	return &GeminiBatchTransport{
		apiKey:     apiKey,
		httpClient: &http.Client{Timeout: 10 * time.Minute},
		apiBase:    geminiAPIBase,
		uploadBase: geminiAPIBase,
	}
}

func (c *GeminiBatchTransport) UploadJSONL(
	ctx context.Context,
	displayName string,
	source io.ReadSeeker,
	size int64,
) (GeminiRemoteFile, error) {
	if size < 0 || size > GeminiBatchMaxInputBytes {
		return GeminiRemoteFile{}, fmt.Errorf("Gemini batch input is %d bytes; limit %d", size, GeminiBatchMaxInputBytes)
	}
	metadata, err := json.Marshal(map[string]any{
		"file": map[string]string{
			"display_name": displayName,
			"mime_type":    "application/jsonl",
		},
	})
	if err != nil {
		return GeminiRemoteFile{}, err
	}
	endpoint := strings.TrimRight(c.uploadURL(), "/") + "/upload/v1beta/files"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(metadata))
	if err != nil {
		return GeminiRemoteFile{}, err
	}
	c.setAPIKey(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Goog-Upload-Protocol", "resumable")
	req.Header.Set("X-Goog-Upload-Command", "start")
	req.Header.Set("X-Goog-Upload-Header-Content-Length", strconv.FormatInt(size, 10))
	req.Header.Set("X-Goog-Upload-Header-Content-Type", "application/jsonl")
	resp, err := c.client().Do(req)
	if err != nil {
		return GeminiRemoteFile{}, &GeminiAmbiguousOperationError{Operation: "file upload", Err: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		apiErr := readGeminiBatchAPIError(resp)
		if resp.StatusCode >= 500 {
			return GeminiRemoteFile{}, &GeminiAmbiguousOperationError{Operation: "file upload", Err: apiErr}
		}
		return GeminiRemoteFile{}, apiErr
	}
	uploadURL := resp.Header.Get("X-Goog-Upload-URL")
	_ = resp.Body.Close()
	if uploadURL == "" {
		return GeminiRemoteFile{}, errors.New("Gemini upload response omitted X-Goog-Upload-URL")
	}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return GeminiRemoteFile{}, fmt.Errorf("rewind Gemini batch input: %w", err)
	}

	uploadReq, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, io.LimitReader(source, size))
	if err != nil {
		return GeminiRemoteFile{}, err
	}
	uploadReq.ContentLength = size
	uploadReq.Header.Set("Content-Type", "application/jsonl")
	uploadReq.Header.Set("X-Goog-Upload-Offset", "0")
	uploadReq.Header.Set("X-Goog-Upload-Command", "upload, finalize")
	resp, err = c.client().Do(uploadReq)
	if err != nil {
		return GeminiRemoteFile{}, &GeminiAmbiguousOperationError{Operation: "file upload", Err: err}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		apiErr := readGeminiBatchAPIError(resp)
		if resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode >= 500 {
			return GeminiRemoteFile{}, &GeminiAmbiguousOperationError{Operation: "file upload", Err: apiErr}
		}
		return GeminiRemoteFile{}, apiErr
	}
	defer resp.Body.Close()
	var envelope struct {
		File GeminiRemoteFile `json:"file"`
		GeminiRemoteFile
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, geminiErrorBodyMax)).Decode(&envelope); err != nil {
		return GeminiRemoteFile{}, &GeminiAmbiguousOperationError{Operation: "file upload", Err: fmt.Errorf("decode upload response: %w", err)}
	}
	if envelope.File.Name == "" {
		envelope.File = envelope.GeminiRemoteFile
	}
	if envelope.File.Name == "" {
		return GeminiRemoteFile{}, &GeminiAmbiguousOperationError{Operation: "file upload", Err: errors.New("response omitted file name")}
	}
	return envelope.File, nil
}

func (c *GeminiBatchTransport) CreateBatch(
	ctx context.Context,
	model, displayName, inputFileName string,
) (GeminiRemoteBatch, error) {
	body, err := json.Marshal(map[string]any{
		"batch": map[string]any{
			"displayName": displayName,
			"inputConfig": map[string]string{"fileName": inputFileName},
		},
	})
	if err != nil {
		return GeminiRemoteBatch{}, err
	}
	endpoint := fmt.Sprintf(
		"%s/v1beta/models/%s:batchGenerateContent",
		strings.TrimRight(c.baseURL(), "/"), url.PathEscape(model),
	)
	resp, err := c.doJSON(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		var apiErr *GeminiBatchAPIError
		if errors.As(err, &apiErr) && apiErr.StatusCode < 500 && apiErr.StatusCode != http.StatusRequestTimeout {
			return GeminiRemoteBatch{}, err
		}
		return GeminiRemoteBatch{}, &GeminiAmbiguousOperationError{Operation: "batch creation", Err: err}
	}
	defer resp.Body.Close()
	batch, err := decodeGeminiRemoteBatch(resp.Body)
	if err != nil {
		return GeminiRemoteBatch{}, &GeminiAmbiguousOperationError{Operation: "batch creation", Err: err}
	}
	if batch.Name == "" {
		return GeminiRemoteBatch{}, &GeminiAmbiguousOperationError{Operation: "batch creation", Err: errors.New("response omitted batch name")}
	}
	return batch, nil
}

func (c *GeminiBatchTransport) GetBatch(ctx context.Context, name string) (GeminiRemoteBatch, error) {
	endpoint, err := c.resourceURL("batches", name, "")
	if err != nil {
		return GeminiRemoteBatch{}, err
	}
	resp, err := c.doJSON(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return GeminiRemoteBatch{}, err
	}
	defer resp.Body.Close()
	return decodeGeminiRemoteBatch(resp.Body)
}

func (c *GeminiBatchTransport) ListBatches(ctx context.Context) ([]GeminiRemoteBatch, error) {
	var batches []GeminiRemoteBatch
	pageToken := ""
	for {
		endpoint := strings.TrimRight(c.baseURL(), "/") + "/v1beta/batches?pageSize=" + strconv.Itoa(geminiBatchPageSize)
		if pageToken != "" {
			endpoint += "&pageToken=" + url.QueryEscape(pageToken)
		}
		resp, err := c.doJSON(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		var envelope struct {
			Batches       []json.RawMessage `json:"batches"`
			Operations    []json.RawMessage `json:"operations"`
			NextPageToken string            `json:"nextPageToken"`
		}
		decodeErr := json.NewDecoder(io.LimitReader(resp.Body, geminiErrorBodyMax*8)).Decode(&envelope)
		_ = resp.Body.Close()
		if decodeErr != nil {
			return nil, fmt.Errorf("decode Gemini batch list: %w", decodeErr)
		}
		if len(envelope.Batches) == 0 {
			envelope.Batches = envelope.Operations
		}
		for _, raw := range envelope.Batches {
			batch, err := decodeGeminiRemoteBatch(bytes.NewReader(raw))
			if err != nil {
				return nil, err
			}
			batches = append(batches, batch)
		}
		if envelope.NextPageToken == "" {
			return batches, nil
		}
		pageToken = envelope.NextPageToken
	}
}

func (c *GeminiBatchTransport) ListFiles(ctx context.Context) ([]GeminiRemoteFile, error) {
	var files []GeminiRemoteFile
	pageToken := ""
	for {
		endpoint := strings.TrimRight(c.baseURL(), "/") + "/v1beta/files?pageSize=" + strconv.Itoa(geminiBatchPageSize)
		if pageToken != "" {
			endpoint += "&pageToken=" + url.QueryEscape(pageToken)
		}
		resp, err := c.doJSON(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		var envelope struct {
			Files         []GeminiRemoteFile `json:"files"`
			NextPageToken string             `json:"nextPageToken"`
		}
		decodeErr := json.NewDecoder(io.LimitReader(resp.Body, geminiErrorBodyMax*8)).Decode(&envelope)
		_ = resp.Body.Close()
		if decodeErr != nil {
			return nil, fmt.Errorf("decode Gemini file list: %w", decodeErr)
		}
		files = append(files, envelope.Files...)
		if envelope.NextPageToken == "" {
			return files, nil
		}
		pageToken = envelope.NextPageToken
	}
}

func (c *GeminiBatchTransport) CancelBatch(ctx context.Context, name string) error {
	endpoint, err := c.resourceURL("batches", name, ":cancel")
	if err != nil {
		return err
	}
	resp, err := c.doJSON(ctx, http.MethodPost, endpoint, []byte("{}"))
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func (c *GeminiBatchTransport) DeleteBatch(ctx context.Context, name string) error {
	endpoint, err := c.resourceURL("batches", name, "")
	if err != nil {
		return err
	}
	resp, err := c.doJSON(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func (c *GeminiBatchTransport) DeleteFile(ctx context.Context, name string) error {
	endpoint, err := c.resourceURL("files", name, "")
	if err != nil {
		return err
	}
	resp, err := c.doJSON(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func (c *GeminiBatchTransport) DownloadFile(ctx context.Context, name string) (io.ReadCloser, error) {
	kind, id, err := splitGeminiResource(name)
	if err != nil {
		return nil, err
	}
	if kind != "files" {
		return nil, fmt.Errorf("expected Gemini file resource, got %q", name)
	}
	endpoint := fmt.Sprintf(
		"%s/download/v1beta/files/%s:download?alt=media",
		strings.TrimRight(c.baseURL(), "/"), url.PathEscape(id),
	)
	resp, err := c.doJSON(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func NormalizeGeminiBatchState(remoteState string) (string, error) {
	switch strings.ToUpper(strings.TrimSpace(remoteState)) {
	case "JOB_STATE_PENDING", "JOB_STATE_QUEUED", "BATCH_STATE_PENDING", "BATCH_STATE_QUEUED", "PENDING", "QUEUED":
		return "pending", nil
	case "JOB_STATE_RUNNING", "BATCH_STATE_RUNNING", "RUNNING":
		return "running", nil
	case "JOB_STATE_CANCELLING", "BATCH_STATE_CANCELLING", "CANCELLING":
		return "cancelling", nil
	case "JOB_STATE_SUCCEEDED", "BATCH_STATE_SUCCEEDED", "SUCCEEDED":
		return "succeeded", nil
	case "JOB_STATE_FAILED", "BATCH_STATE_FAILED", "FAILED":
		return "failed", nil
	case "JOB_STATE_CANCELLED", "BATCH_STATE_CANCELLED", "CANCELLED":
		return "cancelled", nil
	case "JOB_STATE_EXPIRED", "BATCH_STATE_EXPIRED", "EXPIRED":
		return "expired", nil
	default:
		return "", fmt.Errorf("unknown Gemini batch state %q", remoteState)
	}
}

func decodeGeminiRemoteBatch(reader io.Reader) (GeminiRemoteBatch, error) {
	var raw struct {
		Name        string `json:"name"`
		DisplayName string `json:"displayName"`
		State       string `json:"state"`
		CreateTime  string `json:"createTime"`
		UpdateTime  string `json:"updateTime"`
		Metadata    struct {
			Name        string `json:"name"`
			DisplayName string `json:"displayName"`
			State       string `json:"state"`
			CreateTime  string `json:"createTime"`
			UpdateTime  string `json:"updateTime"`
			Output      struct {
				ResponsesFile string `json:"responsesFile"`
			} `json:"output"`
		} `json:"metadata"`
		Response struct {
			ResponsesFile string `json:"responsesFile"`
		} `json:"response"`
		Dest struct {
			FileName string `json:"fileName"`
		} `json:"dest"`
		Output struct {
			FileName      string `json:"fileName"`
			ResponsesFile string `json:"responsesFile"`
		} `json:"output"`
		OutputConfig struct {
			FileName string `json:"fileName"`
		} `json:"outputConfig"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(reader, geminiErrorBodyMax)).Decode(&raw); err != nil {
		return GeminiRemoteBatch{}, fmt.Errorf("decode Gemini batch: %w", err)
	}
	if raw.DisplayName == "" {
		raw.DisplayName = raw.Metadata.DisplayName
	}
	if raw.State == "" {
		raw.State = raw.Metadata.State
	}
	if raw.CreateTime == "" {
		raw.CreateTime = raw.Metadata.CreateTime
	}
	if raw.UpdateTime == "" {
		raw.UpdateTime = raw.Metadata.UpdateTime
	}
	output := raw.Metadata.Output.ResponsesFile
	if output == "" {
		output = raw.Response.ResponsesFile
	}
	if output == "" {
		output = raw.Dest.FileName
	}
	if output == "" {
		output = raw.Output.ResponsesFile
	}
	if output == "" {
		output = raw.Output.FileName
	}
	if output == "" {
		output = raw.OutputConfig.FileName
	}
	message := ""
	if raw.Error != nil {
		status := raw.Error.Status
		if status == "" {
			status = geminiRPCStatusName(raw.Error.Code)
		}
		message = strings.TrimSpace(strings.Join([]string{status, raw.Error.Message}, ": "))
	}
	return GeminiRemoteBatch{
		Name:           raw.Name,
		DisplayName:    raw.DisplayName,
		State:          raw.State,
		OutputFileName: output,
		ErrorMessage:   strings.Trim(message, ": "),
		CreateTime:     raw.CreateTime,
		UpdateTime:     raw.UpdateTime,
	}, nil
}

func geminiRPCStatusName(code int) string {
	switch code {
	case 1:
		return "CANCELLED"
	case 2:
		return "UNKNOWN"
	case 3:
		return "INVALID_ARGUMENT"
	case 4:
		return "DEADLINE_EXCEEDED"
	case 5:
		return "NOT_FOUND"
	case 6:
		return "ALREADY_EXISTS"
	case 7:
		return "PERMISSION_DENIED"
	case 8:
		return "RESOURCE_EXHAUSTED"
	case 9:
		return "FAILED_PRECONDITION"
	case 10:
		return "ABORTED"
	case 11:
		return "OUT_OF_RANGE"
	case 12:
		return "UNIMPLEMENTED"
	case 13:
		return "INTERNAL"
	case 14:
		return "UNAVAILABLE"
	case 15:
		return "DATA_LOSS"
	case 16:
		return "UNAUTHENTICATED"
	default:
		return ""
	}
}

func (c *GeminiBatchTransport) doJSON(
	ctx context.Context,
	method, endpoint string,
	body []byte,
) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, err
	}
	c.setAPIKey(req)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, readGeminiBatchAPIError(resp)
	}
	return resp, nil
}

func readGeminiBatchAPIError(resp *http.Response) error {
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, geminiErrorBodyMax))
	return &GeminiBatchAPIError{StatusCode: resp.StatusCode, Body: body}
}

func (c *GeminiBatchTransport) resourceURL(expectedKind, name, suffix string) (string, error) {
	kind, id, err := splitGeminiResource(name)
	if err != nil {
		return "", err
	}
	if kind != expectedKind {
		return "", fmt.Errorf("expected Gemini %s resource, got %q", expectedKind, name)
	}
	return fmt.Sprintf(
		"%s/v1beta/%s/%s%s",
		strings.TrimRight(c.baseURL(), "/"), kind, url.PathEscape(id), suffix,
	), nil
}

func splitGeminiResource(name string) (kind, id string, err error) {
	parts := strings.Split(name, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid Gemini resource name %q", name)
	}
	return parts[0], parts[1], nil
}

func (c *GeminiBatchTransport) setAPIKey(req *http.Request) {
	req.Header.Set("x-goog-api-key", c.apiKey)
}

func (c *GeminiBatchTransport) client() *http.Client {
	if c.httpClient != nil {
		return c.httpClient
	}
	return &http.Client{Timeout: 10 * time.Minute}
}

func (c *GeminiBatchTransport) baseURL() string {
	if c.apiBase != "" {
		return c.apiBase
	}
	return geminiAPIBase
}

func (c *GeminiBatchTransport) uploadURL() string {
	if c.uploadBase != "" {
		return c.uploadBase
	}
	return c.baseURL()
}
