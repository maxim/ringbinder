package scanner

import (
	"context"
	"path/filepath"
	"strings"
	"time"
)

type FileInfo struct {
	Path        string
	ModTime     time.Time
	Size        int64
	ContentType string // "pdf", "jpeg", "png"
}

type Scanner interface {
	Scan(ctx context.Context, paths []string, results chan<- FileInfo) error
}

var supportedExts = map[string]string{
	".pdf":  "pdf",
	".png":  "png",
	".jpg":  "jpeg",
	".jpeg": "jpeg",
}

func classifyFile(path string) (string, bool) {
	ext := strings.ToLower(filepath.Ext(path))
	ct, ok := supportedExts[ext]
	return ct, ok
}
