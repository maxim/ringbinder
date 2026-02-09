package db

import "testing"

func TestBuildPageMarkdown(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		path      string
		pageIndex int
		body      string
		want      string
	}{
		{
			name:      "basic",
			path:      "/Users/max/Documents/report.pdf",
			pageIndex: 2,
			body:      "The quick brown fox...",
			want: `---
file: report.pdf
type: pdf
path: /Users/max/Documents/report.pdf
page: 3
---

The quick brown fox...`,
		},
		{
			name:      "empty body",
			path:      "/tmp/empty.pdf",
			pageIndex: 0,
			body:      "",
			want: `---
file: empty.pdf
type: pdf
path: /tmp/empty.pdf
page: 1
---

`,
		},
		{
			name:      "single page still includes page",
			path:      "/docs/single.png",
			pageIndex: 0,
			body:      "Only page",
			want: `---
file: single.png
type: png
path: /docs/single.png
page: 1
---

Only page`,
		},
		{
			name:      "path with spaces and special chars",
			path:      "/Users/max/My Docs/report (final) #1.PDF",
			pageIndex: 9,
			body:      "Body",
			want: `---
file: report (final) #1.PDF
type: pdf
path: /Users/max/My Docs/report (final) #1.PDF
page: 10
---

Body`,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := BuildPageMarkdown(tt.path, tt.pageIndex, tt.body)
			if got != tt.want {
				t.Fatalf("BuildPageMarkdown() = %q, want %q", got, tt.want)
			}
		})
	}
}
