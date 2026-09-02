package cmd

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/maxim/ringbinder/internal/db"
	"github.com/spf13/cobra"
)

func init() {
	ocrCmd.Flags().StringArray("model", nil, "OCR model in priority order (repeat: mistral or gemini)")
	ocrCmd.Flags().Int("limit", 0, "Maximum number of pending documents to process")
	rootCmd.AddCommand(ocrCmd)
}

var ocrCmd = &cobra.Command{
	Use:   "ocr",
	Short: "Run OCR on documents",
	Long:  "Processes missing pages with the selected OCR models and stores successful results.",
	RunE:  runOCR,
}

func runOCR(cmd *cobra.Command, args []string) error {
	ensureCommandContext(cmd)
	cfg, err := loadCommandConfig(cmd, "model")
	if err != nil {
		return err
	}
	settings, err := resolveOCRSettings(cmd, cfg)
	if err != nil {
		return err
	}
	limit, err := readOCRLimit(cmd)
	if err != nil {
		return err
	}

	// OCR runs fail before touching SQLite unless every configured model can be
	// constructed. Cost and batch commands deliberately do not do this.
	providers, err := newOCRProviderChain(settings.models, time.Now().UTC())
	if err != nil {
		return err
	}
	dbPath, err := resolveDatabasePath(cmd, cfg)
	if err != nil {
		return err
	}
	coordinator, err := acquireOCRCoordinator(dbPath)
	if err != nil {
		return err
	}
	defer coordinator.Close()
	database, err := db.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	progressOutput := newCommandProgress(cmd)
	defer func() { progressOutput.Finish(cmd.Context().Err() != nil) }()
	return processOCRChain(
		cmd.Context(), database, providers, settings.models, limit,
		commandStdout(cmd), progressOutput.ErrWriter(), progressOutput,
	)
}

func replacePagesWhileActive(ctx context.Context, writeMu *sync.Mutex, replace func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	writeMu.Lock()
	defer writeMu.Unlock()

	// Cancellation cannot interrupt a SQLite transaction already in progress,
	// so the last safe boundary is immediately after acquiring the write lock.
	if err := ctx.Err(); err != nil {
		return err
	}
	return replace()
}

func classifyPath(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".pdf":
		return "pdf"
	case ".jpg", ".jpeg":
		return "jpeg"
	case ".png":
		return "png"
	default:
		return ""
	}
}
