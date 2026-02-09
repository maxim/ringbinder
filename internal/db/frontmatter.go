package db

import (
	"fmt"
	"path/filepath"
	"strings"
)

func BuildPageMarkdown(path string, pageIndex int, body string) string {
	filename := filepath.Base(path)
	fileType := strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")

	return fmt.Sprintf(
		"---\nfile: %s\ntype: %s\npath: %s\npage: %d\n---\n\n%s",
		filename,
		fileType,
		path,
		pageIndex+1,
		body,
	)
}
