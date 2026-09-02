package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/maxim/ringbinder/internal/db"
	"github.com/maxim/ringbinder/internal/ocr"
	"github.com/maxim/ringbinder/internal/progress"
	"github.com/spf13/cobra"
)

type freshBatchGroup struct {
	file  *os.File
	size  int64
	plans []db.GeminiRequestPlan
}

type retryBatchGroup struct {
	file          *os.File
	size          int64
	requestIDs    []int64
	replacementOf *int64
}

// createGeminiBatchWork is the command boundary for the one durable
// preparation commit. Keeping it replaceable lets command tests force that
// boundary to fail without making database preparation itself UI-aware.
var createGeminiBatchWork = func(
	database *db.DB,
	creations []db.GeminiBatchCreation,
	blocked []db.GeminiBlockedRequest,
	now time.Time,
) ([]int64, error) {
	return database.CreateGeminiBatchWork(creations, blocked, now)
}

type countingReadSeeker struct {
	source io.ReadSeeker
	onRead func(int)
}

func (reader *countingReadSeeker) Read(buffer []byte) (int, error) {
	count, err := reader.source.Read(buffer)
	if count > 0 && reader.onRead != nil {
		reader.onRead(count)
	}
	return count, err
}

func (reader *countingReadSeeker) Seek(offset int64, whence int) (int64, error) {
	return reader.source.Seek(offset, whence)
}

func runBatchStart(cmd *cobra.Command, args []string) error {
	ensureCommandContext(cmd)
	limit, err := readOCRLimit(cmd)
	if err != nil {
		return err
	}
	command, err := openGeminiBatchCommand(cmd, true, true)
	if err != nil {
		return err
	}
	defer command.Close()

	out := commandStdout(cmd)
	coordinator := newCommandProgress(cmd)
	defer func() { coordinator.Finish(cmd.Context().Err() != nil) }()

	selecting := coordinator.StartPhase(progress.PhaseOptions{
		Label: "Selecting documents for Gemini batch OCR",
	})
	contents, err := command.database.PendingContentsForGeminiBatch()
	if err != nil {
		coordinator.ClosePhase(selecting)
		return fmt.Errorf("query pending contents with unassigned pages: %w", err)
	}
	// The selection query does not take the command context. Check it as soon
	// as it returns so cancellation cannot begin planning newly selected work.
	if contextErr := cmd.Context().Err(); contextErr != nil {
		coordinator.StopPhase(selecting)
		return contextErr
	}
	if limit > 0 && limit < len(contents) {
		contents = contents[:limit]
	}
	if len(contents) == 0 {
		coordinator.ClosePhase(selecting)
		fmt.Fprintln(out, "No documents have unassigned pages pending Gemini batch OCR.")
		return reportBatchBlockedSummaryTo(out, command.database)
	}
	coordinator.CompletePhase(selecting)

	preparing := coordinator.StartPhase(progress.PhaseOptions{
		Label: "Preparing Gemini input", Total: len(contents), Unit: "documents", Detail: "0 requests",
	})
	planner := ocr.NewGeminiClient("", time.Now().UTC())
	transport := newGeminiBatchAPI(command.apiKey)
	group, err := newFreshBatchGroup()
	if err != nil {
		coordinator.ClosePhase(preparing)
		return err
	}
	var groups []*freshBatchGroup
	defer func() {
		_ = group.Close()
		for _, sealed := range groups {
			_ = sealed.Close()
		}
	}()
	var commandErrors []error
	var blockedWork []db.GeminiBlockedRequest
	requestCount := 0
	sealGroup := func() error {
		if len(group.plans) == 0 {
			return nil
		}
		fresh, sealErr := newFreshBatchGroup()
		if sealErr != nil {
			err = sealErr
			return sealErr
		}
		groups = append(groups, group)
		group = fresh
		return nil
	}

	for _, content := range contents {
		if err != nil {
			break
		}
		if contextErr := cmd.Context().Err(); contextErr != nil {
			coordinator.StopPhase(preparing)
			return contextErr
		}
		preparing.SetCurrent(fmt.Sprintf("document %d", content.ID))
		path, pathErr := matchingContentPath(command.database, content.ID, content.Checksum)
		if pathErr != nil {
			coordinator.Warningf("warning: %v; run ringbinder sweep\n", pathErr)
			preparing.Advance()
			continue
		}
		preparing.SetCurrent(path)
		fileType := classifyPath(path)
		if fileType == "" {
			coordinator.Warningf("warning: skipping unsupported OCR file %s\n", path)
			preparing.Advance()
			continue
		}
		contentStartGroup := len(groups)
		contentStartSize := group.size
		contentStartPlans := len(group.plans)
		contentStartRequestCount := requestCount
		var blockedPlans []db.GeminiBlockedRequest
		missingRanges, prepareErr := command.database.MissingUnownedPageRanges(content.ID)
		yield := func(request ocr.GeminiPreparedRequest) error {
			plan := db.GeminiRequestPlan{
				ContentID: content.ID, RequestKey: newGeminiLocalKey(), FileType: fileType,
				PageStart: request.PageStart, PageEnd: request.PageEnd,
			}
			line, lineErr := marshalGeminiBatchLine(plan.RequestKey, request.Body)
			if lineErr != nil {
				return lineErr
			}
			if int64(len(line)) > ocr.GeminiBatchMaxInputBytes {
				blockedPlans = append(blockedPlans, db.GeminiBlockedRequest{
					Plan: plan, Message: "serialized request exceeds Gemini's batch input limit",
				})
				return nil
			}
			if group.size > 0 && group.size+int64(len(line)) > ocr.GeminiBatchMaxInputBytes {
				if checksumErr := verifyContentPath(path, content.Checksum); checksumErr != nil {
					return checksumErr
				}
				if err := sealGroup(); err != nil {
					return err
				}
			}
			if _, lineErr = group.file.Write(line); lineErr != nil {
				return fmt.Errorf("write private Gemini batch input: %w", lineErr)
			}
			group.size += int64(len(line))
			group.plans = append(group.plans, plan)
			requestCount++
			preparing.SetDetail(fmt.Sprintf("%d requests", requestCount))
			return nil
		}
		reject := func(sizeErr *ocr.GeminiRangeSizeError) error {
			blockedPlans = append(blockedPlans, db.GeminiBlockedRequest{
				Plan: db.GeminiRequestPlan{
					ContentID: content.ID, RequestKey: newGeminiLocalKey(), FileType: fileType,
					PageStart: sizeErr.PageStart, PageEnd: sizeErr.PageEnd,
				},
				Message: sizeErr.Error(),
			})
			return nil
		}
		for _, pageRange := range missingRanges {
			if prepareErr != nil {
				break
			}
			prepareErr = walkGeminiMissingRange(
				cmd.Context(), planner, path, fileType,
				pageRange.Start, pageRange.End, yield, reject,
			)
		}
		// Nothing is persisted until every selected item is prepared. On
		// cancellation, discard all in-memory groups instead of blocking work.
		if contextErr := cmd.Context().Err(); contextErr != nil {
			coordinator.StopPhase(preparing)
			return contextErr
		}
		checksumErr := verifyContentPath(path, content.Checksum)
		if checksumErr != nil {
			if rollbackErr := rollbackFreshBatchContent(
				&group, &groups, contentStartGroup, contentStartSize, contentStartPlans,
			); rollbackErr != nil {
				commandErrors = append(commandErrors, rollbackErr)
			} else {
				requestCount = contentStartRequestCount
				preparing.SetDetail(fmt.Sprintf("%d requests", requestCount))
			}
			coordinator.Warningf("warning: %v\n", checksumErr)
			preparing.Advance()
			continue
		}
		blockedWork = append(blockedWork, blockedPlans...)
		if prepareErr != nil {
			var planningErr *ocr.GeminiPlanningError
			if errors.As(prepareErr, &planningErr) {
				blockedWork = append(blockedWork, db.GeminiBlockedRequest{
					Plan: db.GeminiRequestPlan{
						ContentID: content.ID, RequestKey: newGeminiLocalKey(), FileType: fileType,
						PageStart: planningErr.PageStart, PageEnd: planningErr.PageEnd,
					},
					Message: planningErr.Error(),
				})
			} else if rollbackErr := rollbackFreshBatchContent(
				&group, &groups, contentStartGroup, contentStartSize, contentStartPlans,
			); rollbackErr != nil {
				commandErrors = append(commandErrors, rollbackErr)
			} else {
				requestCount = contentStartRequestCount
				preparing.SetDetail(fmt.Sprintf("%d requests", requestCount))
			}
			coordinator.Warningf("warning: cannot prepare %s for Gemini batch OCR: %v\n", path, prepareErr)
		}
		preparing.Advance()
	}
	if err != nil {
		commandErrors = append(commandErrors, err)
	} else if sealErr := sealGroup(); sealErr != nil {
		commandErrors = append(commandErrors, sealErr)
	}
	if len(groups) == 0 && len(blockedWork) == 0 {
		coordinator.ClosePhase(preparing)
		if len(commandErrors) == 0 {
			fmt.Fprintln(out, "No valid pending page ranges could be prepared for Gemini batch OCR.")
		}
		if summaryErr := reportBatchBlockedSummaryTo(out, command.database); summaryErr != nil {
			commandErrors = append(commandErrors, summaryErr)
		}
		return errors.Join(commandErrors...)
	}

	if contextErr := cmd.Context().Err(); contextErr != nil {
		coordinator.StopPhase(preparing)
		return contextErr
	}
	now := time.Now().UTC()
	prices := ocr.GeminiBatchPrices(now)
	creations := make([]db.GeminiBatchCreation, len(groups))
	for i, sealed := range groups {
		creations[i] = db.GeminiBatchCreation{
			DisplayName: newGeminiDisplayName(now), Model: ocr.GeminiBatchModel,
			InputPrice: int64(prices.Input), OutputPrice: int64(prices.Output),
			Requests: sealed.plans,
		}
	}
	if contextErr := cmd.Context().Err(); contextErr != nil {
		coordinator.StopPhase(preparing)
		return contextErr
	}
	batchIDs, persistErr := createGeminiBatchWork(command.database, creations, blockedWork, now)
	if persistErr != nil {
		coordinator.ClosePhase(preparing)
		commandErrors = append(commandErrors, fmt.Errorf("persist prepared Gemini batch work: %w", persistErr))
		return errors.Join(commandErrors...)
	}
	// Prepared work becomes durable only after this transaction; its result is
	// the preparation phase's completion rather than a duplicate status line.
	coordinator.ClosePhase(preparing)
	for i, batchID := range batchIDs {
		fmt.Fprintf(out, "Gemini batch %d prepared with %d request(s).\n", batchID, len(groups[i].plans))
	}
	for i, batchID := range batchIDs {
		if contextErr := cmd.Context().Err(); contextErr != nil {
			commandErrors = append(commandErrors, contextErr)
			break
		}
		uploadErr := uploadAndSubmitGeminiBatch(
			cmd, command.database, transport, batchID, groups[i].file, groups[i].size, coordinator,
		)
		if uploadErr == nil {
			fmt.Fprintf(out, "Gemini batch %d submitted.\n", batchID)
			continue
		}
		commandErrors = append(commandErrors, fmt.Errorf("Gemini batch %d: %w", batchID, uploadErr))
		if cmd.Context().Err() != nil || ocr.IsGeminiGlobalFailure(uploadErr) {
			break
		}
	}
	if summaryErr := reportBatchBlockedSummaryTo(out, command.database); summaryErr != nil {
		commandErrors = append(commandErrors, summaryErr)
	}
	return errors.Join(commandErrors...)
}

func uploadAndSubmitGeminiBatch(
	cmd *cobra.Command,
	database *db.DB,
	transport geminiBatchAPI,
	batchID int64,
	input io.ReadSeeker,
	size int64,
	coordinators ...*progressCoordinator,
) error {
	batch, err := database.GetGeminiBatch(batchID)
	if err != nil {
		return err
	}
	if batch == nil {
		return fmt.Errorf("Gemini batch %d disappeared", batchID)
	}
	if contextErr := cmd.Context().Err(); contextErr != nil {
		return contextErr
	}
	if err := database.SetGeminiBatchUploadUnknown(batchID, time.Now().UTC()); err != nil {
		return err
	}
	coordinator := firstProgressCoordinator(coordinators)
	var tracker *progress.UploadTracker
	if coordinator != nil {
		tracker = coordinator.StartUpload(batchID, size)
		defer coordinator.CloseUpload(tracker)
	} else {
		out := commandStdout(cmd)
		tracker = progress.NewUpload(out, progressWriterIsTerminal(out), batchID, size)
		defer tracker.Close()
	}
	trackedInput := &countingReadSeeker{source: input, onRead: tracker.AddBytes}
	remoteFile, err := transport.UploadJSONL(cmd.Context(), batch.DisplayName, trackedInput, size)
	if err != nil {
		if coordinator != nil {
			coordinator.StopUpload(tracker)
		} else {
			tracker.Stopped()
		}
		// Once upload starts, cancellation can race with remote finalization even
		// when a transport forgets to classify that uncertainty explicitly.
		ambiguous := ocr.IsGeminiAmbiguousOperation(err) ||
			errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
		if ambiguous {
			_ = database.SetGeminiBatchError(batchID, err.Error(), time.Now().UTC())
		} else {
			_ = database.SetGeminiBatchPrepared(batchID, err.Error(), time.Now().UTC())
		}
		return err
	}
	if coordinator != nil {
		coordinator.CompleteUpload(tracker)
	} else {
		tracker.Complete()
	}
	// A successful upload can arrive alongside cancellation. Record it before
	// returning so recovery knows the remote input exists and never uploads it twice.
	if err := database.SetGeminiBatchUploaded(batchID, remoteFile.Name, time.Now().UTC()); err != nil {
		return err
	}
	return submitUploadedGeminiBatch(cmd, database, transport, batchID, coordinator)
}

func submitUploadedGeminiBatch(
	cmd *cobra.Command,
	database *db.DB,
	transport geminiBatchAPI,
	batchID int64,
	coordinators ...*progressCoordinator,
) error {
	batch, err := database.GetGeminiBatch(batchID)
	if err != nil {
		return err
	}
	if batch == nil {
		return fmt.Errorf("Gemini batch %d disappeared", batchID)
	}
	requests, err := database.GeminiRequestsForBatch(batchID)
	if err != nil {
		return err
	}
	if !geminiBatchMembershipMatches(batch.RequestKeys, requests) {
		message := "local request membership changed after upload; refusing non-idempotent submission"
		if err := database.DetachGeminiBatchRequestsForRegeneration(batchID, message, time.Now().UTC()); err != nil {
			return err
		}
		if _, err := database.FinalizeGeminiBatch(batchID, time.Now().UTC()); err != nil {
			return err
		}
		return errors.New(message)
	}
	now := time.Now().UTC()
	prices := ocr.GeminiBatchPrices(now)
	if err := database.SetGeminiBatchPrices(batchID, int64(prices.Input), int64(prices.Output), now); err != nil {
		return err
	}
	if contextErr := cmd.Context().Err(); contextErr != nil {
		return contextErr
	}
	if err := database.SetGeminiBatchSubmissionUnknown(batchID, now); err != nil {
		return err
	}
	coordinator := firstProgressCoordinator(coordinators)
	var submitting *progress.Reporter
	if coordinator != nil {
		submitting = coordinator.StartPhase(progress.PhaseOptions{
			Label: fmt.Sprintf("Submitting Gemini batch %d", batchID),
		})
	}
	remote, err := transport.CreateBatch(cmd.Context(), batch.Model, batch.DisplayName, batch.InputFileName)
	if err != nil {
		if submitting != nil {
			if cmd.Context().Err() != nil {
				coordinator.StopPhase(submitting)
			} else {
				coordinator.ClosePhase(submitting)
			}
		}
		if ocr.IsGeminiAmbiguousOperation(err) {
			_ = database.SetGeminiBatchError(batchID, err.Error(), time.Now().UTC())
			return err
		}
		var apiErr *ocr.GeminiBatchAPIError
		if errors.As(err, &apiErr) && apiErr.StatusCode >= 400 && apiErr.StatusCode < 500 &&
			apiErr.StatusCode != 408 && apiErr.StatusCode != 429 {
			blockErr := database.BlockGeminiBatchRequests(batchID, err.Error(), time.Now().UTC())
			_, finalizeErr := database.FinalizeGeminiBatch(batchID, time.Now().UTC())
			return errors.Join(err, blockErr, finalizeErr)
		}
		_ = database.SetGeminiBatchState(batchID, db.GeminiBatchUploaded, "", err.Error(), time.Now().UTC())
		return err
	}
	state, err := ocr.NormalizeGeminiBatchState(remote.State)
	if err != nil {
		state = db.GeminiBatchPending
		remote.ErrorMessage = err.Error()
	}
	// A successful submission can race with cancellation too; durable remote
	// identity is required to reconcile it rather than submit a duplicate batch.
	setErr := database.SetGeminiBatchRemote(
		batchID, remote.Name, state, remote.OutputFileName, remote.ErrorMessage, time.Now().UTC(),
	)
	if submitting != nil {
		if setErr != nil {
			coordinator.ClosePhase(submitting)
		} else {
			coordinator.CompletePhase(submitting)
		}
	}
	return setErr
}

func geminiBatchMembershipMatches(manifest []string, requests []db.GeminiBatchRequest) bool {
	if len(manifest) != len(requests) {
		return false
	}
	keys := make(map[string]bool, len(requests))
	for _, request := range requests {
		keys[request.RequestKey] = true
	}
	for _, key := range manifest {
		if !keys[key] {
			return false
		}
	}
	return true
}

func walkGeminiMissingRange(
	ctx context.Context,
	planner *ocr.GeminiClient,
	path, fileType string,
	start, end int,
	yield func(ocr.GeminiPreparedRequest) error,
	reject func(*ocr.GeminiRangeSizeError) error,
) error {
	// Use maximal page-limit chunks first. Midpoint recursion below is reserved
	// for requests that still exceed byte limits after this deterministic split.
	if fileType == "pdf" && end-start > geminiPDFPagesPerChunk {
		for chunkStart := start; chunkStart < end; chunkStart += geminiPDFPagesPerChunk {
			chunkEnd := min(chunkStart+geminiPDFPagesPerChunk, end)
			if err := walkGeminiMissingRange(
				ctx, planner, path, fileType, chunkStart, chunkEnd, yield, reject,
			); err != nil {
				return err
			}
		}
		return nil
	}

	request, err := planner.PrepareRangeRequest(ctx, path, fileType, start, end)
	if err == nil {
		return yield(request)
	}
	var sizeErr *ocr.GeminiRangeSizeError
	if !errors.As(err, &sizeErr) {
		return &ocr.GeminiPlanningError{
			PageStart: start,
			PageEnd:   end,
			Cause:     fmt.Errorf("%s: %w", path, err),
		}
	}
	if end-start == 1 {
		return reject(sizeErr)
	}
	mid := start + (end-start)/2
	if err := walkGeminiMissingRange(ctx, planner, path, fileType, start, mid, yield, reject); err != nil {
		return err
	}
	return walkGeminiMissingRange(ctx, planner, path, fileType, mid, end, yield, reject)
}

func marshalGeminiBatchLine(key string, request []byte) ([]byte, error) {
	line, err := json.Marshal(struct {
		Key     string          `json:"key"`
		Request json.RawMessage `json:"request"`
	}{Key: key, Request: request})
	if err != nil {
		return nil, fmt.Errorf("encode Gemini batch request %s: %w", key, err)
	}
	return append(line, '\n'), nil
}

// A content item may cross transport groups, but it enters prepared work
// atomically. If its final validation fails, discard every segment while
// retaining plans that preceded it in the first group.
func rollbackFreshBatchContent(
	group **freshBatchGroup,
	groups *[]*freshBatchGroup,
	startGroup int,
	startSize int64,
	startPlans int,
) error {
	if len(*groups) == startGroup {
		return rollbackFreshBatchGroup(*group, startSize, startPlans)
	}

	restored := (*groups)[startGroup]
	var rollbackErrors []error
	if err := rollbackFreshBatchGroup(restored, startSize, startPlans); err != nil {
		rollbackErrors = append(rollbackErrors, err)
	}
	for _, discarded := range (*groups)[startGroup+1:] {
		if err := discarded.Close(); err != nil {
			rollbackErrors = append(rollbackErrors, err)
		}
	}
	if *group != restored {
		if err := (*group).Close(); err != nil {
			rollbackErrors = append(rollbackErrors, err)
		}
	}
	*groups = (*groups)[:startGroup]
	*group = restored
	return errors.Join(rollbackErrors...)
}

func rollbackFreshBatchGroup(group *freshBatchGroup, size int64, planCount int) error {
	if err := group.file.Truncate(size); err != nil {
		return fmt.Errorf("truncate private Gemini batch input: %w", err)
	}
	if _, err := group.file.Seek(size, io.SeekStart); err != nil {
		return fmt.Errorf("rewind private Gemini batch input: %w", err)
	}
	group.size = size
	group.plans = group.plans[:planCount]
	return nil
}

func newFreshBatchGroup() (*freshBatchGroup, error) {
	file, err := newPrivateTempFile("ringbinder-gemini-input-")
	if err != nil {
		return nil, err
	}
	return &freshBatchGroup{file: file}, nil
}

func (group *freshBatchGroup) Close() error {
	if group == nil || group.file == nil {
		return nil
	}
	err := group.file.Close()
	group.file = nil
	return err
}

func newPrivateTempFile(prefix string) (*os.File, error) {
	file, err := os.CreateTemp("", prefix)
	if err != nil {
		return nil, fmt.Errorf("create private temporary file: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return nil, err
	}
	// The open descriptor remains usable on Darwin/Linux while unlinking keeps
	// document payloads out of the filesystem after crashes or interruption.
	if err := os.Remove(file.Name()); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("unlink private temporary file: %w", err)
	}
	return file, nil
}

func newGeminiDisplayName(now time.Time) string {
	return fmt.Sprintf("ringbinder-batch-%s-%s", now.UTC().Format("20060102T150405.000000000Z"), randomHex(12))
}

func newGeminiLocalKey() string {
	return "rb-" + randomHex(16)
}

func randomHex(bytes int) string {
	data := make([]byte, bytes)
	if _, err := rand.Read(data); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(data)
}
