package pathutil

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestResolveGlobs_ResolvesPathsAndRoots(t *testing.T) {
	root := t.TempDir()
	docsDir := filepath.Join(root, "docs")
	docsSubDir := filepath.Join(docsDir, "sub")
	imagesDir := filepath.Join(root, "images")
	mustMkdirAll(t, docsSubDir)
	mustMkdirAll(t, imagesDir)

	docA := filepath.Join(docsDir, "a.pdf")
	docB := filepath.Join(docsSubDir, "b.pdf")
	mustWriteFile(t, docA, "a")
	mustWriteFile(t, docB, "b")

	patterns := []string{
		filepath.Join(docsDir, "**", "*.pdf"),
		docA,
		imagesDir,
	}

	resolved, roots, warnings, err := ResolveGlobs(patterns)
	if err != nil {
		t.Fatalf("ResolveGlobs() error = %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("ResolveGlobs() warnings = %v, want none", warnings)
	}

	wantResolved := []string{docA, docB, imagesDir}
	assertPathSetEqual(t, resolved, wantResolved)

	wantRoots := []string{docsDir, imagesDir}
	assertPathSetEqual(t, roots, wantRoots)
}

func TestResolveGlobs_WarnsWhenNoMatches(t *testing.T) {
	root := t.TempDir()
	pattern := filepath.Join(root, "missing", "**", "*.pdf")

	resolved, roots, warnings, err := ResolveGlobs([]string{pattern})
	if err != nil {
		t.Fatalf("ResolveGlobs() error = %v", err)
	}
	if len(resolved) != 0 {
		t.Fatalf("ResolveGlobs() resolved = %v, want none", resolved)
	}
	if len(warnings) != 1 {
		t.Fatalf("ResolveGlobs() warnings count = %d, want 1", len(warnings))
	}
	if !strings.Contains(warnings[0], pattern) {
		t.Fatalf("warning = %q, want pattern %q included", warnings[0], pattern)
	}

	wantRoots := []string{filepath.Join(root, "missing")}
	assertPathSetEqual(t, roots, wantRoots)
}

func TestResolveGlobs_NonGlobMissingPathErrors(t *testing.T) {
	root := t.TempDir()
	_, _, _, err := ResolveGlobs([]string{filepath.Join(root, "missing.pdf")})
	if err == nil {
		t.Fatalf("ResolveGlobs() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "stat path") {
		t.Fatalf("ResolveGlobs() error = %v, want stat path error", err)
	}
}

func TestGlobRoot(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)

	tests := []struct {
		name    string
		pattern string
		want    string
	}{
		{
			name:    "absolute file pattern",
			pattern: filepath.Join(string(filepath.Separator), "data", "reports", "*.pdf"),
			want:    filepath.Join(string(filepath.Separator), "data", "reports"),
		},
		{
			name:    "absolute nested glob segment",
			pattern: filepath.Join(string(filepath.Separator), "data", "*", "stuff"),
			want:    filepath.Join(string(filepath.Separator), "data"),
		},
		{
			name:    "relative recursive",
			pattern: filepath.Join("reports", "**", "*.pdf"),
			want:    filepath.Join(cwd, "reports"),
		},
		{
			name:    "glob from current directory",
			pattern: filepath.Join("**", "*.pdf"),
			want:    cwd,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := GlobRoot(tt.pattern)
			if err != nil {
				t.Fatalf("GlobRoot() error = %v", err)
			}

			wantAbs, err := filepath.Abs(tt.want)
			if err != nil {
				t.Fatalf("filepath.Abs() error = %v", err)
			}
			if got != wantAbs {
				t.Fatalf("GlobRoot() = %q, want %q", got, wantAbs)
			}
		})
	}
}

func TestMatchesAny(t *testing.T) {
	root := t.TempDir()
	tmpPath := filepath.Join(root, "alpha", "beta", "foo.tmp")
	pdfPath := filepath.Join(root, "temp", "sub", "file.pdf")
	mustMkdirAll(t, filepath.Dir(tmpPath))
	mustMkdirAll(t, filepath.Dir(pdfPath))
	mustWriteFile(t, tmpPath, "tmp")
	mustWriteFile(t, pdfPath, "pdf")

	tests := []struct {
		name     string
		path     string
		patterns []string
		want     bool
		wantErr  bool
	}{
		{
			name:     "basename glob",
			path:     tmpPath,
			patterns: []string{"*.tmp"},
			want:     true,
		},
		{
			name:     "exact absolute path",
			path:     tmpPath,
			patterns: []string{tmpPath},
			want:     true,
		},
		{
			name:     "absolute recursive path glob",
			path:     pdfPath,
			patterns: []string{filepath.Join(root, "temp", "**")},
			want:     true,
		},
		{
			name:     "no match",
			path:     pdfPath,
			patterns: []string{filepath.Join(root, "temp", "*.pdf")},
			want:     false,
		},
		{
			name:     "invalid pattern",
			path:     tmpPath,
			patterns: []string{"[invalid"},
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := MatchesAny(tt.path, tt.patterns)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("MatchesAny() error = nil, want error")
				}
				return
			}

			if err != nil {
				t.Fatalf("MatchesAny() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("MatchesAny() = %v, want %v", got, tt.want)
			}
		})
	}
}

func assertPathSetEqual(t *testing.T, got, want []string) {
	t.Helper()

	gotSet := make(map[string]struct{}, len(got))
	for _, p := range got {
		gotSet[filepath.Clean(p)] = struct{}{}
	}
	wantSet := make(map[string]struct{}, len(want))
	for _, p := range want {
		wantSet[filepath.Clean(p)] = struct{}{}
	}

	if !reflect.DeepEqual(gotSet, wantSet) {
		t.Fatalf("path set mismatch\ngot:  %v\nwant: %v", got, want)
	}
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", path, err)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
}
