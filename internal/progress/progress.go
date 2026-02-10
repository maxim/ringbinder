package progress

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

const trackerSpinnerInterval = 80 * time.Millisecond

type workerState struct {
	filename string
	active   bool
}

// Tracker manages OCR progress rendering in TTY and non-TTY modes.
type Tracker struct {
	mu sync.Mutex

	out   io.Writer
	isTTY bool

	total   int
	workers []workerState

	start time.Time
	now   func() time.Time

	completed int
	succeeded int
	failed    int
	skipped   int

	renderedLines int
	spinner       *Spinner
	cursorHidden  bool
	finished      bool
}

func New(out io.Writer, isTTY bool, total, concurrency int) *Tracker {
	if out == nil {
		out = io.Discard
	}
	if total < 0 {
		total = 0
	}
	if concurrency < 1 {
		concurrency = 1
	}

	t := &Tracker{
		out:     out,
		isTTY:   isTTY,
		total:   total,
		workers: make([]workerState, concurrency),
		start:   time.Now(),
		now:     time.Now,
	}
	if isTTY {
		t.spinner = NewSpinner(trackerSpinnerInterval, func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			if t.finished {
				return
			}
			t.renderLocked()
		})

		t.mu.Lock()
		t.renderLocked()
		t.mu.Unlock()
	}
	return t
}

func (t *Tracker) WorkerStart(slotID int, filename string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return
	}
	if !t.validSlot(slotID) {
		return
	}

	t.workers[slotID] = workerState{
		filename: truncate(filename, 50),
		active:   true,
	}
}

func (t *Tracker) WorkerDone(slotID int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return
	}

	filename := t.workerFilenameAndReset(slotID)
	t.completed++
	t.succeeded++

	if !t.isTTY {
		t.renderNonTTYLocked("OK", filename, "")
	}
}

func (t *Tracker) WorkerError(slotID int, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return
	}

	filename := t.workerFilenameAndReset(slotID)
	t.completed++
	t.failed++

	if !t.isTTY {
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		t.renderNonTTYLocked("FAIL", filename, msg)
	}
}

func (t *Tracker) Skip(filename string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return
	}

	t.completed++
	t.skipped++
	if !t.isTTY {
		t.renderNonTTYLocked("SKIP", truncate(filename, 50), "")
	}
}

func (t *Tracker) Finish() {
	var spinner *Spinner

	t.mu.Lock()
	if t.finished {
		t.mu.Unlock()
		return
	}
	t.finished = true
	spinner = t.spinner
	t.spinner = nil
	t.mu.Unlock()

	if spinner != nil {
		spinner.Stop()
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.isTTY {
		t.clearRenderedLocked()
		if t.cursorHidden {
			fmt.Fprint(t.out, "\x1b[?25h")
			t.cursorHidden = false
		}
	}
	fmt.Fprintf(
		t.out,
		"OCR complete: %d succeeded, %d skipped, %d failed.\n",
		t.succeeded,
		t.skipped,
		t.failed,
	)
}

func (t *Tracker) validSlot(slotID int) bool {
	return slotID >= 0 && slotID < len(t.workers)
}

func (t *Tracker) workerFilenameAndReset(slotID int) string {
	if !t.validSlot(slotID) {
		return "(unknown)"
	}
	filename := t.workers[slotID].filename
	t.workers[slotID] = workerState{}
	if filename == "" {
		return "(unknown)"
	}
	return filename
}

func (t *Tracker) renderLocked() {
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

func (t *Tracker) clearRenderedLocked() {
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

func (t *Tracker) renderLinesLocked() []string {
	completed := t.completed
	if t.total > 0 && completed > t.total {
		completed = t.total
	}

	percent := 100
	if t.total > 0 {
		percent = (completed * 100) / t.total
	}

	spinner := ' '
	if t.spinner != nil {
		spinner = t.spinner.Frame()
	}
	line1 := fmt.Sprintf(
		"%c OCR %d/%d (%d%%) · ETA %s",
		spinner,
		completed,
		t.total,
		percent,
		t.etaLocked(),
	)

	const barWidth = 30
	filled := barWidth
	if t.total > 0 {
		filled = (completed * barWidth) / t.total
	}
	if filled < 0 {
		filled = 0
	}
	if filled > barWidth {
		filled = barWidth
	}
	line2 := fmt.Sprintf(
		"  [%s%s]",
		strings.Repeat("█", filled),
		strings.Repeat("░", barWidth-filled),
	)

	lines := make([]string, 0, 3+len(t.workers))
	lines = append(lines, line1, line2)
	for i, worker := range t.workers {
		if worker.active {
			lines = append(lines, fmt.Sprintf("  %d: %s", i+1, truncate(worker.filename, 50)))
			continue
		}
		lines = append(lines, fmt.Sprintf("  %d: \x1b[2m(idle)\x1b[0m", i+1))
	}
	lines = append(
		lines,
		fmt.Sprintf("  ✓ %d  ✗ %d  ⊘ %d", t.succeeded, t.failed, t.skipped),
	)
	return lines
}

func (t *Tracker) renderNonTTYLocked(status, filename, detail string) {
	completed := t.completed
	if t.total > 0 && completed > t.total {
		completed = t.total
	}
	if filename == "" {
		filename = "(unknown)"
	}
	if detail == "" {
		fmt.Fprintf(t.out, "[%d/%d] %s: %s\n", completed, t.total, status, filename)
		return
	}
	fmt.Fprintf(t.out, "[%d/%d] %s: %s (%s)\n", completed, t.total, status, filename, detail)
}

func (t *Tracker) etaLocked() string {
	processed := t.succeeded + t.failed
	if processed == 0 {
		return "--"
	}

	remaining := t.total - t.completed
	if remaining <= 0 {
		return "0s"
	}

	elapsed := t.now().Sub(t.start)
	if elapsed < 0 {
		elapsed = 0
	}

	eta := time.Duration(float64(elapsed) / float64(processed) * float64(remaining))
	return formatDuration(eta)
}

func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	if d < time.Second {
		return "<1s"
	}

	d = d.Round(time.Second)
	hours := int(d / time.Hour)
	minutes := int((d % time.Hour) / time.Minute)
	seconds := int((d % time.Minute) / time.Second)

	if hours > 0 {
		return fmt.Sprintf("%dh%02dm", hours, minutes)
	}
	if minutes > 0 {
		return fmt.Sprintf("%dm%02ds", minutes, seconds)
	}
	return fmt.Sprintf("%ds", seconds)
}

func truncate(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}

	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	if maxRunes <= 3 {
		return string(runes[:maxRunes])
	}
	return string(runes[:maxRunes-3]) + "..."
}
