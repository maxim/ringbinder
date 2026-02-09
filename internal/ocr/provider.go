package ocr

import (
	"context"
	"encoding/json"
)

type PageResult struct {
	PageIndex   int
	Markdown    string
	Annotations json.RawMessage
}

type Provider interface {
	OCRFile(ctx context.Context, filePath string, fileType string) ([]PageResult, error)
	PricePerPage() float64
}
