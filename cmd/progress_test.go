package cmd

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/maxim/ringbinder/internal/progress"
	"github.com/spf13/cobra"
)

func TestNewCommandProgressChecksActualOutputWriter(t *testing.T) {
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)

	oldTerminalCheck := progressWriterIsTerminal
	var checked io.Writer
	progressWriterIsTerminal = func(writer io.Writer) bool {
		checked = writer
		return false
	}
	t.Cleanup(func() { progressWriterIsTerminal = oldTerminalCheck })

	coordinator := newCommandProgress(cmd)
	phase := coordinator.StartPhase(progress.PhaseOptions{Label: "Testing output"})
	coordinator.ClosePhase(phase)

	if checked != &out {
		t.Fatalf("terminal check writer = %T, want command output writer", checked)
	}
	if strings.Contains(out.String(), "\x1b[") {
		t.Fatalf("redirected output contains ANSI controls: %q", out.String())
	}
}

func TestProgressCoordinatorSynchronizesWarningWithTTYPhase(t *testing.T) {
	var stdout, stderr bytes.Buffer
	coordinator := newProgressCoordinator(&stdout, &stderr, true)
	phase := coordinator.StartPhase(progress.PhaseOptions{
		Label: "Preparing Gemini input", Total: 2, Unit: "documents",
	})
	phase.Advance()
	coordinator.Warningf("warning: source changed\n")
	coordinator.ClosePhase(phase)

	if got := stderr.String(); got != "warning: source changed\n" {
		t.Fatalf("stderr = %q", got)
	}
	output := stdout.String()
	warningClear := strings.LastIndex(output, "\x1b[2K")
	if warningClear < 0 || !strings.Contains(output, "Preparing Gemini input") {
		t.Fatalf("TTY output did not clear and redraw around warning: %q", output)
	}
	if !strings.Contains(output, "\x1b[?25h") {
		t.Fatalf("TTY output did not restore the cursor: %q", output)
	}
}

func TestProgressCoordinatorNestsUploadAndRestoresParent(t *testing.T) {
	var stdout, stderr bytes.Buffer
	coordinator := newProgressCoordinator(&stdout, &stderr, true)
	parent := coordinator.StartPhase(progress.PhaseOptions{Label: "Checking Gemini batches", Total: 1, Unit: "batches"})
	upload := coordinator.StartUpload(7, 10)
	upload.AddBytes(10)
	coordinator.CompleteUpload(upload)
	parent.Advance()
	coordinator.CompletePhase(parent)

	output := stdout.String()
	if !strings.Contains(output, "Checking Gemini batches") || !strings.Contains(output, "Uploading Gemini batch 7") {
		t.Fatalf("nested output = %q", output)
	}
	if strings.Count(output, "\x1b[?25h") < 2 {
		t.Fatalf("nested renderers did not restore their cursors: %q", output)
	}
}

func TestProgressCoordinatorFinishStopsOnlyTopNestedRenderer(t *testing.T) {
	var stdout, stderr bytes.Buffer
	coordinator := newProgressCoordinator(&stdout, &stderr, false)
	parent := coordinator.StartPhase(progress.PhaseOptions{Label: "Checking Gemini batches"})
	child := coordinator.StartPhase(progress.PhaseOptions{Label: "Submitting Gemini batch 7"})
	coordinator.Finish(true)

	output := stdout.String()
	if strings.Count(output, "Stopped:") != 1 ||
		!strings.Contains(output, "Stopped: Submitting Gemini batch 7 ·") ||
		strings.Contains(output, "Stopped: Checking Gemini batches ·") {
		t.Fatalf("nested cancellation outcomes = %q, want only the top renderer", output)
	}
	// Keep references live through Finish to document the parent/child nesting.
	if parent == nil || child == nil {
		t.Fatal("expected nested renderers")
	}
}

func TestProgressCoordinatorFinishStopsActivePhase(t *testing.T) {
	var stdout, stderr bytes.Buffer
	coordinator := newProgressCoordinator(&stdout, &stderr, false)
	phase := coordinator.StartPhase(progress.PhaseOptions{Label: "OCR", Total: 3, Unit: "documents processed"})
	phase.Advance()
	coordinator.Finish(true)

	if got := stdout.String(); !strings.Contains(got, "Stopped at 1/3 documents processed: OCR ·") {
		t.Fatalf("cancellation output = %q", got)
	}
}
