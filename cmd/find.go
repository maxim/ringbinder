package cmd

import (
	"fmt"
	"os"
	"strings"

	"github.com/mattn/go-isatty"
	"github.com/maxim/ringbinder/internal/config"
	"github.com/maxim/ringbinder/internal/db"
	"github.com/maxim/ringbinder/internal/format"
	"github.com/spf13/cobra"
)

var (
	findVerbose bool
	findJSON    bool
	findLimit   int
	findOffset  int
	findMode    string
	findRawFTS  string
	findTrigram bool
)

func init() {
	findCmd.Flags().BoolVarP(&findVerbose, "verbose", "v", false, "Show text snippets")
	findCmd.Flags().BoolVar(&findJSON, "json", false, "Emit NDJSON records for machine consumption")
	findCmd.Flags().IntVar(&findLimit, "limit", 50, "Maximum number of results to return")
	findCmd.Flags().IntVar(&findOffset, "offset", 0, "Result offset for pagination")
	findCmd.Flags().StringVar(&findMode, "mode", "and", "Token combine mode: and|or")
	findCmd.Flags().StringVar(&findRawFTS, "fts", "", "Raw FTS5 query; skips tokenization")
	findCmd.Flags().BoolVar(&findTrigram, "trigram", false, "Also query trigram FTS index for OCR-noisy/partial matches")
	rootCmd.AddCommand(findCmd)
}

var findCmd = &cobra.Command{
	Use:   "find [query]",
	Short: "Full-text search across OCR'd documents",
	Long:  "Searches OCR text across indexed documents. Use --json for machine-readable NDJSON output.",
	Args: func(cmd *cobra.Command, args []string) error {
		rawFTS, err := cmd.Flags().GetString("fts")
		if err != nil {
			return fmt.Errorf("read --fts flag: %w", err)
		}
		if len(args) == 0 && strings.TrimSpace(rawFTS) == "" {
			return fmt.Errorf("requires a query argument or --fts")
		}
		return nil
	},
	RunE: runFind,
}

func runFind(cmd *cobra.Command, args []string) error {
	query := strings.Join(args, " ")

	database, err := db.Open(config.DefaultDir())
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	results, err := database.SearchWithOptions(db.SearchOptions{
		Query:           query,
		RawFTS:          findRawFTS,
		Mode:            findMode,
		Limit:           findLimit,
		Offset:          findOffset,
		IncludePathLike: true,
		UseTrigram:      findTrigram,
	})
	if err != nil {
		return fmt.Errorf("search: %w", err)
	}

	if findJSON {
		return format.WriteFindResultsNDJSON(os.Stdout, results)
	}

	if len(results) == 0 {
		fmt.Println("No results found.")
		return nil
	}

	color := isatty.IsTerminal(os.Stdout.Fd())
	fmt.Print(format.FormatFindResults(results, findVerbose, color))
	return nil
}
