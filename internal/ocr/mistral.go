package ocr

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	mistralEndpoint     = "https://api.mistral.ai/v1/ocr"
	mistralModel        = "mistral-ocr-latest"
	mistralPricePerPage = 0.002 // $2 per 1,000 pages
	maxAttempts         = 5
)

type MistralClient struct {
	apiKey      string
	httpClient  *http.Client
	endpoint    string
	sleep       func(context.Context, time.Duration) error
	randFloat64 func() float64
}

func NewMistralClient(apiKey string) *MistralClient {
	return &MistralClient{
		apiKey:      apiKey,
		httpClient:  &http.Client{Timeout: 5 * time.Minute},
		endpoint:    mistralEndpoint,
		sleep:       sleepWithContext,
		randFloat64: rand.Float64,
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

const maxFileSize = 200 * 1024 * 1024 // 200 MB

func (c *MistralClient) OCRFile(ctx context.Context, filePath string, fileType string) ([]PageResult, error) {
	info, err := os.Stat(filePath)
	if err != nil {
		return nil, fmt.Errorf("stat file: %w", err)
	}
	if info.Size() > maxFileSize {
		return nil, fmt.Errorf("file too large (%d MB, max %d MB)", info.Size()/(1024*1024), maxFileSize/(1024*1024))
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("read file: %w", err)
	}

	b64 := base64.StdEncoding.EncodeToString(data)

	var req mistralRequest
	req.Model = mistralModel

	switch fileType {
	case "pdf":
		req.Document = mistralDocument{
			Type:        "document_url",
			DocumentURL: "data:application/pdf;base64," + b64,
		}
	case "jpeg":
		req.Document = mistralDocument{
			Type:     "image_url",
			ImageURL: "data:image/jpeg;base64," + b64,
		}
	case "png":
		req.Document = mistralDocument{
			Type:     "image_url",
			ImageURL: "data:image/png;base64," + b64,
		}
	default:
		return nil, fmt.Errorf("unsupported file type: %s", fileType)
	}

	respBody, err := c.doWithRetry(ctx, req)
	if err != nil {
		return nil, err
	}

	var resp mistralResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	results := make([]PageResult, len(resp.Pages))
	for i, page := range resp.Pages {
		var annotations json.RawMessage
		if page.Dimensions != nil {
			annotations, _ = json.Marshal(page.Dimensions)
		}
		results[i] = PageResult{
			PageIndex:   page.Index,
			Markdown:    page.Markdown,
			Annotations: annotations,
		}
	}

	return results, nil
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

func (c *MistralClient) doWithRetry(ctx context.Context, req mistralRequest) ([]byte, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}

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

	backoff := 1.0 // seconds
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			if err := sleep(ctx, time.Duration(backoff*float64(time.Second))); err != nil {
				return nil, err
			}
		}

		httpReq, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

		resp, err := c.httpClient.Do(httpReq)
		if err != nil {
			return nil, fmt.Errorf("http request: %w", err)
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
		if retryable {
			if attempt == maxAttempts {
				errMsg := strings.TrimSpace(string(respBody))
				if len(errMsg) > 200 {
					errMsg = errMsg[:200]
				}
				return nil, fmt.Errorf("API error %d after %d attempts: %s", resp.StatusCode, maxAttempts, errMsg)
			}

			nextBackoff := math.Min(backoff*2, 60)
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if secs, err := strconv.ParseFloat(ra, 64); err == nil {
					if secs < 0 {
						secs = 0
					}
					nextBackoff = math.Min(secs, 60)
				}
			}
			backoff = math.Min(nextBackoff*(0.5+randFloat64()), 60)
			continue
		}

		// Extract error message
		errMsg := strings.TrimSpace(string(respBody))
		if len(errMsg) > 200 {
			errMsg = errMsg[:200]
		}
		return nil, fmt.Errorf("API error %d: %s", resp.StatusCode, errMsg)
	}

	return nil, fmt.Errorf("max attempts exceeded")
}
