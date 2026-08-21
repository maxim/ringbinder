package progress

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestUploadTrackerNonTTYLifecycle(t *testing.T) {
	t.Parallel()

	const total = int64(512 * 1024 * 1024)
	var out bytes.Buffer
	tracker := NewUpload(&out, false, 17, total)
	tracker.AddBytes(128 * 1024 * 1024)
	tracker.Stopped()
	tracker.Close()

	want := "Uploading Gemini batch 17: 512 MiB.\n" +
		"Gemini batch 17 upload stopped at 128 MiB / 512 MiB.\n"
	if got := out.String(); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if strings.Contains(out.String(), "\x1b[") {
		t.Fatalf("non-TTY output contains ANSI controls: %q", out.String())
	}
}

func TestUploadTrackerNonTTYCompletion(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	tracker := NewUpload(&out, false, 8, 1536)
	tracker.AddBytes(1536)
	tracker.Complete()

	want := "Uploading Gemini batch 8: 1.5 KiB.\n" +
		"Gemini batch 8 upload complete: 1.5 KiB.\n"
	if got := out.String(); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestUploadTrackerTTYRenderingAndETA(t *testing.T) {
	t.Parallel()

	const mib = int64(1024 * 1024)
	var out bytes.Buffer
	tracker := NewUpload(&out, true, 17, 512*mib)

	current := time.Unix(1, 0)
	tracker.mu.Lock()
	tracker.now = func() time.Time { return current }
	tracker.mu.Unlock()
	tracker.AddBytes(int(128 * mib))

	tracker.mu.Lock()
	current = current.Add(12 * time.Second)
	tracker.renderLocked()
	tracker.mu.Unlock()
	tracker.Close()

	got := out.String()
	mustContain(t, got, "Uploading Gemini batch 17 · 128 MiB / 512 MiB (25%) · ETA 36s")
	mustContain(t, got, "[███████░░░░░░░░░░░░░░░░░░░░░░░]")
	mustContain(t, got, "\x1b[?25l")
	mustContain(t, got, "\x1b[2K")
	mustContain(t, got, "\x1b[?25h")
}

func TestUploadTrackerTTYAutomaticallyRedrawsProgress(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	tracker := NewUpload(&out, true, 6, 1024)
	tracker.AddBytes(256)
	time.Sleep(3 * trackerSpinnerInterval)
	tracker.Close()

	mustContain(t, out.String(), "256 B / 1 KiB (25%)")
}

func TestUploadTrackerTTYCompletionIsFullAndClearedBeforeOutcome(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	tracker := NewUpload(&out, true, 9, 1024)
	tracker.AddBytes(2048)
	tracker.mu.Lock()
	tracker.renderLocked()
	tracker.mu.Unlock()
	tracker.Complete()

	got := out.String()
	mustContain(t, got, "1 KiB / 1 KiB (100%) · ETA 0s")
	mustContain(t, got, "[██████████████████████████████]")
	cursorRestored := strings.LastIndex(got, "\x1b[?25h")
	outcome := strings.LastIndex(got, "Gemini batch 9 upload complete: 1 KiB.")
	if cursorRestored < 0 || outcome < cursorRestored {
		t.Fatalf("cursor restore and outcome order is wrong: %q", got)
	}
}

func TestUploadTrackerShowsUnknownETABeforeFirstByte(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	tracker := NewUpload(&out, true, 3, 1024)
	tracker.Close()

	mustContain(t, out.String(), "0 B / 1 KiB (0%) · ETA --")
}

func TestUploadTrackerClampsReportedBytes(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	tracker := NewUpload(&out, false, 4, 1024)
	tracker.AddBytes(2048)
	tracker.Stopped()

	mustContain(t, out.String(), "upload stopped at 1 KiB / 1 KiB")
}

func TestUploadTrackerDeferredCloseRestoresCursorAfterPanic(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	func() {
		tracker := NewUpload(&out, true, 5, 1024)
		defer func() {
			if recover() == nil {
				t.Fatal("expected panic")
			}
		}()
		defer tracker.Close()
		panic("upload panic")
	}()

	mustContain(t, out.String(), "\x1b[?25h")
}

func TestFormatBytes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		bytes int64
		want  string
	}{
		{bytes: 0, want: "0 B"},
		{bytes: 1023, want: "1023 B"},
		{bytes: 1024, want: "1 KiB"},
		{bytes: 1536, want: "1.5 KiB"},
		{bytes: 1024 * 1024, want: "1 MiB"},
		{bytes: 3 * 1024 * 1024 * 1024, want: "3 GiB"},
	}
	for _, test := range tests {
		if got := formatBytes(test.bytes); got != test.want {
			t.Errorf("formatBytes(%d) = %q, want %q", test.bytes, got, test.want)
		}
	}
}
