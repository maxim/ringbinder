package cmd

import (
	"testing"
	"time"
)

func TestParseDocListTimeFlag_Empty(t *testing.T) {
	t.Parallel()

	parsed, err := parseDocListTimeFlag("")
	if err != nil {
		t.Fatalf("parseDocListTimeFlag() error = %v", err)
	}
	if parsed != nil {
		t.Fatalf("parseDocListTimeFlag() = %v, want nil", parsed)
	}
}

func TestParseDocListTimeFlag_RFC3339(t *testing.T) {
	t.Parallel()

	parsed, err := parseDocListTimeFlag("2025-01-15T10:30:00Z")
	if err != nil {
		t.Fatalf("parseDocListTimeFlag() error = %v", err)
	}
	if parsed == nil {
		t.Fatalf("parseDocListTimeFlag() = nil, want time")
	}

	want := time.Date(2025, time.January, 15, 10, 30, 0, 0, time.UTC)
	if !parsed.Equal(want) {
		t.Fatalf("parseDocListTimeFlag() = %s, want %s", parsed.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
	}
}

func TestParseDocListTimeFlag_DateOnly(t *testing.T) {
	t.Parallel()

	parsed, err := parseDocListTimeFlag("2025-01-15")
	if err != nil {
		t.Fatalf("parseDocListTimeFlag() error = %v", err)
	}
	if parsed == nil {
		t.Fatalf("parseDocListTimeFlag() = nil, want time")
	}

	want := time.Date(2025, time.January, 15, 0, 0, 0, 0, time.UTC)
	if !parsed.Equal(want) {
		t.Fatalf("parseDocListTimeFlag() = %s, want %s", parsed.Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
	}
}

func TestParseDocListTimeFlag_Invalid(t *testing.T) {
	t.Parallel()

	if _, err := parseDocListTimeFlag("01/15/2025"); err == nil {
		t.Fatalf("parseDocListTimeFlag() error = nil, want error")
	}
}
