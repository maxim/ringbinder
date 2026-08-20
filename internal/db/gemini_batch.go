package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const maxGeminiBatchErrorBytes = 2_048

const (
	GeminiBatchPrepared          = "prepared"
	GeminiBatchUploadUnknown     = "upload_unknown"
	GeminiBatchUploaded          = "uploaded"
	GeminiBatchSubmissionUnknown = "submission_unknown"
	GeminiBatchPending           = "pending"
	GeminiBatchRunning           = "running"
	GeminiBatchCancelling        = "cancelling"
	GeminiBatchSucceeded         = "succeeded"
	GeminiBatchFailed            = "failed"
	GeminiBatchCancelled         = "cancelled"
	GeminiBatchExpired           = "expired"

	GeminiRequestAssigned  = "assigned"
	GeminiRequestStaged    = "staged"
	GeminiRequestRetryable = "retryable"
	GeminiRequestBlocked   = "blocked"
)

type GeminiBatch struct {
	ID             int64
	DisplayName    string
	Model          string
	RequestKeys    []string
	State          string
	InputFileName  string
	OutputFileName string
	RemoteName     string
	InputPrice     int64
	OutputPrice    int64
	ReplacementOf  *int64
	LastError      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type GeminiBatchCreation struct {
	DisplayName   string
	Model         string
	InputPrice    int64
	OutputPrice   int64
	ReplacementOf *int64
	Requests      []GeminiRequestPlan
}

type GeminiBlockedRequest struct {
	Plan    GeminiRequestPlan
	Message string
}

type GeminiRequestPlan struct {
	ContentID        int64
	RequestKey       string
	FileType         string
	PageStart        int
	PageEnd          int
	AttemptCount     int
	ReplacementCount int
	SplitDepth       int
}

type GeminiBatchRequest struct {
	ID                int64
	ContentID         int64
	BatchID           *int64
	RequestKey        string
	FileType          string
	PageStart         int
	PageEnd           int
	State             string
	AttemptCount      int
	ReplacementCount  int
	PreviousBatchID   *int64
	SplitDepth        int
	InputTokens       *int64
	OutputTokens      *int64
	KnownCost         int64
	CostIndeterminate bool
	LastError         string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	Path              string
	Checksum          string
	PageCount         int
}

type GeminiStagedPage struct {
	PageIndex int
	Markdown  string
}

type GeminiCleanup struct {
	ID           int64
	ResourceKind string
	ResourceName string
	LastError    string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

func (db *DB) PendingContentsForDirect() ([]Content, int, error) {
	contents, err := db.pendingContentsWithRequestFilter(
		`NOT EXISTS (
		   SELECT 1 FROM gemini_batch_requests r
		   WHERE r.content_id = c.id AND r.batch_id IS NOT NULL
		 )`,
	)
	if err != nil {
		return nil, 0, err
	}

	var excluded int
	err = db.QueryRow(
		`SELECT COUNT(*)
		 FROM contents c
		 WHERE c.ocr_pending = 1
		   AND EXISTS (SELECT 1 FROM documents d WHERE d.content_id = c.id AND d.deleted = 0)
		   AND EXISTS (
		     SELECT 1 FROM gemini_batch_requests r
		     WHERE r.content_id = c.id AND r.batch_id IS NOT NULL
		   )`,
	).Scan(&excluded)
	return contents, excluded, err
}

func (db *DB) PendingContentsForGeminiBatch() ([]Content, error) {
	return db.pendingContentsWithRequestFilter(
		`NOT EXISTS (
		   SELECT 1 FROM gemini_batch_requests r
		   WHERE r.content_id = c.id
		 )`,
	)
}

func (db *DB) pendingContentsWithRequestFilter(filter string) ([]Content, error) {
	rows, err := db.Query(
		`SELECT c.id, c.checksum, c.page_count, c.ocr_pending
		 FROM contents c
		 WHERE c.ocr_pending = 1
		   AND EXISTS (
		     SELECT 1 FROM documents d
		     WHERE d.content_id = c.id AND d.deleted = 0
		   )
		   AND ` + filter + `
		 ORDER BY c.id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var contents []Content
	for rows.Next() {
		content, err := scanContent(rows)
		if err != nil {
			return nil, err
		}
		contents = append(contents, content)
	}
	return contents, rows.Err()
}

func (db *DB) CountTrackedGeminiBatches() (int, error) {
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM gemini_batches`).Scan(&count)
	return count, err
}

func (db *DB) CountGeminiCleanup() (int, error) {
	var count int
	err := db.QueryRow(`SELECT COUNT(*) FROM gemini_batch_cleanup`).Scan(&count)
	return count, err
}

func (db *DB) GetContentByID(contentID int64) (*Content, error) {
	content, err := scanContent(db.QueryRow(
		`SELECT id, checksum, page_count, ocr_pending FROM contents WHERE id = ?`,
		contentID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &content, nil
}

func (db *DB) GetDocumentPathsForContent(contentID int64) ([]string, error) {
	rows, err := db.Query(
		`SELECT path FROM documents
		 WHERE content_id = ? AND deleted = 0
		 ORDER BY id`,
		contentID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var paths []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	return paths, rows.Err()
}

func (db *DB) CreateGeminiBatch(
	displayName, model string,
	inputPrice, outputPrice int64,
	replacementOf *int64,
	plans []GeminiRequestPlan,
	now time.Time,
) (int64, error) {
	ids, err := db.CreateGeminiBatchWork([]GeminiBatchCreation{{
		DisplayName: displayName, Model: model, InputPrice: inputPrice,
		OutputPrice: outputPrice, ReplacementOf: replacementOf, Requests: plans,
	}}, nil, now)
	if err != nil {
		return 0, err
	}
	return ids[0], nil
}

func (db *DB) CreateGeminiBatchWork(
	creations []GeminiBatchCreation,
	blocked []GeminiBlockedRequest,
	now time.Time,
) (batchIDs []int64, err error) {
	if len(creations) == 0 && len(blocked) == 0 {
		return nil, errors.New("Gemini batch work is empty")
	}
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer rollbackOnError(tx, &err)
	stamp := now.UTC().Format(time.RFC3339Nano)
	for _, creation := range creations {
		if len(creation.Requests) == 0 {
			return nil, errors.New("Gemini batch must contain at least one request")
		}
		requestKeys := make([]string, len(creation.Requests))
		for i, plan := range creation.Requests {
			requestKeys[i] = plan.RequestKey
		}
		batchID, insertErr := insertGeminiBatch(
			tx, creation.DisplayName, creation.Model, requestKeys,
			creation.InputPrice, creation.OutputPrice, creation.ReplacementOf, now,
		)
		if insertErr != nil {
			return nil, insertErr
		}
		batchIDs = append(batchIDs, batchID)
		for _, plan := range creation.Requests {
			if _, err = tx.Exec(
				`INSERT INTO gemini_batch_requests (
				   content_id, batch_id, request_key, file_type, page_start, page_end, state,
				   attempt_count, replacement_count, split_depth, created_at, updated_at
				 ) VALUES (?, ?, ?, ?, ?, ?, 'assigned', ?, ?, ?, ?, ?)`,
				plan.ContentID, batchID, plan.RequestKey, plan.FileType, plan.PageStart, plan.PageEnd,
				plan.AttemptCount, plan.ReplacementCount, plan.SplitDepth, stamp, stamp,
			); err != nil {
				return nil, err
			}
		}
	}
	for _, item := range blocked {
		plan := item.Plan
		if _, err = tx.Exec(
			`INSERT INTO gemini_batch_requests (
			   content_id, request_key, file_type, page_start, page_end, state,
			   attempt_count, replacement_count, split_depth, last_error,
			   created_at, updated_at
			 ) VALUES (?, ?, ?, ?, ?, 'blocked', ?, ?, ?, ?, ?, ?)`,
			plan.ContentID, plan.RequestKey, plan.FileType, plan.PageStart, plan.PageEnd,
			plan.AttemptCount, plan.ReplacementCount, plan.SplitDepth,
			truncateGeminiError(item.Message), stamp, stamp,
		); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return batchIDs, nil
}

func (db *DB) CreateBlockedGeminiRequest(plan GeminiRequestPlan, message string, now time.Time) (int64, error) {
	stamp := now.UTC().Format(time.RFC3339Nano)
	res, err := db.Exec(
		`INSERT INTO gemini_batch_requests (
		   content_id, request_key, file_type, page_start, page_end, state,
		   attempt_count, replacement_count, split_depth, last_error,
		   created_at, updated_at
		 ) VALUES (?, ?, ?, ?, ?, 'blocked', ?, ?, ?, ?, ?, ?)`,
		plan.ContentID, plan.RequestKey, plan.FileType, plan.PageStart, plan.PageEnd,
		plan.AttemptCount, plan.ReplacementCount, plan.SplitDepth,
		truncateGeminiError(message), stamp, stamp,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (db *DB) CreateGeminiBatchForRequests(
	displayName, model string,
	inputPrice, outputPrice int64,
	replacementOf *int64,
	requestIDs []int64,
	now time.Time,
) (batchID int64, err error) {
	if len(requestIDs) == 0 {
		return 0, errors.New("Gemini batch must contain at least one request")
	}

	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer rollbackOnError(tx, &err)

	requestKeys := make([]string, 0, len(requestIDs))
	for _, requestID := range requestIDs {
		var key string
		if err = tx.QueryRow(
			`SELECT request_key FROM gemini_batch_requests
			 WHERE id = ? AND state = 'retryable' AND batch_id IS NULL`,
			requestID,
		).Scan(&key); err != nil {
			return 0, fmt.Errorf("load retryable Gemini request %d: %w", requestID, err)
		}
		requestKeys = append(requestKeys, key)
	}
	batchID, err = insertGeminiBatch(tx, displayName, model, requestKeys, inputPrice, outputPrice, replacementOf, now)
	if err != nil {
		return 0, err
	}
	stamp := now.UTC().Format(time.RFC3339Nano)
	for _, requestID := range requestIDs {
		res, execErr := tx.Exec(
			`UPDATE gemini_batch_requests
			 SET batch_id = ?, state = 'assigned', updated_at = ?
			 WHERE id = ? AND state = 'retryable' AND batch_id IS NULL`,
			batchID, stamp, requestID,
		)
		if execErr != nil {
			return 0, execErr
		}
		changed, execErr := res.RowsAffected()
		if execErr != nil {
			return 0, execErr
		}
		if changed != 1 {
			return 0, fmt.Errorf("Gemini request %d is not retryable", requestID)
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return batchID, nil
}

func insertGeminiBatch(
	tx *sql.Tx,
	displayName, model string,
	requestKeys []string,
	inputPrice, outputPrice int64,
	replacementOf *int64,
	now time.Time,
) (int64, error) {
	stamp := now.UTC().Format(time.RFC3339Nano)
	manifest, err := json.Marshal(requestKeys)
	if err != nil {
		return 0, fmt.Errorf("encode Gemini request manifest: %w", err)
	}
	res, err := tx.Exec(
		`INSERT INTO gemini_batches (
		   display_name, model, request_keys, state, input_price, output_price,
		   replacement_of, created_at, updated_at
		 ) VALUES (?, ?, ?, 'prepared', ?, ?, ?, ?, ?)`,
		displayName, model, string(manifest), inputPrice, outputPrice, replacementOf, stamp, stamp,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (db *DB) ListGeminiBatches() ([]GeminiBatch, error) {
	rows, err := db.Query(
		`SELECT id, display_name, model, request_keys, state, input_file_name,
		        output_file_name, remote_name, input_price, output_price,
		        replacement_of, last_error, created_at, updated_at
		 FROM gemini_batches
		 ORDER BY id DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var batches []GeminiBatch
	for rows.Next() {
		batch, err := scanGeminiBatch(rows)
		if err != nil {
			return nil, err
		}
		batches = append(batches, batch)
	}
	return batches, rows.Err()
}

func (db *DB) GetGeminiBatch(batchID int64) (*GeminiBatch, error) {
	batch, err := scanGeminiBatch(db.QueryRow(
		`SELECT id, display_name, model, request_keys, state, input_file_name,
		        output_file_name, remote_name, input_price, output_price,
		        replacement_of, last_error, created_at, updated_at
		 FROM gemini_batches WHERE id = ?`,
		batchID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &batch, nil
}

func scanGeminiBatch(scanner interface{ Scan(...any) error }) (GeminiBatch, error) {
	var batch GeminiBatch
	var inputFile, outputFile, remoteName sql.NullString
	var replacementOf sql.NullInt64
	var requestKeys, createdAt, updatedAt string
	if err := scanner.Scan(
		&batch.ID, &batch.DisplayName, &batch.Model, &requestKeys, &batch.State,
		&inputFile, &outputFile, &remoteName, &batch.InputPrice, &batch.OutputPrice,
		&replacementOf, &batch.LastError, &createdAt, &updatedAt,
	); err != nil {
		return GeminiBatch{}, err
	}
	if err := json.Unmarshal([]byte(requestKeys), &batch.RequestKeys); err != nil {
		return GeminiBatch{}, fmt.Errorf("decode Gemini request manifest: %w", err)
	}
	batch.InputFileName = inputFile.String
	batch.OutputFileName = outputFile.String
	batch.RemoteName = remoteName.String
	if replacementOf.Valid {
		value := replacementOf.Int64
		batch.ReplacementOf = &value
	}
	var err error
	batch.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return GeminiBatch{}, fmt.Errorf("parse Gemini batch created_at: %w", err)
	}
	batch.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return GeminiBatch{}, fmt.Errorf("parse Gemini batch updated_at: %w", err)
	}
	return batch, nil
}

func (db *DB) SetGeminiBatchPrepared(batchID int64, message string, now time.Time) error {
	return db.updateGeminiBatch(
		batchID,
		`state = 'prepared', last_error = ?, updated_at = ?`,
		truncateGeminiError(message), now.UTC().Format(time.RFC3339Nano),
	)
}

func (db *DB) SetGeminiBatchPrices(batchID, inputPrice, outputPrice int64, now time.Time) error {
	return db.updateGeminiBatch(
		batchID,
		`input_price = ?, output_price = ?, updated_at = ?`,
		inputPrice, outputPrice, now.UTC().Format(time.RFC3339Nano),
	)
}

func (db *DB) SetGeminiBatchUploadUnknown(batchID int64, now time.Time) error {
	return db.updateGeminiBatch(
		batchID,
		`state = 'upload_unknown', last_error = '', updated_at = ?`,
		now.UTC().Format(time.RFC3339Nano),
	)
}

func (db *DB) SetGeminiBatchUploaded(batchID int64, inputFileName string, now time.Time) error {
	return db.updateGeminiBatch(
		batchID,
		`state = 'uploaded', input_file_name = ?, last_error = '', updated_at = ?`,
		inputFileName, now.UTC().Format(time.RFC3339Nano),
	)
}

func (db *DB) SetGeminiBatchSubmissionUnknown(batchID int64, now time.Time) error {
	return db.updateGeminiBatch(
		batchID,
		`state = 'submission_unknown', last_error = '', updated_at = ?`,
		now.UTC().Format(time.RFC3339Nano),
	)
}

func (db *DB) SetGeminiBatchRemote(
	batchID int64,
	remoteName, state, outputFileName, message string,
	now time.Time,
) error {
	return db.updateGeminiBatch(
		batchID,
		`remote_name = ?, state = ?, output_file_name = NULLIF(?, ''),
		 last_error = ?, updated_at = ?`,
		remoteName, state, outputFileName, truncateGeminiError(message), now.UTC().Format(time.RFC3339Nano),
	)
}

func (db *DB) SetGeminiBatchState(
	batchID int64,
	state, outputFileName, message string,
	now time.Time,
) error {
	return db.updateGeminiBatch(
		batchID,
		`state = ?, output_file_name = COALESCE(NULLIF(?, ''), output_file_name),
		 last_error = ?, updated_at = ?`,
		state, outputFileName, truncateGeminiError(message), now.UTC().Format(time.RFC3339Nano),
	)
}

func (db *DB) SetGeminiBatchError(batchID int64, message string, now time.Time) error {
	return db.updateGeminiBatch(
		batchID,
		`last_error = ?, updated_at = ?`,
		truncateGeminiError(message), now.UTC().Format(time.RFC3339Nano),
	)
}

func (db *DB) updateGeminiBatch(batchID int64, assignment string, args ...any) error {
	args = append(args, batchID)
	res, err := db.Exec(`UPDATE gemini_batches SET `+assignment+` WHERE id = ?`, args...)
	if err != nil {
		return err
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("Gemini batch %d not found", batchID)
	}
	return nil
}

func (db *DB) GeminiRequestsForBatch(batchID int64) ([]GeminiBatchRequest, error) {
	return db.queryGeminiRequests(
		`WHERE r.batch_id = ? ORDER BY r.id`,
		batchID,
	)
}

func (db *DB) GeminiRequestByID(requestID int64) (*GeminiBatchRequest, error) {
	requests, err := db.queryGeminiRequests(`WHERE r.id = ?`, requestID)
	if err != nil {
		return nil, err
	}
	if len(requests) == 0 {
		return nil, nil
	}
	return &requests[0], nil
}

func (db *DB) RetryableGeminiRequests() ([]GeminiBatchRequest, error) {
	return db.queryGeminiRequests(`WHERE r.state = 'retryable' ORDER BY r.id`)
}

func (db *DB) BlockedGeminiRequests() ([]GeminiBatchRequest, error) {
	return db.queryGeminiRequests(`WHERE r.state = 'blocked' ORDER BY r.id`)
}

func (db *DB) queryGeminiRequests(where string, args ...any) ([]GeminiBatchRequest, error) {
	rows, err := db.Query(
		`SELECT r.id, r.content_id, r.batch_id, r.request_key, r.file_type,
		        r.page_start, r.page_end, r.state, r.attempt_count,
		        r.replacement_count, r.previous_batch_id, r.split_depth, r.input_tokens,
		        r.output_tokens, r.known_cost, r.cost_indeterminate,
		        r.last_error, r.created_at, r.updated_at,
		        COALESCE((
		          SELECT d.path FROM documents d
		          WHERE d.content_id = r.content_id AND d.deleted = 0
		          ORDER BY d.id LIMIT 1
		        ), ''), c.checksum, c.page_count
		 FROM gemini_batch_requests r
		 JOIN contents c ON c.id = r.content_id
		 `+where,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var requests []GeminiBatchRequest
	for rows.Next() {
		request, err := scanGeminiRequest(rows)
		if err != nil {
			return nil, err
		}
		requests = append(requests, request)
	}
	return requests, rows.Err()
}

func scanGeminiRequest(scanner interface{ Scan(...any) error }) (GeminiBatchRequest, error) {
	var request GeminiBatchRequest
	var batchID, previousBatchID, inputTokens, outputTokens sql.NullInt64
	var indeterminate int
	var createdAt, updatedAt string
	if err := scanner.Scan(
		&request.ID, &request.ContentID, &batchID, &request.RequestKey, &request.FileType,
		&request.PageStart, &request.PageEnd, &request.State, &request.AttemptCount,
		&request.ReplacementCount, &previousBatchID, &request.SplitDepth, &inputTokens,
		&outputTokens, &request.KnownCost, &indeterminate, &request.LastError,
		&createdAt, &updatedAt, &request.Path, &request.Checksum, &request.PageCount,
	); err != nil {
		return GeminiBatchRequest{}, err
	}
	if batchID.Valid {
		request.BatchID = &batchID.Int64
	}
	if previousBatchID.Valid {
		request.PreviousBatchID = &previousBatchID.Int64
	}
	if inputTokens.Valid {
		request.InputTokens = &inputTokens.Int64
	}
	if outputTokens.Valid {
		request.OutputTokens = &outputTokens.Int64
	}
	request.CostIndeterminate = indeterminate == 1
	var err error
	request.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return GeminiBatchRequest{}, fmt.Errorf("parse Gemini request created_at: %w", err)
	}
	request.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return GeminiBatchRequest{}, fmt.Errorf("parse Gemini request updated_at: %w", err)
	}
	return request, nil
}

func (db *DB) StageGeminiRequest(
	requestID int64,
	pages []GeminiStagedPage,
	inputTokens, outputTokens *int64,
	knownCost int64,
	indeterminate bool,
	now time.Time,
) error {
	return db.stageGeminiRequest(
		requestID, GeminiRequestAssigned, pages, inputTokens, outputTokens,
		knownCost, indeterminate, now,
	)
}

func (db *DB) StageGeminiDirectRequest(
	requestID int64,
	pages []GeminiStagedPage,
	knownCost int64,
	indeterminate bool,
	now time.Time,
) error {
	return db.stageGeminiRequest(
		requestID, GeminiRequestBlocked, pages, nil, nil,
		knownCost, indeterminate, now,
	)
}

func (db *DB) stageGeminiRequest(
	requestID int64,
	expectedState string,
	pages []GeminiStagedPage,
	inputTokens, outputTokens *int64,
	knownCost int64,
	indeterminate bool,
	now time.Time,
) (err error) {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer rollbackOnError(tx, &err)

	var contentID int64
	var start, end int
	var state string
	if err = tx.QueryRow(
		`SELECT content_id, page_start, page_end, state
		 FROM gemini_batch_requests WHERE id = ?`,
		requestID,
	).Scan(&contentID, &start, &end, &state); err != nil {
		return err
	}
	if state != expectedState {
		return fmt.Errorf("Gemini request %d is not %s", requestID, expectedState)
	}
	if len(pages) != end-start {
		return fmt.Errorf("Gemini request %d returned %d pages; expected %d", requestID, len(pages), end-start)
	}
	seen := make(map[int]bool, len(pages))
	for _, page := range pages {
		if page.PageIndex < start || page.PageIndex >= end || seen[page.PageIndex] {
			return fmt.Errorf("Gemini request %d returned invalid absolute page index %d", requestID, page.PageIndex)
		}
		seen[page.PageIndex] = true
		if _, err = tx.Exec(
			`INSERT INTO gemini_batch_pages (request_id, content_id, page_index, markdown)
			 VALUES (?, ?, ?, ?)
			 ON CONFLICT(request_id, page_index) DO UPDATE SET markdown = excluded.markdown`,
			requestID, contentID, page.PageIndex, page.Markdown,
		); err != nil {
			return err
		}
	}
	indeterminateValue := 0
	if indeterminate {
		indeterminateValue = 1
	}
	if _, err = tx.Exec(
		`UPDATE gemini_batch_requests
		 SET batch_id = NULL, state = 'staged', input_tokens = ?, output_tokens = ?,
		     known_cost = ?, cost_indeterminate = ?, last_error = '', updated_at = ?
		 WHERE id = ?`,
		inputTokens, outputTokens, knownCost, indeterminateValue,
		now.UTC().Format(time.RFC3339Nano), requestID,
	); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) BlockGeminiRequest(requestID int64, message string, now time.Time) error {
	return db.detachGeminiRequest(requestID, GeminiRequestBlocked, message, "", now)
}

func (db *DB) BlockUnownedGeminiRequest(requestID int64, message string, now time.Time) error {
	res, err := db.Exec(
		`UPDATE gemini_batch_requests
		 SET state = 'blocked', last_error = ?, updated_at = ?
		 WHERE id = ? AND state = 'retryable' AND batch_id IS NULL`,
		truncateGeminiError(message), now.UTC().Format(time.RFC3339Nano), requestID,
	)
	if err != nil {
		return err
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("Gemini request %d is not retryable", requestID)
	}
	return nil
}

func (db *DB) RetryGeminiRequest(requestID int64, message string, now time.Time) (bool, error) {
	var attempts int
	if err := db.QueryRow(
		`SELECT attempt_count FROM gemini_batch_requests WHERE id = ?`, requestID,
	).Scan(&attempts); err != nil {
		return false, err
	}
	if attempts >= 1 {
		return false, db.BlockGeminiRequest(requestID, message, now)
	}
	return true, db.detachGeminiRequest(
		requestID,
		GeminiRequestRetryable,
		message,
		`attempt_count = attempt_count + 1,`,
		now,
	)
}

func (db *DB) detachGeminiRequest(
	requestID int64,
	state, message, extraAssignment string,
	now time.Time,
) error {
	res, err := db.Exec(
		`UPDATE gemini_batch_requests
		 SET previous_batch_id = batch_id, batch_id = NULL, state = ?, `+extraAssignment+`
		     last_error = ?, updated_at = ?
		 WHERE id = ? AND state = 'assigned'`,
		state, truncateGeminiError(message), now.UTC().Format(time.RFC3339Nano), requestID,
	)
	if err != nil {
		return err
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("Gemini request %d is not assigned", requestID)
	}
	return nil
}

func (db *DB) SplitGeminiRequest(
	requestID int64,
	leftKey, rightKey, message string,
	now time.Time,
) (requestIDs []int64, err error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer rollbackOnError(tx, &err)

	var request GeminiRequestPlan
	var batchID, previousBatchID sql.NullInt64
	var state string
	if err = tx.QueryRow(
		`SELECT content_id, file_type, page_start, page_end, attempt_count,
		        replacement_count, batch_id, previous_batch_id, split_depth, state
		 FROM gemini_batch_requests WHERE id = ?`,
		requestID,
	).Scan(
		&request.ContentID, &request.FileType, &request.PageStart, &request.PageEnd,
		&request.AttemptCount, &request.ReplacementCount, &batchID, &previousBatchID,
		&request.SplitDepth, &state,
	); err != nil {
		return nil, err
	}
	if (state != GeminiRequestAssigned && state != GeminiRequestRetryable) || request.PageEnd-request.PageStart < 2 {
		return nil, fmt.Errorf("Gemini request %d cannot be split", requestID)
	}
	if _, err = tx.Exec(`DELETE FROM gemini_batch_requests WHERE id = ?`, requestID); err != nil {
		return nil, err
	}

	lineage := previousBatchID
	if batchID.Valid {
		lineage = batchID
	}
	mid := request.PageStart + (request.PageEnd-request.PageStart)/2
	stamp := now.UTC().Format(time.RFC3339Nano)
	for _, child := range []struct {
		key        string
		start, end int
	}{
		{leftKey, request.PageStart, mid},
		{rightKey, mid, request.PageEnd},
	} {
		res, execErr := tx.Exec(
			`INSERT INTO gemini_batch_requests (
			   content_id, request_key, file_type, page_start, page_end, state,
			   attempt_count, replacement_count, previous_batch_id, split_depth, last_error,
			   created_at, updated_at
			 ) VALUES (?, ?, ?, ?, ?, 'retryable', ?, ?, ?, ?, ?, ?, ?)`,
			request.ContentID, child.key, request.FileType, child.start, child.end,
			request.AttemptCount, request.ReplacementCount, lineage, request.SplitDepth+1,
			truncateGeminiError(message), stamp, stamp,
		)
		if execErr != nil {
			return nil, execErr
		}
		id, execErr := res.LastInsertId()
		if execErr != nil {
			return nil, execErr
		}
		requestIDs = append(requestIDs, id)
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return requestIDs, nil
}

func (db *DB) DetachGeminiBatchForReplacement(
	batchID int64,
	message string,
	now time.Time,
) (retryable []int64, err error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer rollbackOnError(tx, &err)

	rows, err := tx.Query(
		`SELECT id, replacement_count
		 FROM gemini_batch_requests
		 WHERE batch_id = ? AND state = 'assigned'
		 ORDER BY id`,
		batchID,
	)
	if err != nil {
		return nil, err
	}
	var requests []struct {
		id, replacements int64
	}
	for rows.Next() {
		var request struct {
			id, replacements int64
		}
		if err = rows.Scan(&request.id, &request.replacements); err != nil {
			_ = rows.Close()
			return nil, err
		}
		requests = append(requests, request)
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}

	stamp := now.UTC().Format(time.RFC3339Nano)
	for _, request := range requests {
		state := GeminiRequestBlocked
		replacementIncrement := 0
		if request.replacements < 1 {
			state = GeminiRequestRetryable
			replacementIncrement = 1
			retryable = append(retryable, request.id)
		}
		if _, err = tx.Exec(
			`UPDATE gemini_batch_requests
			 SET previous_batch_id = batch_id, batch_id = NULL, state = ?,
			     replacement_count = replacement_count + ?,
			     last_error = ?, updated_at = ?
			 WHERE id = ?`,
			state, replacementIncrement, truncateGeminiError(message), stamp, request.id,
		); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return retryable, nil
}

func (db *DB) DetachGeminiBatchRequestsForRegeneration(batchID int64, message string, now time.Time) error {
	_, err := db.Exec(
		`UPDATE gemini_batch_requests
		 SET previous_batch_id = batch_id, batch_id = NULL, state = 'retryable', last_error = ?, updated_at = ?
		 WHERE batch_id = ? AND state = 'assigned'`,
		truncateGeminiError(message), now.UTC().Format(time.RFC3339Nano), batchID,
	)
	return err
}

func (db *DB) BlockGeminiBatchRequests(batchID int64, message string, now time.Time) error {
	_, err := db.Exec(
		`UPDATE gemini_batch_requests
		 SET previous_batch_id = batch_id, batch_id = NULL, state = 'blocked', last_error = ?, updated_at = ?
		 WHERE batch_id = ? AND state = 'assigned'`,
		truncateGeminiError(message), now.UTC().Format(time.RFC3339Nano), batchID,
	)
	return err
}

func (db *DB) PromoteReadyGeminiContents() (promoted int, err error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer rollbackOnError(tx, &err)

	rows, err := tx.Query(
		`SELECT c.id, c.page_count
		 FROM contents c
		 WHERE c.ocr_pending = 1
		   AND EXISTS (
		     SELECT 1 FROM gemini_batch_requests r
		     WHERE r.content_id = c.id AND r.state = 'staged'
		   )
		 ORDER BY c.id`,
	)
	if err != nil {
		return 0, err
	}
	var candidates []struct {
		id    int64
		pages int
	}
	for rows.Next() {
		var candidate struct {
			id    int64
			pages int
		}
		if err = rows.Scan(&candidate.id, &candidate.pages); err != nil {
			_ = rows.Close()
			return 0, err
		}
		candidates = append(candidates, candidate)
	}
	if err = rows.Close(); err != nil {
		return 0, err
	}

	for _, candidate := range candidates {
		var count int
		var minimum, maximum sql.NullInt64
		if err = tx.QueryRow(
			`SELECT COUNT(*), MIN(page_index), MAX(page_index)
			 FROM gemini_batch_pages WHERE content_id = ?`,
			candidate.id,
		).Scan(&count, &minimum, &maximum); err != nil {
			return 0, err
		}
		if count != candidate.pages || !minimum.Valid || !maximum.Valid ||
			minimum.Int64 != 0 || maximum.Int64 != int64(candidate.pages-1) {
			continue
		}

		pageRows, queryErr := tx.Query(
			`SELECT page_index, markdown FROM gemini_batch_pages
			 WHERE content_id = ? ORDER BY page_index`,
			candidate.id,
		)
		if queryErr != nil {
			return 0, queryErr
		}
		var pages []PageInput
		for pageRows.Next() {
			var page PageInput
			if err = pageRows.Scan(&page.PageIndex, &page.Markdown); err != nil {
				_ = pageRows.Close()
				return 0, err
			}
			pages = append(pages, page)
		}
		if err = pageRows.Err(); err != nil {
			_ = pageRows.Close()
			return 0, err
		}
		if err = pageRows.Close(); err != nil {
			return 0, err
		}
		if len(pages) != candidate.pages {
			return 0, fmt.Errorf("content %d staged page coverage changed during promotion", candidate.id)
		}
		if err = replaceContentPagesTx(tx, candidate.id, pages); err != nil {
			return 0, err
		}
		if _, err = tx.Exec(`DELETE FROM gemini_batch_requests WHERE content_id = ?`, candidate.id); err != nil {
			return 0, err
		}
		promoted++
	}

	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return promoted, nil
}

func (db *DB) ReplaceContentPagesDirect(contentID int64, pages []PageInput) (err error) {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer rollbackOnError(tx, &err)

	var owned int
	if err = tx.QueryRow(
		`SELECT COUNT(*) FROM gemini_batch_requests
		 WHERE content_id = ? AND batch_id IS NOT NULL`,
		contentID,
	).Scan(&owned); err != nil {
		return err
	}
	if owned != 0 {
		return fmt.Errorf("content %d is owned by Gemini batch OCR", contentID)
	}
	if err = replaceContentPagesTx(tx, contentID, pages); err != nil {
		return err
	}
	// Successful whole-file direct OCR supersedes partial, unowned batch staging.
	if _, err = tx.Exec(`DELETE FROM gemini_batch_requests WHERE content_id = ?`, contentID); err != nil {
		return err
	}
	return tx.Commit()
}

func replaceContentPagesTx(tx *sql.Tx, contentID int64, pages []PageInput) error {
	for _, page := range pages {
		if _, err := tx.Exec(
			`INSERT INTO pages (content_id, page_index, markdown, search_text)
			 VALUES (?, ?, ?, ?)
			 ON CONFLICT(content_id, page_index) DO UPDATE SET
			   markdown = excluded.markdown,
			   search_text = excluded.search_text`,
			contentID, page.PageIndex, page.Markdown, normalizeSearchText(page.Markdown),
		); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(
		`DELETE FROM pages WHERE content_id = ? AND page_index >= ?`,
		contentID, len(pages),
	); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE contents SET ocr_pending = 0 WHERE id = ?`, contentID)
	return err
}

func (db *DB) ForgetGeminiBatch(batchID int64, now time.Time) (batch *GeminiBatch, err error) {
	batch, err = db.GetGeminiBatch(batchID)
	if err != nil || batch == nil {
		return batch, err
	}

	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer rollbackOnError(tx, &err)
	stamp := now.UTC().Format(time.RFC3339Nano)
	if _, err = tx.Exec(
		`UPDATE gemini_batch_requests
		 SET previous_batch_id = batch_id, batch_id = NULL, state = 'blocked',
		     last_error = 'batch forgotten locally', updated_at = ?
		 WHERE batch_id = ? AND state = 'assigned'`,
		stamp, batchID,
	); err != nil {
		return nil, err
	}
	if _, err = tx.Exec(`DELETE FROM gemini_batches WHERE id = ?`, batchID); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return batch, nil
}

func (db *DB) FinalizeGeminiBatch(batchID int64, now time.Time) (cleanup []GeminiCleanup, err error) {
	batch, err := db.GetGeminiBatch(batchID)
	if err != nil {
		return nil, err
	}
	if batch == nil {
		return nil, nil
	}

	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer rollbackOnError(tx, &err)
	var assigned int
	if err = tx.QueryRow(
		`SELECT COUNT(*) FROM gemini_batch_requests WHERE batch_id = ?`, batchID,
	).Scan(&assigned); err != nil {
		return nil, err
	}
	if assigned != 0 {
		return nil, fmt.Errorf("Gemini batch %d still owns %d request(s)", batchID, assigned)
	}

	resources := []struct {
		kind, name string
	}{
		{"batch", batch.RemoteName},
		{"file", batch.OutputFileName},
		{"file", batch.InputFileName},
	}
	stamp := now.UTC().Format(time.RFC3339Nano)
	for _, resource := range resources {
		if resource.name == "" {
			continue
		}
		if _, err = tx.Exec(
			`INSERT INTO gemini_batch_cleanup (
			   resource_kind, resource_name, created_at, updated_at
			 ) VALUES (?, ?, ?, ?)
			 ON CONFLICT(resource_kind, resource_name) DO NOTHING`,
			resource.kind, resource.name, stamp, stamp,
		); err != nil {
			return nil, err
		}
	}
	if _, err = tx.Exec(`DELETE FROM gemini_batches WHERE id = ?`, batchID); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return db.ListGeminiCleanup()
}

func (db *DB) ListGeminiCleanup() ([]GeminiCleanup, error) {
	rows, err := db.Query(
		`SELECT id, resource_kind, resource_name, last_error, created_at, updated_at
		 FROM gemini_batch_cleanup ORDER BY id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cleanup []GeminiCleanup
	for rows.Next() {
		var item GeminiCleanup
		var createdAt, updatedAt string
		if err := rows.Scan(
			&item.ID, &item.ResourceKind, &item.ResourceName, &item.LastError,
			&createdAt, &updatedAt,
		); err != nil {
			return nil, err
		}
		item.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			return nil, err
		}
		item.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
		if err != nil {
			return nil, err
		}
		cleanup = append(cleanup, item)
	}
	return cleanup, rows.Err()
}

func (db *DB) DeleteGeminiCleanup(cleanupID int64) error {
	_, err := db.Exec(`DELETE FROM gemini_batch_cleanup WHERE id = ?`, cleanupID)
	return err
}

func (db *DB) SetGeminiCleanupError(cleanupID int64, message string, now time.Time) error {
	_, err := db.Exec(
		`UPDATE gemini_batch_cleanup
		 SET last_error = ?, updated_at = ? WHERE id = ?`,
		truncateGeminiError(message), now.UTC().Format(time.RFC3339Nano), cleanupID,
	)
	return err
}

func truncateGeminiError(message string) string {
	message = strings.TrimSpace(message)
	if len(message) <= maxGeminiBatchErrorBytes {
		return message
	}
	return message[:maxGeminiBatchErrorBytes]
}

func rollbackOnError(tx *sql.Tx, err *error) {
	if *err != nil {
		_ = tx.Rollback()
	}
}

func sortGeminiRequestsByRange(requests []GeminiBatchRequest) {
	sort.Slice(requests, func(i, j int) bool {
		if requests[i].ContentID != requests[j].ContentID {
			return requests[i].ContentID < requests[j].ContentID
		}
		if requests[i].PageStart != requests[j].PageStart {
			return requests[i].PageStart < requests[j].PageStart
		}
		return requests[i].ID < requests[j].ID
	})
}
