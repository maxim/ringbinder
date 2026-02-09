package ocr

import (
	"context"
)

type PageResult struct {
	PageIndex int
	Markdown  string
}

type Provider interface {
	OCRFile(ctx context.Context, filePath string, fileType string) ([]PageResult, error)
	PricePerPage() float64
}
