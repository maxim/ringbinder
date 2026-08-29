package ocr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
)

type PageResult struct {
	PageIndex int
	Markdown  string
	Model     string
}

type RangeResult struct {
	Pages   []PageResult
	Billing BillingReport
}

// Provider processes only a requested source-page range so successful pages
// from earlier models or runs never need to be sent over the network again.
type Provider interface {
	OCRRangeResult(
		ctx context.Context,
		filePath string,
		fileType string,
		start int,
		end int,
	) (RangeResult, error)
}

type FailureKind string

const (
	FailureUnclassified FailureKind = "unclassified"
	FailureTemporary    FailureKind = "temporary"
	FailureDocument     FailureKind = "document"
	FailureGlobal       FailureKind = "global"
)

type Failure struct {
	Kind FailureKind
	Err  error
}

func (e *Failure) Error() string { return e.Err.Error() }
func (e *Failure) Unwrap() error { return e.Err }

func DocumentFailure(err error) error {
	if err == nil {
		return nil
	}
	return &Failure{Kind: FailureDocument, Err: err}
}

func TemporaryFailure(err error) error {
	if err == nil {
		return nil
	}
	return &Failure{Kind: FailureTemporary, Err: err}
}

func GlobalFailure(err error) error {
	if err == nil {
		return nil
	}
	return &Failure{Kind: FailureGlobal, Err: err}
}

// Fallback is deliberately conservative because advancing the chain can incur
// another provider charge. Only verified permanent document/content failures
// advance; temporary and ambiguous failures stay pending for a later run.
func ClassifyFailure(err error) FailureKind {
	if err == nil {
		return FailureUnclassified
	}
	var failure *Failure
	if errors.As(err, &failure) {
		return failure.Kind
	}
	if errors.Is(err, context.Canceled) {
		return FailureGlobal
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return FailureTemporary
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return FailureTemporary
	}

	var rangeSize *GeminiRangeSizeError
	if errors.As(err, &rangeSize) {
		return FailureDocument
	}

	var promptBlock *geminiPromptBlockError
	if errors.As(err, &promptBlock) {
		switch promptBlock.reason {
		case "SAFETY", "BLOCKLIST", "PROHIBITED_CONTENT", "IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT":
			return FailureDocument
		default:
			return FailureUnclassified
		}
	}

	var finish *geminiFinishError
	if errors.As(err, &finish) {
		switch finish.reason {
		case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "MAX_TOKENS",
			"IMAGE_SAFETY", "IMAGE_PROHIBITED_CONTENT":
			return FailureDocument
		default:
			return FailureUnclassified
		}
	}

	var apiErr *apiError
	if !errors.As(err, &apiErr) {
		return FailureUnclassified
	}
	if apiErr.StatusCode == http.StatusRequestTimeout ||
		apiErr.StatusCode == http.StatusTooManyRequests ||
		apiErr.StatusCode >= http.StatusInternalServerError {
		return FailureTemporary
	}
	if apiErr.StatusCode == http.StatusUnauthorized ||
		apiErr.StatusCode == http.StatusForbidden ||
		apiErr.StatusCode == http.StatusPaymentRequired ||
		structuredGlobalError(apiErr.Body) {
		return FailureGlobal
	}
	if structuredDocumentError(apiErr.Body) {
		return FailureDocument
	}
	return FailureUnclassified
}

type providerErrorBody struct {
	Error *struct {
		Code    any    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
		Type    string `json:"type"`
		Param   string `json:"param"`
	} `json:"error"`
	Code    any    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
	Type    string `json:"type"`
	Param   string `json:"param"`
}

func structuredGlobalError(body []byte) bool {
	code, message, status, kind, param, ok := providerErrorFields(body)
	if !ok {
		return false
	}
	joined := strings.ToLower(strings.Join([]string{code, message, status, kind, param}, " "))
	return containsAny(joined,
		"authentication", "permission", "api key", "credential", "billing",
		"quota project", "account suspended", "unknown_model", "unknown model",
		"model_not_found", "model not found",
	) || (strings.Contains(joined, "models/") && strings.Contains(joined, "not found"))
}

func structuredDocumentError(body []byte) bool {
	code, message, status, kind, param, ok := providerErrorFields(body)
	if !ok {
		return false
	}
	joined := strings.ToLower(strings.Join([]string{code, message, status, kind, param}, " "))
	// These terms must come from structured provider fields. A bare generic HTTP
	// status or arbitrary malformed success body is intentionally insufficient.
	return containsAny(joined,
		"safety", "recitation", "prohibited_content", "blocked content",
		"document rejected", "unsupported document", "invalid document",
		"corrupt document", "file content", "page content",
	)
}

func providerErrorFields(body []byte) (string, string, string, string, string, bool) {
	var envelope providerErrorBody
	if err := json.Unmarshal(body, &envelope); err != nil {
		return "", "", "", "", "", false
	}
	if envelope.Error != nil {
		return fmt.Sprint(envelope.Error.Code), envelope.Error.Message,
			envelope.Error.Status, envelope.Error.Type, envelope.Error.Param, true
	}
	return fmt.Sprint(envelope.Code), envelope.Message, envelope.Status,
		envelope.Type, envelope.Param, envelope.Message != "" || envelope.Code != nil
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}
