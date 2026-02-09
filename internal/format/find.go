package format

import (
	"fmt"
	"strings"

	"github.com/maxim/ringbinder/internal/db"
)

const (
	ansiReset     = "\x1b[0m"
	ansiBold      = "\x1b[1m"
	ansiDim       = "\x1b[2m"
	ansiItalic    = "\x1b[3m"
	ansiHighlight = "\x1b[1;33m"

	matchStart = ">>>"
	matchEnd   = "<<<"
)

// FormatFindResults formats search results for terminal output.
func FormatFindResults(results []db.SearchResult, verbose, color bool) string {
	var b strings.Builder

	for i, r := range results {
		b.WriteString(styleBold(r.Path, color))
		if r.PageCount > 1 {
			b.WriteString(" ")
			b.WriteString(styleDim(fmt.Sprintf("(page %d)", r.PageIndex+1), color))
		}
		b.WriteByte('\n')

		if verbose {
			snippet := formatSnippet(r.Snippet, color)
			if snippet != "" {
				b.WriteString(indentAllLines(snippet, "    "))
				b.WriteByte('\n')
			}
		}

		if verbose && i < len(results)-1 {
			b.WriteByte('\n')
		}
	}

	if verbose && len(results) > 0 {
		b.WriteByte('\n')
	}
	b.WriteString(styleSummary(fmt.Sprintf("%d result(s) found.", len(results)), color))
	b.WriteByte('\n')
	return b.String()
}

func formatSnippet(snippet string, color bool) string {
	if !color {
		return strings.ReplaceAll(strings.ReplaceAll(snippet, matchStart, ""), matchEnd, "")
	}

	return highlightSnippet(snippet)
}

func highlightSnippet(snippet string) string {
	var out strings.Builder
	out.WriteString(ansiDim)

	for {
		start := strings.Index(snippet, matchStart)
		if start == -1 {
			out.WriteString(strings.ReplaceAll(snippet, matchEnd, ""))
			out.WriteString(ansiReset)
			return out.String()
		}

		out.WriteString(snippet[:start])
		snippet = snippet[start+len(matchStart):]

		end := strings.Index(snippet, matchEnd)
		if end == -1 {
			out.WriteString(snippet)
			out.WriteString(ansiReset)
			return out.String()
		}

		out.WriteString(ansiReset)
		out.WriteString(ansiHighlight)
		out.WriteString(snippet[:end])
		out.WriteString(ansiReset)
		out.WriteString(ansiDim)
		snippet = snippet[end+len(matchEnd):]
	}
}

func styleBold(s string, color bool) string {
	if !color {
		return s
	}
	return ansiBold + s + ansiReset
}

func styleDim(s string, color bool) string {
	if !color {
		return s
	}
	return ansiDim + s + ansiReset
}

func styleSummary(s string, color bool) string {
	if !color {
		return s
	}
	return ansiDim + ansiItalic + s + ansiReset
}

func indentAllLines(s, indent string) string {
	if s == "" {
		return ""
	}

	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = indent + lines[i]
	}
	return strings.Join(lines, "\n")
}
