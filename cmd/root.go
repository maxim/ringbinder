package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

var (
	version      = "devel"
	cfgFile      string
	databaseFile string
)

var rootCmd = newRootCommand()

func newRootCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "ringbinder",
		Short:   "Scan, OCR, and search your documents",
		Long:    "Ringbinder scans your filesystem for PDFs and images, runs them through OCR, and lets you full-text search the results. Use --json on supported commands for automation/tooling.",
		Version: version,
	}
}

func Execute() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Restore default handling after graceful cancellation so a second signal
	// can force termination if command cleanup gets stuck.
	go func() {
		<-ctx.Done()
		stop()
	}()
	return rootCmd.ExecuteContext(ctx)
}

func init() {
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default ~/.config/ringbinder/config.yml)")
	rootCmd.PersistentFlags().StringVar(&databaseFile, "database", "", "database file path (default ~/.config/ringbinder/ringbinder.db)")
}

func exitErr(msg string, args ...any) {
	fmt.Fprintf(os.Stderr, msg+"\n", args...)
	os.Exit(1)
}
