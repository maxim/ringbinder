package cmd

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

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

func TestExecuteAllowsSecondInterruptToTerminate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix signal behavior")
	}
	if os.Getenv("RINGBINDER_SIGNAL_HELPER") == "1" {
		rootCmd = &cobra.Command{
			Use: "signal-helper",
			RunE: func(cmd *cobra.Command, _ []string) error {
				fmt.Println("ready")
				<-cmd.Context().Done()
				fmt.Println("cancelled")
				select {}
			},
		}
		rootCmd.SetArgs([]string{})
		_ = Execute()
		return
	}

	command := exec.Command(os.Args[0], "-test.run=^TestExecuteAllowsSecondInterruptToTerminate$")
	command.Env = append(os.Environ(), "RINGBINDER_SIGNAL_HELPER=1")
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	finished := false
	t.Cleanup(func() {
		if !finished {
			_ = command.Process.Kill()
		}
	})

	lines := make(chan string, 2)
	scanner := bufio.NewScanner(stdout)
	go func() {
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()
	waitForSignalHelperLine(t, lines, "ready")
	if err := command.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	waitForSignalHelperLine(t, lines, "cancelled")
	// Execute unregisters its handler asynchronously after cancellation.
	time.Sleep(100 * time.Millisecond)
	if err := command.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case waitErr := <-done:
		finished = true
		exitErr, ok := waitErr.(*exec.ExitError)
		if !ok {
			t.Fatalf("helper wait error = %v, stderr = %q", waitErr, stderr.String())
		}
		status, ok := exitErr.Sys().(syscall.WaitStatus)
		if !ok || !status.Signaled() || status.Signal() != syscall.SIGINT {
			t.Fatalf("helper status = %v, stderr = %q; want SIGINT", exitErr.Sys(), stderr.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second interrupt did not terminate helper")
	}
}

func waitForSignalHelperLine(t *testing.T, lines <-chan string, want string) {
	t.Helper()
	select {
	case line := <-lines:
		if line != want {
			t.Fatalf("helper line = %q, want %q", line, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for helper line %q", want)
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
