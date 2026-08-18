package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestVersionDefaultsToDevel(t *testing.T) {
	if version != "devel" {
		t.Fatalf("version = %q, want devel", version)
	}
}

func TestRootCommandVersionOutput(t *testing.T) {
	cmd := newRootCommand()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"--version"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got, want := output.String(), "ringbinder version devel\n"; got != want {
		t.Fatalf("version output = %q, want %q", got, want)
	}
}

func TestRootCommandUsesPackageVersion(t *testing.T) {
	if rootCmd.Version != version {
		t.Fatalf("rootCmd.Version = %q, want %q", rootCmd.Version, version)
	}
}

func TestRedoFlagsRemoved(t *testing.T) {
	for _, cmd := range []*cobra.Command{sweepCmd, costCmd, ocrCmd} {
		if cmd.Flags().Lookup("redo") != nil {
			t.Errorf("%s still defines --redo", cmd.Name())
		}
		err := cmd.ParseFlags([]string{"--redo"})
		if err == nil || !strings.Contains(err.Error(), "unknown flag: --redo") {
			t.Errorf("%s --redo parse error = %v, want unknown flag", cmd.Name(), err)
		}
	}
}

func TestOCRBatchFlagsRegisteredAsIntegers(t *testing.T) {
	for _, cmd := range []*cobra.Command{costCmd, ocrCmd} {
		flag := cmd.Flags().Lookup("limit")
		if flag == nil {
			t.Errorf("%s does not define --limit", cmd.Name())
			continue
		}
		if err := cmd.Flags().Set("limit", "not-a-number"); err == nil {
			t.Errorf("%s --limit accepted a non-integer", cmd.Name())
		}
	}
}
