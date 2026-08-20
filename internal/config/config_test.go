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

func TestLoad_PreservesSettingPresence(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(cfgPath, []byte("model: '   '\nsweep_concurrency: 0\nocr_concurrency: 0\n"), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Model == nil || *cfg.Model != "   " {
		t.Fatalf("Model = %#v, want present whitespace value", cfg.Model)
	}
	if cfg.SweepConcurrency == nil || *cfg.SweepConcurrency != 0 {
		t.Fatalf("SweepConcurrency = %#v, want present zero value", cfg.SweepConcurrency)
	}
	if cfg.OCRConcurrency == nil || *cfg.OCRConcurrency != 0 {
		t.Fatalf("OCRConcurrency = %#v, want present zero value", cfg.OCRConcurrency)
	}

	blankPath := filepath.Join(t.TempDir(), "blank.yml")
	if err := os.WriteFile(blankPath, []byte("model:\nsweep_concurrency:\nocr_concurrency:\n"), 0644); err != nil {
		t.Fatalf("WriteFile(blank) error = %v", err)
	}
	blank, err := Load(blankPath)
	if err != nil {
		t.Fatalf("Load(blank) error = %v", err)
	}
	if blank.Model == nil || *blank.Model != "" {
		t.Fatalf("blank Model = %#v, want present empty value", blank.Model)
	}
	if blank.SweepConcurrency == nil || *blank.SweepConcurrency != 0 {
		t.Fatalf("blank SweepConcurrency = %#v, want present zero value", blank.SweepConcurrency)
	}
	if blank.OCRConcurrency == nil || *blank.OCRConcurrency != 0 {
		t.Fatalf("blank OCRConcurrency = %#v, want present zero value", blank.OCRConcurrency)
	}

	missingPath := filepath.Join(t.TempDir(), "missing.yml")
	missing, err := Load(missingPath)
	if err != nil {
		t.Fatalf("Load(missing) error = %v", err)
	}
	if missing.Model != nil || missing.SweepConcurrency != nil || missing.OCRConcurrency != nil {
		t.Fatalf(
			"missing settings = (%#v, %#v, %#v), want nil",
			missing.Model,
			missing.SweepConcurrency,
			missing.OCRConcurrency,
		)
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
