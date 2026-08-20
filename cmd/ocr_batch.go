package cmd

import (
	"fmt"

	"github.com/maxim/ringbinder/internal/db"
	"github.com/spf13/cobra"
)

type contentBatch struct {
	contents  []db.Content
	total     int
	excluded  int
	truncated bool
}

func readOCRLimit(cmd *cobra.Command) (int, error) {
	flag := cmd.Flags().Lookup("limit")
	// Zero is the internal unlimited sentinel only when the CLI flag is omitted;
	// an explicit zero is rejected so every user-supplied batch size is positive.
	if flag == nil || !flag.Changed {
		return 0, nil
	}

	limit, err := cmd.Flags().GetInt("limit")
	if err != nil {
		return 0, fmt.Errorf("read --limit flag: %w", err)
	}
	if limit < 1 {
		return 0, fmt.Errorf("--limit must be >= 1")
	}
	return limit, nil
}

func pendingContentBatch(database *db.DB, limit int) (contentBatch, error) {
	contents, excluded, err := database.PendingContentsForDirect()
	if err != nil {
		return contentBatch{}, err
	}

	batch := contentBatch{contents: contents, total: len(contents), excluded: excluded}
	if limit > 0 && limit < batch.total {
		batch.contents = contents[:limit]
		batch.truncated = true
	}
	return batch, nil
}
