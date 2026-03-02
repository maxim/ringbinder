package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/maxim/ringbinder/internal/config"
	"github.com/maxim/ringbinder/internal/db"
	"github.com/spf13/cobra"
)

var (
	docGetPath string
	docGetJSON bool

	docListAfter  string
	docListBefore string
	docListLimit  int
	docListOffset int
	docListJSON   bool
)

func init() {
	docGetCmd.Flags().StringVar(&docGetPath, "path", "", "Document path (exact match)")
	docGetCmd.Flags().BoolVar(&docGetJSON, "json", false, "Emit JSON output")

	docListCmd.Flags().StringVar(&docListAfter, "after", "", "Include documents created at or after this time (RFC3339 or YYYY-MM-DD)")
	docListCmd.Flags().StringVar(&docListBefore, "before", "", "Include documents created before this time (RFC3339 or YYYY-MM-DD)")
	docListCmd.Flags().IntVar(&docListLimit, "limit", 50, "Maximum number of results to return")
	docListCmd.Flags().IntVar(&docListOffset, "offset", 0, "Result offset for pagination")
	docListCmd.Flags().BoolVar(&docListJSON, "json", false, "Emit NDJSON output")

	docCmd.AddCommand(docGetCmd)
	docCmd.AddCommand(docListCmd)
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

var docListCmd = &cobra.Command{
	Use:   "list",
	Short: "List documents with optional created-at range filters",
	RunE:  runDocList,
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

func runDocList(cmd *cobra.Command, args []string) error {
	if docListLimit < 0 {
		return fmt.Errorf("--limit must be >= 0")
	}
	if docListOffset < 0 {
		return fmt.Errorf("--offset must be >= 0")
	}

	after, err := parseDocListTimeFlag(docListAfter)
	if err != nil {
		return fmt.Errorf("parse --after: %w", err)
	}
	before, err := parseDocListTimeFlag(docListBefore)
	if err != nil {
		return fmt.Errorf("parse --before: %w", err)
	}

	database, err := db.Open(config.DefaultDir())
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	documents, err := database.ListDocuments(db.ListOptions{
		After:  after,
		Before: before,
		Limit:  docListLimit,
		Offset: docListOffset,
	})
	if err != nil {
		return fmt.Errorf("list documents: %w", err)
	}

	if docListJSON {
		jsonEncoder := json.NewEncoder(os.Stdout)
		jsonEncoder.SetEscapeHTML(false)
		for _, document := range documents {
			payload := docGetOutputJSON{
				Path:       document.Path,
				CreatedAt:  document.CreatedAt.Format(time.RFC3339Nano),
				ModifiedAt: document.ModifiedAt.Format(time.RFC3339Nano),
				PageCount:  document.PageCount,
				OCRPending: document.OCRPending,
				Deleted:    document.Deleted,
			}
			if err := jsonEncoder.Encode(payload); err != nil {
				return err
			}
		}
		return nil
	}

	for _, document := range documents {
		fmt.Printf(
			"%s  %s  (%d pages)\n",
			document.CreatedAt.Format(time.RFC3339Nano),
			document.Path,
			document.PageCount,
		)
	}
	if len(documents) > 0 {
		fmt.Println()
	}
	fmt.Printf("%d document(s).\n", len(documents))
	return nil
}

func parseDocListTimeFlag(raw string) (*time.Time, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil, nil
	}

	layouts := []string{
		time.RFC3339,
		"2006-01-02",
	}
	for _, layout := range layouts {
		parsed, err := time.Parse(layout, value)
		if err == nil {
			if layout == "2006-01-02" {
				parsed = parsed.UTC()
			}
			return &parsed, nil
		}
	}

	return nil, fmt.Errorf("invalid value %q (accepted: RFC3339 or YYYY-MM-DD)", raw)
}
