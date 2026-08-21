package progress

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

const uploadBarWidth = 30

// UploadTracker renders byte progress for one serial Gemini batch upload.
type UploadTracker struct {
	mu sync.Mutex

	out     io.Writer
	isTTY   bool
	batchID int64
	total   int64

	uploaded  int64
	firstByte time.Time
	now       func() time.Time

	renderedLines int
	spinner       *Spinner
	cursorHidden  bool
	closed        bool
	outcomeShown  bool
}

func NewUpload(out io.Writer, isTTY bool, batchID, total int64) *UploadTracker {
	if out == nil {
		out = io.Discard
	}
	if total < 0 {
		total = 0
	}

	tracker := &UploadTracker{
		out:     out,
		isTTY:   isTTY,
		batchID: batchID,
		total:   total,
		now:     time.Now,
	}
	if !isTTY {
		fmt.Fprintf(out, "Uploading Gemini batch %d: %s.\n", batchID, formatBytes(total))
		return tracker
	}

	spinner := NewSpinner(trackerSpinnerInterval, func() {
		tracker.mu.Lock()
		defer tracker.mu.Unlock()
		if tracker.closed {
			return
		}
		tracker.renderLocked()
	})
	tracker.mu.Lock()
	tracker.spinner = spinner
	tracker.renderLocked()
	tracker.mu.Unlock()
	return tracker
}

// AddBytes records bytes read from the local upload source. It does not imply
// that Gemini acknowledged those bytes.
func (t *UploadTracker) AddBytes(count int) {
	if count <= 0 {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	if t.uploaded == 0 {
		t.firstByte = t.now()
	}
	t.uploaded += int64(count)
	if t.uploaded > t.total {
		t.uploaded = t.total
	}
}

// Close stops rendering and restores the terminal. It is safe to call more
// than once, including from a deferred panic-safety cleanup.
func (t *UploadTracker) Close() {
	var spinner *Spinner

	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	spinner = t.spinner
	t.spinner = nil
	t.mu.Unlock()

	if spinner != nil {
		spinner.Stop()
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	t.clearRenderedLocked()
	if t.cursorHidden {
		fmt.Fprint(t.out, "\x1b[?25h")
		t.cursorHidden = false
	}
}

func (t *UploadTracker) Complete() {
	t.Close()

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.outcomeShown {
		return
	}
	t.outcomeShown = true
	fmt.Fprintf(t.out, "Gemini batch %d upload complete: %s.\n", t.batchID, formatBytes(t.total))
}

func (t *UploadTracker) Stopped() {
	t.Close()

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.outcomeShown {
		return
	}
	t.outcomeShown = true
	fmt.Fprintf(
		t.out,
		"Gemini batch %d upload stopped at %s / %s.\n",
		t.batchID,
		formatBytes(t.uploaded),
		formatBytes(t.total),
	)
}

func (t *UploadTracker) renderLocked() {
	if !t.cursorHidden {
		fmt.Fprint(t.out, "\x1b[?25l")
		t.cursorHidden = true
	}

	t.clearRenderedLocked()
	lines := t.renderLinesLocked()
	for _, line := range lines {
		fmt.Fprintln(t.out, line)
	}
	t.renderedLines = len(lines)
}

func (t *UploadTracker) clearRenderedLocked() {
	if t.renderedLines == 0 {
		return
	}

	fmt.Fprintf(t.out, "\x1b[%dA", t.renderedLines)
	for i := 0; i < t.renderedLines; i++ {
		fmt.Fprint(t.out, "\r\x1b[2K")
		if i < t.renderedLines-1 {
			fmt.Fprint(t.out, "\x1b[1B")
		}
	}
	if t.renderedLines > 1 {
		fmt.Fprintf(t.out, "\x1b[%dA", t.renderedLines-1)
	}
	t.renderedLines = 0
}

func (t *UploadTracker) renderLinesLocked() []string {
	percent := 100
	if t.total > 0 {
		percent = int((t.uploaded * 100) / t.total)
	}

	spinner := ' '
	if t.spinner != nil {
		spinner = t.spinner.Frame()
	}
	status := fmt.Sprintf(
		"%c Uploading Gemini batch %d · %s / %s (%d%%) · ETA %s",
		spinner,
		t.batchID,
		formatBytes(t.uploaded),
		formatBytes(t.total),
		percent,
		t.etaLocked(),
	)

	filled := uploadBarWidth
	if t.total > 0 {
		filled = int((t.uploaded * uploadBarWidth) / t.total)
	}
	if filled < 0 {
		filled = 0
	}
	if filled > uploadBarWidth {
		filled = uploadBarWidth
	}
	bar := fmt.Sprintf(
		"  [%s%s]",
		strings.Repeat("█", filled),
		strings.Repeat("░", uploadBarWidth-filled),
	)
	return []string{status, bar}
}

func (t *UploadTracker) etaLocked() string {
	if t.uploaded == 0 || t.firstByte.IsZero() {
		return "--"
	}
	remaining := t.total - t.uploaded
	if remaining <= 0 {
		return "0s"
	}
	elapsed := t.now().Sub(t.firstByte)
	if elapsed <= 0 {
		return "--"
	}

	eta := time.Duration(float64(elapsed) / float64(t.uploaded) * float64(remaining))
	return formatDuration(eta)
}

func formatBytes(bytes int64) string {
	if bytes < 0 {
		bytes = 0
	}
	if bytes < 1024 {
		return fmt.Sprintf("%d B", bytes)
	}

	units := []string{"KiB", "MiB", "GiB", "TiB"}
	value := float64(bytes)
	unit := "B"
	for _, candidate := range units {
		value /= 1024
		unit = candidate
		if value < 1024 || candidate == units[len(units)-1] {
			break
		}
	}
	if value == float64(int64(value)) {
		return fmt.Sprintf("%.0f %s", value, unit)
	}
	return fmt.Sprintf("%.1f %s", value, unit)
}
