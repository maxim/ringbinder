package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad_ExpandsDatabasePathAndBareHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cfgPath := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(cfgPath, []byte(`database_path: "  ~/dbs/ringbinder.db  "
paths:
  - "~"
  - "~/Documents"
  - "~other/Documents"
`), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	wantDBPath := filepath.Join(home, "dbs", "ringbinder.db")
	if cfg.DatabasePath != wantDBPath {
		t.Fatalf("DatabasePath = %q, want %q", cfg.DatabasePath, wantDBPath)
	}

	wantPaths := []string{
		home,
		filepath.Join(home, "Documents"),
		"~other/Documents",
	}
	if len(cfg.Paths) != len(wantPaths) {
		t.Fatalf("Paths = %#v, want %#v", cfg.Paths, wantPaths)
	}
	for i := range wantPaths {
		if cfg.Paths[i] != wantPaths[i] {
			t.Fatalf("Paths[%d] = %q, want %q", i, cfg.Paths[i], wantPaths[i])
		}
	}
}

func TestLoad_ModelAndExcludeScalarOrList(t *testing.T) {
	tests := []struct {
		name        string
		yaml        string
		wantModels  []string
		wantExclude []string
	}{
		{name: "absent"},
		{name: "scalars", yaml: "model: mistral\nexclude: '*.tmp.pdf'\n", wantModels: []string{"mistral"}, wantExclude: []string{"*.tmp.pdf"}},
		{name: "flow lists", yaml: "model: [gemini, mistral]\nexclude: ['a.pdf', '*.png']\n", wantModels: []string{"gemini", "mistral"}, wantExclude: []string{"a.pdf", "*.png"}},
		{name: "block lists", yaml: "model:\n  - gemini\n  - mistral\nexclude:\n  - a.pdf\n", wantModels: []string{"gemini", "mistral"}, wantExclude: []string{"a.pdf"}},
		{name: "empty excludes", yaml: "exclude: []\n", wantExclude: []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yml")
			if err := os.WriteFile(path, []byte(tt.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if tt.wantModels == nil {
				if cfg.Model != nil {
					t.Fatalf("Model = %#v, want absent", cfg.Model)
				}
			} else if fmt.Sprint(*cfg.Model) != fmt.Sprint(tt.wantModels) {
				t.Fatalf("Model = %v, want %v", *cfg.Model, tt.wantModels)
			}
			if fmt.Sprint(cfg.Exclude) != fmt.Sprint(tt.wantExclude) {
				t.Fatalf("Exclude = %v, want %v", cfg.Exclude, tt.wantExclude)
			}
		})
	}
}

func TestLoad_RejectsInvalidModelAndStaleConcurrency(t *testing.T) {
	tests := []struct {
		name, yaml, want string
	}{
		{name: "null model", yaml: "model:\n", want: "model cannot be null"},
		{name: "empty model list", yaml: "model: []\n", want: "model cannot be empty"},
		{name: "blank model", yaml: "model: [' ']\n", want: "model entries cannot be blank"},
		{name: "wrong model type", yaml: "model: 1\n", want: "model must be a string"},
		{name: "null exclude", yaml: "exclude:\n", want: "exclude cannot be null"},
		{name: "blank exclude", yaml: "exclude: [' ']\n", want: "exclude entries cannot be blank"},
		{name: "non-string exclude", yaml: "exclude: [1]\n", want: "exclude entries must be strings"},
		{name: "stale concurrency", yaml: "ocr_concurrency: 4\n", want: "ocr_concurrency is no longer supported"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yml")
			if err := os.WriteFile(path, []byte(tt.yaml), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load() error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestLoad_EmptyDatabasePathIsOmitted(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(cfgPath, []byte("database_path: '   '\n"), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.DatabasePath != "" {
		t.Fatalf("DatabasePath = %q, want empty", cfg.DatabasePath)
	}
}

func TestResolveDatabasePath_PrecedenceAndExpansion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	got, err := ResolveDatabasePath(" ~/cli.db ", true, "~/config.db")
	if err != nil {
		t.Fatalf("ResolveDatabasePath() error = %v", err)
	}
	want := filepath.Join(home, "cli.db")
	if got != want {
		t.Fatalf("ResolveDatabasePath() = %q, want %q", got, want)
	}

	got, err = ResolveDatabasePath("", false, "~/config.db")
	if err != nil {
		t.Fatalf("ResolveDatabasePath(config) error = %v", err)
	}
	want = filepath.Join(home, "config.db")
	if got != want {
		t.Fatalf("ResolveDatabasePath(config) = %q, want %q", got, want)
	}
}

func TestResolveDatabasePath_DefaultAndEmptyValues(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	got, err := ResolveDatabasePath("", false, "   ")
	if err != nil {
		t.Fatalf("ResolveDatabasePath(default) error = %v", err)
	}
	want := DefaultDatabasePath()
	if got != want {
		t.Fatalf("ResolveDatabasePath(default) = %q, want %q", got, want)
	}

	if _, err := ResolveDatabasePath("   ", true, "~/config.db"); err == nil {
		t.Fatalf("ResolveDatabasePath(empty CLI) error = nil, want error")
	}
}
