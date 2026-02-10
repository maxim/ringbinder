package progress

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestSpinnerTicksAtInterval(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	spinner := NewSpinner(40*time.Millisecond, func() {
		calls.Add(1)
	})

	time.Sleep(220 * time.Millisecond)
	spinner.Stop()

	got := calls.Load()
	if got < 4 || got > 8 {
		t.Fatalf("tick count = %d, want between 4 and 8", got)
	}
}

func TestSpinnerFrameAdvancesEachTick(t *testing.T) {
	t.Parallel()

	framesCh := make(chan rune, 32)
	var spinner *Spinner
	spinner = NewSpinner(10*time.Millisecond, func() {
		framesCh <- spinner.Frame()
	})

	target := len(spinnerFrames) + 2
	frames := make([]rune, 0, target)
	timeout := time.After(600 * time.Millisecond)
	for len(frames) < target {
		select {
		case frame := <-framesCh:
			frames = append(frames, frame)
		case <-timeout:
			spinner.Stop()
			t.Fatalf("timed out waiting for %d frames, got %d", target, len(frames))
		}
	}
	spinner.Stop()

	start := indexOfFrame(frames[0])
	if start < 0 {
		t.Fatalf("first frame %q is not a spinner frame", frames[0])
	}
	for i, frame := range frames {
		want := spinnerFrames[(start+i)%len(spinnerFrames)]
		if frame != want {
			t.Fatalf("frame[%d] = %q, want %q", i, frame, want)
		}
	}
}

func TestSpinnerStopsCleanly(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	spinner := NewSpinner(20*time.Millisecond, func() {
		calls.Add(1)
	})

	deadline := time.Now().Add(400 * time.Millisecond)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if calls.Load() == 0 {
		spinner.Stop()
		t.Fatal("spinner did not tick before timeout")
	}

	spinner.Stop()
	before := calls.Load()
	time.Sleep(80 * time.Millisecond)
	after := calls.Load()
	if after != before {
		t.Fatalf("spinner ticked after stop: before=%d after=%d", before, after)
	}

	// Must be safe to call repeatedly.
	spinner.Stop()
}

func indexOfFrame(frame rune) int {
	for i, spinnerFrame := range spinnerFrames {
		if spinnerFrame == frame {
			return i
		}
	}
	return -1
}
