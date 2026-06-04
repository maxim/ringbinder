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

func TestRunSweep_ExplicitPathsAndDatabaseBypassInvalidConfig(t *testing.T) {
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
	if err := runSweep(cmd, []string{scanDir}); err != nil {
		t.Fatalf("runSweep() error = %v", err)
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("database file stat error = %v", err)
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
	cmd.Flags().Bool("redo", false, "")
	cmd.Flags().StringSlice("exclude", nil, "")
	return cmd
}
