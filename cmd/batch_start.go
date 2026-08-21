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

	"github.com/mattn/go-isatty"
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

type countingReadSeeker struct {
	source io.ReadSeeker
	onRead func(int)
}

var batchStdoutIsTerminal = func() bool {
	return isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd())
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
	limit, err := readOCRLimit(cmd)
	if err != nil {
		return err
	}
	command, err := openGeminiBatchCommand(cmd, true, true)
	if err != nil {
		return err
	}
	defer command.Close()

	contents, err := command.database.PendingContentsForGeminiBatch()
	if err != nil {
		return fmt.Errorf("query untouched pending contents: %w", err)
	}
	if limit > 0 && limit < len(contents) {
		contents = contents[:limit]
	}
	if len(contents) == 0 {
		fmt.Println("No untouched documents pending Gemini batch OCR.")
		return nil
	}

	planner := ocr.NewGeminiClient("", time.Now().UTC())
	transport := newGeminiBatchAPI(command.apiKey)
	group, err := newFreshBatchGroup()
	if err != nil {
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
		path, pathErr := matchingContentPath(command.database, content.ID, content.Checksum)
		if pathErr != nil {
			fmt.Fprintf(os.Stderr, "warning: %v; run ringbinder sweep\n", pathErr)
			continue
		}
		fileType := classifyPath(path)
		if fileType == "" {
			fmt.Fprintf(os.Stderr, "warning: skipping unsupported OCR file %s\n", path)
			continue
		}
		contentStartGroup := len(groups)
		contentStartSize := group.size
		contentStartPlans := len(group.plans)
		var blockedPlans []db.GeminiBlockedRequest
		prepareErr := planner.WalkFileRequests(cmd.Context(), path, fileType, func(request ocr.GeminiPreparedRequest) error {
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
			return nil
		}, func(sizeErr *ocr.GeminiRangeSizeError) error {
			blockedPlans = append(blockedPlans, db.GeminiBlockedRequest{
				Plan: db.GeminiRequestPlan{
					ContentID: content.ID, RequestKey: newGeminiLocalKey(), FileType: fileType,
					PageStart: sizeErr.PageStart, PageEnd: sizeErr.PageEnd,
				},
				Message: sizeErr.Error(),
			})
			return nil
		})
		// Nothing is persisted until every selected item is prepared. On
		// cancellation, discard all in-memory groups instead of blocking work.
		if contextErr := cmd.Context().Err(); contextErr != nil {
			return contextErr
		}
		checksumErr := verifyContentPath(path, content.Checksum)
		if checksumErr != nil {
			if rollbackErr := rollbackFreshBatchContent(
				&group, &groups, contentStartGroup, contentStartSize, contentStartPlans,
			); rollbackErr != nil {
				commandErrors = append(commandErrors, rollbackErr)
			}
			fmt.Fprintf(os.Stderr, "warning: %v\n", checksumErr)
			continue
		}
		blockedWork = append(blockedWork, blockedPlans...)
		if prepareErr != nil {
			var planningErr *ocr.GeminiPlanningError
			if errors.As(prepareErr, &planningErr) {
				blockedWork = append(blockedWork, db.GeminiBlockedRequest{
					Plan: db.GeminiRequestPlan{
						ContentID: content.ID, RequestKey: newGeminiLocalKey(), FileType: fileType,
						PageStart: planningErr.PageStart, PageEnd: content.PageCount,
					},
					Message: planningErr.Error(),
				})
			} else if rollbackErr := rollbackFreshBatchContent(
				&group, &groups, contentStartGroup, contentStartSize, contentStartPlans,
			); rollbackErr != nil {
				commandErrors = append(commandErrors, rollbackErr)
			}
			fmt.Fprintf(os.Stderr, "warning: cannot prepare %s for Gemini batch OCR: %v\n", path, prepareErr)
		}
	}
	if err != nil {
		commandErrors = append(commandErrors, err)
	} else if sealErr := sealGroup(); sealErr != nil {
		commandErrors = append(commandErrors, sealErr)
	}
	if len(groups) == 0 && len(blockedWork) == 0 {
		if len(commandErrors) == 0 {
			fmt.Println("No valid untouched documents could be prepared for Gemini batch OCR.")
		}
		return errors.Join(commandErrors...)
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
	batchIDs, persistErr := command.database.CreateGeminiBatchWork(creations, blockedWork, now)
	if persistErr != nil {
		commandErrors = append(commandErrors, fmt.Errorf("persist prepared Gemini batch work: %w", persistErr))
		return errors.Join(commandErrors...)
	}
	for i, batchID := range batchIDs {
		fmt.Printf("Gemini batch %d prepared with %d request(s).\n", batchID, len(groups[i].plans))
	}
	for i, batchID := range batchIDs {
		if contextErr := cmd.Context().Err(); contextErr != nil {
			commandErrors = append(commandErrors, contextErr)
			break
		}
		uploadErr := uploadAndSubmitGeminiBatch(
			cmd, command.database, transport, batchID, groups[i].file, groups[i].size,
		)
		if uploadErr == nil {
			fmt.Printf("Gemini batch %d submitted.\n", batchID)
			continue
		}
		commandErrors = append(commandErrors, fmt.Errorf("Gemini batch %d: %w", batchID, uploadErr))
		if cmd.Context().Err() != nil || ocr.IsGeminiGlobalFailure(uploadErr) {
			break
		}
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
	tracker := progress.NewUpload(os.Stdout, batchStdoutIsTerminal(), batchID, size)
	defer tracker.Close()
	trackedInput := &countingReadSeeker{source: input, onRead: tracker.AddBytes}
	remoteFile, err := transport.UploadJSONL(cmd.Context(), batch.DisplayName, trackedInput, size)
	if err != nil {
		tracker.Stopped()
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
	tracker.Complete()
	if err := database.SetGeminiBatchUploaded(batchID, remoteFile.Name, time.Now().UTC()); err != nil {
		return err
	}
	return submitUploadedGeminiBatch(cmd, database, transport, batchID)
}

func submitUploadedGeminiBatch(
	cmd *cobra.Command,
	database *db.DB,
	transport geminiBatchAPI,
	batchID int64,
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
	remote, err := transport.CreateBatch(cmd.Context(), batch.Model, batch.DisplayName, batch.InputFileName)
	if err != nil {
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
	return database.SetGeminiBatchRemote(
		batchID, remote.Name, state, remote.OutputFileName, remote.ErrorMessage, time.Now().UTC(),
	)
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
