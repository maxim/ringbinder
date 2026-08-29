package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/maxim/ringbinder/internal/config"
	"github.com/maxim/ringbinder/internal/db"
	"github.com/maxim/ringbinder/internal/ocr"
	"github.com/spf13/cobra"
)

func init() {
	batchCostCmd.Flags().Int("limit", 0, "Maximum number of pending content items to estimate")
	batchStartCmd.Flags().Int("limit", 0, "Maximum number of pending content items to start")
	batchListCmd.Flags().Bool("json", false, "Output one JSON status envelope")
	batchFailuresCmd.Flags().Bool("json", false, "Output newline-delimited JSON")
	batchRetryCmd.Flags().String("mode", "", "Retry mode (required: direct)")

	batchCmd.AddCommand(
		batchCostCmd,
		batchStartCmd,
		batchContinueCmd,
		batchListCmd,
		batchCancelCmd,
		batchForgetCmd,
		batchFailuresCmd,
		batchRetryCmd,
	)
	rootCmd.AddCommand(batchCmd)
}

type geminiBatchAPI interface {
	UploadJSONL(context.Context, string, io.ReadSeeker, int64) (ocr.GeminiRemoteFile, error)
	CreateBatch(context.Context, string, string, string) (ocr.GeminiRemoteBatch, error)
	GetBatch(context.Context, string) (ocr.GeminiRemoteBatch, error)
	ListBatches(context.Context) ([]ocr.GeminiRemoteBatch, error)
	ListFiles(context.Context) ([]ocr.GeminiRemoteFile, error)
	CancelBatch(context.Context, string) error
	DeleteBatch(context.Context, string) error
	DeleteFile(context.Context, string) error
	DownloadFile(context.Context, string) (io.ReadCloser, error)
}

var newGeminiBatchAPI = func(apiKey string) geminiBatchAPI {
	return ocr.NewGeminiBatchTransport(apiKey)
}

var batchCmd = &cobra.Command{
	Use:   "batch",
	Short: "Manage discounted asynchronous Gemini OCR",
	Long:  "Starts and advances explicit asynchronous Gemini Batch API OCR. Ordinary ringbinder ocr remains direct and blocking.",
}

var batchCostCmd = &cobra.Command{
	Use:   "cost",
	Short: "Estimate discounted Gemini batch OCR cost",
	Args:  cobra.NoArgs,
	RunE:  runBatchCost,
}

var batchStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start discounted OCR for unassigned pending pages",
	Args:  cobra.NoArgs,
	RunE:  runBatchStart,
}

var batchContinueCmd = &cobra.Command{
	Use:   "continue",
	Short: "Advance every tracked Gemini batch once",
	Args:  cobra.NoArgs,
	RunE:  runBatchContinue,
}

var batchListCmd = &cobra.Command{
	Use:   "list",
	Short: "Refresh and list tracked Gemini batches without importing output",
	Args:  cobra.NoArgs,
	RunE:  runBatchList,
}

var batchCancelCmd = &cobra.Command{
	Use:   "cancel <local-id>",
	Short: "Request asynchronous cancellation of one Gemini batch",
	Args:  cobra.ExactArgs(1),
	RunE:  runBatchCancel,
}

var batchForgetCmd = &cobra.Command{
	Use:   "forget <local-id>",
	Short: "Forget one batch while retaining completed OCR pages",
	Args:  cobra.ExactArgs(1),
	RunE:  runBatchForget,
}

var batchFailuresCmd = &cobra.Command{
	Use:   "failures",
	Short: "List blocked Gemini batch OCR page ranges",
	Args:  cobra.NoArgs,
	RunE:  runBatchFailures,
}

var batchRetryCmd = &cobra.Command{
	Use:   "retry <request-id> --mode direct",
	Short: "Retry one blocked page range synchronously at direct Gemini pricing",
	Long:  "Retries one blocked range directly. Blocked requests can never be returned to discounted batch processing.",
	Args:  cobra.ExactArgs(1),
	RunE:  runBatchRetry,
}

type batchCommandContext struct {
	config       *config.Config
	database     *db.DB
	databasePath string
	apiKey       string
	coordinator  *ocrCoordinatorLock
}

func openGeminiBatchCommand(
	cmd *cobra.Command,
	requireAPIKey, lockCoordinator bool,
) (*batchCommandContext, error) {
	cfg, err := loadCommandConfig(cmd)
	if err != nil {
		return nil, err
	}
	dbPath, err := resolveDatabasePath(cmd, cfg)
	if err != nil {
		return nil, err
	}

	apiKey := ""
	if requireAPIKey {
		apiKey = os.Getenv("GEMINI_API_KEY")
		if apiKey == "" {
			return nil, errors.New("GEMINI_API_KEY environment variable is not set")
		}
	}

	var coordinator *ocrCoordinatorLock
	if lockCoordinator {
		coordinator, err = acquireOCRCoordinator(dbPath)
		if err != nil {
			return nil, err
		}
	}
	database, err := db.Open(dbPath)
	if err != nil {
		if coordinator != nil {
			_ = coordinator.Close()
		}
		return nil, fmt.Errorf("open database: %w", err)
	}
	return &batchCommandContext{
		config:       cfg,
		database:     database,
		databasePath: dbPath,
		apiKey:       apiKey,
		coordinator:  coordinator,
	}, nil
}

func (ctx *batchCommandContext) Close() {
	if ctx == nil {
		return
	}
	if ctx.database != nil {
		_ = ctx.database.Close()
	}
	if ctx.coordinator != nil {
		_ = ctx.coordinator.Close()
	}
}
