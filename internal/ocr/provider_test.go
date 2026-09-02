package ocr

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func TestClassifyFailure(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want FailureKind
	}{
		{name: "typed temporary", err: TemporaryFailure(errors.New("timeout")), want: FailureTemporary},
		{name: "typed document", err: DocumentFailure(errors.New("refusal")), want: FailureDocument},
		{name: "typed global", err: GlobalFailure(errors.New("account")), want: FailureGlobal},
		{name: "408", err: &apiError{StatusCode: http.StatusRequestTimeout}, want: FailureTemporary},
		{name: "429", err: &apiError{StatusCode: http.StatusTooManyRequests}, want: FailureTemporary},
		{name: "5xx", err: &apiError{StatusCode: http.StatusBadGateway}, want: FailureTemporary},
		{name: "401", err: &apiError{StatusCode: http.StatusUnauthorized}, want: FailureGlobal},
		{name: "403", err: &apiError{StatusCode: http.StatusForbidden}, want: FailureGlobal},
		{name: "unknown model", err: &apiError{StatusCode: 400, Body: []byte(`{"object":"error","code":"unknown_model","param":"model"}`)}, want: FailureGlobal},
		{name: "Gemini model not found", err: &apiError{StatusCode: 404, Body: []byte(`{"error":{"status":"NOT_FOUND","message":"models/missing is not found","code":"model_not_found"}}`)}, want: FailureGlobal},
		{name: "billing", err: &apiError{StatusCode: 400, Body: []byte(`{"error":{"status":"FAILED_PRECONDITION","message":"billing account disabled"}}`)}, want: FailureGlobal},
		{name: "structured safety", err: &apiError{StatusCode: 400, Body: []byte(`{"error":{"status":"SAFETY","message":"blocked content"}}`)}, want: FailureDocument},
		{name: "Gemini prompt safety", err: &geminiPromptBlockError{reason: "SAFETY"}, want: FailureDocument},
		{name: "Gemini image safety finish", err: &geminiFinishError{reason: "IMAGE_SAFETY"}, want: FailureDocument},
		{name: "Gemini safety finish", err: &geminiFinishError{reason: "SAFETY"}, want: FailureDocument},
		{name: "Gemini recitation finish", err: &geminiFinishError{reason: "RECITATION"}, want: FailureDocument},
		{name: "local range size", err: &GeminiRangeSizeError{PageStart: 0, PageEnd: 1, Cause: errors.New("local limit")}, want: FailureDocument},
		{name: "malformed success", err: errors.New("decode candidate JSON"), want: FailureUnclassified},
	}
	for _, status := range []int{400, 413, 415, 422} {
		tests = append(tests, struct {
			name string
			err  error
			want FailureKind
		}{name: fmt.Sprintf("generic %d", status), err: &apiError{StatusCode: status, Body: []byte(`{"error":{"message":"invalid request"}}`)}, want: FailureUnclassified})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ClassifyFailure(test.err); got != test.want {
				t.Fatalf("ClassifyFailure() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestDecodeGeminiPromptSafetyBlockIsDocumentFailure(t *testing.T) {
	_, err := decodeGeminiResults([]byte(`{
		"promptFeedback":{"blockReason":"PROHIBITED_CONTENT"},
		"usageMetadata":{"promptTokenCount":1}
	}`), 1)
	if ClassifyFailure(err) != FailureDocument {
		t.Fatalf("error = %v, classification = %q", err, ClassifyFailure(err))
	}
}

func TestResponseReportedModelTakesPrecedence(t *testing.T) {
	mistralBody := []byte(`{
		"model":"mistral-concrete-version",
		"pages":[{"index":0,"markdown":"text","images":[]}],
		"usage_info":{"pages_processed":1}
	}`)
	mistralPages, err := decodeResults(mistralBody, 1)
	if err != nil {
		t.Fatal(err)
	}
	if mistralPages[0].Model != "mistral-concrete-version" {
		t.Fatalf("Mistral model = %q", mistralPages[0].Model)
	}

	payload := `{"pages":[{"page_index":0,"transcription":"text","page_description":"page","visual_elements":[]}]}`
	geminiBody := []byte(`{
		"modelVersion":"gemini-concrete-version",
		"candidates":[{"content":{"parts":[{"text":` + fmt.Sprintf("%q", payload) + `}]},"finishReason":"STOP","index":0}],
		"usageMetadata":{"promptTokenCount":1}
	}`)
	geminiPages, err := decodeGeminiResults(geminiBody, 1)
	if err != nil {
		t.Fatal(err)
	}
	if geminiPages[0].Model != "gemini-concrete-version" {
		t.Fatalf("Gemini model = %q", geminiPages[0].Model)
	}

	geminiBody = []byte(`{
		"candidates":[{"content":{"parts":[{"text":` + fmt.Sprintf("%q", payload) + `}]},"finishReason":"STOP","index":0}],
		"usageMetadata":{"promptTokenCount":1}
	}`)
	geminiPages, err = decodeGeminiResults(geminiBody, 1)
	if err != nil {
		t.Fatal(err)
	}
	if geminiPages[0].Model != GeminiDirectModel {
		t.Fatalf("Gemini fallback model = %q, want %q", geminiPages[0].Model, GeminiDirectModel)
	}
}
