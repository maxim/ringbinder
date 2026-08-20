package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

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
			Error:        request.LastError,
		}
		if jsonOutput {
			if err := encoder.Encode(output); err != nil {
				return err
			}
			continue
		}
		fmt.Printf(
			"Request %d\t%s\tpages %s\tattempts %d\t%s\n",
			output.RequestID,
			strings.Join(output.Paths, ", "),
			formatAbsolutePageRange(output.PageStart, output.PageEnd),
			output.AttemptCount,
			output.Error,
		)
	}
	return nil
}

func formatAbsolutePageRange(start, end int) string {
	if start == end {
		return fmt.Sprintf("%d", start)
	}
	return fmt.Sprintf("%d-%d", start, end)
}
