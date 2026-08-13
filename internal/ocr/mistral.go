package ocr

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	pdfutil "github.com/maxim/ringbinder/internal/pdf"
)

const (
	mistralEndpoint = "https://api.mistral.ai/v1/ocr"
	mistralModel    = "mistral-ocr-4-1"
	// Ringbinder always requests bbox_annotation_format to preserve searchable
	// image/graphic descriptions, so estimates use OCR 4.1 annotated-page pricing.
	mistralPricePerPage  = 0.005 // $5 per 1,000 pages
	maxAttempts          = 5
	maxRequestBytes      = 45 * 1024 * 1024
	maxDecodedInputBytes = 45 * 1024 * 1024
	maxPDFPages          = 1000
	// OCR latency is not reliably proportional to page or byte counts. Keep a
	// fixed per-attempt ceiling; an earlier caller deadline still wins.
	requestTimeout = 15 * time.Minute
)

type pageCounter func(context.Context, io.ReadSeeker) (int, error)
type rangeExtractor func(context.Context, io.ReadSeeker, int, int, int) ([]byte, error)

type MistralClient struct {
	apiKey      string
	httpClient  *http.Client
	endpoint    string
	sleep       func(context.Context, time.Duration) error
	randFloat64 func() float64

	requestByteLimit int
	decodedByteLimit int
	pageLimit        int
	pageCount        pageCounter
	extractRange     rangeExtractor
}

func NewMistralClient(apiKey string) *MistralClient {
	return &MistralClient{
		apiKey:           apiKey,
		httpClient:       &http.Client{Timeout: requestTimeout},
		endpoint:         mistralEndpoint,
		sleep:            sleepWithContext,
		randFloat64:      rand.Float64,
		requestByteLimit: maxRequestBytes,
		decodedByteLimit: maxDecodedInputBytes,
		pageLimit:        maxPDFPages,
		pageCount:        pdfutil.PageCountContext,
		extractRange:     pdfutil.ExtractRange,
	}
}

func NewMistralClientFromEnv() (*MistralClient, error) {
	key := os.Getenv("MISTRAL_API_KEY")
	if key == "" {
		return nil, fmt.Errorf("MISTRAL_API_KEY environment variable is not set")
	}
	return NewMistralClient(key), nil
}

func (c *MistralClient) PricePerPage() float64 {
	return mistralPricePerPage
}

func MistralPricePerPage() float64 {
	return mistralPricePerPage
}

func (c *MistralClient) OCRFile(ctx context.Context, filePath string, fileType string) ([]PageResult, error) {
	if _, err := dataURLPrefix(fileType); err != nil {
		return nil, err
	}
	info, err := os.Stat(filePath)
	if err != nil {
		return nil, fmt.Errorf("stat file: %w", err)
	}
	if info.Size() < 0 || uint64(info.Size()) > uint64(^uint(0)>>1) {
		return nil, fmt.Errorf("%s: file is too large to address in memory", filePath)
	}

	if fileType == "pdf" {
		return c.ocrPDF(ctx, filePath, info.Size())
	}
	return c.ocrImage(ctx, filePath, fileType, info.Size())
}

func (c *MistralClient) ocrImage(ctx context.Context, filePath, fileType string, sourceSize int64) ([]PageResult, error) {
	limit, err := c.effectiveDecodedLimit(fileType)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filePath, err)
	}
	if sourceSize > int64(limit) {
		return nil, fmt.Errorf("%s: oversized %s image cannot be transformed: inline OCR request exceeds %d bytes", filePath, fileType, c.requestLimit())
	}

	f, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("open file: %w", err)
	}
	defer f.Close()

	data, tooLarge, err := readLimited(ctx, f, limit)
	if err != nil {
		return nil, fmt.Errorf("%s: read file: %w", filePath, err)
	}
	if tooLarge {
		return nil, fmt.Errorf("%s: oversized %s image cannot be transformed: inline OCR request exceeds %d bytes", filePath, fileType, c.requestLimit())
	}
	body, err := c.buildRequestBody(data, fileType)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filePath, err)
	}
	respBody, err := c.doWithRetryBody(ctx, body)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filePath, err)
	}
	results, err := decodeResults(respBody, 1)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filePath, err)
	}
	return results, nil
}

func (c *MistralClient) ocrPDF(ctx context.Context, filePath string, sourceSize int64) ([]PageResult, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("open file: %w", err)
	}
	defer f.Close()

	counter := c.pageCount
	if counter == nil {
		counter = pdfutil.PageCountContext
	}
	pageCount, err := counter(ctx, f)
	if err != nil {
		return nil, fmt.Errorf("%s: read PDF page count: %w", filePath, err)
	}
	if pageCount < 1 {
		return nil, fmt.Errorf("%s: PDF contains no pages", filePath)
	}

	// Mistral documents 1,000 pages as a source-document limit. Its `pages`
	// selector has no documented oversized-source or index guarantee, so ranges
	// are extracted locally instead of treating the selector as a bypass.
	// https://docs.mistral.ai/studio-api/document-processing/basic_ocr#faq
	limit, err := c.effectiveDecodedLimit("pdf")
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filePath, err)
	}
	// Apply the serialized-body boundary to original PDFs even though this moves
	// some provider-valid decoded files into extraction. Mistral does not specify
	// whether its size accounting happens before or after Base64 expansion.
	canSendOriginalPDF := pageCount <= c.pdfPageLimit() && sourceSize <= int64(limit)
	if canSendOriginalPDF {
		data, tooLarge, err := readLimited(ctx, f, limit)
		if err != nil {
			return nil, fmt.Errorf("%s: read file: %w", filePath, err)
		}
		if !tooLarge {
			body, err := c.buildRequestBody(data, "pdf")
			if err != nil {
				return nil, fmt.Errorf("%s: %w", filePath, err)
			}
			respBody, err := c.doWithRetryBody(ctx, body)
			if err == nil {
				results, err := decodeResults(respBody, pageCount)
				if err != nil {
					return nil, fmt.Errorf("%s: %w", filePath, err)
				}
				return results, nil
			}

			if !isPayloadTooLarge(err) {
				return nil, fmt.Errorf("%s: %w", filePath, err)
			}
			if pageCount == 1 {
				return nil, fmt.Errorf("%s: source page 1 was rejected as too large: %w", filePath, err)
			}

			mid := pageCount / 2
			var results []PageResult
			if err := c.ocrPDFInterval(ctx, f, filePath, sourceSize, pageCount, 0, mid, &results); err != nil {
				return nil, err
			}
			if err := c.ocrPDFInterval(ctx, f, filePath, sourceSize, pageCount, mid, pageCount, &results); err != nil {
				return nil, err
			}
			return results, nil
		}
	}

	var results []PageResult
	if err := c.ocrPDFInterval(ctx, f, filePath, sourceSize, pageCount, 0, pageCount, &results); err != nil {
		return nil, err
	}
	return results, nil
}

type pdfChunk struct {
	start int
	end   int
	data  []byte
}

func (c *MistralClient) ocrPDFInterval(
	ctx context.Context,
	source io.ReadSeeker,
	filePath string,
	sourceSize int64,
	totalPages int,
	start, end int,
	results *[]PageResult,
) error {
	for pos := start; pos < end; {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk, err := c.planPDFChunk(ctx, source, sourceSize, totalPages, pos, end)
		if err != nil {
			return fmt.Errorf("%s, %s: %w", filePath, formatPageRange(pos, min(end, pos+c.pdfPageLimit())), err)
		}

		body, err := c.buildRequestBody(chunk.data, "pdf")
		if err != nil {
			return fmt.Errorf("%s, %s: %w", filePath, formatPageRange(chunk.start, chunk.end), err)
		}
		respBody, err := c.doWithRetryBody(ctx, body)
		if err != nil {
			if !isPayloadTooLarge(err) {
				return fmt.Errorf("%s, %s: %w", filePath, formatPageRange(chunk.start, chunk.end), err)
			}
			if chunk.end-chunk.start == 1 {
				return fmt.Errorf("%s, source page %d was rejected as too large: %w", filePath, chunk.start+1, err)
			}

			mid := chunk.start + (chunk.end-chunk.start)/2
			if err := c.ocrPDFInterval(ctx, source, filePath, sourceSize, totalPages, chunk.start, mid, results); err != nil {
				return err
			}
			if err := c.ocrPDFInterval(ctx, source, filePath, sourceSize, totalPages, mid, chunk.end, results); err != nil {
				return err
			}
			pos = chunk.end
			continue
		}

		part, err := decodeResults(respBody, chunk.end-chunk.start)
		if err != nil {
			return fmt.Errorf("%s, %s: %w", filePath, formatPageRange(chunk.start, chunk.end), err)
		}
		for i := range part {
			part[i].PageIndex += chunk.start
		}
		*results = append(*results, part...)
		pos = chunk.end
	}
	return nil
}

func (c *MistralClient) planPDFChunk(
	ctx context.Context,
	source io.ReadSeeker,
	sourceSize int64,
	totalPages int,
	start, intervalEnd int,
) (pdfChunk, error) {
	maxEnd := min(intervalEnd, start+c.pdfPageLimit())
	byteLimit, err := c.effectiveDecodedLimit("pdf")
	if err != nil {
		return pdfChunk{}, err
	}

	pagesAvailable := maxEnd - start
	seedPages := pagesAvailable
	if sourceSize > 0 && totalPages > 0 {
		avgBytes := max(int64(1), sourceSize/int64(totalPages))
		seedPages = int(int64(byteLimit) / avgBytes)
		seedPages = max(1, min(seedPages, pagesAvailable))
	}
	seedEnd := start + seedPages

	extractor := c.extractRange
	if extractor == nil {
		extractor = pdfutil.ExtractRange
	}
	measure := func(candidateEnd int) (pdfChunk, bool, error) {
		if err := ctx.Err(); err != nil {
			return pdfChunk{}, false, err
		}
		data, err := extractor(ctx, source, start, candidateEnd, byteLimit)
		if errors.Is(err, pdfutil.ErrRangeTooLarge) {
			return pdfChunk{}, false, nil
		}
		if err != nil {
			return pdfChunk{}, false, err
		}
		if size, err := c.requestBodySize(len(data), "pdf"); err != nil {
			return pdfChunk{}, false, err
		} else if size > c.requestLimit() {
			return pdfChunk{}, false, nil
		}
		return pdfChunk{start: start, end: candidateEnd, data: data}, true, nil
	}

	best, fits, err := measure(seedEnd)
	if err != nil {
		return pdfChunk{}, err
	}
	low, high := start+1, seedEnd-1
	if fits {
		low, high = seedEnd+1, maxEnd
	}
	for low <= high {
		mid := low + (high-low)/2
		candidate, fits, err := measure(mid)
		if err != nil {
			return pdfChunk{}, err
		}
		if fits {
			best = candidate
			low = mid + 1
		} else {
			high = mid - 1
		}
	}
	if best.end == 0 {
		return pdfChunk{}, fmt.Errorf("source page %d exceeds the decoded or serialized OCR request limit", start+1)
	}
	return best, nil
}

func formatPageRange(start, end int) string {
	if end-start == 1 {
		return fmt.Sprintf("source page %d", start+1)
	}
	return fmt.Sprintf("source pages %d-%d", start+1, end)
}

func (c *MistralClient) requestLimit() int {
	if c.requestByteLimit > 0 {
		return c.requestByteLimit
	}
	return maxRequestBytes
}

func (c *MistralClient) decodedLimit() int {
	if c.decodedByteLimit > 0 {
		return c.decodedByteLimit
	}
	return maxDecodedInputBytes
}

func (c *MistralClient) pdfPageLimit() int {
	if c.pageLimit > 0 {
		return c.pageLimit
	}
	return maxPDFPages
}

func (c *MistralClient) effectiveDecodedLimit(fileType string) (int, error) {
	requestLimit := c.requestLimit()
	if size, err := c.requestBodySize(0, fileType); err != nil {
		return 0, err
	} else if size > requestLimit {
		return 0, fmt.Errorf("OCR request framing exceeds the %d-byte request limit", requestLimit)
	}

	low, high := 0, c.decodedLimit()
	for low < high {
		mid := low + (high-low+1)/2
		size, err := c.requestBodySize(mid, fileType)
		if err != nil {
			return 0, err
		}
		if size <= requestLimit {
			low = mid
		} else {
			high = mid - 1
		}
	}

	// With production's equal 45 MiB caps, Base64 and JSON framing make the
	// request cap fail first. Keep the decoded guard because the provider
	// documents a separate file limit and injected/test limits may differ.
	return low, nil
}

func (c *MistralClient) requestBodySize(decodedLen int, fileType string) (int, error) {
	if decodedLen < 0 {
		return 0, errors.New("negative decoded length")
	}
	emptyBody, err := marshalRequest("", fileType)
	if err != nil {
		return 0, err
	}
	encodedLen := base64.StdEncoding.EncodedLen(decodedLen)
	if encodedLen > int(^uint(0)>>1)-len(emptyBody) {
		return 0, errors.New("OCR request size overflows int")
	}
	return len(emptyBody) + encodedLen, nil
}

func (c *MistralClient) buildRequestBody(data []byte, fileType string) ([]byte, error) {
	encoded := base64.StdEncoding.EncodeToString(data)
	body, err := marshalRequest(encoded, fileType)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	expected, err := c.requestBodySize(len(data), fileType)
	if err != nil {
		return nil, err
	}
	if len(body) != expected {
		return nil, fmt.Errorf("internal request sizing mismatch: calculated %d bytes, built %d", expected, len(body))
	}
	if len(body) > c.requestLimit() {
		return nil, fmt.Errorf("serialized OCR request is %d bytes, limit %d", len(body), c.requestLimit())
	}
	return body, nil
}

func marshalRequest(encoded, fileType string) ([]byte, error) {
	prefix, err := dataURLPrefix(fileType)
	if err != nil {
		return nil, err
	}
	req := mistralRequest{
		Model:                mistralModel,
		BBoxAnnotationFormat: buildBBoxAnnotationFormat(),
	}
	if fileType == "pdf" {
		req.Document = mistralDocument{Type: "document_url", DocumentURL: prefix + encoded}
	} else {
		req.Document = mistralDocument{Type: "image_url", ImageURL: prefix + encoded}
	}
	return json.Marshal(req)
}

func dataURLPrefix(fileType string) (string, error) {
	switch fileType {
	case "pdf":
		return "data:application/pdf;base64,", nil
	case "jpeg":
		return "data:image/jpeg;base64,", nil
	case "png":
		return "data:image/png;base64,", nil
	default:
		return "", fmt.Errorf("unsupported file type: %s", fileType)
	}
}

func readLimited(ctx context.Context, rs io.ReadSeeker, maxBytes int) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if _, err := rs.Seek(0, io.SeekStart); err != nil {
		return nil, false, err
	}
	data, err := io.ReadAll(io.LimitReader(rs, int64(maxBytes)+1))
	if err != nil {
		return nil, false, err
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if len(data) > maxBytes {
		return nil, true, nil
	}
	return data, false, nil
}

func decodeResults(respBody []byte, expectedCount int) ([]PageResult, error) {
	var resp mistralResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	expected := make([]int, expectedCount)
	for i := range expected {
		expected[i] = i
	}
	actual := make([]string, 0, len(resp.Pages))
	pages := make(map[int]*mistralPage, len(resp.Pages))
	valid := true
	for _, page := range resp.Pages {
		if page == nil {
			actual = append(actual, "null")
			valid = false
			continue
		}
		if page.Index == nil {
			actual = append(actual, "missing")
			valid = false
			continue
		}
		index := *page.Index
		actual = append(actual, strconv.Itoa(index))
		if index < 0 || index >= expectedCount {
			valid = false
			continue
		}
		if _, exists := pages[index]; exists {
			valid = false
			continue
		}
		pages[index] = page
	}
	if len(pages) != expectedCount {
		valid = false
	}
	if !valid {
		return nil, fmt.Errorf("invalid response page indexes: expected %v, actual [%s]", expected, strings.Join(actual, " "))
	}

	indexes := make([]int, 0, len(pages))
	for index := range pages {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	results := make([]PageResult, 0, len(indexes))
	for _, index := range indexes {
		page := pages[index]
		markdown := page.Markdown
		for _, image := range page.Images {
			imageType := strings.TrimSpace(image.ImageAnnotation.ImageType)
			description := strings.TrimSpace(image.ImageAnnotation.Description)
			if description == "" {
				continue
			}
			if imageType == "" {
				imageType = "image"
			}
			markdown += fmt.Sprintf("\n\n[Image: %s — %s]", imageType, description)
		}
		results = append(results, PageResult{PageIndex: index, Markdown: markdown})
	}
	return results, nil
}

func buildBBoxAnnotationFormat() bboxAnnotationFormat {
	return bboxAnnotationFormat{
		Type: "json_schema",
		JSONSchema: bboxJSONSchemaDef{
			Name:        "image_annotation",
			Description: "Describe each image on the page so it can be indexed in full text search.",
			Strict:      true,
			Schema: bboxSchemaDefinition{
				Type: "object",
				Properties: map[string]bboxPropertyDef{
					"image_type": {
						Type:        "string",
						Description: "Short label for the visual, such as chart, table, diagram, or photo.",
					},
					"description": {
						Type:        "string",
						Description: "One to two sentence description of what the image contains.",
					},
				},
				Required:             []string{"image_type", "description"},
				AdditionalProperties: false,
			},
		},
	}
}

func sleepWithContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type apiError struct {
	StatusCode int
	Body       []byte
	Attempts   int
}

func isPayloadTooLarge(err error) bool {
	var apiErr *apiError
	// Only an HTTP 413 is an unambiguous size rejection. Generic 422 responses
	// and human-readable message text must remain visible instead of triggering
	// undocumented subdivision behavior.
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusRequestEntityTooLarge
}

func (e *apiError) Error() string {
	message := strings.TrimSpace(string(e.Body))
	if len(message) > 200 {
		message = message[:200]
	}
	if e.Attempts > 1 {
		return fmt.Sprintf("API error %d after %d attempts: %s", e.StatusCode, e.Attempts, message)
	}
	return fmt.Sprintf("API error %d: %s", e.StatusCode, message)
}

func (c *MistralClient) doWithRetry(ctx context.Context, req mistralRequest) ([]byte, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	return c.doWithRetryBody(ctx, body)
}

func (c *MistralClient) doWithRetryBody(ctx context.Context, body []byte) ([]byte, error) {
	endpoint := c.endpoint
	if endpoint == "" {
		endpoint = mistralEndpoint
	}
	sleep := c.sleep
	if sleep == nil {
		sleep = sleepWithContext
	}
	randFloat64 := c.randFloat64
	if randFloat64 == nil {
		randFloat64 = rand.Float64
	}
	httpClient := c.httpClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: requestTimeout}
	}

	backoff := 1.0 // seconds
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			if err := sleep(ctx, time.Duration(backoff*float64(time.Second))); err != nil {
				return nil, err
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

		resp, err := httpClient.Do(httpReq)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if attempt == maxAttempts {
				return nil, fmt.Errorf("http request failed after %d attempts: %w", maxAttempts, err)
			}
			backoff = math.Min(backoff*2, 60)
			backoff = math.Min(backoff*(0.5+randFloat64()), 60)
			continue
		}

		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("read response: %w", err)
		}
		if resp.StatusCode == http.StatusOK {
			return respBody, nil
		}

		retryable := resp.StatusCode == http.StatusTooManyRequests ||
			(resp.StatusCode >= http.StatusInternalServerError && resp.StatusCode < 600)
		if !retryable || attempt == maxAttempts {
			return nil, &apiError{StatusCode: resp.StatusCode, Body: respBody, Attempts: attempt}
		}

		nextBackoff := math.Min(backoff*2, 60)
		if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "" {
			if seconds, err := strconv.ParseFloat(retryAfter, 64); err == nil {
				if seconds < 0 {
					seconds = 0
				}
				nextBackoff = math.Min(seconds, 60)
			}
		}
		backoff = math.Min(nextBackoff*(0.5+randFloat64()), 60)
	}

	return nil, fmt.Errorf("max attempts exceeded")
}
