package cmd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/maxim/ringbinder/internal/db"
	"github.com/maxim/ringbinder/internal/ocr"
	"github.com/spf13/cobra"
)

const maxAutomaticRequestsPerContinue = 100

type batchContinueTotals struct {
	billing   ocr.BillingReport
	accounted bool
}

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
	command, err := openGeminiBatchCommand(cmd, true, true)
	if err != nil {
		return err
	}
	defer command.Close()
	transport := newGeminiBatchAPI(command.apiKey)
	planner := ocr.NewGeminiClient("", time.Now().UTC())

	batches, err := command.database.ListGeminiBatches()
	if err != nil {
		return fmt.Errorf("list Gemini batches: %w", err)
	}
	var totals batchContinueTotals
	var commandErrors []error
	globalFailure := false
	// List order is newest-first for users; lifecycle advancement is oldest-first
	// so long-running work is reconciled before newer submissions.
	for i := len(batches) - 1; i >= 0; i-- {
		batch := batches[i]
		advanceErr := advanceGeminiBatch(cmd, command.database, transport, planner, batch, &totals)
		if advanceErr == nil {
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

	if !globalFailure {
		if _, promoteErr := command.database.PromoteReadyGeminiContents(); promoteErr != nil {
			commandErrors = append(commandErrors, fmt.Errorf("promote completed Gemini content: %w", promoteErr))
		}
		retryErrors := submitRetryableGeminiRequests(cmd, command.database, transport, planner)
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
		commandErrors = append(commandErrors, retryGeminiCleanup(cmd, command.database, transport)...)
	}

	if totals.accounted {
		if totals.billing.Indeterminate {
			fmt.Printf("Known batch OCR cost: %s (actual cost may be higher)\n", ocr.FormatCurrency(totals.billing.KnownCost))
		} else {
			fmt.Printf("Batch OCR cost: %s\n", ocr.FormatCurrency(totals.billing.KnownCost))
		}
	}
	if len(batches) == 0 {
		cleanup, countErr := command.database.CountGeminiCleanup()
		if countErr == nil && cleanup == 0 {
			fmt.Println("No tracked Gemini batch work to continue.")
		}
	}
	if len(commandErrors) > 0 {
		fmt.Fprintf(os.Stderr, "warning: batch continue completed with %d error(s)\n", len(commandErrors))
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
) error {
	switch batch.State {
	case db.GeminiBatchPrepared:
		return resumePreparedGeminiBatch(cmd, database, transport, planner, batch)
	case db.GeminiBatchUploadUnknown:
		return adoptUnknownGeminiUpload(cmd, database, transport, batch)
	case db.GeminiBatchUploaded:
		return submitUploadedGeminiBatch(cmd, database, transport, batch.ID)
	case db.GeminiBatchSubmissionUnknown:
		return adoptUnknownGeminiSubmission(cmd, database, transport, batch)
	case db.GeminiBatchSucceeded:
		return accountSucceededGeminiBatch(cmd, database, transport, batch, totals)
	case db.GeminiBatchFailed, db.GeminiBatchExpired:
		if batch.OutputFileName != "" {
			return accountSucceededGeminiBatch(cmd, database, transport, batch, totals)
		}
		return replaceOutputlessGeminiBatch(database, batch, totals)
	case db.GeminiBatchCancelled:
		totals.accounted = true
		totals.billing.Indeterminate = true
		if err := database.BlockGeminiBatchRequests(batch.ID, "remote Gemini batch was cancelled without output", time.Now().UTC()); err != nil {
			return err
		}
		_, err := database.FinalizeGeminiBatch(batch.ID, time.Now().UTC())
		return err
	case db.GeminiBatchPending, db.GeminiBatchRunning, db.GeminiBatchCancelling:
		return refreshAndHandleGeminiBatch(cmd, database, transport, batch, totals)
	default:
		return fmt.Errorf("unsupported local Gemini batch state %q", batch.State)
	}
}

func resumePreparedGeminiBatch(
	cmd *cobra.Command,
	database *db.DB,
	transport geminiBatchAPI,
	planner *ocr.GeminiClient,
	batch db.GeminiBatch,
) error {
	requests, err := database.GeminiRequestsForBatch(batch.ID)
	if err != nil {
		return err
	}
	input, err := newPrivateTempFile("ringbinder-gemini-input-")
	if err != nil {
		return err
	}
	defer input.Close()
	var size int64
	for _, request := range requests {
		path, pathErr := matchingContentPath(database, request.ContentID, request.Checksum)
		if pathErr != nil {
			if blockErr := database.BlockGeminiRequest(request.ID, pathErr.Error(), time.Now().UTC()); blockErr != nil {
				return blockErr
			}
			fmt.Fprintf(os.Stderr, "warning: %v\n", pathErr)
			continue
		}
		prepared, prepareErr := planner.PrepareRangeRequest(
			cmd.Context(), path, request.FileType, request.PageStart, request.PageEnd,
		)
		// Cancellation belongs to the command, not this request. Keep its durable
		// assignment unchanged so a later continue can resume it.
		if contextErr := cmd.Context().Err(); contextErr != nil {
			return contextErr
		}
		if prepareErr == nil {
			prepareErr = verifyContentPath(path, request.Checksum)
		}
		if prepareErr != nil {
			if blockErr := database.BlockGeminiRequest(request.ID, prepareErr.Error(), time.Now().UTC()); blockErr != nil {
				return blockErr
			}
			continue
		}
		line, err := marshalGeminiBatchLine(request.RequestKey, prepared.Body)
		if err != nil {
			return err
		}
		if size+int64(len(line)) > ocr.GeminiBatchMaxInputBytes {
			return fmt.Errorf("regenerated input exceeds Gemini's batch input limit")
		}
		if _, err := input.Write(line); err != nil {
			return err
		}
		size += int64(len(line))
	}
	if size == 0 {
		_, err := database.FinalizeGeminiBatch(batch.ID, time.Now().UTC())
		return err
	}
	return uploadAndSubmitGeminiBatch(cmd, database, transport, batch.ID, input, size)
}

func adoptUnknownGeminiUpload(
	cmd *cobra.Command,
	database *db.DB,
	transport geminiBatchAPI,
	batch db.GeminiBatch,
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
	return submitUploadedGeminiBatch(cmd, database, transport, batch.ID)
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
		if remote.DisplayName == batch.DisplayName {
			matches = append(matches, remote)
		}
	}
	if len(matches) != 1 {
		return fmt.Errorf("ambiguous batch adoption found %d exact display-name matches; use batch forget to escape", len(matches))
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

func refreshAndHandleGeminiBatch(
	cmd *cobra.Command,
	database *db.DB,
	transport geminiBatchAPI,
	batch db.GeminiBatch,
	totals *batchContinueTotals,
) error {
	remote, err := transport.GetBatch(cmd.Context(), batch.RemoteName)
	if err != nil {
		if ocr.IsGeminiBatchNotFound(err) {
			batch.LastError = "remote Gemini batch resource is no longer available"
			if batch.State == db.GeminiBatchCancelling {
				return finalizeUnavailableGeminiOutput(database, batch, totals, batch.LastError)
			}
			return replaceOutputlessGeminiBatch(database, batch, totals)
		}
		return err
	}
	state, err := ocr.NormalizeGeminiBatchState(remote.State)
	if err != nil {
		return err
	}
	if batch.State == db.GeminiBatchCancelling &&
		(state == db.GeminiBatchPending || state == db.GeminiBatchRunning || state == db.GeminiBatchCancelling) {
		state = db.GeminiBatchCancelling
	}
	if err := database.SetGeminiBatchState(
		batch.ID, state, remote.OutputFileName, remote.ErrorMessage, time.Now().UTC(),
	); err != nil {
		return err
	}
	batch.State = state
	batch.LastError = remote.ErrorMessage
	if remote.OutputFileName != "" {
		batch.OutputFileName = remote.OutputFileName
	}
	switch state {
	case db.GeminiBatchSucceeded:
		return accountSucceededGeminiBatch(cmd, database, transport, batch, totals)
	case db.GeminiBatchFailed, db.GeminiBatchExpired:
		if batch.OutputFileName != "" {
			return accountSucceededGeminiBatch(cmd, database, transport, batch, totals)
		}
		return replaceOutputlessGeminiBatch(database, batch, totals)
	case db.GeminiBatchCancelled:
		totals.accounted = true
		totals.billing.Indeterminate = true
		if err := database.BlockGeminiBatchRequests(batch.ID, "remote Gemini batch was cancelled without output", time.Now().UTC()); err != nil {
			return err
		}
		_, err := database.FinalizeGeminiBatch(batch.ID, time.Now().UTC())
		return err
	default:
		return nil
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
) error {
	if batch.OutputFileName == "" {
		remote, refreshErr := transport.GetBatch(cmd.Context(), batch.RemoteName)
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
	body, err := transport.DownloadFile(cmd.Context(), batch.OutputFileName)
	if err != nil {
		if ocr.IsGeminiBatchNotFound(err) {
			return finalizeUnavailableGeminiOutput(
				database, batch, totals, "Gemini output file is no longer available",
			)
		}
		return err
	}
	defer body.Close()
	accountErr := accountGeminiOutput(body, database, batch, totals)
	var incomplete *incompleteGeminiOutputError
	if errors.As(accountErr, &incomplete) {
		return accountErr
	}
	_, promoteErr := database.PromoteReadyGeminiContents()
	_, finalizeErr := database.FinalizeGeminiBatch(batch.ID, time.Now().UTC())
	return errors.Join(accountErr, promoteErr, finalizeErr)
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

func accountGeminiOutput(
	reader io.Reader,
	database *db.DB,
	batch db.GeminiBatch,
	totals *batchContinueTotals,
) error {
	requests, err := database.GeminiRequestsForBatch(batch.ID)
	if err != nil {
		return err
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
	// staging any response, without retaining provider payloads in SQLite.
	output, err := newPrivateTempFile("ringbinder-gemini-output-")
	if err != nil {
		return err
	}
	defer output.Close()
	stored := make(map[string]storedGeminiOutput, len(requests))
	duplicates := make(map[string]bool)
	var validationErrors []error
	var offset int64
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), ocr.GeminiBatchMaxResponseBytes+1)
	for scanner.Scan() {
		line := bytes.Clone(scanner.Bytes())
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if len(line) > ocr.GeminiBatchMaxResponseBytes {
			return blockOversizedGeminiOutput(database, batch, totals)
		}
		var envelope struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal(line, &envelope); err != nil || envelope.Key == "" {
			validationErrors = append(validationErrors, fmt.Errorf("malformed keyed Gemini output line"))
			continue
		}
		if !manifest[envelope.Key] {
			validationErrors = append(validationErrors, fmt.Errorf("Gemini output contains foreign key %q", envelope.Key))
			continue
		}
		if _, local := byKey[envelope.Key]; !local {
			// Manifest-known keys can disappear when sweep cascades orphan content.
			continue
		}
		if _, exists := stored[envelope.Key]; exists {
			duplicates[envelope.Key] = true
			continue
		}
		if _, err := output.Write(line); err != nil {
			return err
		}
		stored[envelope.Key] = storedGeminiOutput{offset: offset, length: len(line)}
		offset += int64(len(line))
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, bufio.ErrTooLong) {
			return blockOversizedGeminiOutput(database, batch, totals)
		}
		return &incompleteGeminiOutputError{Err: err}
	}
	for key, request := range byKey {
		if duplicates[key] {
			totals.accounted = true
			totals.billing.Indeterminate = true
			_, err := database.RetryGeminiRequest(request.ID, "duplicate output key", time.Now().UTC())
			if err != nil {
				validationErrors = append(validationErrors, err)
			}
			continue
		}
		location, exists := stored[key]
		if !exists {
			totals.accounted = true
			totals.billing.Indeterminate = true
			_, err := database.RetryGeminiRequest(request.ID, "missing output key", time.Now().UTC())
			if err != nil {
				validationErrors = append(validationErrors, err)
			}
			continue
		}
		line := make([]byte, location.length)
		if _, err := output.ReadAt(line, location.offset); err != nil {
			validationErrors = append(validationErrors, err)
			continue
		}
		if err := accountGeminiOutputLine(line, database, batch, request, totals); err != nil {
			validationErrors = append(validationErrors, err)
		}
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
	decoded, decodeErr := ocr.DecodeGeminiBatchResult(output.Response, request.PageEnd-request.PageStart, prices)
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
) []error {
	requests, err := database.RetryableGeminiRequests()
	if err != nil {
		return []error{err}
	}
	if len(requests) > maxAutomaticRequestsPerContinue {
		requests = requests[:maxAutomaticRequestsPerContinue]
	}
	group, err := newRetryBatchGroup()
	if err != nil {
		return []error{err}
	}
	defer func() { _ = group.Close() }()
	var commandErrors []error
	flush := func() bool {
		if len(group.requestIDs) == 0 {
			return false
		}
		flushErr := submitRetryBatchGroup(cmd, database, transport, group)
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
		path, pathErr := matchingContentPath(database, request.ContentID, request.Checksum)
		if pathErr != nil {
			if blockErr := database.BlockUnownedGeminiRequest(request.ID, pathErr.Error(), time.Now().UTC()); blockErr != nil {
				commandErrors = append(commandErrors, blockErr)
			}
			continue
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
			continue
		}
		line, lineErr := marshalGeminiBatchLine(request.RequestKey, prepared.Body)
		if lineErr != nil {
			commandErrors = append(commandErrors, lineErr)
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
			continue
		}
		group.size += int64(len(line))
		if len(group.requestIDs) == 0 && request.PreviousBatchID != nil {
			value := *request.PreviousBatchID
			group.replacementOf = &value
		}
		group.requestIDs = append(group.requestIDs, request.ID)
	}
	if err == nil && !stopped {
		flush()
	}
	return commandErrors
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
	fmt.Printf("Gemini retry batch %d prepared with %d request(s).\n", batchID, len(group.requestIDs))
	return uploadAndSubmitGeminiBatch(cmd, database, transport, batchID, group.file, group.size)
}

func retryGeminiCleanup(
	cmd *cobra.Command,
	database *db.DB,
	transport geminiBatchAPI,
) []error {
	cleanup, err := database.ListGeminiCleanup()
	if err != nil {
		return []error{err}
	}
	var commandErrors []error
	for _, item := range cleanup {
		var deleteErr error
		switch item.ResourceKind {
		case "file":
			deleteErr = transport.DeleteFile(cmd.Context(), item.ResourceName)
		case "batch":
			deleteErr = transport.DeleteBatch(cmd.Context(), item.ResourceName)
		default:
			deleteErr = fmt.Errorf("unknown cleanup resource kind %q", item.ResourceKind)
		}
		if deleteErr == nil || ocr.IsGeminiBatchNotFound(deleteErr) {
			if err := database.DeleteGeminiCleanup(item.ID); err != nil {
				commandErrors = append(commandErrors, err)
			}
			continue
		}
		_ = database.SetGeminiCleanupError(item.ID, deleteErr.Error(), time.Now().UTC())
		commandErrors = append(commandErrors, fmt.Errorf("clean up Gemini %s %s: %w", item.ResourceKind, item.ResourceName, deleteErr))
		if ocr.IsGeminiGlobalFailure(deleteErr) {
			break
		}
	}
	return commandErrors
}
