package cmd

import (
	"strings"
	"testing"

	"github.com/maxim/ringbinder/internal/config"
	"github.com/spf13/cobra"
)

func TestResolveSweepConcurrency_PrecedenceAndDefault(t *testing.T) {
	seven := 7

	tests := []struct {
		name           string
		cfg            *config.Config
		cliConcurrency string
		want           int
	}{
		{name: "default", want: defaultSweepConcurrency},
		{name: "config", cfg: &config.Config{SweepConcurrency: &seven}, want: 7},
		{name: "CLI overrides config", cfg: &config.Config{SweepConcurrency: &seven}, cliConcurrency: "2", want: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := sweepSettingsCommand(t, tt.cliConcurrency)
			got, err := resolveSweepConcurrency(cmd, tt.cfg)
			if err != nil {
				t.Fatalf("resolveSweepConcurrency() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("resolveSweepConcurrency() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestResolveSweepConcurrency_RejectsExplicitInvalidValues(t *testing.T) {
	zero := 0
	negative := -1

	tests := []struct {
		name           string
		cfg            *config.Config
		cliConcurrency string
		wantError      string
	}{
		{name: "zero CLI", cliConcurrency: "0", wantError: "--concurrency must be >= 1"},
		{name: "negative CLI", cliConcurrency: "-1", wantError: "--concurrency must be >= 1"},
		{name: "zero config", cfg: &config.Config{SweepConcurrency: &zero}, wantError: "sweep_concurrency must be >= 1"},
		{name: "negative config", cfg: &config.Config{SweepConcurrency: &negative}, wantError: "sweep_concurrency must be >= 1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := sweepSettingsCommand(t, tt.cliConcurrency)
			_, err := resolveSweepConcurrency(cmd, tt.cfg)
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("resolveSweepConcurrency() error = %v, want contains %q", err, tt.wantError)
			}
		})
	}
}

func sweepSettingsCommand(t *testing.T, cliConcurrency string) *cobra.Command {
	t.Helper()

	cmd := &cobra.Command{}
	cmd.Flags().Int("concurrency", defaultSweepConcurrency, "")
	if cliConcurrency != "" {
		if err := cmd.Flags().Set("concurrency", cliConcurrency); err != nil {
			t.Fatalf("Set(concurrency) error = %v", err)
		}
	}
	return cmd
}
