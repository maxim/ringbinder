package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/maxim/ringbinder/internal/db"
	"github.com/maxim/ringbinder/internal/ocr"
	"github.com/maxim/ringbinder/internal/progress"
	"github.com/spf13/cobra"
)

const maxAutomaticRequestsPerContinue = 100

type batchContinueTotals struct {
	billing   ocr.BillingReport
	accounted bool
}

type batchAdvanceDisposition int

const (
	batchAdvanceDidWork batchAdvanceDisposition = iota
	batchAdvancePollOnly
)

type incompleteGeminiOutputError struct {
	Err error
}

func (e *incompleteGeminiOutputError) Error() string {
	return fmt.Sprintf("incomplete Gemini output download: %v", e.Err)
}

func (e *incompleteGeminiOutputError) Unwrap() error { return e.Err }

type storedGeminiOutput struct {
	offset int64
	length int
}

type geminiOutputSpool struct {
	file             *os.File
	byKey            map[string]db.GeminiBatchRequest
	stored           map[string]storedGeminiOutput
	duplicates       map[string]bool
	validationErrors []error
}

type geminiOutputLine struct {
	Key      string          `json:"key"`
	Response json.RawMessage `json:"response"`
	Error    json.RawMessage `json:"error"`
}

type geminiRequestError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}

func runBatchContinue(cmd *cobra.Command, args []string) error {
	ensureCommandContext(cmd)
	command, err := openGeminiBatchCommand(cmd, true, true)
	if err != nil {
		return err
	}
	defer command.Close()
	transport := newGeminiBatchAPI(command.apiKey)
	planner := ocr.NewGeminiClient("", time.Now().UTC())
	out := commandStdout(cmd)
	coordinator := newCommandProgress(cmd)
	defer func() { coordinator.Finish(cmd.Context().Err() != nil) }()

	batches, err := command.database.ListGeminiBatches()
	if err != nil {
		return fmt.Errorf("list Gemini batches: %w", err)
	}
	var checking *progress.Reporter
	if len(batches) > 0 {
		checking = coordinator.StartPhase(progress.PhaseOptions{
			Label: "Checking Gemini batches", Total: len(batches), Unit: "batches",
		})
	}
	var totals batchContinueTotals
	var commandErrors []error
	globalFailure := false
	didWork := false
	pollOnlyBatches := 0
	// List order is newest-first for users; lifecycle advancement is oldest-first
	// so long-running work is reconciled before newer submissions.
	for i := len(batches) - 1; i >= 0; i-- {
		if contextErr := cmd.Context().Err(); contextErr != nil {
			commandErrors = append(commandErrors, contextErr)
			globalFailure = true
			break
		}
		batch := batches[i]
		if checking != nil {
			checking.SetCurrent(fmt.Sprintf("Gemini batch %d", batch.ID))
		}
		disposition, advanceErr := advanceGeminiBatch(
			cmd, command.database, transport, planner, batch, &totals, coordinator,
		)
		if checking != nil && cmd.Context().Err() == nil {
			checking.Advance()
		}
		if advanceErr == nil {
			if disposition == batchAdvancePollOnly {
				pollOnlyBatches++
			} else {
				didWork = true
			}
			continue
		}
		commandErrors = append(commandErrors, fmt.Errorf("Gemini batch %d: %w", batch.ID, advanceErr))
		if cmd.Context().Err() != nil {
			globalFailure = true
			break
		}
		_ = command.database.SetGeminiBatchError(batch.ID, advanceErr.Error(), time.Now().UTC())
		if ocr.IsGeminiGlobalFailure(advanceErr) {
			globalFailure = true
			break
		}
	}
	if checking != nil {
		if cmd.Context().Err() != nil {
			if coordinator.StoppedOutcomeShown() {
				coordinator.ClosePhase(checking)
			} else {
				coordinator.StopPhase(checking)
			}
		} else if len(commandErrors) == 0 {
			coordinator.CompletePhase(checking)
		} else {
			coordinator.ClosePhase(checking)
		}
	}

	if !globalFailure {
		completed, retireErr := command.database.RetireCompletedGeminiRequests()
		if retireErr != nil {
			commandErrors = append(commandErrors, fmt.Errorf("retire completed Gemini requests: %w", retireErr))
		} else if completed > 0 {
			didWork = true
		}
		retryFound, retryErrors := submitRetryableGeminiRequests(
			cmd, command.database, transport, planner, coordinator,
		)
		if retryFound {
			didWork = true
		}
		commandErrors = append(commandErrors, retryErrors...)
		if contextErr := cmd.Context().Err(); contextErr != nil {
			if !errors.Is(errors.Join(retryErrors...), contextErr) {
				commandErrors = append(commandErrors, contextErr)
			}
			globalFailure = true
		} else {
			for _, retryErr := range retryErrors {
				if ocr.IsGeminiGlobalFailure(retryErr) {
					globalFailure = true
					break
				}
			}
		}
	}
	if !globalFailure {
		cleanupFound, cleanupErrors := retryGeminiCleanup(cmd, command.database, transport, coordinator)
		if cleanupFound {
			didWork = true
		}
		commandErrors = append(commandErrors, cleanupErrors...)
	}
	blockedSummary, summaryErr := loadBatchBlockedSummary(command.database)
	if summaryErr != nil {
		commandErrors = append(commandErrors, fmt.Errorf("summarize blocked Gemini requests: %w", summaryErr))
	}

	if totals.accounted {
		if totals.billing.Indeterminate {
			fmt.Fprintf(out, "Known batch OCR cost: %s (actual cost may be higher)\n", ocr.FormatCurrency(totals.billing.KnownCost))
		} else {
			fmt.Fprintf(out, "Batch OCR cost: %s\n", ocr.FormatCurrency(totals.billing.KnownCost))
		}
	}
	if len(commandErrors) == 0 && !didWork {
		if len(batches) == 0 && blockedSummary.Requests == 0 {
			fmt.Fprintln(out, "No tracked Gemini batch work to continue.")
		} else if pollOnlyBatches == len(batches) && len(batches) > 0 {
			batchLabel := "batches"
			if len(batches) == 1 {
				batchLabel = "batch"
			}
			fmt.Fprintf(out, "Checked %d Gemini %s; nothing ready to process.\n", len(batches), batchLabel)
		}
	}
	printBatchBlockedSummaryTo(out, blockedSummary)
	if len(commandErrors) > 0 {
		coordinator.Warningf("warning: batch continue completed with %d error(s)\n", len(commandErrors))
	}
	return errors.Join(commandErrors...)
}

func advanceGeminiBatch(
	cmd *cobra.Command,
	database *db.DB,
	transport geminiBatchAPI,
	planner *ocr.GeminiClient,
	batch db.GeminiBatch,
	totals *batchContinueTotals,
	coordinators ...*progressCoordinator,
) (batchAdvanceDisposition, error) {
	coordinator := firstProgressCoordinator(coordinators)
	switch batch.State {
	case db.GeminiBatchPrepared:
		return batchAdvanceDidWork, resumePreparedGeminiBatch(cmd, database, transport, planner, batch, coordinator)
	case db.GeminiBatchUploadUnknown:
		return batchAdvanceDidWork, adoptUnknownGeminiUpload(cmd, database, transport, batch, coordinator)
	case db.GeminiBatchUploaded:
		return batchAdvanceDidWork, submitUploadedGeminiBatch(cmd, database, transport, batch.ID, coordinator)
	case db.GeminiBatchSubmissionUnknown:
		return batchAdvanceDidWork, adoptUnknownGeminiSubmission(cmd, database, transport, batch)
	case db.GeminiBatchSucceeded:
		return batchAdvanceDidWork, accountSucceededGeminiBatch(cmd, database, transport, batch, totals, coordinator)
	case db.GeminiBatchFailed, db.GeminiBatchExpired:
		if batch.OutputFileName != "" {
			return batchAdvanceDidWork, accountSucceededGeminiBatch(cmd, database, transport, batch, totals, coordinator)
		}
		return batchAdvanceDidWork, replaceOutputlessGeminiBatch(database, batch, totals)
	case db.GeminiBatchCancelled:
		totals.accounted = true
		totals.billing.Indeterminate = true
		if err := database.BlockGeminiBatchRequests(batch.ID, "remote Gemini batch was cancelled without output", time.Now().UTC()); err != nil {
			return batchAdvanceDidWork, err
		}
		_, err := database.FinalizeGeminiBatch(batch.ID, time.Now().UTC())
		return batchAdvanceDidWork, err
	case db.GeminiBatchPending, db.GeminiBatchRunning, db.GeminiBatchCancelling:
		return refreshAndHandleGeminiBatch(cmd, database, transport, batch, totals, coordinator)
	default:
		return batchAdvanceDidWork, fmt.Errorf("unsupported local Gemini batch state %q", batch.State)
	}
}

func resumePreparedGeminiBatch(
	cmd *cobra.Command,
	database *db.DB,
	transport geminiBatchAPI,
	planner *ocr.GeminiClient,
	batch db.GeminiBatch,
	coordinators ...*progressCoordinator,
) error {
	requests, err := database.GeminiRequestsForBatch(batch.ID)
	if err != nil {
		return err
	}
	coordinator := firstProgressCoordinator(coordinators)
	var regenerating *progress.Reporter
	if coordinator != nil && len(requests) > 0 {
		regenerating = coordinator.StartPhase(progress.PhaseOptions{
			Label: fmt.Sprintf("Regenerating Gemini batch %d input", batch.ID),
			Total: len(requests), Unit: "requests",
		})
	}
	finishRegenerating := func(stopped bool, complete bool) {
		if regenerating == nil {
			return
		}
		if stopped {
			coordinator.StopPhase(regenerating)
		} else if complete {
			coordinator.CompletePhase(regenerating)
		} else {
			coordinator.ClosePhase(regenerating)
		}
		regenerating = nil
	}
	input, err := newPrivateTempFile("ringbinder-gemini-input-")
	if err != nil {
		finishRegenerating(false, false)
		return err
	}
	defer input.Close()
	var size int64
	for _, request := range requests {
		if contextErr := cmd.Context().Err(); contextErr != nil {
			finishRegenerating(true, false)
			return contextErr
		}
		if regenerating != nil {
			regenerating.SetCurrent(request.Path)
		}
		path, pathErr := matchingContentPath(database, request.ContentID, request.Checksum)
		if contextErr := cmd.Context().Err(); contextErr != nil {
			finishRegenerating(true, false)
			return contextErr
		}
		if pathErr != nil {
			if blockErr := database.BlockGeminiRequest(request.ID, pathErr.Error(), time.Now().UTC()); blockErr != nil {
				finishRegenerating(false, false)
				return blockErr
			}
			commandWarning(coordinator, commandStderr(cmd), "warning: %v\n", pathErr)
			if regenerating != nil {
				regenerating.Advance()
			}
			continue
		}
		if regenerating != nil {
			regenerating.SetCurrent(path)
		}
		prepared, prepareErr := planner.PrepareRangeRequest(
			cmd.Context(), path, request.FileType, request.PageStart, request.PageEnd,
		)
		// Cancellation belongs to the command, not this request. Keep its durable
		// assignment unchanged so a later continue can resume it.
		if contextErr := cmd.Context().Err(); contextErr != nil {
			finishRegenerating(true, false)
			return contextErr
		}
		if prepareErr == nil {
			prepareErr = verifyContentPath(path, request.Checksum)
			if contextErr := cmd.Context().Err(); contextErr != nil {
				finishRegenerating(true, false)
				return contextErr
			}
		}
		if prepareErr != nil {
			if blockErr := database.BlockGeminiRequest(request.ID, prepareErr.Error(), time.Now().UTC()); blockErr != nil {
				finishRegenerating(false, false)
				return blockErr
			}
			if regenerating != nil {
				regenerating.Advance()
			}
			continue
		}
		line, err := marshalGeminiBatchLine(request.RequestKey, prepared.Body)
		if err != nil {
			finishRegenerating(false, false)
			return err
		}
		if size+int64(len(line)) > ocr.GeminiBatchMaxInputBytes {
			finishRegenerating(false, false)
			return fmt.Errorf("regenerated input exceeds Gemini's batch input limit")
		}
		if _, err := input.Write(line); err != nil {
			finishRegenerating(false, false)
			return err
		}
		size += int64(len(line))
		if regenerating != nil {
			regenerating.Advance()
		}
	}
	finishRegenerating(false, true)
	if size == 0 {
		_, err := database.FinalizeGeminiBatch(batch.ID, time.Now().UTC())
		return err
	}
	return uploadAndSubmitGeminiBatch(cmd, database, transport, batch.ID, input, size, coordinator)
}

func adoptUnknownGeminiUpload(
	cmd *cobra.Command,
	database *db.DB,
	transport geminiBatchAPI,
	batch db.GeminiBatch,
	coordinators ...*progressCoordinator,
) error {
	files, err := transport.ListFiles(cmd.Context())
	if err != nil {
		return err
	}
	matches := make([]ocr.GeminiRemoteFile, 0, 1)
	for _, file := range files {
		if file.DisplayName == batch.DisplayName {
			matches = append(matches, file)
		}
	}
	if len(matches) != 1 {
		return fmt.Errorf("ambiguous upload adoption found %d exact display-name matches; use batch forget to escape", len(matches))
	}
	if err := database.SetGeminiBatchUploaded(batch.ID, matches[0].Name, time.Now().UTC()); err != nil {
		return err
	}
	return submitUploadedGeminiBatch(cmd, database, transport, batch.ID, firstProgressCoordinator(coordinators))
}

func adoptUnknownGeminiSubmission(
	cmd *cobra.Command,
	database *db.DB,
	transport geminiBatchAPI,
	batch db.GeminiBatch,
) error {
	batches, err := transport.ListBatches(cmd.Context())
	if err != nil {
		return err
	}
	matches := make([]ocr.GeminiRemoteBatch, 0, 1)
	for _, remote := range batches {
		if geminiBatchAdoptionMatches(batch, remote) {
			matches = append(matches, remote)
		}
	}
	if len(matches) != 1 {
		return fmt.Errorf("ambiguous batch adoption found %d exact display/model/input matches; use batch forget to escape", len(matches))
	}
	state, err := ocr.NormalizeGeminiBatchState(matches[0].State)
	if err != nil {
		return err
	}
	return database.SetGeminiBatchRemote(
		batch.ID, matches[0].Name, state, matches[0].OutputFileName,
		matches[0].ErrorMessage, time.Now().UTC(),
	)
}

// Display names are not unique. Require every immutable submission field so a
// recovery scan cannot attach unrelated billable output; missing provenance
// fails closed because it cannot establish identity safely.
func geminiBatchAdoptionMatches(batch db.GeminiBatch, remote ocr.GeminiRemoteBatch) bool {
	if batch.DisplayName == "" || batch.Model == "" || batch.InputFileName == "" ||
		remote.DisplayName == "" || remote.Model == "" || remote.InputFileName == "" {
		return false
	}
	return remote.DisplayName == batch.DisplayName &&
		normalizeGeminiBatchModel(remote.Model) == normalizeGeminiBatchModel(batch.Model) &&
		remote.InputFileName == batch.InputFileName
}

func normalizeGeminiBatchModel(model string) string {
	return strings.TrimPrefix(strings.TrimSpace(model), "models/")
}

func refreshAndHandleGeminiBatch(
	cmd *cobra.Command,
	database *db.DB,
	transport geminiBatchAPI,
	batch db.GeminiBatch,
	totals *batchContinueTotals,
	coordinators ...*progressCoordinator,
) (batchAdvanceDisposition, error) {
	remote, err := transport.GetBatch(cmd.Context(), batch.RemoteName)
	// A transport can return a completed response after its context is
	// cancelled. Do not advance local ownership from that stale observation.
	if contextErr := cmd.Context().Err(); contextErr != nil {
		return batchAdvanceDidWork, contextErr
	}
	if err != nil {
		if ocr.IsGeminiBatchNotFound(err) {
			batch.LastError = "remote Gemini batch resource is no longer available"
			if batch.State == db.GeminiBatchCancelling {
				return batchAdvanceDidWork, finalizeUnavailableGeminiOutput(
					database, batch, totals, batch.LastError,
				)
			}
			return batchAdvanceDidWork, replaceOutputlessGeminiBatch(database, batch, totals)
		}
		return batchAdvanceDidWork, err
	}
	state, err := ocr.NormalizeGeminiBatchState(remote.State)
	if err != nil {
		return batchAdvanceDidWork, err
	}
	if batch.State == db.GeminiBatchCancelling &&
		(state == db.GeminiBatchPending || state == db.GeminiBatchRunning || state == db.GeminiBatchCancelling) {
		state = db.GeminiBatchCancelling
	}
	if err := database.SetGeminiBatchState(
		batch.ID, state, remote.OutputFileName, remote.ErrorMessage, time.Now().UTC(),
	); err != nil {
		return batchAdvanceDidWork, err
	}
	batch.State = state
	batch.LastError = remote.ErrorMessage
	if remote.OutputFileName != "" {
		batch.OutputFileName = remote.OutputFileName
	}
	switch state {
	case db.GeminiBatchPending, db.GeminiBatchRunning, db.GeminiBatchCancelling:
		return batchAdvancePollOnly, nil
	case db.GeminiBatchSucceeded:
		return batchAdvanceDidWork, accountSucceededGeminiBatch(cmd, database, transport, batch, totals, coordinators...)
	case db.GeminiBatchFailed, db.GeminiBatchExpired:
		if batch.OutputFileName != "" {
			return batchAdvanceDidWork, accountSucceededGeminiBatch(cmd, database, transport, batch, totals, coordinators...)
		}
		return batchAdvanceDidWork, replaceOutputlessGeminiBatch(database, batch, totals)
	case db.GeminiBatchCancelled:
		totals.accounted = true
		totals.billing.Indeterminate = true
		if err := database.BlockGeminiBatchRequests(batch.ID, "remote Gemini batch was cancelled without output", time.Now().UTC()); err != nil {
			return batchAdvanceDidWork, err
		}
		_, err := database.FinalizeGeminiBatch(batch.ID, time.Now().UTC())
		return batchAdvanceDidWork, err
	default:
		return batchAdvanceDidWork, fmt.Errorf("unsupported remote Gemini batch state %q", state)
	}
}

func replaceOutputlessGeminiBatch(database *db.DB, batch db.GeminiBatch, totals *batchContinueTotals) error {
	totals.accounted = true
	totals.billing.Indeterminate = true
	message := batch.LastError
	if message == "" {
		message = "remote Gemini batch ended without retrievable output"
	}
	upperMessage := strings.ToUpper(message)
	if strings.Contains(upperMessage, "PERMISSION_DENIED") ||
		strings.Contains(upperMessage, "UNAUTHENTICATED") ||
		strings.Contains(upperMessage, "INVALID_ARGUMENT") {
		if err := database.BlockGeminiBatchRequests(batch.ID, message, time.Now().UTC()); err != nil {
			return err
		}
	} else if _, err := database.DetachGeminiBatchForReplacement(batch.ID, message, time.Now().UTC()); err != nil {
		return err
	}
	_, err := database.FinalizeGeminiBatch(batch.ID, time.Now().UTC())
	return err
}

func accountSucceededGeminiBatch(
	cmd *cobra.Command,
	database *db.DB,
	transport geminiBatchAPI,
	batch db.GeminiBatch,
	totals *batchContinueTotals,
	coordinators ...*progressCoordinator,
) error {
	if batch.OutputFileName == "" {
		remote, refreshErr := transport.GetBatch(cmd.Context(), batch.RemoteName)
		if contextErr := cmd.Context().Err(); contextErr != nil {
			return contextErr
		}
		if refreshErr != nil {
			if ocr.IsGeminiBatchNotFound(refreshErr) {
				return finalizeUnavailableGeminiOutput(
					database, batch, totals, "succeeded Gemini batch expired before output was available",
				)
			}
			return refreshErr
		}
		if remote.OutputFileName == "" {
			return finalizeUnavailableGeminiOutput(
				database, batch, totals, "succeeded Gemini batch omitted its output file after confirmation",
			)
		}
		batch.OutputFileName = remote.OutputFileName
		if err := database.SetGeminiBatchState(
			batch.ID, db.GeminiBatchSucceeded, remote.OutputFileName, remote.ErrorMessage, time.Now().UTC(),
		); err != nil {
			return err
		}
	}

	coordinator := firstProgressCoordinator(coordinators)
	var downloading *progress.Reporter
	if coordinator != nil {
		downloading = coordinator.StartPhase(progress.PhaseOptions{
			Label: fmt.Sprintf("Downloading and validating Gemini batch %d output", batch.ID),
		})
	}
	finishDownloading := func(stopped, complete bool) {
		if downloading == nil {
			return
		}
		if stopped {
			coordinator.StopPhase(downloading)
		} else if complete {
			coordinator.CompletePhase(downloading)
		} else {
			coordinator.ClosePhase(downloading)
		}
		downloading = nil
	}
	body, err := transport.DownloadFile(cmd.Context(), batch.OutputFileName)
	if err != nil {
		finishDownloading(cmd.Context().Err() != nil, false)
		if ocr.IsGeminiBatchNotFound(err) {
			return finalizeUnavailableGeminiOutput(
				database, batch, totals, "Gemini output file is no longer available",
			)
		}
		return err
	}
	defer body.Close()

	var importing *progress.Reporter
	accountErr := accountGeminiOutputWithCallbacks(
		cmd.Context(), body, database, batch, totals,
		func(total int) {
			finishDownloading(false, true)
			if coordinator != nil && total > 0 {
				importing = coordinator.StartPhase(progress.PhaseOptions{
					Label: fmt.Sprintf("Importing Gemini batch %d output", batch.ID),
					Total: total, Unit: "requests",
				})
			}
		},
		func() {
			if importing != nil {
				importing.Advance()
			}
		},
	)
	if downloading != nil {
		finishDownloading(cmd.Context().Err() != nil, false)
	}
	if importing != nil {
		if cmd.Context().Err() != nil {
			coordinator.StopPhase(importing)
		} else if accountErr == nil {
			coordinator.CompletePhase(importing)
		} else {
			coordinator.ClosePhase(importing)
		}
	}
	var incomplete *incompleteGeminiOutputError
	if errors.As(accountErr, &incomplete) {
		return accountErr
	}
	// A complete output can be staged one key at a time, but cancellation before
	// bookkeeping must leave the immutable batch available for a later resume.
	if contextErr := cmd.Context().Err(); contextErr != nil {
		return errors.Join(accountErr, contextErr)
	}
	_, retireErr := database.RetireCompletedGeminiRequests()
	if contextErr := cmd.Context().Err(); contextErr != nil {
		return errors.Join(accountErr, retireErr, contextErr)
	}
	_, finalizeErr := database.FinalizeGeminiBatch(batch.ID, time.Now().UTC())
	return errors.Join(accountErr, retireErr, finalizeErr)
}

func finalizeUnavailableGeminiOutput(
	database *db.DB,
	batch db.GeminiBatch,
	totals *batchContinueTotals,
	message string,
) error {
	totals.accounted = true
	totals.billing.Indeterminate = true
	if err := database.BlockGeminiBatchRequests(batch.ID, message, time.Now().UTC()); err != nil {
		return err
	}
	_, err := database.FinalizeGeminiBatch(batch.ID, time.Now().UTC())
	return err
}

// accountGeminiOutput is the contextless test path; commands use
// accountGeminiOutputWithCallbacks so imports remain cancellable.
func accountGeminiOutput(
	reader io.Reader,
	database *db.DB,
	batch db.GeminiBatch,
	totals *batchContinueTotals,
) error {
	return accountGeminiOutputWithCallbacks(context.Background(), reader, database, batch, totals, nil, nil)
}

// accountGeminiOutputWithCallbacks keeps validation and writes separate. The
// output is fully spooled before any request changes, so interruption while
// downloading leaves the batch assignment resumable.
func accountGeminiOutputWithCallbacks(
	ctx context.Context,
	reader io.Reader,
	database *db.DB,
	batch db.GeminiBatch,
	totals *batchContinueTotals,
	validated func(total int),
	processed func(),
) error {
	spool, err := spoolGeminiOutput(ctx, reader, database, batch, totals)
	if err != nil {
		return err
	}
	defer spool.file.Close()
	if validated != nil {
		validated(len(batch.RequestKeys))
	}
	return applyGeminiOutput(ctx, spool, database, batch, totals, processed)
}

func spoolGeminiOutput(
	ctx context.Context,
	reader io.Reader,
	database *db.DB,
	batch db.GeminiBatch,
	totals *batchContinueTotals,
) (_ *geminiOutputSpool, err error) {
	requests, err := database.GeminiRequestsForBatch(batch.ID)
	if err != nil {
		return nil, err
	}
	byKey := make(map[string]db.GeminiBatchRequest, len(requests))
	for _, request := range requests {
		byKey[request.RequestKey] = request
	}
	manifest := make(map[string]bool, len(batch.RequestKeys))
	for _, key := range batch.RequestKeys {
		manifest[key] = true
	}

	// Spool raw lines to an unlinked file and retain only offsets. This validates
	// the complete key set (especially late duplicates and missing keys) before
	// applying any response, without retaining provider payloads in SQLite.
	output, err := newPrivateTempFile("ringbinder-gemini-output-")
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = output.Close()
		}
	}()
	spool := &geminiOutputSpool{
		file:       output,
		byKey:      byKey,
		stored:     make(map[string]storedGeminiOutput, len(requests)),
		duplicates: make(map[string]bool),
	}
	var offset int64
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), ocr.GeminiBatchMaxResponseBytes+1)
	for {
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, &incompleteGeminiOutputError{Err: contextErr}
		}
		if !scanner.Scan() {
			break
		}
		if contextErr := ctx.Err(); contextErr != nil {
			return nil, &incompleteGeminiOutputError{Err: contextErr}
		}
		line := bytes.Clone(scanner.Bytes())
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if len(line) > ocr.GeminiBatchMaxResponseBytes {
			return nil, blockOversizedGeminiOutput(database, batch, totals)
		}
		var envelope struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal(line, &envelope); err != nil || envelope.Key == "" {
			spool.validationErrors = append(spool.validationErrors, fmt.Errorf("malformed keyed Gemini output line"))
			continue
		}
		if !manifest[envelope.Key] {
			spool.validationErrors = append(spool.validationErrors, fmt.Errorf("Gemini output contains foreign key %q", envelope.Key))
			continue
		}
		if _, local := byKey[envelope.Key]; !local {
			// Manifest-known keys can disappear when sweep cascades orphan content.
			continue
		}
		if _, exists := spool.stored[envelope.Key]; exists {
			spool.duplicates[envelope.Key] = true
			continue
		}
		if _, err := output.Write(line); err != nil {
			return nil, err
		}
		spool.stored[envelope.Key] = storedGeminiOutput{offset: offset, length: len(line)}
		offset += int64(len(line))
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return nil, blockOversizedGeminiOutput(database, batch, totals)
		}
		return nil, &incompleteGeminiOutputError{Err: err}
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, &incompleteGeminiOutputError{Err: contextErr}
	}
	return spool, nil
}

func applyGeminiOutput(
	ctx context.Context,
	spool *geminiOutputSpool,
	database *db.DB,
	batch db.GeminiBatch,
	totals *batchContinueTotals,
	processed func(),
) error {
	validationErrors := append([]error(nil), spool.validationErrors...)
	seen := make(map[string]bool, len(batch.RequestKeys))
	applyRequest := func(key string, request db.GeminiBatchRequest) {
		if spool.duplicates[key] {
			totals.accounted = true
			totals.billing.Indeterminate = true
			_, err := database.RetryGeminiRequest(request.ID, "duplicate output key", time.Now().UTC())
			if err != nil {
				validationErrors = append(validationErrors, err)
			}
			return
		}
		location, exists := spool.stored[key]
		if !exists {
			totals.accounted = true
			totals.billing.Indeterminate = true
			_, err := database.RetryGeminiRequest(request.ID, "missing output key", time.Now().UTC())
			if err != nil {
				validationErrors = append(validationErrors, err)
			}
			return
		}
		line := make([]byte, location.length)
		if _, err := spool.file.ReadAt(line, location.offset); err != nil {
			validationErrors = append(validationErrors, err)
			return
		}
		if err := accountGeminiOutputLine(line, database, batch, request, totals); err != nil {
			validationErrors = append(validationErrors, err)
		}
	}
	// Staging is deliberately resumable: keys applied before cancellation stay
	// staged, while the untouched manifest suffix remains assigned for continue.
	for _, key := range batch.RequestKeys {
		if contextErr := ctx.Err(); contextErr != nil {
			return &incompleteGeminiOutputError{Err: contextErr}
		}
		if seen[key] {
			if processed != nil {
				processed()
			}
			continue
		}
		seen[key] = true
		if request, local := spool.byKey[key]; local {
			applyRequest(key, request)
		}
		// A manifest key whose local request disappeared is an intentional no-op,
		// but it still advances the immutable manifest-count progress bar.
		if processed != nil {
			processed()
		}
	}
	// Keep legacy/corrupt manifests recoverable: a still-owned request omitted
	// from the manifest follows the pre-existing missing-output retry path.
	for key, request := range spool.byKey {
		if seen[key] {
			continue
		}
		if contextErr := ctx.Err(); contextErr != nil {
			return &incompleteGeminiOutputError{Err: contextErr}
		}
		applyRequest(key, request)
	}
	return errors.Join(validationErrors...)
}

// Scanner cannot recover the key of an oversized line, so none of this
// batch's output can be safely attributed. Block it without applying the
// otherwise valid spooled lines; the caller can then finalize it safely.
func blockOversizedGeminiOutput(
	database *db.DB,
	batch db.GeminiBatch,
	totals *batchContinueTotals,
) error {
	message := fmt.Sprintf("Gemini output line exceeds %d bytes", ocr.GeminiBatchMaxResponseBytes)
	totals.accounted = true
	totals.billing.Indeterminate = true
	return errors.Join(
		errors.New(message),
		database.BlockGeminiBatchRequests(batch.ID, message, time.Now().UTC()),
	)
}

func accountGeminiOutputLine(
	line []byte,
	database *db.DB,
	batch db.GeminiBatch,
	request db.GeminiBatchRequest,
	totals *batchContinueTotals,
) error {
	var output geminiOutputLine
	if err := json.Unmarshal(line, &output); err != nil {
		_, retryErr := database.RetryGeminiRequest(request.ID, "malformed output envelope", time.Now().UTC())
		return errors.Join(err, retryErr)
	}
	if len(output.Error) > 0 && string(output.Error) != "null" {
		totals.accounted = true
		totals.billing.Indeterminate = true
		return handleGeminiRequestError(database, request, output.Error)
	}
	if len(output.Response) == 0 || string(output.Response) == "null" {
		totals.accounted = true
		totals.billing.Indeterminate = true
		_, err := database.RetryGeminiRequest(request.ID, "output omitted response and error", time.Now().UTC())
		return err
	}
	prices := ocr.GeminiTokenPrices{Input: ocr.Currency(batch.InputPrice), Output: ocr.Currency(batch.OutputPrice)}
	// Some batch responses omit modelVersion. Use the model snapshotted for this
	// attempt, not today's default; a missing snapshot then fails closed.
	decoded, decodeErr := ocr.DecodeGeminiBatchResult(
		output.Response, request.PageEnd-request.PageStart, batch.Model, prices,
	)
	totals.accounted = true
	totals.billing.Add(decoded.Billing)
	if decodeErr != nil {
		if request.PageEnd-request.PageStart > 1 && ocr.IsGeminiMaxTokensError(decodeErr) {
			_, err := database.SplitGeminiRequest(
				request.ID, newGeminiLocalKey(), newGeminiLocalKey(), decodeErr.Error(), time.Now().UTC(),
			)
			return err
		}
		_, err := database.RetryGeminiRequest(request.ID, decodeErr.Error(), time.Now().UTC())
		return err
	}
	pages := make([]db.GeminiStagedPage, len(decoded.Pages))
	for i, page := range decoded.Pages {
		pages[i] = db.GeminiStagedPage{
			PageIndex: request.PageStart + page.PageIndex,
			Markdown:  page.Markdown,
			Model:     page.Model,
		}
	}
	return database.StageGeminiRequest(
		request.ID, pages, decoded.InputTokens, decoded.OutputTokens,
		int64(decoded.Billing.KnownCost), decoded.Billing.Indeterminate, time.Now().UTC(),
	)
}

func handleGeminiRequestError(
	database *db.DB,
	request db.GeminiBatchRequest,
	raw json.RawMessage,
) error {
	var requestError geminiRequestError
	if err := json.Unmarshal(raw, &requestError); err != nil {
		_, retryErr := database.RetryGeminiRequest(request.ID, "malformed per-request error", time.Now().UTC())
		return errors.Join(err, retryErr)
	}
	message := strings.TrimSpace(strings.Join([]string{requestError.Status, requestError.Message}, ": "))
	message = strings.Trim(message, ": ")
	if message == "" {
		message = fmt.Sprintf("Gemini request failed with code %d", requestError.Code)
	}
	lowerMessage := strings.ToLower(message)
	adaptive := requestError.Code == 413 || strings.Contains(strings.ToUpper(message), "MAX_TOKENS") ||
		strings.Contains(lowerMessage, "payload too large") ||
		strings.Contains(lowerMessage, "payload size limit") ||
		strings.Contains(lowerMessage, "request too large")
	if adaptive {
		if request.PageEnd-request.PageStart > 1 {
			_, err := database.SplitGeminiRequest(
				request.ID, newGeminiLocalKey(), newGeminiLocalKey(), message, time.Now().UTC(),
			)
			return err
		}
		return database.BlockGeminiRequest(request.ID, message, time.Now().UTC())
	}
	recoverable := recoverableGeminiRequestCode(requestError.Code) ||
		requestError.Status == "UNAVAILABLE" || requestError.Status == "INTERNAL" ||
		requestError.Status == "RESOURCE_EXHAUSTED" || requestError.Status == "DEADLINE_EXCEEDED"
	if recoverable {
		_, err := database.RetryGeminiRequest(request.ID, message, time.Now().UTC())
		return err
	}
	return database.BlockGeminiRequest(request.ID, message, time.Now().UTC())
}

func recoverableGeminiRequestCode(code int) bool {
	if code == 429 || code >= 500 {
		return true
	}
	// Batch JSONL uses canonical google.rpc.Code values rather than HTTP codes.
	switch code {
	case 2, 4, 8, 10, 13, 14, 15:
		return true
	default:
		return false
	}
}

func submitRetryableGeminiRequests(
	cmd *cobra.Command,
	database *db.DB,
	transport geminiBatchAPI,
	planner *ocr.GeminiClient,
	coordinators ...*progressCoordinator,
) (bool, []error) {
	requests, err := database.RetryableGeminiRequests()
	if err != nil {
		return false, []error{err}
	}
	if len(requests) == 0 {
		return false, nil
	}
	capped := len(requests) > maxAutomaticRequestsPerContinue
	if capped {
		requests = requests[:maxAutomaticRequestsPerContinue]
	}
	coordinator := firstProgressCoordinator(coordinators)
	var preparing *progress.Reporter
	if coordinator != nil {
		preparing = coordinator.StartPhase(progress.PhaseOptions{
			Label: "Preparing Gemini retry input", Total: len(requests), Unit: "selected requests",
		})
	}
	finishPreparing := func(stopped, complete bool) {
		if preparing == nil {
			return
		}
		if stopped {
			coordinator.StopPhase(preparing)
		} else if complete {
			coordinator.CompletePhase(preparing)
		} else {
			coordinator.ClosePhase(preparing)
		}
		preparing = nil
	}
	group, err := newRetryBatchGroup()
	if err != nil {
		finishPreparing(false, false)
		return true, []error{err}
	}
	defer func() { _ = group.Close() }()
	var commandErrors []error
	flush := func() bool {
		if len(group.requestIDs) == 0 {
			return false
		}
		flushErr := submitRetryBatchGroup(cmd, database, transport, group, coordinator)
		if flushErr != nil {
			commandErrors = append(commandErrors, flushErr)
		}
		_ = group.Close()
		if ocr.IsGeminiGlobalFailure(flushErr) {
			return true
		}
		group, err = newRetryBatchGroup()
		if err != nil {
			commandErrors = append(commandErrors, err)
			return true
		}
		return false
	}

	stopped := false
	for _, request := range requests {
		if err != nil {
			break
		}
		if contextErr := cmd.Context().Err(); contextErr != nil {
			commandErrors = append(commandErrors, contextErr)
			stopped = true
			break
		}
		if preparing != nil {
			preparing.SetCurrent(request.Path)
		}
		path, pathErr := matchingContentPath(database, request.ContentID, request.Checksum)
		if contextErr := cmd.Context().Err(); contextErr != nil {
			commandErrors = append(commandErrors, contextErr)
			stopped = true
			break
		}
		if pathErr != nil {
			if blockErr := database.BlockUnownedGeminiRequest(request.ID, pathErr.Error(), time.Now().UTC()); blockErr != nil {
				commandErrors = append(commandErrors, blockErr)
			}
			if preparing != nil {
				preparing.Advance()
			}
			continue
		}
		if preparing != nil {
			preparing.SetCurrent(path)
		}
		prepared, prepareErr := planner.PrepareRangeRequest(
			cmd.Context(), path, request.FileType, request.PageStart, request.PageEnd,
		)
		// Cancellation belongs to the command, not this request. Keep it
		// retryable so a later continue can resume it.
		if contextErr := cmd.Context().Err(); contextErr != nil {
			commandErrors = append(commandErrors, contextErr)
			stopped = true
			break
		}
		if prepareErr == nil {
			prepareErr = verifyContentPath(path, request.Checksum)
			if contextErr := cmd.Context().Err(); contextErr != nil {
				commandErrors = append(commandErrors, contextErr)
				stopped = true
				break
			}
		}
		if prepareErr != nil {
			if ocr.IsGeminiRangeSizeError(prepareErr) && request.PageEnd-request.PageStart > 1 {
				if _, splitErr := database.SplitGeminiRequest(
					request.ID, newGeminiLocalKey(), newGeminiLocalKey(), prepareErr.Error(), time.Now().UTC(),
				); splitErr != nil {
					commandErrors = append(commandErrors, splitErr)
				}
			} else if blockErr := database.BlockUnownedGeminiRequest(request.ID, prepareErr.Error(), time.Now().UTC()); blockErr != nil {
				commandErrors = append(commandErrors, blockErr)
			}
			if preparing != nil {
				preparing.Advance()
			}
			continue
		}
		line, lineErr := marshalGeminiBatchLine(request.RequestKey, prepared.Body)
		if lineErr != nil {
			commandErrors = append(commandErrors, lineErr)
			if preparing != nil {
				preparing.Advance()
			}
			continue
		}
		if len(group.requestIDs) > 0 && !sameBatchLineage(group.replacementOf, request.PreviousBatchID) {
			if stopped = flush(); stopped {
				break
			}
		}
		if group.size > 0 && group.size+int64(len(line)) > ocr.GeminiBatchMaxInputBytes {
			if stopped = flush(); stopped {
				break
			}
		}
		if _, lineErr = group.file.Write(line); lineErr != nil {
			commandErrors = append(commandErrors, lineErr)
			if preparing != nil {
				preparing.Advance()
			}
			continue
		}
		group.size += int64(len(line))
		if len(group.requestIDs) == 0 && request.PreviousBatchID != nil {
			value := *request.PreviousBatchID
			group.replacementOf = &value
		}
		group.requestIDs = append(group.requestIDs, request.ID)
		if preparing != nil {
			preparing.Advance()
		}
	}
	if err == nil && !stopped {
		flush()
	}
	if cmd.Context().Err() != nil {
		finishPreparing(true, false)
	} else {
		finishPreparing(false, !stopped && len(commandErrors) == 0)
	}
	if capped && !stopped && cmd.Context().Err() == nil {
		commandProgressf(
			coordinator,
			commandStdout(cmd),
			"More retryable Gemini requests remain; run `ringbinder batch continue` again.\n",
		)
	}
	return true, commandErrors
}

func sameBatchLineage(left, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func newRetryBatchGroup() (*retryBatchGroup, error) {
	file, err := newPrivateTempFile("ringbinder-gemini-retry-")
	if err != nil {
		return nil, err
	}
	return &retryBatchGroup{file: file}, nil
}

func (group *retryBatchGroup) Close() error {
	if group == nil || group.file == nil {
		return nil
	}
	err := group.file.Close()
	group.file = nil
	return err
}

func submitRetryBatchGroup(
	cmd *cobra.Command,
	database *db.DB,
	transport geminiBatchAPI,
	group *retryBatchGroup,
	coordinators ...*progressCoordinator,
) error {
	now := time.Now().UTC()
	prices := ocr.GeminiBatchPrices(now)
	batchID, err := database.CreateGeminiBatchForRequests(
		newGeminiDisplayName(now), ocr.GeminiBatchModel,
		int64(prices.Input), int64(prices.Output), group.replacementOf,
		group.requestIDs, now,
	)
	if err != nil {
		return err
	}
	coordinator := firstProgressCoordinator(coordinators)
	commandProgressf(
		coordinator,
		commandStdout(cmd),
		"Gemini retry batch %d prepared with %d request(s).\n",
		batchID,
		len(group.requestIDs),
	)
	return uploadAndSubmitGeminiBatch(cmd, database, transport, batchID, group.file, group.size, coordinator)
}

func retryGeminiCleanup(
	cmd *cobra.Command,
	database *db.DB,
	transport geminiBatchAPI,
	coordinators ...*progressCoordinator,
) (bool, []error) {
	cleanup, err := database.ListGeminiCleanup()
	if err != nil {
		return false, []error{err}
	}
	if len(cleanup) == 0 {
		return false, nil
	}
	coordinator := firstProgressCoordinator(coordinators)
	var cleaning *progress.Reporter
	if coordinator != nil {
		cleaning = coordinator.StartPhase(progress.PhaseOptions{
			Label: "Cleaning up Gemini resources", Total: len(cleanup), Unit: "resources",
		})
	}
	finishCleaning := func(stopped, complete bool) {
		if cleaning == nil {
			return
		}
		if stopped {
			coordinator.StopPhase(cleaning)
		} else if complete {
			coordinator.CompletePhase(cleaning)
		} else {
			coordinator.ClosePhase(cleaning)
		}
		cleaning = nil
	}
	var commandErrors []error
	stopped := false
	for _, item := range cleanup {
		if contextErr := cmd.Context().Err(); contextErr != nil {
			commandErrors = append(commandErrors, contextErr)
			stopped = true
			break
		}
		if cleaning != nil {
			cleaning.SetCurrent(item.ResourceName)
		}
		var deleteErr error
		switch item.ResourceKind {
		case "file":
			deleteErr = transport.DeleteFile(cmd.Context(), item.ResourceName)
		case "batch":
			deleteErr = transport.DeleteBatch(cmd.Context(), item.ResourceName)
		default:
			deleteErr = fmt.Errorf("unknown cleanup resource kind %q", item.ResourceKind)
		}
		invalidDelete := ocr.IsGeminiDeleteInvalidArgument(deleteErr)
		if deleteErr == nil || ocr.IsGeminiBatchNotFound(deleteErr) || invalidDelete {
			if err := database.DeleteGeminiCleanup(item.ID); err != nil {
				commandErrors = append(commandErrors, err)
			} else if invalidDelete {
				// Retrying an identical delete cannot fix an invalid argument. Retire
				// the detached cleanup row so it cannot poison every later continue.
				commandWarning(
					coordinator,
					commandStderr(cmd),
					"warning: Gemini permanently rejected cleanup of %s %s; Ringbinder will not retry it\n",
					item.ResourceKind,
					item.ResourceName,
				)
			}
		} else {
			_ = database.SetGeminiCleanupError(item.ID, deleteErr.Error(), time.Now().UTC())
			commandErrors = append(commandErrors, fmt.Errorf("clean up Gemini %s %s: %w", item.ResourceKind, item.ResourceName, deleteErr))
		}
		if cleaning != nil {
			cleaning.Advance()
		}
		if ocr.IsGeminiGlobalFailure(deleteErr) {
			break
		}
	}
	if stopped || cmd.Context().Err() != nil {
		finishCleaning(true, false)
	} else {
		finishCleaning(false, len(commandErrors) == 0)
	}
	return true, commandErrors
}
