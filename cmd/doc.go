package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/maxim/ringbinder/internal/config"
	"github.com/maxim/ringbinder/internal/db"
	"github.com/spf13/cobra"
)

var (
	docGetPath string
	docGetJSON bool
)

func init() {
	docGetCmd.Flags().StringVar(&docGetPath, "path", "", "Document path (exact match)")
	docGetCmd.Flags().BoolVar(&docGetJSON, "json", false, "Emit JSON output")

	docCmd.AddCommand(docGetCmd)
	rootCmd.AddCommand(docCmd)
}

var docCmd = &cobra.Command{
	Use:   "doc",
	Short: "Document metadata commands",
}

var docGetCmd = &cobra.Command{
	Use:   "get --path <path>",
	Short: "Fetch metadata for a document",
	RunE:  runDocGet,
}

type docGetOutputJSON struct {
	Path       string `json:"path"`
	CreatedAt  string `json:"created_at"`
	ModifiedAt string `json:"modified_at"`
	PageCount  int    `json:"page_count"`
	OCRPending bool   `json:"ocr_pending"`
	Deleted    bool   `json:"deleted"`
}

func runDocGet(cmd *cobra.Command, args []string) error {
	if docGetPath == "" {
		return fmt.Errorf("--path is required")
	}

	database, err := db.Open(config.DefaultDir())
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	document, err := database.GetDocumentByPath(docGetPath)
	if err != nil {
		return fmt.Errorf("load document: %w", err)
	}
	if document == nil {
		return fmt.Errorf("document not found: %s", docGetPath)
	}

	payload := docGetOutputJSON{
		Path:       document.Path,
		CreatedAt:  document.CreatedAt.Format(time.RFC3339Nano),
		ModifiedAt: document.ModifiedAt.Format(time.RFC3339Nano),
		PageCount:  document.PageCount,
		OCRPending: document.OCRPending,
		Deleted:    document.Deleted,
	}

	if docGetJSON {
		jsonEncoder := json.NewEncoder(os.Stdout)
		jsonEncoder.SetEscapeHTML(false)
		return jsonEncoder.Encode(payload)
	}

	fmt.Printf("path: %s\n", payload.Path)
	fmt.Printf("created_at: %s\n", payload.CreatedAt)
	fmt.Printf("modified_at: %s\n", payload.ModifiedAt)
	fmt.Printf("page_count: %d\n", payload.PageCount)
	fmt.Printf("ocr_pending: %t\n", payload.OCRPending)
	fmt.Printf("deleted: %t\n", payload.Deleted)
	return nil
}
