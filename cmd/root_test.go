package cmd

import (
	"bytes"
	"testing"
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
