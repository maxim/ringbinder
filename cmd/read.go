package cmd

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var (
	readPath    string
	readPage    int
	readContext int
	readStart   int
	readEnd     int
	readJSON    bool
)

func init() {
	readCmd.Flags().StringVar(&readPath, "path", "", "Document path (exact match)")
	readCmd.Flags().IntVar(&readPage, "page", -1, "0-based page index to read")
	readCmd.Flags().IntVar(&readContext, "context", 0, "Include N neighboring pages around --page")
	readCmd.Flags().IntVar(&readStart, "start", -1, "0-based range start (inclusive)")
	readCmd.Flags().IntVar(&readEnd, "end", -1, "0-based range end (inclusive)")
	readCmd.Flags().BoolVar(&readJSON, "json", false, "Emit JSON output")
	rootCmd.AddCommand(readCmd)
}

var readCmd = &cobra.Command{
	Use:   "read --path <path> --page <index>",
	Short: "Read OCR markdown for one or more pages",
	Long:  "Fetches full OCR markdown for a document page, with optional neighboring context pages.",
	RunE:  runRead,
}

type readPageJSON struct {
	PageIndex int    `json:"page_index"`
	Markdown  string `json:"markdown"`
}

type readOutputJSON struct {
	Path  string         `json:"path"`
	Pages []readPageJSON `json:"pages"`
}

func runRead(cmd *cobra.Command, args []string) error {
	if readPath == "" {
		return fmt.Errorf("--path is required")
	}

	startIndex, endIndex, err := resolveReadRange(readPage, readContext, readStart, readEnd)
	if err != nil {
		return err
	}

	database, err := openDatabase(cmd)
	if err != nil {
		return err
	}
	defer database.Close()

	pageRecords, err := database.GetPagesMarkdownByPathAndRange(readPath, startIndex, endIndex)
	if err != nil {
		return fmt.Errorf("read pages: %w", err)
	}
	if len(pageRecords) == 0 {
		return fmt.Errorf("no pages found for %q in range %d..%d", readPath, startIndex, endIndex)
	}

	if readJSON {
		pages := make([]readPageJSON, len(pageRecords))
		for i, pageRecord := range pageRecords {
			pages[i] = readPageJSON{
				PageIndex: pageRecord.PageIndex,
				Markdown:  pageRecord.Markdown,
			}
		}

		jsonEncoder := json.NewEncoder(os.Stdout)
		jsonEncoder.SetEscapeHTML(false)
		return jsonEncoder.Encode(readOutputJSON{
			Path:  readPath,
			Pages: pages,
		})
	}

	fmt.Printf("%s\n", readPath)
	for i, pageRecord := range pageRecords {
		if i > 0 {
			fmt.Println()
		}
		fmt.Printf("--- page %d ---\n", pageRecord.PageIndex+1)
		fmt.Println(pageRecord.Markdown)
	}

	return nil
}

func resolveReadRange(pageIndex, contextPages, startIndex, endIndex int) (int, int, error) {
	if contextPages < 0 {
		return 0, 0, fmt.Errorf("--context must be >= 0")
	}

	if pageIndex >= 0 {
		if startIndex >= 0 || endIndex >= 0 {
			return 0, 0, fmt.Errorf("--page cannot be combined with --start/--end")
		}

		rangeStart := pageIndex - contextPages
		if rangeStart < 0 {
			rangeStart = 0
		}
		rangeEnd := pageIndex + contextPages
		return rangeStart, rangeEnd, nil
	}

	if startIndex < 0 || endIndex < 0 {
		return 0, 0, fmt.Errorf("either --page or both --start and --end are required")
	}
	if startIndex > endIndex {
		return 0, 0, fmt.Errorf("--start must be <= --end")
	}

	return startIndex, endIndex, nil
}
