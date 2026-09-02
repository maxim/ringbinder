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
	geminiModel       = "gemini-3.8-flash"
	geminiEndpoint    = "https://generativelanguage.googleapis.com/v1beta/models/" + geminiModel + ":generateContent"
	GeminiDirectModel = geminiModel

	geminiMaxPDFPages      = 20
	geminiMaxRequestBytes  = 95 * 1024 * 1024
	geminiMaxDecodedBytes  = 45 * 1024 * 1024
	geminiMaxResponseBytes = 16 * 1024 * 1024

	GeminiBatchModel = geminiModel
	// Reserve 100 MB below Gemini's 2 GB uploaded-input limit so framing and
	// provider-side size accounting cannot turn an exactly packed job invalid.
	GeminiBatchMaxInputBytes    = int64(1_900_000_000)
	GeminiBatchMaxResponseBytes = geminiMaxResponseBytes
)

// GeminiClient uses the REST API directly so the OCR provider has no SDK
// dependency and its request and billing behavior remain explicit.
type GeminiPreparedRequest struct {
	PageStart int
	PageEnd   int
	Body      []byte
}

type GeminiDecodedResult struct {
	Pages        []PageResult
	InputTokens  *int64
	OutputTokens *int64
	Billing      BillingReport
}

type GeminiPlanningError struct {
	PageStart int
	PageEnd   int
	Cause     error
}

func (e *GeminiPlanningError) Error() string {
	if e.PageEnd > e.PageStart {
		return fmt.Sprintf(
			"cannot plan Gemini request for %s: %v",
			formatPageRange(e.PageStart, e.PageEnd), e.Cause,
		)
	}
	return fmt.Sprintf("cannot plan Gemini request at source page %d: %v", e.PageStart+1, e.Cause)
}

func (e *GeminiPlanningError) Unwrap() error { return e.Cause }

type GeminiRangeSizeError struct {
	PageStart int
	PageEnd   int
	Cause     error
}

func (e *GeminiRangeSizeError) Error() string {
	return fmt.Sprintf("%s exceeds Gemini request limits: %v", formatPageRange(e.PageStart, e.PageEnd), e.Cause)
}

func (e *GeminiRangeSizeError) Unwrap() error { return e.Cause }

func IsGeminiRangeSizeError(err error) bool {
	var sizeError *GeminiRangeSizeError
	return errors.As(err, &sizeError)
}

type geminiRequestSizeError struct {
	message string
}

func (e *geminiRequestSizeError) Error() string { return e.message }

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

func (c *GeminiClient) PrepareFileRequests(ctx context.Context, filePath, fileType string) ([]GeminiPreparedRequest, error) {
	var requests []GeminiPreparedRequest
	var rejected error
	err := c.WalkFileRequests(
		ctx,
		filePath,
		fileType,
		func(request GeminiPreparedRequest) error {
			requests = append(requests, request)
			return nil
		},
		func(sizeErr *GeminiRangeSizeError) error {
			if rejected == nil {
				rejected = sizeErr
			}
			return nil
		},
	)
	if err != nil {
		return requests, err
	}
	return requests, rejected
}

func (c *GeminiClient) WalkFileRequests(
	ctx context.Context,
	filePath, fileType string,
	yield func(GeminiPreparedRequest) error,
	reject func(*GeminiRangeSizeError) error,
) error {
	if _, err := geminiMIMEType(fileType); err != nil {
		return err
	}
	info, err := os.Stat(filePath)
	if err != nil {
		return fmt.Errorf("stat file: %w", err)
	}
	if fileType != "pdf" {
		request, err := c.prepareImageRequest(ctx, filePath, fileType, info.Size())
		if err != nil {
			var sizeErr *GeminiRangeSizeError
			if reject != nil && errors.As(err, &sizeErr) {
				return reject(sizeErr)
			}
			return err
		}
		return yield(request)
	}

	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open file: %w", err)
	}
	defer f.Close()
	pageCount := c.pageCount
	if pageCount == nil {
		pageCount = pdfutil.PageCountContext
	}
	pages, err := pageCount(ctx, f)
	if err != nil {
		return fmt.Errorf("%s: read PDF page count: %w", filePath, err)
	}
	if pages < 1 {
		return fmt.Errorf("%s: PDF contains no pages", filePath)
	}

	for start := 0; start < pages; {
		chunk, err := c.planPDFChunk(ctx, f, info.Size(), pages, start, pages)
		if err != nil {
			var sizeErr *GeminiRangeSizeError
			if reject != nil && errors.As(err, &sizeErr) {
				if rejectErr := reject(sizeErr); rejectErr != nil {
					return rejectErr
				}
				start = sizeErr.PageEnd
				continue
			}
			cause := fmt.Errorf("%s, %s: %w", filePath, formatPageRange(start, min(pages, start+c.pageLimitValue())), err)
			return &GeminiPlanningError{
				PageStart: start,
				PageEnd:   min(pages, start+c.pageLimitValue()),
				Cause:     cause,
			}
		}
		body, err := c.buildRequestBody(chunk.data, "pdf", chunk.end-chunk.start)
		if err != nil {
			cause := fmt.Errorf("%s, %s: %w", filePath, formatPageRange(chunk.start, chunk.end), err)
			return &GeminiPlanningError{PageStart: chunk.start, PageEnd: chunk.end, Cause: cause}
		}
		if err := yield(GeminiPreparedRequest{PageStart: chunk.start, PageEnd: chunk.end, Body: body}); err != nil {
			return err
		}
		start = chunk.end
	}
	return nil
}

func (c *GeminiClient) PrepareRangeRequest(
	ctx context.Context,
	filePath, fileType string,
	start, end int,
) (GeminiPreparedRequest, error) {
	if start < 0 || end <= start {
		return GeminiPreparedRequest{}, errors.New("invalid Gemini page range")
	}
	if fileType != "pdf" {
		if start != 0 || end != 1 {
			return GeminiPreparedRequest{}, errors.New("image OCR range must be page 1")
		}
		info, err := os.Stat(filePath)
		if err != nil {
			return GeminiPreparedRequest{}, fmt.Errorf("stat file: %w", err)
		}
		return c.prepareImageRequest(ctx, filePath, fileType, info.Size())
	}

	if end-start > c.pageLimitValue() {
		return GeminiPreparedRequest{}, &GeminiRangeSizeError{
			PageStart: start,
			PageEnd:   end,
			Cause:     fmt.Errorf("range exceeds the %d-page request limit", c.pageLimitValue()),
		}
	}
	info, err := os.Stat(filePath)
	if err != nil {
		return GeminiPreparedRequest{}, fmt.Errorf("stat file: %w", err)
	}
	f, err := os.Open(filePath)
	if err != nil {
		return GeminiPreparedRequest{}, fmt.Errorf("open file: %w", err)
	}
	defer f.Close()
	pages, err := c.pageCount(ctx, f)
	if err != nil {
		return GeminiPreparedRequest{}, fmt.Errorf("%s: read PDF page count: %w", filePath, err)
	}
	if end > pages {
		return GeminiPreparedRequest{}, fmt.Errorf("planned page range exceeds current PDF page count %d", pages)
	}
	chunk, original, err := c.originalPDFChunk(ctx, f, info.Size(), pages, start, end)
	if err != nil {
		return GeminiPreparedRequest{}, err
	}
	if original {
		body, err := c.buildRequestBody(chunk.data, "pdf", end-start)
		if err != nil {
			return GeminiPreparedRequest{}, err
		}
		return GeminiPreparedRequest{PageStart: start, PageEnd: end, Body: body}, nil
	}
	limit, err := c.effectiveDecodedLimit("pdf")
	if err != nil {
		var sizeErr *geminiRequestSizeError
		if errors.As(err, &sizeErr) {
			return GeminiPreparedRequest{}, &GeminiRangeSizeError{PageStart: start, PageEnd: end, Cause: err}
		}
		return GeminiPreparedRequest{}, err
	}
	extract := c.extractRange
	if extract == nil {
		extract = pdfutil.ExtractRange
	}
	data, err := extract(ctx, f, start, end, limit)
	if err != nil {
		if errors.Is(err, pdfutil.ErrRangeTooLarge) {
			return GeminiPreparedRequest{}, &GeminiRangeSizeError{PageStart: start, PageEnd: end, Cause: err}
		}
		return GeminiPreparedRequest{}, err
	}
	body, err := c.buildRequestBody(data, "pdf", end-start)
	if err != nil {
		var sizeErr *geminiRequestSizeError
		if errors.As(err, &sizeErr) {
			return GeminiPreparedRequest{}, &GeminiRangeSizeError{PageStart: start, PageEnd: end, Cause: err}
		}
		return GeminiPreparedRequest{}, err
	}
	return GeminiPreparedRequest{PageStart: start, PageEnd: end, Body: body}, nil
}

func (c *GeminiClient) prepareImageRequest(
	ctx context.Context,
	filePath, fileType string,
	sourceSize int64,
) (GeminiPreparedRequest, error) {
	limit, err := c.effectiveDecodedLimit(fileType)
	if err != nil {
		return GeminiPreparedRequest{}, fmt.Errorf("%s: %w", filePath, err)
	}
	if sourceSize > int64(limit) {
		return GeminiPreparedRequest{}, &GeminiRangeSizeError{
			PageStart: 0,
			PageEnd:   1,
			Cause:     fmt.Errorf("oversized %s image cannot be transformed", fileType),
		}
	}
	f, err := os.Open(filePath)
	if err != nil {
		return GeminiPreparedRequest{}, fmt.Errorf("open file: %w", err)
	}
	defer f.Close()
	data, tooLarge, err := readLimited(ctx, f, limit)
	if err != nil {
		return GeminiPreparedRequest{}, fmt.Errorf("%s: read file: %w", filePath, err)
	}
	if tooLarge {
		return GeminiPreparedRequest{}, &GeminiRangeSizeError{
			PageStart: 0,
			PageEnd:   1,
			Cause:     fmt.Errorf("oversized %s image cannot be transformed", fileType),
		}
	}
	body, err := c.buildRequestBody(data, fileType, 1)
	if err != nil {
		return GeminiPreparedRequest{}, fmt.Errorf("%s: %w", filePath, err)
	}
	return GeminiPreparedRequest{PageStart: 0, PageEnd: 1, Body: body}, nil
}

func (c *GeminiClient) OCRRange(
	ctx context.Context,
	filePath, fileType string,
	start, end int,
) ([]PageResult, BillingReport, error) {
	if start < 0 || end <= start {
		return nil, BillingReport{}, errors.New("invalid Gemini page range")
	}
	if fileType != "pdf" {
		if start != 0 || end != 1 {
			return nil, BillingReport{}, errors.New("image OCR range must be page 1")
		}
		return c.OCRFile(ctx, filePath, fileType)
	}
	info, err := os.Stat(filePath)
	if err != nil {
		return nil, BillingReport{}, fmt.Errorf("stat file: %w", err)
	}
	f, err := os.Open(filePath)
	if err != nil {
		return nil, BillingReport{}, fmt.Errorf("open file: %w", err)
	}
	defer f.Close()
	pages, err := c.pageCount(ctx, f)
	if err != nil {
		return nil, BillingReport{}, fmt.Errorf("%s: read PDF page count: %w", filePath, err)
	}
	if end > pages {
		return nil, BillingReport{}, fmt.Errorf("planned page range exceeds current PDF page count %d", pages)
	}
	var results []PageResult
	var report BillingReport
	if err := c.ocrPDFInterval(ctx, f, filePath, info.Size(), pages, start, end, &results, &report); err != nil {
		return results, report, err
	}
	return results, report, nil
}

func (c *GeminiClient) OCRRangeResult(
	ctx context.Context,
	filePath, fileType string,
	start, end int,
) (RangeResult, error) {
	pages, report, err := c.OCRRange(ctx, filePath, fileType, start, end)
	return RangeResult{Pages: pages, Billing: report}, err
}

func DecodeGeminiBatchResult(
	body []byte,
	expectedPages int,
	fallbackModel string,
	prices GeminiTokenPrices,
) (GeminiDecodedResult, error) {
	pages, err := decodeGeminiResultsWithModel(body, expectedPages, fallbackModel)
	usage := geminiUsageFromBody(body)
	result := GeminiDecodedResult{Pages: pages, Billing: BillingReport{Indeterminate: usage == nil}}
	if usage != nil {
		input := *usage.PromptTokenCount
		output := *usage.CandidatesTokenCount + *usage.ThoughtsTokenCount
		result.InputTokens = &input
		result.OutputTokens = &output
		result.Billing.KnownCost = GeminiCostWithPrices(prices, input, output)
	}
	return result, err
}

func IsGeminiMaxTokensError(err error) bool {
	return isGeminiMaxTokens(err)
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
		return nil, BillingReport{}, DocumentFailure(fmt.Errorf("%s: oversized %s image cannot be transformed: inline OCR request exceeds %d bytes", filePath, fileType, c.requestLimit()))
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
		return nil, BillingReport{}, DocumentFailure(fmt.Errorf("%s: oversized %s image cannot be transformed: inline OCR request exceeds %d bytes", filePath, fileType, c.requestLimit()))
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

func (c *GeminiClient) originalPDFChunk(
	ctx context.Context,
	source io.ReadSeeker,
	sourceSize int64,
	totalPages, start, end int,
) (pdfChunk, bool, error) {
	if totalPages < 1 || totalPages > c.pageLimitValue() || start != 0 || end != totalPages {
		return pdfChunk{}, false, nil
	}
	limit, err := c.effectiveDecodedLimit("pdf")
	if err != nil {
		return pdfChunk{}, false, err
	}
	if sourceSize > int64(limit) {
		return pdfChunk{}, false, nil
	}
	data, tooLarge, err := readLimited(ctx, source, limit)
	if err != nil {
		return pdfChunk{}, false, err
	}
	// readLimited leaves successful reads at EOF. Later adaptive extraction is
	// safe because internal/pdf.readContext rewinds; post-read misses rewind here
	// defensively before returning to the extraction planner.
	rewindMiss := func() (pdfChunk, bool, error) {
		if _, err := source.Seek(0, io.SeekStart); err != nil {
			return pdfChunk{}, false, err
		}
		return pdfChunk{}, false, nil
	}
	if tooLarge {
		return rewindMiss()
	}
	size, err := c.requestBodySize(len(data), "pdf", totalPages)
	if err != nil {
		return pdfChunk{}, false, err
	}
	if size > c.requestLimit() {
		return rewindMiss()
	}
	return pdfChunk{start: 0, end: totalPages, data: data}, true, nil
}

func (c *GeminiClient) planPDFChunk(ctx context.Context, source io.ReadSeeker, sourceSize int64, totalPages, start, intervalEnd int) (pdfChunk, error) {
	if chunk, original, err := c.originalPDFChunk(ctx, source, sourceSize, totalPages, start, intervalEnd); err != nil {
		return pdfChunk{}, err
	} else if original {
		return chunk, nil
	}
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
		if size > c.requestLimit() {
			return pdfChunk{}, false, nil
		}
		return pdfChunk{start: start, end: candidateEnd, data: data}, true, nil
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
		cause := fmt.Errorf("source page %d exceeds the decoded or serialized OCR request limit", start+1)
		return pdfChunk{}, &GeminiRangeSizeError{PageStart: start, PageEnd: start + 1, Cause: cause}
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
		if ClassifyFailure(err) == FailureDocument || semanticAttempt == 1 {
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
			backoff = geminiNextBackoff(backoff, "", time.Now(), randFloat)
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
				return nil, report, TemporaryFailure(fmt.Errorf("read response after %d attempts: %w", maxAttempts, readErr))
			}
			backoff = geminiNextBackoff(backoff, "", time.Now(), randFloat)
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
		retryable := resp.StatusCode == http.StatusRequestTimeout ||
			resp.StatusCode == http.StatusTooManyRequests ||
			(resp.StatusCode >= 500 && resp.StatusCode < 600)
		if resp.StatusCode >= 500 && usage == nil {
			report.Indeterminate = true
		}
		if !retryable || attempt == maxAttempts {
			return nil, report, &apiError{StatusCode: resp.StatusCode, Body: response, Attempts: attempt}
		}
		backoff = geminiNextBackoff(
			backoff, resp.Header.Get("Retry-After"), time.Now(), randFloat,
		)
	}
	panic("unreachable")
}

// Keep provider-requested delays as a floor after jittering local backoff.
// Both are capped so one response cannot make this bounded command wait indefinitely.
func geminiNextBackoff(
	backoff time.Duration,
	retryAfter string,
	now time.Time,
	random func() float64,
) time.Duration {
	next := time.Duration(math.Min(float64(backoff*2), float64(time.Minute)))
	delay := time.Duration(math.Min(float64(next)*(0.5+random()), float64(time.Minute)))
	retryDelay, ok := geminiRetryAfterDelay(retryAfter, now)
	if ok && retryDelay > delay {
		return retryDelay
	}
	return delay
}

func geminiRetryAfterDelay(value string, now time.Time) (time.Duration, bool) {
	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil {
		date, dateErr := http.ParseTime(value)
		if dateErr != nil {
			return 0, false
		}
		seconds = date.Sub(now).Seconds()
	}
	if math.IsNaN(seconds) {
		return 0, false
	}
	seconds = math.Max(0, math.Min(seconds, 60))
	return time.Duration(seconds * float64(time.Second)), true
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
		return 0, &geminiRequestSizeError{message: "OCR request size overflows int"}
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
		return nil, &geminiRequestSizeError{message: fmt.Sprintf(
			"serialized OCR request is %d bytes, limit %d", len(body), c.requestLimit(),
		)}
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
	Candidates     []*geminiCandidate    `json:"candidates"`
	UsageMetadata  *geminiUsage          `json:"usageMetadata"`
	ModelVersion   string                `json:"modelVersion"`
	PromptFeedback *geminiPromptFeedback `json:"promptFeedback"`
}

type geminiPromptFeedback struct {
	BlockReason string `json:"blockReason"`
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

type geminiPromptBlockError struct{ reason string }

func (e *geminiPromptBlockError) Error() string {
	return fmt.Sprintf("Gemini blocked the prompt with reason %s", e.reason)
}

func (e *geminiFinishError) Error() string {
	if e.reason == "RECITATION" {
		return "Gemini stopped generation for potential recitation (RECITATION)"
	}
	return fmt.Sprintf("Gemini stopped generation with finish reason %s", e.reason)
}
func isGeminiMaxTokens(err error) bool {
	var finish *geminiFinishError
	return errors.As(err, &finish) && finish.reason == "MAX_TOKENS"
}

func decodeGeminiResults(body []byte, expectedPages int) ([]PageResult, error) {
	return decodeGeminiResultsWithModel(body, expectedPages, geminiModel)
}

func decodeGeminiResultsWithModel(body []byte, expectedPages int, fallbackModel string) ([]PageResult, error) {
	var response geminiResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if response.PromptFeedback != nil &&
		response.PromptFeedback.BlockReason != "" &&
		response.PromptFeedback.BlockReason != "BLOCK_REASON_UNSPECIFIED" {
		return nil, &geminiPromptBlockError{reason: response.PromptFeedback.BlockReason}
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
	model := strings.TrimSpace(response.ModelVersion)
	if model == "" {
		model = strings.TrimSpace(fallbackModel)
		if model == "" {
			return nil, errors.New("Gemini response omitted modelVersion and no fallback model was provided")
		}
	}
	indexes := make([]int, 0, len(pages))
	for index := range pages {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	results := make([]PageResult, 0, len(indexes))
	for _, index := range indexes {
		results = append(results, PageResult{
			PageIndex: index,
			Markdown:  renderGeminiMarkdown(pages[index]),
			Model:     model,
		})
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
	usage := geminiUsageFromBody(body)
	if usage == nil {
		return nil
	}
	report := BillingReport{KnownCost: GeminiCost(runAt, *usage.PromptTokenCount, *usage.CandidatesTokenCount+*usage.ThoughtsTokenCount)}
	return &report
}

func geminiUsageFromBody(body []byte) *geminiUsage {
	// Decode usage independently so malformed candidate output cannot hide an
	// otherwise authoritative charge from the same response.
	var envelope struct {
		UsageMetadata *geminiUsage `json:"usageMetadata"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || envelope.UsageMetadata == nil || envelope.UsageMetadata.PromptTokenCount == nil {
		return nil
	}
	usage := envelope.UsageMetadata
	zero := int64(0)
	if usage.CandidatesTokenCount == nil {
		usage.CandidatesTokenCount = &zero
	}
	if usage.ThoughtsTokenCount == nil {
		usage.ThoughtsTokenCount = &zero
	}
	if *usage.PromptTokenCount < 0 || *usage.CandidatesTokenCount < 0 || *usage.ThoughtsTokenCount < 0 {
		return nil
	}
	return usage
}
