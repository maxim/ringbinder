package progress

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReporterNonTTYLifecycleIsCompact(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	reporter := NewReporter(&out, false, PhaseOptions{
		Label: "Preparing OCR", Total: 3, Unit: "documents inspected",
	})
	reporter.SetCurrent("/private/tmp/invoice.pdf")
	reporter.Advance()
	reporter.Advance()
	reporter.Complete()

	got := out.String()
	for _, want := range []string{
		"Preparing OCR started: 0/3 documents inspected.",
		"Preparing OCR complete: 2/3 documents inspected ·",
	} {
		mustContain(t, got, want)
	}
	if strings.Contains(out.String(), "\x1b[") {
		t.Fatalf("non-TTY output contains ANSI controls: %q", out.String())
	}
}

func TestReporterTTYUsesElapsedBarAndThreeNameCap(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	reporter := NewReporter(&out, true, PhaseOptions{
		Label: "OCR", Total: 5, Unit: "documents processed", Detail: "0/9 pages saved",
	})
	reporter.mu.Lock()
	reporter.start = time.Unix(0, 0)
	reporter.now = func() time.Time { return time.Unix(10, 0) }
	reporter.mu.Unlock()
	for i := 0; i < 4; i++ {
		reporter.StartItem(fmt.Sprintf("%d", i), fmt.Sprintf("/docs/document-%d.pdf", i))
	}
	reporter.Advance()
	reporter.Close()

	got := out.String()
	mustContain(t, got, "OCR · 1/5 documents processed · 0/9 pages saved · 10s")
	mustContain(t, got, "[██████░")
	mustContain(t, got, "document-0.pdf, document-1.pdf, document-2.pdf +1 more")
	if strings.Contains(got, "%") || strings.Contains(got, "ETA") {
		t.Fatalf("phase status must not render percentage or ETA: %q", got)
	}
	mustContain(t, got, "\x1b[?25l")
	mustContain(t, got, "\x1b[?25h")
}

func TestReporterPauseResumeAndWriteAbove(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	reporter := NewReporter(&out, true, PhaseOptions{Label: "Refreshing Gemini batches"})
	reporter.Pause()
	paused := out.String()
	mustContain(t, paused, "\x1b[?25h")
	reporter.Resume()
	reporter.WriteAbove(func() { fmt.Fprint(&out, "warning: temporary failure\n") })
	reporter.Close()

	got := out.String()
	warning := strings.Index(got, "warning: temporary failure")
	if warning < 0 {
		t.Fatalf("warning missing from %q", got)
	}
	if strings.LastIndex(got[:warning], "\x1b[2K") < 0 ||
		strings.Index(got[warning:], "Refreshing Gemini batches") < 0 {
		t.Fatalf("warning was not cleared and redrawn around: %q", got)
	}
}

func TestReporterConcurrentEventsAndIdempotentClose(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	reporter := NewReporter(&out, true, PhaseOptions{Label: "OCR", Total: 100, Unit: "documents processed"})
	var workers sync.WaitGroup
	for i := 0; i < 100; i++ {
		i := i
		workers.Add(1)
		go func() {
			defer workers.Done()
			key := fmt.Sprintf("%d", i)
			reporter.StartItem(key, fmt.Sprintf("document-%d.pdf", i))
			reporter.SetDetail(fmt.Sprintf("%d/100 pages saved", i))
			reporter.FinishItem(key)
			reporter.Advance()
		}()
	}
	workers.Wait()
	reporter.Close()
	reporter.Close()

	mustContain(t, out.String(), "\x1b[?25h")
}

func TestReporterDeferredCloseRestoresCursorAfterActivePanic(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	func() {
		reporter := NewReporter(&out, true, PhaseOptions{Label: "Preparing direct retry", Total: 2, Unit: "page ranges"})
		reporter.Advance()
		defer reporter.Close()
		defer func() {
			if recover() == nil {
				t.Fatal("expected panic")
			}
		}()
		panic("provider panic")
	}()

	got := out.String()
	if strings.Index(got, "\x1b[?25l") < 0 ||
		strings.LastIndex(got, "\x1b[?25h") < strings.Index(got, "\x1b[?25l") {
		t.Fatalf("active panic cleanup did not restore the cursor: %q", got)
	}
}

func mustContain(t *testing.T, got, needle string) {
	t.Helper()
	if !strings.Contains(got, needle) {
		t.Fatalf("output missing %q\nfull output:\n%s", needle, got)
	}
}
