package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var cfgFile string

var rootCmd = &cobra.Command{
	Use:   "ringbinder",
	Short: "Scan, OCR, and search your documents",
	Long:  "Ringbinder scans your filesystem for PDFs and images, runs them through OCR, and lets you full-text search the results.",
}

func Execute() error {
	return rootCmd.Execute()
}

func init() {
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default ~/.config/ringbinder/config.yml)")
}

func exitErr(msg string, args ...any) {
	fmt.Fprintf(os.Stderr, msg+"\n", args...)
	os.Exit(1)
}
