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
	geminiEndpoint = "https://generativelanguage.googleapis.com/v1beta/models/gemini-3.7-flash:generateContent"
	geminiModel    = "gemini-3.7-flash"

	geminiMaxPDFPages      = 20
	geminiMaxRequestBytes  = 95 * 1024 * 1024
	geminiMaxDecodedBytes  = 45 * 1024 * 1024
	geminiMaxResponseBytes = 16 * 1024 * 1024
)

// GeminiClient uses the REST API directly so the OCR provider has no SDK
// dependency and its request and billing behavior remain explicit.
type GeminiClient struct {
	apiKey      string
	runAt       time.Time
	httpClient  *http.Client
	endpoint    string
	sleep       func(context.Context, time.Duration) error
	randFloat64 func() float64

	requestByteLimit  int
	decodedByteLimit  int
	responseByteLimit int
	pageLimit         int
	pageCount         pageCounter
	extractRange      rangeExtractor
}

func NewGeminiClient(apiKey string, runAt time.Time) *GeminiClient {
	return &GeminiClient{
		apiKey:            apiKey,
		runAt:             runAt,
		httpClient:        &http.Client{Timeout: requestTimeout},
		endpoint:          geminiEndpoint,
		sleep:             sleepWithContext,
		randFloat64:       rand.Float64,
		requestByteLimit:  geminiMaxRequestBytes,
		decodedByteLimit:  geminiMaxDecodedBytes,
		responseByteLimit: geminiMaxResponseBytes,
		pageLimit:         geminiMaxPDFPages,
		pageCount:         pdfutil.PageCountContext,
		extractRange:      pdfutil.ExtractRange,
	}
}

func NewGeminiClientFromEnv(runAt time.Time) (*GeminiClient, error) {
	key := os.Getenv("GEMINI_API_KEY")
	if key == "" {
		return nil, errors.New("GEMINI_API_KEY environment variable is not set")
	}
	return NewGeminiClient(key, runAt), nil
}

func (c *GeminiClient) OCRFile(ctx context.Context, filePath string, fileType string) ([]PageResult, BillingReport, error) {
	if _, err := geminiMIMEType(fileType); err != nil {
		return nil, BillingReport{}, err
	}
	info, err := os.Stat(filePath)
	if err != nil {
		return nil, BillingReport{}, fmt.Errorf("stat file: %w", err)
	}
	if info.Size() < 0 || uint64(info.Size()) > uint64(^uint(0)>>1) {
		return nil, BillingReport{}, fmt.Errorf("%s: file is too large to address in memory", filePath)
	}
	if fileType == "pdf" {
		return c.ocrPDF(ctx, filePath, info.Size())
	}
	return c.ocrImage(ctx, filePath, fileType, info.Size())
}

func (c *GeminiClient) ocrImage(ctx context.Context, filePath, fileType string, sourceSize int64) ([]PageResult, BillingReport, error) {
	limit, err := c.effectiveDecodedLimit(fileType)
	if err != nil {
		return nil, BillingReport{}, fmt.Errorf("%s: %w", filePath, err)
	}
	if sourceSize > int64(limit) {
		return nil, BillingReport{}, fmt.Errorf("%s: oversized %s image cannot be transformed: inline OCR request exceeds %d bytes", filePath, fileType, c.requestLimit())
	}
	f, err := os.Open(filePath)
	if err != nil {
		return nil, BillingReport{}, fmt.Errorf("open file: %w", err)
	}
	defer f.Close()
	data, tooLarge, err := readLimited(ctx, f, limit)
	if err != nil {
		return nil, BillingReport{}, fmt.Errorf("%s: read file: %w", filePath, err)
	}
	if tooLarge {
		return nil, BillingReport{}, fmt.Errorf("%s: oversized %s image cannot be transformed: inline OCR request exceeds %d bytes", filePath, fileType, c.requestLimit())
	}
	body, err := c.buildRequestBody(data, fileType, 1)
	if err != nil {
		return nil, BillingReport{}, fmt.Errorf("%s: %w", filePath, err)
	}
	results, report, err := c.generateAndDecode(ctx, body, 1)
	if err != nil {
		return nil, report, fmt.Errorf("%s: %w", filePath, err)
	}
	return results, report, nil
}

func (c *GeminiClient) ocrPDF(ctx context.Context, filePath string, sourceSize int64) ([]PageResult, BillingReport, error) {
	var report BillingReport
	f, err := os.Open(filePath)
	if err != nil {
		return nil, report, fmt.Errorf("open file: %w", err)
	}
	defer f.Close()
	pageCount := c.pageCount
	if pageCount == nil {
		pageCount = pdfutil.PageCountContext
	}
	pages, err := pageCount(ctx, f)
	if err != nil {
		return nil, report, fmt.Errorf("%s: read PDF page count: %w", filePath, err)
	}
	if pages < 1 {
		return nil, report, fmt.Errorf("%s: PDF contains no pages", filePath)
	}
	var results []PageResult
	if err := c.ocrPDFInterval(ctx, f, filePath, sourceSize, pages, 0, pages, &results, &report); err != nil {
		return nil, report, err
	}
	return results, report, nil
}

func (c *GeminiClient) ocrPDFInterval(ctx context.Context, source io.ReadSeeker, filePath string, sourceSize int64, totalPages, start, end int, results *[]PageResult, report *BillingReport) error {
	for pos := start; pos < end; {
		if err := ctx.Err(); err != nil {
			return err
		}
		chunk, err := c.planPDFChunk(ctx, source, sourceSize, totalPages, pos, end)
		if err != nil {
			return fmt.Errorf("%s, %s: %w", filePath, formatPageRange(pos, min(end, pos+c.pageLimitValue())), err)
		}
		body, err := c.buildRequestBody(chunk.data, "pdf", chunk.end-chunk.start)
		if err != nil {
			return fmt.Errorf("%s, %s: %w", filePath, formatPageRange(chunk.start, chunk.end), err)
		}
		part, requestReport, err := c.generateAndDecode(ctx, body, chunk.end-chunk.start)
		report.Add(requestReport)
		if err != nil {
			if c.shouldSplitPDF(err, chunk.end-chunk.start) {
				mid := chunk.start + (chunk.end-chunk.start)/2
				if splitErr := c.ocrPDFInterval(ctx, source, filePath, sourceSize, totalPages, chunk.start, mid, results, report); splitErr != nil {
					return splitErr
				}
				if splitErr := c.ocrPDFInterval(ctx, source, filePath, sourceSize, totalPages, mid, chunk.end, results, report); splitErr != nil {
					return splitErr
				}
				pos = chunk.end
				continue
			}
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

func (c *GeminiClient) shouldSplitPDF(err error, pageCount int) bool {
	if pageCount < 2 {
		return false
	}
	return isPayloadTooLarge(err) || isGeminiMaxTokens(err)
}

func (c *GeminiClient) planPDFChunk(ctx context.Context, source io.ReadSeeker, sourceSize int64, totalPages, start, intervalEnd int) (pdfChunk, error) {
	maxEnd := min(intervalEnd, start+c.pageLimitValue())
	limit, err := c.effectiveDecodedLimit("pdf")
	if err != nil {
		return pdfChunk{}, err
	}
	available := maxEnd - start
	seed := available
	if sourceSize > 0 && totalPages > 0 {
		average := max(int64(1), sourceSize/int64(totalPages))
		seed = int(int64(limit) / average)
		seed = max(1, min(seed, available))
	}
	extract := c.extractRange
	if extract == nil {
		extract = pdfutil.ExtractRange
	}
	measure := func(candidateEnd int) (pdfChunk, bool, error) {
		if err := ctx.Err(); err != nil {
			return pdfChunk{}, false, err
		}
		data, err := extract(ctx, source, start, candidateEnd, limit)
		if errors.Is(err, pdfutil.ErrRangeTooLarge) {
			return pdfChunk{}, false, nil
		}
		if err != nil {
			return pdfChunk{}, false, err
		}
		size, err := c.requestBodySize(len(data), "pdf", candidateEnd-start)
		if err != nil {
			return pdfChunk{}, false, err
		}
		return pdfChunk{start: start, end: candidateEnd, data: data}, size <= c.requestLimit(), nil
	}
	best, fits, err := measure(start + seed)
	if err != nil {
		return pdfChunk{}, err
	}
	low, high := start+1, start+seed-1
	if fits {
		low, high = start+seed+1, maxEnd
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

func (c *GeminiClient) generateAndDecode(ctx context.Context, body []byte, expectedPages int) ([]PageResult, BillingReport, error) {
	var report BillingReport
	for semanticAttempt := 0; semanticAttempt < 2; semanticAttempt++ {
		response, requestReport, err := c.doWithRetry(ctx, body)
		report.Add(requestReport)
		if err != nil {
			return nil, report, err
		}
		results, err := decodeGeminiResults(response, expectedPages)
		if err == nil {
			return results, report, nil
		}
		// A semantic retry can incur another charge, so malformed generations get
		// exactly one recovery attempt. Multi-page output exhaustion instead needs
		// smaller source ranges; repeating the unchanged request would waste that
		// bounded retry and likely produce the same truncation.
		if expectedPages > 1 && isGeminiMaxTokens(err) {
			return nil, report, err
		}
		if semanticAttempt == 1 {
			return nil, report, err
		}
	}
	panic("unreachable")
}

func (c *GeminiClient) doWithRetry(ctx context.Context, body []byte) ([]byte, BillingReport, error) {
	endpoint := c.endpoint
	if endpoint == "" {
		endpoint = geminiEndpoint
	}
	httpClient := c.httpClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: requestTimeout}
	}
	sleep := c.sleep
	if sleep == nil {
		sleep = sleepWithContext
	}
	randFloat := c.randFloat64
	if randFloat == nil {
		randFloat = rand.Float64
	}
	var report BillingReport
	backoff := time.Second
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			if err := sleep(ctx, backoff); err != nil {
				return nil, report, err
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, report, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, report, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-goog-api-key", c.apiKey)
		resp, err := httpClient.Do(req)
		if err != nil {
			report.Indeterminate = true
			if ctx.Err() != nil {
				return nil, report, ctx.Err()
			}
			if attempt == maxAttempts {
				return nil, report, fmt.Errorf("http request failed after %d attempts: %w", maxAttempts, err)
			}
			backoff = geminiNextBackoff(backoff, randFloat)
			continue
		}
		response, overflow, readErr := readGeminiResponse(resp.Body, c.responseLimit())
		resp.Body.Close()
		if readErr != nil {
			report.Indeterminate = true
			if ctx.Err() != nil {
				return nil, report, ctx.Err()
			}
			if attempt == maxAttempts {
				return nil, report, fmt.Errorf("read response after %d attempts: %w", maxAttempts, readErr)
			}
			backoff = geminiNextBackoff(backoff, randFloat)
			continue
		}
		if overflow {
			report.Indeterminate = true
			return nil, report, fmt.Errorf("response exceeds %d-byte limit", c.responseLimit())
		}
		if resp.StatusCode == http.StatusOK {
			report.Add(geminiBilling(response, c.runAt))
			return response, report, nil
		}
		usage := geminiBillingIfPresent(response, c.runAt)
		if usage != nil {
			report.Add(*usage)
		}
		retryable := resp.StatusCode == http.StatusTooManyRequests || (resp.StatusCode >= 500 && resp.StatusCode < 600)
		if resp.StatusCode >= 500 && usage == nil {
			report.Indeterminate = true
		}
		if !retryable || attempt == maxAttempts {
			return nil, report, &apiError{StatusCode: resp.StatusCode, Body: response, Attempts: attempt}
		}
		backoff = geminiNextBackoff(backoff, randFloat)
	}
	panic("unreachable")
}

func geminiNextBackoff(backoff time.Duration, random func() float64) time.Duration {
	next := time.Duration(math.Min(float64(backoff*2), float64(time.Minute)))
	return time.Duration(math.Min(float64(next)*(0.5+random()), float64(time.Minute)))
}

func readGeminiResponse(body io.Reader, limit int) ([]byte, bool, error) {
	data, err := io.ReadAll(io.LimitReader(body, int64(limit)+1))
	if err != nil {
		return nil, false, err
	}
	return data, len(data) > limit, nil
}

func (c *GeminiClient) requestLimit() int {
	if c.requestByteLimit > 0 {
		return c.requestByteLimit
	}
	return geminiMaxRequestBytes
}
func (c *GeminiClient) decodedLimit() int {
	if c.decodedByteLimit > 0 {
		return c.decodedByteLimit
	}
	return geminiMaxDecodedBytes
}
func (c *GeminiClient) responseLimit() int {
	if c.responseByteLimit > 0 {
		return c.responseByteLimit
	}
	return geminiMaxResponseBytes
}
func (c *GeminiClient) pageLimitValue() int {
	if c.pageLimit > 0 {
		return c.pageLimit
	}
	return geminiMaxPDFPages
}

func (c *GeminiClient) effectiveDecodedLimit(fileType string) (int, error) {
	if size, err := c.requestBodySize(0, fileType, 1); err != nil {
		return 0, err
	} else if size > c.requestLimit() {
		return 0, fmt.Errorf("OCR request framing exceeds the %d-byte request limit", c.requestLimit())
	}
	low, high := 0, c.decodedLimit()
	for low < high {
		mid := low + (high-low+1)/2
		size, err := c.requestBodySize(mid, fileType, 1)
		if err != nil {
			return 0, err
		}
		if size <= c.requestLimit() {
			low = mid
		} else {
			high = mid - 1
		}
	}
	return low, nil
}

func (c *GeminiClient) requestBodySize(decoded int, fileType string, expectedPages int) (int, error) {
	if decoded < 0 {
		return 0, errors.New("negative decoded length")
	}
	body, err := marshalGeminiRequest("", fileType, expectedPages)
	if err != nil {
		return 0, err
	}
	encoded := base64.StdEncoding.EncodedLen(decoded)
	if encoded > int(^uint(0)>>1)-len(body) {
		return 0, errors.New("OCR request size overflows int")
	}
	return len(body) + encoded, nil
}

func (c *GeminiClient) buildRequestBody(data []byte, fileType string, expectedPages int) ([]byte, error) {
	body, err := marshalGeminiRequest(base64.StdEncoding.EncodeToString(data), fileType, expectedPages)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	expected, err := c.requestBodySize(len(data), fileType, expectedPages)
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

func geminiMIMEType(fileType string) (string, error) {
	switch fileType {
	case "pdf":
		return "application/pdf", nil
	case "jpeg":
		return "image/jpeg", nil
	case "png":
		return "image/png", nil
	default:
		return "", fmt.Errorf("unsupported file type: %s", fileType)
	}
}

func marshalGeminiRequest(encoded, fileType string, expectedPages int) ([]byte, error) {
	mime, err := geminiMIMEType(fileType)
	if err != nil {
		return nil, err
	}
	level := "MEDIA_RESOLUTION_HIGH"
	if fileType == "pdf" {
		level = "MEDIA_RESOLUTION_MEDIUM"
	}
	req := geminiRequest{Contents: []geminiContent{{Parts: []geminiPart{
		{InlineData: &geminiInlineData{MIMEType: mime, Data: encoded}, MediaResolution: &geminiMediaResolution{Level: level}},
		{Text: geminiOCRPrompt},
	}}}, GenerationConfig: geminiGenerationConfig{
		ThinkingConfig: &geminiThinkingConfig{ThinkingLevel: "LOW"}, ResponseMIMEType: "application/json", ResponseJSONSchema: geminiPageSchema(expectedPages), MaxOutputTokens: 65_536,
	}}
	return json.Marshal(req)
}

const geminiOCRPrompt = "Transcribe each page faithfully as Markdown in natural reading order, preserving headings, paragraphs, lists, tables, labels, handwriting, capitalization, and meaningful line breaks. Also thoroughly and factually describe the full page and every visual or design element for searchability, including images, diagrams, logos, and peripheral backgrounds, borders, and decoration."

type geminiRequest struct {
	Contents         []geminiContent        `json:"contents"`
	GenerationConfig geminiGenerationConfig `json:"generationConfig"`
}
type geminiContent struct {
	Parts []geminiPart `json:"parts"`
}
type geminiPart struct {
	Text            string                 `json:"text,omitempty"`
	InlineData      *geminiInlineData      `json:"inlineData,omitempty"`
	MediaResolution *geminiMediaResolution `json:"mediaResolution,omitempty"`
}
type geminiInlineData struct {
	MIMEType string `json:"mimeType"`
	Data     string `json:"data"`
}
type geminiMediaResolution struct {
	Level string `json:"level"`
}
type geminiGenerationConfig struct {
	ThinkingConfig     *geminiThinkingConfig `json:"thinkingConfig"`
	ResponseMIMEType   string                `json:"responseMimeType"`
	ResponseJSONSchema geminiSchema          `json:"responseJsonSchema"`
	MaxOutputTokens    int                   `json:"maxOutputTokens"`
}
type geminiThinkingConfig struct {
	ThinkingLevel string `json:"thinkingLevel"`
}
type geminiSchema struct {
	Type                 string                  `json:"type"`
	Properties           map[string]geminiSchema `json:"properties,omitempty"`
	Items                *geminiSchema           `json:"items,omitempty"`
	Required             []string                `json:"required,omitempty"`
	AdditionalProperties *bool                   `json:"additionalProperties,omitempty"`
	Minimum              *int                    `json:"minimum,omitempty"`
	Maximum              *int                    `json:"maximum,omitempty"`
	MinItems             *int                    `json:"minItems,omitempty"`
	MaxItems             *int                    `json:"maxItems,omitempty"`
}

func geminiPageSchema(expectedPages int) geminiSchema {
	noExtras := false
	minimumIndex, maximumIndex := 0, max(0, expectedPages-1)
	visual := geminiSchema{Type: "object", Properties: map[string]geminiSchema{"type": {Type: "string"}, "description": {Type: "string"}}, Required: []string{"type", "description"}, AdditionalProperties: &noExtras}
	page := geminiSchema{Type: "object", Properties: map[string]geminiSchema{"page_index": {Type: "integer", Minimum: &minimumIndex, Maximum: &maximumIndex}, "transcription": {Type: "string"}, "page_description": {Type: "string"}, "visual_elements": {Type: "array", Items: &visual}}, Required: []string{"page_index", "transcription", "page_description", "visual_elements"}, AdditionalProperties: &noExtras}
	// Each extracted PDF chunk is a standalone request. Requiring chunk-local
	// indexes lets validation prove completeness before callers add the absolute
	// document offset.
	return geminiSchema{Type: "object", Properties: map[string]geminiSchema{"pages": {Type: "array", Items: &page, MinItems: &expectedPages, MaxItems: &expectedPages}}, Required: []string{"pages"}, AdditionalProperties: &noExtras}
}

type geminiResponse struct {
	Candidates    []*geminiCandidate `json:"candidates"`
	UsageMetadata *geminiUsage       `json:"usageMetadata"`
}
type geminiCandidate struct {
	Content      *geminiContentResponse `json:"content"`
	FinishReason *string                `json:"finishReason"`
	Index        *int                   `json:"index"`
}
type geminiContentResponse struct {
	Parts []*geminiResponsePart `json:"parts"`
}
type geminiResponsePart struct {
	Text *string `json:"text"`
}
type geminiUsage struct {
	PromptTokenCount     *int64 `json:"promptTokenCount"`
	CandidatesTokenCount *int64 `json:"candidatesTokenCount"`
	ThoughtsTokenCount   *int64 `json:"thoughtsTokenCount"`
}
type geminiOCRResult struct {
	Pages []*geminiPage `json:"pages"`
}
type geminiPage struct {
	PageIndex       *int             `json:"page_index"`
	Transcription   *string          `json:"transcription"`
	PageDescription *string          `json:"page_description"`
	VisualElements  *[]*geminiVisual `json:"visual_elements"`
}
type geminiVisual struct {
	Type        *string `json:"type"`
	Description *string `json:"description"`
}

type geminiFinishError struct{ reason string }

func (e *geminiFinishError) Error() string {
	return fmt.Sprintf("invalid Gemini finish reason: %s", e.reason)
}
func isGeminiMaxTokens(err error) bool {
	var finish *geminiFinishError
	return errors.As(err, &finish) && finish.reason == "MAX_TOKENS"
}

func decodeGeminiResults(body []byte, expectedPages int) ([]PageResult, error) {
	var response geminiResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if len(response.Candidates) != 1 || response.Candidates[0] == nil {
		return nil, fmt.Errorf("expected exactly one usable candidate, got %d", len(response.Candidates))
	}
	candidate := response.Candidates[0]
	if candidate.FinishReason == nil {
		return nil, errors.New("candidate is missing finish reason")
	}
	if *candidate.FinishReason != "STOP" {
		return nil, &geminiFinishError{reason: *candidate.FinishReason}
	}
	if candidate.Index != nil && *candidate.Index != 0 {
		return nil, errors.New("candidate index must be 0 when present")
	}
	if candidate.Content == nil || len(candidate.Content.Parts) != 1 || candidate.Content.Parts[0] == nil || candidate.Content.Parts[0].Text == nil {
		return nil, errors.New("candidate must contain exactly one JSON text payload")
	}
	var payload geminiOCRResult
	if err := json.Unmarshal([]byte(*candidate.Content.Parts[0].Text), &payload); err != nil {
		return nil, fmt.Errorf("decode candidate JSON: %w", err)
	}
	actual := make([]string, 0, len(payload.Pages))
	pages := make(map[int]*geminiPage, len(payload.Pages))
	valid := true
	for _, page := range payload.Pages {
		if page == nil {
			actual = append(actual, "null")
			valid = false
			continue
		}
		if page.PageIndex == nil {
			actual = append(actual, "missing")
			valid = false
			continue
		}
		index := *page.PageIndex
		actual = append(actual, strconv.Itoa(index))
		if index < 0 || index >= expectedPages {
			valid = false
			continue
		}
		if _, exists := pages[index]; exists {
			valid = false
			continue
		}
		if page.Transcription == nil || page.PageDescription == nil || strings.TrimSpace(*page.PageDescription) == "" || page.VisualElements == nil {
			valid = false
			continue
		}
		for _, visual := range *page.VisualElements {
			if visual == nil || visual.Type == nil || visual.Description == nil || strings.TrimSpace(*visual.Type) == "" || strings.TrimSpace(*visual.Description) == "" {
				valid = false
				break
			}
		}
		pages[index] = page
	}
	if len(pages) != expectedPages {
		valid = false
	}
	if !valid {
		return nil, fmt.Errorf("invalid response page indexes or fields: expected %v, actual [%s]", expectedIndexes(expectedPages), strings.Join(actual, " "))
	}
	indexes := make([]int, 0, len(pages))
	for index := range pages {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	results := make([]PageResult, 0, len(indexes))
	for _, index := range indexes {
		results = append(results, PageResult{PageIndex: index, Markdown: renderGeminiMarkdown(pages[index])})
	}
	return results, nil
}

func expectedIndexes(count int) []int {
	indexes := make([]int, count)
	for i := range indexes {
		indexes[i] = i
	}
	return indexes
}
func renderGeminiMarkdown(page *geminiPage) string {
	markdown := *page.Transcription
	if markdown != "" {
		markdown += "\n\n"
	}
	markdown += "[Page: " + strings.TrimSpace(*page.PageDescription) + "]"
	for _, visual := range *page.VisualElements {
		markdown += "\n\n[Image: " + strings.TrimSpace(*visual.Type) + " — " + strings.TrimSpace(*visual.Description) + "]"
	}
	return markdown
}

func geminiBilling(body []byte, runAt time.Time) BillingReport {
	if report := geminiBillingIfPresent(body, runAt); report != nil {
		return *report
	}
	return BillingReport{Indeterminate: true}
}
func geminiBillingIfPresent(body []byte, runAt time.Time) *BillingReport {
	// Decode usage independently so malformed candidate output cannot hide an
	// otherwise authoritative charge from the same response.
	var envelope struct {
		UsageMetadata *geminiUsage `json:"usageMetadata"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.UsageMetadata == nil || envelope.UsageMetadata.PromptTokenCount == nil || envelope.UsageMetadata.CandidatesTokenCount == nil || envelope.UsageMetadata.ThoughtsTokenCount == nil {
		return nil
	}
	usage := envelope.UsageMetadata
	if *usage.PromptTokenCount < 0 || *usage.CandidatesTokenCount < 0 || *usage.ThoughtsTokenCount < 0 {
		return nil
	}
	report := BillingReport{KnownCost: GeminiCost(runAt, *usage.PromptTokenCount, *usage.CandidatesTokenCount+*usage.ThoughtsTokenCount)}
	return &report
}
