package config

import (
	"os"
	"path/filepath"
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
