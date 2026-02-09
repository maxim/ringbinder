package cmd

import (
	"fmt"
	"strings"

	"github.com/maxim/ringbinder/internal/config"
	"github.com/maxim/ringbinder/internal/db"
	"github.com/spf13/cobra"
)

func init() {
	rootCmd.AddCommand(findCmd)
}

var findCmd = &cobra.Command{
	Use:   "find <query>",
	Short: "Full-text search across OCR'd documents",
	Long:  "Searches the OCR'd text content of all indexed documents and shows matching file paths, pages, and text snippets.",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runFind,
}

func runFind(cmd *cobra.Command, args []string) error {
	query := strings.Join(args, " ")

	database, err := db.Open(config.DefaultDir())
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	results, err := database.Search(query)
	if err != nil {
		return fmt.Errorf("search: %w", err)
	}

	if len(results) == 0 {
		fmt.Println("No results found.")
		return nil
	}

	for _, r := range results {
		fmt.Printf("%s (page %d)\n  %s\n\n", r.Path, r.PageIndex+1, r.Snippet)
	}

	fmt.Printf("%d result(s) found.\n", len(results))
	return nil
}
