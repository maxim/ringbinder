package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/mattn/go-isatty"
	"github.com/maxim/ringbinder/internal/progress"
	"github.com/spf13/cobra"
)

// progressWriterIsTerminal is injectable so command tests can exercise both
// render modes without depending on the test process's output descriptor.
var progressWriterIsTerminal = func(writer io.Writer) bool {
	descriptor, ok := writer.(interface{ Fd() uintptr })
	if !ok {
		return false
	}
	return isatty.IsTerminal(descriptor.Fd()) || isatty.IsCygwinTerminal(descriptor.Fd())
}

func progressStdoutIsTerminal() bool {
	return progressWriterIsTerminal(os.Stdout)
}

func ensureCommandContext(cmd *cobra.Command) context.Context {
	if cmd == nil {
		return context.Background()
	}
	if cmd.Context() == nil {
		cmd.SetContext(context.Background())
	}
	return cmd.Context()
}

func commandStdout(cmd *cobra.Command) io.Writer {
	if cmd == nil {
		return os.Stdout
	}
	return cmd.OutOrStdout()
}

func commandStderr(cmd *cobra.Command) io.Writer {
	if cmd == nil {
		return os.Stderr
	}
	return cmd.ErrOrStderr()
}

// progressCoordinator gives a command one owner for live stdout renderers.
// It keeps warnings on stderr while clearing and redrawing any active stdout
// phase, so concurrent OCR warnings remain readable in a terminal.
type progressCoordinator struct {
	mu sync.Mutex

	out    io.Writer
	errOut io.Writer
	isTTY  bool
	stack  []progress.Renderer

	stoppedOutcomeShown bool
}

func newProgressCoordinator(out, errOut io.Writer, isTTY bool) *progressCoordinator {
	if out == nil {
		out = io.Discard
	}
	if errOut == nil {
		errOut = io.Discard
	}
	return &progressCoordinator{out: out, errOut: errOut, isTTY: isTTY}
}

func newCommandProgress(cmd *cobra.Command) *progressCoordinator {
	out := commandStdout(cmd)
	return newProgressCoordinator(out, commandStderr(cmd), progressWriterIsTerminal(out))
}

func firstProgressCoordinator(coordinators []*progressCoordinator) *progressCoordinator {
	if len(coordinators) == 0 {
		return nil
	}
	return coordinators[0]
}

func commandWarning(coordinator *progressCoordinator, fallback io.Writer, format string, args ...any) {
	if coordinator != nil {
		coordinator.Warningf(format, args...)
		return
	}
	fmt.Fprintf(fallback, format, args...)
}

func commandProgressf(coordinator *progressCoordinator, fallback io.Writer, format string, args ...any) {
	if coordinator != nil {
		coordinator.Printf(format, args...)
		return
	}
	fmt.Fprintf(fallback, format, args...)
}

func (c *progressCoordinator) ErrWriter() io.Writer {
	return coordinatedWriter{coordinator: c, target: c.errOut}
}

func (c *progressCoordinator) StartPhase(options progress.PhaseOptions) *progress.Reporter {
	if c == nil {
		return progress.NewReporter(io.Discard, false, options)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pauseTopLocked()
	reporter := progress.NewReporter(c.out, c.isTTY, options)
	c.stack = append(c.stack, reporter)
	return reporter
}

func (c *progressCoordinator) StartUpload(batchID, total int64) *progress.UploadTracker {
	if c == nil {
		return progress.NewUpload(io.Discard, false, batchID, total)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pauseTopLocked()
	tracker := progress.NewUpload(c.out, c.isTTY, batchID, total)
	c.stack = append(c.stack, tracker)
	return tracker
}

func (c *progressCoordinator) ClosePhase(reporter *progress.Reporter) {
	if c == nil || reporter == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	reporter.Close()
	c.removeLocked(reporter)
}

func (c *progressCoordinator) CompletePhase(reporter *progress.Reporter) {
	if c == nil || reporter == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	reporter.Complete()
	c.removeLocked(reporter)
}

func (c *progressCoordinator) StopPhase(reporter *progress.Reporter) {
	if c == nil || reporter == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopLocked(reporter)
	c.removeLocked(reporter)
}

// StoppedOutcomeShown reports whether this command has already displayed its
// one cancellation outcome. It is safe for orchestration phases to check while
// another phase is closing.
func (c *progressCoordinator) StoppedOutcomeShown() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stoppedOutcomeShown
}

func (c *progressCoordinator) CloseUpload(tracker *progress.UploadTracker) {
	if c == nil || tracker == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	tracker.Close()
	c.removeLocked(tracker)
}

func (c *progressCoordinator) CompleteUpload(tracker *progress.UploadTracker) {
	if c == nil || tracker == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	tracker.Complete()
	c.removeLocked(tracker)
}

func (c *progressCoordinator) StopUpload(tracker *progress.UploadTracker) {
	if c == nil || tracker == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopLocked(tracker)
	c.removeLocked(tracker)
}

// Finish closes all live renderers, or leaves one cancellation outcome before
// cleanup when the command context was cancelled.
func (c *progressCoordinator) Finish(cancelled bool) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if cancelled && len(c.stack) > 0 {
		// Only the visible nested renderer reports cancellation. Paused parents
		// are closed quietly so one interrupt produces one stopped outcome.
		top := c.stack[len(c.stack)-1]
		c.stopLocked(top)
		c.stack = c.stack[:len(c.stack)-1]
	}
	for i := len(c.stack) - 1; i >= 0; i-- {
		c.stack[i].Close()
	}
	c.stack = nil
}

// Printf writes durable stdout without leaving it inside a TTY display.
func (c *progressCoordinator) Printf(format string, args ...any) {
	if c == nil {
		return
	}
	c.write(func() { fmt.Fprintf(c.out, format, args...) })
}

// Warningf is the batch-command warning path. It shares the active renderer's
// lock, but intentionally writes the warning to the command's stderr writer.
func (c *progressCoordinator) Warningf(format string, args ...any) {
	if c == nil {
		return
	}
	c.write(func() { fmt.Fprintf(c.errOut, format, args...) })
}

func (c *progressCoordinator) write(write func()) {
	if c == nil || write == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.stack) == 0 {
		write()
		return
	}
	c.stack[len(c.stack)-1].WriteAbove(write)
}

func (c *progressCoordinator) stopLocked(renderer progress.Renderer) {
	if c.stoppedOutcomeShown {
		renderer.Close()
		return
	}
	switch renderer := renderer.(type) {
	case *progress.Reporter:
		renderer.Stopped()
	case *progress.UploadTracker:
		renderer.Stopped()
	default:
		renderer.Close()
		return
	}
	c.stoppedOutcomeShown = true
}

func (c *progressCoordinator) pauseTopLocked() {
	if len(c.stack) == 0 {
		return
	}
	c.stack[len(c.stack)-1].Pause()
}

func (c *progressCoordinator) removeLocked(target progress.Renderer) {
	index := -1
	for i, renderer := range c.stack {
		if renderer == target {
			index = i
			break
		}
	}
	if index < 0 {
		return
	}
	wasTop := index == len(c.stack)-1
	c.stack = append(c.stack[:index], c.stack[index+1:]...)
	if wasTop && len(c.stack) > 0 {
		c.stack[len(c.stack)-1].Resume()
	}
}

type coordinatedWriter struct {
	coordinator *progressCoordinator
	target      io.Writer
}

func (writer coordinatedWriter) Write(data []byte) (int, error) {
	if writer.coordinator == nil {
		return writer.target.Write(data)
	}
	var (
		count int
		err   error
	)
	writer.coordinator.write(func() {
		count, err = writer.target.Write(data)
	})
	return count, err
}
