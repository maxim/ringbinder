package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestRunDocList_ExplicitDatabaseBypassesInvalidConfig(t *testing.T) {
	resetCommandState(t)

	invalidConfig := filepath.Join(t.TempDir(), "invalid.yml")
	if err := os.WriteFile(invalidConfig, []byte("paths: [\n"), 0644); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}
	cfgFile = invalidConfig

	dbPath := filepath.Join(t.TempDir(), "doc-list.db")
	cmd := commandWithDatabaseFlag(t, dbPath)
	if err := runDocList(cmd, nil); err != nil {
		t.Fatalf("runDocList() error = %v", err)
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("database file stat error = %v", err)
	}
}

func TestRunDocList_UsesConfigDatabasePath(t *testing.T) {
	resetCommandState(t)

	dbPath := filepath.Join(t.TempDir(), "configured.db")
	cfgPath := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(cfgPath, []byte("database_path: "+dbPath+"\n"), 0644); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}
	cfgFile = cfgPath

	cmd := &cobra.Command{}
	if err := runDocList(cmd, nil); err != nil {
		t.Fatalf("runDocList() error = %v", err)
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("configured database file stat error = %v", err)
	}
}

func TestRunSweep_ExplicitPathsStillLoadPersistentSafeguards(t *testing.T) {
	resetCommandState(t)

	invalidConfig := filepath.Join(t.TempDir(), "invalid.yml")
	if err := os.WriteFile(invalidConfig, []byte("paths: [\n"), 0644); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}
	cfgFile = invalidConfig

	scanDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(scanDir, "scan.png"), []byte("scan"), 0644); err != nil {
		t.Fatalf("WriteFile(scan) error = %v", err)
	}

	dbPath := filepath.Join(t.TempDir(), "sweep.db")
	cmd := sweepCommandWithDatabaseFlag(t, dbPath)
	if err := cmd.Flags().Set("concurrency", "4"); err != nil {
		t.Fatalf("Set(concurrency) error = %v", err)
	}
	err := runSweep(cmd, []string{scanDir})
	if err == nil || !strings.Contains(err.Error(), "load config") {
		t.Fatalf("runSweep() error = %v, want config error", err)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("database stat error = %v, want no database", err)
	}
}

func TestRunSweep_WithoutConcurrencyReadsConfigEvenWithPathsAndDatabase(t *testing.T) {
	resetCommandState(t)

	invalidConfig := filepath.Join(t.TempDir(), "invalid.yml")
	if err := os.WriteFile(invalidConfig, []byte("paths: [\n"), 0644); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}
	cfgFile = invalidConfig

	dbPath := filepath.Join(t.TempDir(), "sweep.db")
	cmd := sweepCommandWithDatabaseFlag(t, dbPath)
	err := runSweep(cmd, []string{t.TempDir()})
	if err == nil {
		t.Fatalf("runSweep() error = nil, want config error")
	}
	if !strings.Contains(err.Error(), "load config") {
		t.Fatalf("runSweep() error = %q, want load config error", err.Error())
	}
}

func TestRunSweep_InvalidConfigConcurrencyDoesNotCreateDatabase(t *testing.T) {
	resetCommandState(t)

	cfgPath := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(cfgPath, []byte("sweep_concurrency: 0\n"), 0644); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}
	cfgFile = cfgPath

	dbDir := filepath.Join(t.TempDir(), "database")
	dbPath := filepath.Join(dbDir, "sweep.db")
	cmd := sweepCommandWithDatabaseFlag(t, dbPath)
	err := runSweep(cmd, []string{t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "sweep_concurrency must be >= 1") {
		t.Fatalf("runSweep() error = %v, want sweep_concurrency validation error", err)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("database file stat error = %v, want not exist", err)
	}
	if _, err := os.Stat(dbDir); !os.IsNotExist(err) {
		t.Fatalf("database directory stat error = %v, want not exist", err)
	}
}

func TestRunSweep_WithoutPathsReadsConfigEvenWithDatabaseFlag(t *testing.T) {
	resetCommandState(t)

	invalidConfig := filepath.Join(t.TempDir(), "invalid.yml")
	if err := os.WriteFile(invalidConfig, []byte("paths: [\n"), 0644); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}
	cfgFile = invalidConfig

	dbPath := filepath.Join(t.TempDir(), "sweep.db")
	cmd := sweepCommandWithDatabaseFlag(t, dbPath)
	err := runSweep(cmd, nil)
	if err == nil {
		t.Fatalf("runSweep() error = nil, want config error")
	}
	if !strings.Contains(err.Error(), "load config") {
		t.Fatalf("runSweep() error = %q, want load config error", err.Error())
	}
}

func TestRunSweep_WithoutDatabaseReadsConfigEvenWithPathsAndConcurrency(t *testing.T) {
	resetCommandState(t)

	invalidConfig := filepath.Join(t.TempDir(), "invalid.yml")
	if err := os.WriteFile(invalidConfig, []byte("paths: [\n"), 0644); err != nil {
		t.Fatalf("WriteFile(config) error = %v", err)
	}
	cfgFile = invalidConfig

	cmd := &cobra.Command{}
	cmd.Flags().Int("concurrency", 4, "")
	cmd.Flags().StringSlice("exclude", nil, "")
	if err := cmd.Flags().Set("concurrency", "4"); err != nil {
		t.Fatalf("Set(concurrency) error = %v", err)
	}
	err := runSweep(cmd, []string{t.TempDir()})
	if err == nil {
		t.Fatalf("runSweep() error = nil, want config error")
	}
	if !strings.Contains(err.Error(), "load config") {
		t.Fatalf("runSweep() error = %q, want load config error", err.Error())
	}
}

func resetCommandState(t *testing.T) {
	t.Helper()

	oldCfgFile := cfgFile
	oldDatabaseFile := databaseFile
	cfgFile = ""
	databaseFile = ""
	t.Cleanup(func() {
		cfgFile = oldCfgFile
		databaseFile = oldDatabaseFile
	})
}

func commandWithDatabaseFlag(t *testing.T, dbPath string) *cobra.Command {
	t.Helper()

	cmd := &cobra.Command{}
	cmd.Flags().StringVar(&databaseFile, "database", "", "")
	if err := cmd.Flags().Set("database", dbPath); err != nil {
		t.Fatalf("Set(database) error = %v", err)
	}
	return cmd
}

func sweepCommandWithDatabaseFlag(t *testing.T, dbPath string) *cobra.Command {
	t.Helper()

	cmd := commandWithDatabaseFlag(t, dbPath)
	cmd.Flags().Int("concurrency", 4, "")
	cmd.Flags().StringSlice("exclude", nil, "")
	return cmd
}
