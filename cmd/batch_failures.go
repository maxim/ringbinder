package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/maxim/ringbinder/internal/db"
	"github.com/spf13/cobra"
)

type batchFailureOutput struct {
	RequestID    int64    `json:"request_id"`
	Paths        []string `json:"paths"`
	PageStart    int      `json:"page_start"`
	PageEnd      int      `json:"page_end"`
	AttemptCount int      `json:"attempt_count"`
	Error        string   `json:"error"`
}

type batchBlockedSummary struct {
	Requests int
	Contents int
}

func loadBatchBlockedSummary(database *db.DB) (batchBlockedSummary, error) {
	requests, err := database.BlockedGeminiRequests()
	if err != nil {
		return batchBlockedSummary{}, err
	}
	contents := make(map[int64]struct{}, len(requests))
	for _, request := range requests {
		contents[request.ContentID] = struct{}{}
	}
	return batchBlockedSummary{Requests: len(requests), Contents: len(contents)}, nil
}

func reportBatchBlockedSummary(database *db.DB) error {
	return reportBatchBlockedSummaryTo(os.Stdout, database)
}

func reportBatchBlockedSummaryTo(out io.Writer, database *db.DB) error {
	summary, err := loadBatchBlockedSummary(database)
	if err != nil {
		return fmt.Errorf("summarize blocked Gemini requests: %w", err)
	}
	printBatchBlockedSummaryTo(out, summary)
	return nil
}

func printBatchBlockedSummary(summary batchBlockedSummary) {
	printBatchBlockedSummaryTo(os.Stdout, summary)
}

func printBatchBlockedSummaryTo(out io.Writer, summary batchBlockedSummary) {
	if summary.Requests == 0 {
		return
	}
	rangeLabel := "ranges"
	if summary.Requests == 1 {
		rangeLabel = "range"
	}
	contentLabel := "content items"
	if summary.Contents == 1 {
		contentLabel = "content item"
	}
	verb := "require"
	if summary.Requests == 1 {
		verb = "requires"
	}
	fmt.Fprintf(
		out,
		"%d blocked Gemini batch OCR page %s across %d %s %s attention.\n",
		summary.Requests,
		rangeLabel,
		summary.Contents,
		contentLabel,
		verb,
	)
	fmt.Fprintln(out, "Run `ringbinder batch failures` for details and recovery commands.")
}

func runBatchFailures(cmd *cobra.Command, args []string) error {
	command, err := openGeminiBatchCommand(cmd, false, false)
	if err != nil {
		return err
	}
	defer command.Close()
	requests, err := command.database.BlockedGeminiRequests()
	if err != nil {
		return fmt.Errorf("list blocked Gemini requests: %w", err)
	}
	jsonOutput, err := cmd.Flags().GetBool("json")
	if err != nil {
		return fmt.Errorf("read --json flag: %w", err)
	}
	if len(requests) == 0 && !jsonOutput {
		fmt.Println("No blocked Gemini batch OCR requests.")
		return nil
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	for _, request := range requests {
		paths, err := command.database.GetDocumentPathsForContent(request.ContentID)
		if err != nil {
			return fmt.Errorf("list paths for blocked Gemini request %d: %w", request.ID, err)
		}
		if paths == nil {
			paths = []string{}
		}
		output := batchFailureOutput{
			RequestID:    request.ID,
			Paths:        paths,
			PageStart:    request.PageStart + 1,
			PageEnd:      request.PageEnd,
			AttemptCount: request.AttemptCount,
			Error:        explainGeminiFailure(request.LastError),
		}
		if jsonOutput {
			if err := encoder.Encode(output); err != nil {
				return err
			}
			continue
		}
		fmt.Printf(
			"Request %d\t%s\tpages %s\tautomatic retries %d\t%s\n",
			output.RequestID,
			strings.Join(output.Paths, ", "),
			formatAbsolutePageRange(output.PageStart, output.PageEnd),
			output.AttemptCount,
			output.Error,
		)
	}
	if !jsonOutput {
		fmt.Println()
		fmt.Println("Recovery commands:")
		fmt.Println("  Retry one range at direct Gemini pricing: ringbinder batch retry <request-id> --mode direct")
		fmt.Println("  Retry all pending documents with Mistral: ringbinder ocr --model mistral")
	}
	return nil
}

func explainGeminiFailure(message string) string {
	// Earlier builds persisted this terse decoder error, so clarify existing
	// database rows as well as failures recorded with the current wording.
	if message == "invalid Gemini finish reason: RECITATION" {
		return "Gemini stopped generation for potential recitation (RECITATION)"
	}
	return message
}

func formatAbsolutePageRange(start, end int) string {
	if start == end {
		return fmt.Sprintf("%d", start)
	}
	return fmt.Sprintf("%d-%d", start, end)
}
