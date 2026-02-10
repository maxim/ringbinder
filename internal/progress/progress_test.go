package progress

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTrackerNonTTYOutput(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	tracker := New(&out, false, 3, 2)

	tracker.WorkerStart(0, "invoice.pdf")
	tracker.WorkerDone(0)
	tracker.Skip("receipt.png")
	tracker.WorkerStart(1, "scan.pdf")
	tracker.WorkerError(1, errors.New("boom"))
	tracker.Finish()

	got := out.String()
	mustContain(t, got, "[1/3] OK: invoice.pdf")
	mustContain(t, got, "[2/3] SKIP: receipt.png")
	mustContain(t, got, "[3/3] FAIL: scan.pdf (boom)")
	mustContain(t, got, "OCR complete: 1 succeeded, 1 skipped, 1 failed.")
}

func TestTrackerTTYUsesANSI(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	tracker := New(&out, true, 2, 1)

	tracker.WorkerStart(0, "invoice.pdf")
	tracker.WorkerDone(0)
	tracker.Finish()

	got := out.String()
	mustContain(t, got, "\x1b[?25l")
	mustContain(t, got, "\x1b[2K")
	mustContain(t, got, "\x1b[?25h")
}

func TestTrackerETACalculation(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	tracker := New(&out, true, 4, 1)
	tracker.start = time.Unix(0, 0)
	tracker.now = func() time.Time { return time.Unix(10, 0) }

	tracker.WorkerStart(0, "doc.pdf")
	tracker.WorkerDone(0)

	mustContain(t, out.String(), "ETA 30s")
}

func TestTrackerConcurrentAccess(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	tracker := New(&out, false, 180, 64)

	var wg sync.WaitGroup
	for i := 0; i < 120; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			slot := i % 64
			tracker.WorkerStart(slot, fmt.Sprintf("file-%d.pdf", i))
			if i%7 == 0 {
				tracker.WorkerError(slot, errors.New("fail"))
				return
			}
			tracker.WorkerDone(slot)
		}()
	}
	for i := 0; i < 60; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			tracker.Skip(fmt.Sprintf("skip-%d.bin", i))
		}()
	}
	wg.Wait()
	tracker.Finish()

	got := out.String()
	mustContain(t, got, "OCR complete: 102 succeeded, 60 skipped, 18 failed.")
}

func TestTrackerFinishClearsTTYDisplay(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	tracker := New(&out, true, 1, 1)

	tracker.WorkerStart(0, "invoice.pdf")
	tracker.WorkerDone(0)
	tracker.Finish()

	got := out.String()
	mustContain(t, got, "\x1b[4A")
	mustContain(t, got, "\x1b[?25hOCR complete: 1 succeeded, 0 skipped, 0 failed.\n")
}

func mustContain(t *testing.T, got, needle string) {
	t.Helper()
	if !strings.Contains(got, needle) {
		t.Fatalf("output missing %q\nfull output:\n%s", needle, got)
	}
}
