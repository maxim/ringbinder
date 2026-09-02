package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/maxim/ringbinder/internal/db"
	"github.com/maxim/ringbinder/internal/ocr"
	"github.com/maxim/ringbinder/internal/progress"
	"github.com/spf13/cobra"
)

type batchListItem struct {
	ID             int64  `json:"id"`
	State          string `json:"state"`
	DisplayName    string `json:"display_name"`
	RemoteName     string `json:"remote_name,omitempty"`
	InputFileName  string `json:"input_file_name,omitempty"`
	OutputFileName string `json:"output_file_name,omitempty"`
	RequestCount   int    `json:"request_count"`
	LastError      string `json:"last_error,omitempty"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
	Stale          bool   `json:"stale"`
}

type batchRefreshError struct {
	BatchID *int64 `json:"batch_id"`
	Message string `json:"message"`
}

type batchListEnvelope struct {
	Batches         []batchListItem     `json:"batches"`
	BlockedRequests int                 `json:"blocked_requests"`
	BlockedContents int                 `json:"blocked_contents"`
	CleanupPending  int                 `json:"cleanup_pending"`
	RefreshErrors   []batchRefreshError `json:"refresh_errors"`
}

func runBatchList(cmd *cobra.Command, args []string) error {
	ensureCommandContext(cmd)
	jsonOutput, err := cmd.Flags().GetBool("json")
	if err != nil {
		return fmt.Errorf("read --json flag: %w", err)
	}
	command, err := openGeminiBatchCommand(cmd, true, true)
	if err != nil {
		return err
	}
	defer command.Close()
	transport := newGeminiBatchAPI(command.apiKey)
	out := commandStdout(cmd)
	errOut := commandStderr(cmd)

	var coordinator *progressCoordinator
	if !jsonOutput {
		coordinator = newCommandProgress(cmd)
		defer func() { coordinator.Finish(cmd.Context().Err() != nil) }()
	}
	warn := func(format string, args ...any) {
		if coordinator != nil {
			coordinator.Warningf(format, args...)
			return
		}
		fmt.Fprintf(errOut, format, args...)
	}

	batches, err := command.database.ListGeminiBatches()
	if err != nil {
		return fmt.Errorf("list Gemini batches: %w", err)
	}
	remoteCount := 0
	for _, batch := range batches {
		if batch.RemoteName != "" {
			remoteCount++
		}
	}
	var refreshing *progress.Reporter
	if coordinator != nil && remoteCount > 0 {
		refreshing = coordinator.StartPhase(progress.PhaseOptions{
			Label: "Refreshing Gemini batches", Total: remoteCount, Unit: "batches",
		})
	}
	stale := make(map[int64]bool)
	refreshed := make(map[int64]bool)
	refreshErrors := make([]batchRefreshError, 0)
	for _, batch := range batches {
		if batch.RemoteName == "" {
			continue
		}
		if contextErr := cmd.Context().Err(); contextErr != nil {
			return contextErr
		}
		if refreshing != nil {
			refreshing.SetCurrent(fmt.Sprintf("Gemini batch %d", batch.ID))
		}
		remote, refreshErr := transport.GetBatch(cmd.Context(), batch.RemoteName)
		// GetBatch may finish while its context is cancelled. Returning here keeps
		// a stale terminal response from changing local batch ownership.
		if contextErr := cmd.Context().Err(); contextErr != nil {
			return contextErr
		}
		if refreshErr != nil {
			stale[batch.ID] = true
			warn("warning: refresh Gemini batch %d: %v\n", batch.ID, refreshErr)
			if refreshing != nil {
				refreshing.Advance()
			}
			if ocr.IsGeminiGlobalFailure(refreshErr) {
				refreshErrors = append(refreshErrors, batchRefreshError{BatchID: nil, Message: refreshErr.Error()})
				for _, unrefreshed := range batches {
					if unrefreshed.RemoteName != "" && !refreshed[unrefreshed.ID] {
						stale[unrefreshed.ID] = true
					}
				}
				break
			}
			id := batch.ID
			refreshErrors = append(refreshErrors, batchRefreshError{BatchID: &id, Message: refreshErr.Error()})
			continue
		}
		state, refreshErr := ocr.NormalizeGeminiBatchState(remote.State)
		if refreshErr != nil {
			id := batch.ID
			refreshErrors = append(refreshErrors, batchRefreshError{BatchID: &id, Message: refreshErr.Error()})
			stale[batch.ID] = true
			warn("warning: refresh Gemini batch %d: %v\n", batch.ID, refreshErr)
			if refreshing != nil {
				refreshing.Advance()
			}
			continue
		}
		if batch.State == db.GeminiBatchCancelling &&
			(state == db.GeminiBatchPending || state == db.GeminiBatchRunning || state == db.GeminiBatchCancelling) {
			state = db.GeminiBatchCancelling
		}
		refreshErr = command.database.SetGeminiBatchState(
			batch.ID, state, remote.OutputFileName, remote.ErrorMessage, time.Now().UTC(),
		)
		if refreshErr != nil {
			id := batch.ID
			refreshErrors = append(refreshErrors, batchRefreshError{BatchID: &id, Message: refreshErr.Error()})
			stale[batch.ID] = true
			warn("warning: refresh Gemini batch %d: %v\n", batch.ID, refreshErr)
			if refreshing != nil {
				refreshing.Advance()
			}
			continue
		}
		refreshed[batch.ID] = true
		if refreshing != nil {
			refreshing.Advance()
		}
	}
	if refreshing != nil {
		if cmd.Context().Err() != nil {
			coordinator.StopPhase(refreshing)
		} else {
			// The table or no-work message below is this phase's durable result.
			coordinator.ClosePhase(refreshing)
		}
	}

	batches, err = command.database.ListGeminiBatches()
	if err != nil {
		return fmt.Errorf("reload Gemini batches: %w", err)
	}
	cleanup, err := command.database.CountGeminiCleanup()
	if err != nil {
		return fmt.Errorf("count Gemini cleanup: %w", err)
	}
	blockedSummary, err := loadBatchBlockedSummary(command.database)
	if err != nil {
		return fmt.Errorf("summarize blocked Gemini requests: %w", err)
	}
	envelope := batchListEnvelope{
		Batches:         make([]batchListItem, 0, len(batches)),
		BlockedRequests: blockedSummary.Requests,
		BlockedContents: blockedSummary.Contents,
		CleanupPending:  cleanup,
		RefreshErrors:   refreshErrors,
	}
	for _, batch := range batches {
		requests, queryErr := command.database.GeminiRequestsForBatch(batch.ID)
		if queryErr != nil {
			return fmt.Errorf("list requests for Gemini batch %d: %w", batch.ID, queryErr)
		}
		envelope.Batches = append(envelope.Batches, batchListItem{
			ID:             batch.ID,
			State:          batch.State,
			DisplayName:    batch.DisplayName,
			RemoteName:     batch.RemoteName,
			InputFileName:  batch.InputFileName,
			OutputFileName: batch.OutputFileName,
			RequestCount:   len(requests),
			LastError:      batch.LastError,
			CreatedAt:      batch.CreatedAt.Format(time.RFC3339Nano),
			UpdatedAt:      batch.UpdatedAt.Format(time.RFC3339Nano),
			Stale:          stale[batch.ID],
		})
	}

	if jsonOutput {
		encoder := json.NewEncoder(out)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(envelope); err != nil {
			return err
		}
	} else {
		if len(envelope.Batches) == 0 && blockedSummary.Requests == 0 {
			fmt.Fprintln(out, "No tracked Gemini batches.")
		}
		for _, batch := range envelope.Batches {
			staleLabel := ""
			if batch.Stale {
				staleLabel = " (stale)"
			}
			fmt.Fprintf(out, "%d\t%s%s\t%d request(s)\t%s\n", batch.ID, batch.State, staleLabel, batch.RequestCount, batch.DisplayName)
			if batch.LastError != "" {
				fmt.Fprintf(out, "  Last error: %s\n", batch.LastError)
			}
		}
		if cleanup > 0 {
			fmt.Fprintf(out, "%d remote cleanup pending\n", cleanup)
		}
		printBatchBlockedSummaryTo(out, blockedSummary)
	}
	if len(refreshErrors) > 0 {
		return errors.New("one or more Gemini batch refreshes failed")
	}
	return nil
}

func runBatchCancel(cmd *cobra.Command, args []string) error {
	ensureCommandContext(cmd)
	batchID, err := parsePositiveID(args[0], "batch")
	if err != nil {
		return err
	}
	command, err := openGeminiBatchCommand(cmd, true, true)
	if err != nil {
		return err
	}
	defer command.Close()

	batch, err := command.database.GetGeminiBatch(batchID)
	if err != nil {
		return err
	}
	if batch == nil {
		return fmt.Errorf("Gemini batch %d not found", batchID)
	}
	if batch.RemoteName == "" || batch.State == db.GeminiBatchSucceeded ||
		batch.State == db.GeminiBatchFailed || batch.State == db.GeminiBatchCancelled ||
		batch.State == db.GeminiBatchExpired {
		return fmt.Errorf("Gemini batch %d is not cancellable in state %s", batchID, batch.State)
	}
	// Persist intent before the request so interruption or an ambiguous cancel
	// keeps ownership blocked until batch continue reconciles the remote state.
	if err := command.database.SetGeminiBatchState(
		batchID, db.GeminiBatchCancelling, "", "", time.Now().UTC(),
	); err != nil {
		return err
	}
	coordinator := newCommandProgress(cmd)
	defer func() { coordinator.Finish(cmd.Context().Err() != nil) }()
	cancelling := coordinator.StartPhase(progress.PhaseOptions{
		Label: fmt.Sprintf("Requesting cancellation for Gemini batch %d", batchID),
	})
	transport := newGeminiBatchAPI(command.apiKey)
	if err := transport.CancelBatch(cmd.Context(), batch.RemoteName); err != nil {
		if cmd.Context().Err() != nil {
			coordinator.StopPhase(cancelling)
		} else {
			coordinator.ClosePhase(cancelling)
		}
		return fmt.Errorf("cancel Gemini batch %d: %w", batchID, err)
	}
	// The confirmation below is the phase's durable completion message.
	coordinator.ClosePhase(cancelling)
	fmt.Fprintf(
		commandStdout(cmd),
		"Cancellation requested for Gemini batch %d. Direct OCR remains unavailable until batch continue confirms and handles the terminal state.\n",
		batchID,
	)
	return nil
}

func runBatchForget(cmd *cobra.Command, args []string) error {
	batchID, err := parsePositiveID(args[0], "batch")
	if err != nil {
		return err
	}
	command, err := openGeminiBatchCommand(cmd, false, true)
	if err != nil {
		return err
	}
	defer command.Close()
	batch, err := command.database.ForgetGeminiBatch(batchID, time.Now().UTC())
	if err != nil {
		return err
	}
	if batch == nil {
		return fmt.Errorf("Gemini batch %d not found", batchID)
	}
	fmt.Printf("Forgot Gemini batch %d; completed OCR pages were retained.\n", batchID)
	return nil
}

func parsePositiveID(value, kind string) (int64, error) {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id < 1 {
		return 0, fmt.Errorf("invalid %s ID %q: must be a positive integer", kind, value)
	}
	return id, nil
}
