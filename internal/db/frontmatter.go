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

func StripPageFrontmatter(markdown string) string {
	if !strings.HasPrefix(markdown, "---\n") {
		return markdown
	}

	rest := markdown[len("---\n"):]
	sep := strings.Index(rest, "\n---\n")
	if sep == -1 {
		return markdown
	}

	header := rest[:sep]
	if !strings.Contains(header, "file:") || !strings.Contains(header, "path:") || !strings.Contains(header, "page:") {
		return markdown
	}

	body := rest[sep+len("\n---\n"):]
	return strings.TrimPrefix(body, "\n")
}
