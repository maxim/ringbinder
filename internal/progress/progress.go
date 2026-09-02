package progress

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	trackerSpinnerInterval = 80 * time.Millisecond
	phaseBarWidth          = 30
	maxActiveItems         = 3
)

// PhaseOptions describes one bounded or unbounded foreground operation.
type PhaseOptions struct {
	Label  string
	Total  int
	Unit   string
	Detail string
}

type activeItem struct {
	key  string
	name string
}

// Renderer is the small terminal surface shared by phase and upload progress.
// WriteAbove serializes a durable message with clearing and redrawing a live
// terminal display.
type Renderer interface {
	Pause()
	Resume()
	WriteAbove(func())
	Close()
}

// Reporter renders one compact progress phase. It deliberately reports one
// stable unit chosen by its caller instead of inferring work from retries or
// dynamically split requests.
type Reporter struct {
	mu sync.Mutex

	out   io.Writer
	isTTY bool

	label     string
	unit      string
	total     int
	completed int
	detail    string
	current   string
	active    []activeItem

	start time.Time
	now   func() time.Time

	renderedLines int
	spinner       *Spinner
	cursorHidden  bool
	paused        bool
	closed        bool
	outcomeShown  bool
}

// NewReporter starts rendering immediately. Non-terminal output gets one
// durable start line and no per-item updates.
func NewReporter(out io.Writer, isTTY bool, options PhaseOptions) *Reporter {
	if out == nil {
		out = io.Discard
	}
	if options.Total < 0 {
		options.Total = 0
	}
	if options.Label == "" {
		options.Label = "Working"
	}
	if options.Unit == "" {
		options.Unit = "items"
	}

	reporter := &Reporter{
		out:    out,
		isTTY:  isTTY,
		label:  options.Label,
		unit:   options.Unit,
		total:  options.Total,
		detail: options.Detail,
		start:  time.Now(),
		now:    time.Now,
	}
	if !isTTY {
		fmt.Fprintln(out, reporter.startLine())
		return reporter
	}

	spinner := NewSpinner(trackerSpinnerInterval, func() {
		reporter.mu.Lock()
		defer reporter.mu.Unlock()
		if reporter.closed || reporter.paused {
			return
		}
		reporter.renderLocked()
	})
	reporter.mu.Lock()
	reporter.spinner = spinner
	reporter.renderLocked()
	reporter.mu.Unlock()
	return reporter
}

// Advance records one completed caller-defined unit.
func (r *Reporter) Advance() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	if r.total == 0 || r.completed < r.total {
		r.completed++
	}
	r.renderLocked()
}

// SetDetail replaces the concise, caller-defined status detail.
func (r *Reporter) SetDetail(detail string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.detail = detail
	r.renderLocked()
}

// SetCurrent sets the one context item used by serial phases.
func (r *Reporter) SetCurrent(item string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.current = displayName(item)
	r.renderLocked()
}

// StartItem adds or updates an active concurrent item. Rendering is capped at
// three names so high OCR concurrency never expands the terminal layout.
func (r *Reporter) StartItem(key, item string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	name := displayName(item)
	for i := range r.active {
		if r.active[i].key == key {
			r.active[i].name = name
			r.renderLocked()
			return
		}
	}
	r.active = append(r.active, activeItem{key: key, name: name})
	r.renderLocked()
}

// FinishItem removes a concurrent item after its caller has completed an
// attempted unit.
func (r *Reporter) FinishItem(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	for i := range r.active {
		if r.active[i].key == key {
			r.active = append(r.active[:i], r.active[i+1:]...)
			break
		}
	}
	r.renderLocked()
}

// Pause clears the live display and restores the cursor while another phase
// owns the terminal. The spinner keeps time but cannot redraw until Resume.
func (r *Reporter) Pause() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.paused {
		return
	}
	r.paused = true
	r.clearRenderedLocked()
	r.restoreCursorLocked()
}

// Resume redraws a paused live display.
func (r *Reporter) Resume() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || !r.paused {
		return
	}
	r.paused = false
	r.renderLocked()
}

// Clear removes transient terminal lines without closing the phase.
func (r *Reporter) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.paused {
		return
	}
	r.clearRenderedLocked()
}

// WriteAbove clears a TTY display, runs write, and redraws under one reporter
// lock. It is used for warnings and durable output emitted during a phase.
func (r *Reporter) WriteAbove(write func()) {
	if write == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.paused || !r.isTTY {
		write()
		return
	}
	r.clearRenderedLocked()
	write()
	r.renderLocked()
}

// Close stops rendering and restores the cursor. It is safe to defer, call
// repeatedly, and use during panic cleanup.
func (r *Reporter) Close() {
	var spinner *Spinner

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	spinner = r.spinner
	r.spinner = nil
	// Spinner.Stop waits for its render callback, which takes r.mu. Unlock
	// before stopping it so concurrent cleanup cannot deadlock the reporter.
	r.mu.Unlock()

	if spinner != nil {
		spinner.Stop()
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.clearRenderedLocked()
	r.restoreCursorLocked()
}

// Complete closes the phase and writes its one durable completion line.
func (r *Reporter) Complete() {
	r.Close()

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.outcomeShown {
		return
	}
	r.outcomeShown = true
	elapsed := formatDuration(r.now().Sub(r.start))
	if r.total > 0 {
		fmt.Fprintf(r.out, "%s complete: %d/%d %s · %s.\n", r.label, r.completed, r.total, r.unit, elapsed)
		return
	}
	fmt.Fprintf(r.out, "%s complete · %s.\n", r.label, elapsed)
}

// Stopped closes the phase and writes the one durable cancellation line.
func (r *Reporter) Stopped() {
	r.Close()

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.outcomeShown {
		return
	}
	r.outcomeShown = true
	elapsed := formatDuration(r.now().Sub(r.start))
	if r.total > 0 {
		fmt.Fprintf(r.out, "Stopped at %d/%d %s: %s · %s.\n", r.completed, r.total, r.unit, r.label, elapsed)
		return
	}
	fmt.Fprintf(r.out, "Stopped: %s · %s.\n", r.label, elapsed)
}

func (r *Reporter) startLine() string {
	if r.total > 0 {
		return fmt.Sprintf("%s started: %d/%d %s.", r.label, r.completed, r.total, r.unit)
	}
	return fmt.Sprintf("%s started.", r.label)
}

func (r *Reporter) renderLocked() {
	if !r.isTTY || r.closed || r.paused {
		return
	}
	if !r.cursorHidden {
		fmt.Fprint(r.out, "\x1b[?25l")
		r.cursorHidden = true
	}

	r.clearRenderedLocked()
	lines := r.renderLinesLocked()
	for _, line := range lines {
		fmt.Fprintln(r.out, line)
	}
	r.renderedLines = len(lines)
}

func (r *Reporter) clearRenderedLocked() {
	if r.renderedLines == 0 {
		return
	}

	fmt.Fprintf(r.out, "\x1b[%dA", r.renderedLines)
	for i := 0; i < r.renderedLines; i++ {
		fmt.Fprint(r.out, "\r\x1b[2K")
		if i < r.renderedLines-1 {
			fmt.Fprint(r.out, "\x1b[1B")
		}
	}
	if r.renderedLines > 1 {
		fmt.Fprintf(r.out, "\x1b[%dA", r.renderedLines-1)
	}
	r.renderedLines = 0
}

func (r *Reporter) restoreCursorLocked() {
	if !r.cursorHidden {
		return
	}
	fmt.Fprint(r.out, "\x1b[?25h")
	r.cursorHidden = false
}

func (r *Reporter) renderLinesLocked() []string {
	spinner := ' '
	if r.spinner != nil {
		spinner = r.spinner.Frame()
	}
	status := fmt.Sprintf("%c %s", spinner, r.label)
	if r.total > 0 {
		status += fmt.Sprintf(" · %d/%d %s", r.completed, r.total, r.unit)
	}
	if r.detail != "" {
		status += " · " + r.detail
	}
	status += " · " + formatDuration(r.now().Sub(r.start))

	lines := []string{status}
	if r.total > 0 {
		filled := (r.completed * phaseBarWidth) / r.total
		if filled < 0 {
			filled = 0
		}
		if filled > phaseBarWidth {
			filled = phaseBarWidth
		}
		lines = append(lines, fmt.Sprintf(
			"  [%s%s]",
			strings.Repeat("█", filled),
			strings.Repeat("░", phaseBarWidth-filled),
		))
	}
	if current := r.currentLineLocked(); current != "" {
		lines = append(lines, "  "+current)
	}
	return lines
}

func (r *Reporter) currentLineLocked() string {
	if len(r.active) == 0 {
		return r.current
	}
	count := min(len(r.active), maxActiveItems)
	names := make([]string, 0, count)
	for _, item := range r.active[:count] {
		if item.name != "" {
			names = append(names, item.name)
		}
	}
	if len(names) == 0 {
		return r.current
	}
	line := strings.Join(names, ", ")
	if extra := len(r.active) - count; extra > 0 {
		line += fmt.Sprintf(" +%d more", extra)
	}
	return line
}

func displayName(item string) string {
	if item == "" {
		return ""
	}
	base := filepath.Base(item)
	if base == "." || base == string(filepath.Separator) {
		base = item
	}
	return truncate(base, 50)
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
