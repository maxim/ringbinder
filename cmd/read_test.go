package cmd

import "testing"

func TestResolveReadRange_PageWithContext(t *testing.T) {
	t.Parallel()

	start, end, err := resolveReadRange(3, 1, -1, -1)
	if err != nil {
		t.Fatalf("resolveReadRange() error = %v", err)
	}
	if start != 2 || end != 4 {
		t.Fatalf("resolveReadRange() = (%d, %d), want (2, 4)", start, end)
	}
}

func TestResolveReadRange_ClampsToZero(t *testing.T) {
	t.Parallel()

	start, end, err := resolveReadRange(0, 3, -1, -1)
	if err != nil {
		t.Fatalf("resolveReadRange() error = %v", err)
	}
	if start != 0 || end != 3 {
		t.Fatalf("resolveReadRange() = (%d, %d), want (0, 3)", start, end)
	}
}

func TestResolveReadRange_StartEnd(t *testing.T) {
	t.Parallel()

	start, end, err := resolveReadRange(-1, 0, 4, 8)
	if err != nil {
		t.Fatalf("resolveReadRange() error = %v", err)
	}
	if start != 4 || end != 8 {
		t.Fatalf("resolveReadRange() = (%d, %d), want (4, 8)", start, end)
	}
}

func TestResolveReadRange_RejectsInvalidInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		pageIndex   int
		context     int
		startIndex  int
		endIndex    int
		wantErrText string
	}{
		{
			name:        "negative context",
			pageIndex:   0,
			context:     -1,
			startIndex:  -1,
			endIndex:    -1,
			wantErrText: "--context must be >= 0",
		},
		{
			name:        "page combined with range",
			pageIndex:   1,
			context:     0,
			startIndex:  1,
			endIndex:    2,
			wantErrText: "--page cannot be combined with --start/--end",
		},
		{
			name:        "missing selectors",
			pageIndex:   -1,
			context:     0,
			startIndex:  -1,
			endIndex:    -1,
			wantErrText: "either --page or both --start and --end are required",
		},
		{
			name:        "start greater than end",
			pageIndex:   -1,
			context:     0,
			startIndex:  5,
			endIndex:    2,
			wantErrText: "--start must be <= --end",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, _, err := resolveReadRange(tt.pageIndex, tt.context, tt.startIndex, tt.endIndex)
			if err == nil {
				t.Fatalf("resolveReadRange() error = nil, want %q", tt.wantErrText)
			}
			if err.Error() != tt.wantErrText {
				t.Fatalf("resolveReadRange() error = %q, want %q", err.Error(), tt.wantErrText)
			}
		})
	}
}
