package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/maxim/ringbinder/internal/checksum"
	"github.com/maxim/ringbinder/internal/db"
	"github.com/maxim/ringbinder/internal/ocr"
)

func TestOCRCoordinatorLockHolderProcess(t *testing.T) {
	path := os.Getenv("RINGBINDER_TEST_LOCK_PATH")
	if path == "" {
		return
	}
	lock, err := acquireOCRCoordinator(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "holder lock: %v\n", err)
		os.Exit(2)
	}
	defer lock.Close()
	fmt.Println("ready")
	select {}
}

func TestOCRCoordinatorLockReleasesAfterProcessExit(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "process-lock.db")
	process := exec.Command(os.Args[0], "-test.run=^TestOCRCoordinatorLockHolderProcess$")
	process.Env = append(os.Environ(), "RINGBINDER_TEST_LOCK_PATH="+dbPath)
	stdout, err := process.StdoutPipe()
	if err != nil {
		t.Fatalf("StdoutPipe() error = %v", err)
	}
	process.Stderr = os.Stderr
	if err := process.Start(); err != nil {
		t.Fatalf("process.Start() error = %v", err)
	}
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "ready\n" {
		_ = process.Process.Kill()
		t.Fatalf("holder readiness = %q, %v", line, err)
	}
	start := time.Now()
	lock, err := acquireOCRCoordinator(dbPath)
	if lock != nil {
		_ = lock.Close()
	}
	if err == nil || err.Error() != ocrCoordinatorBusyMessage {
		_ = process.Process.Kill()
		t.Fatalf("contended lock error = %v", err)
	}
	if time.Since(start) > time.Second {
		_ = process.Process.Kill()
		t.Fatalf("contended lock did not fail fast")
	}
	if err := process.Process.Kill(); err != nil {
		t.Fatalf("kill holder: %v", err)
	}
	_ = process.Wait()
	lock, err = acquireOCRCoordinator(dbPath)
	if err != nil {
		t.Fatalf("lock after holder exit error = %v", err)
	}
	_ = lock.Close()
}

func TestOCRCoordinatorCanonicalizesDatabaseSymlinks(t *testing.T) {
	realPath := filepath.Join(t.TempDir(), "real.db")
	if err := os.WriteFile(realPath, nil, 0o600); err != nil {
		t.Fatalf("WriteFile(real DB) error = %v", err)
	}
	aliasPath := filepath.Join(t.TempDir(), "alias.db")
	if err := os.Symlink(realPath, aliasPath); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	lock, err := acquireOCRCoordinator(realPath)
	if err != nil {
		t.Fatalf("real lock error = %v", err)
	}
	defer lock.Close()
	aliasLock, err := acquireOCRCoordinator(aliasPath)
	if aliasLock != nil {
		_ = aliasLock.Close()
	}
	if err == nil || err.Error() != ocrCoordinatorBusyMessage {
		t.Fatalf("alias lock error = %v, want contention", err)
	}
}

func TestOCRCoordinatorCanonicalizesDanglingDatabaseSymlink(t *testing.T) {
	realPath := filepath.Join(t.TempDir(), "future-real.db")
	aliasPath := filepath.Join(t.TempDir(), "future-alias.db")
	if err := os.Symlink(realPath, aliasPath); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}
	aliasLock, err := acquireOCRCoordinator(aliasPath)
	if err != nil {
		t.Fatalf("alias lock error = %v", err)
	}
	defer aliasLock.Close()
	realLock, err := acquireOCRCoordinator(realPath)
	if realLock != nil {
		_ = realLock.Close()
	}
	if err == nil || err.Error() != ocrCoordinatorBusyMessage {
		t.Fatalf("real lock error = %v, want contention", err)
	}
}

func TestOCRCoordinatorLockFailsFastAndReleases(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "lock.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	defer database.Close()
	first, err := acquireOCRCoordinator(dbPath)
	if err != nil {
		t.Fatalf("first acquireOCRCoordinator() error = %v", err)
	}
	second, err := acquireOCRCoordinator(dbPath)
	if second != nil {
		_ = second.Close()
		t.Fatal("second lock unexpectedly succeeded")
	}
	if err == nil || err.Error() != ocrCoordinatorBusyMessage {
		t.Fatalf("second lock error = %v, want %q", err, ocrCoordinatorBusyMessage)
	}
	// flock is independent from SQLite work on the already-open command database.
	if _, err := database.PendingContents(); err != nil {
		t.Fatalf("SQLite read while coordinator held error = %v", err)
	}
	if _, err := database.InsertContent("lock-write", 1); err != nil {
		t.Fatalf("SQLite write while coordinator held error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first.Close() error = %v", err)
	}
	third, err := acquireOCRCoordinator(dbPath)
	if err != nil {
		t.Fatalf("lock after release error = %v", err)
	}
	_ = third.Close()
}

func TestDirectCostReportsBatchOwnedExclusion(t *testing.T) {
	resetCommandState(t)
	dbPath := filepath.Join(t.TempDir(), "direct-cost.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	contentID := addCostContent(t, database, "owned-estimate", 1, true, "/docs/owned-estimate.png")
	if _, err := database.CreateGeminiBatch(
		"estimate-batch", ocr.GeminiBatchModel, 375, 1875, nil,
		[]db.GeminiRequestPlan{{ContentID: contentID, RequestKey: "estimate-key", FileType: "png", PageStart: 0, PageEnd: 1}},
		time.Now().UTC(),
	); err != nil {
		t.Fatalf("CreateGeminiBatch() error = %v", err)
	}
	_ = database.Close()
	cmd := commandWithDatabaseFlag(t, dbPath)
	cmd.Flags().StringArray("model", nil, "")
	cmd.Flags().Int("limit", 0, "")
	if err := cmd.Flags().Set("model", modelMistral); err != nil {
		t.Fatalf("set OCR model: %v", err)
	}
	output := captureStdout(t, func() {
		if err := runCost(cmd, nil); err != nil {
			t.Fatalf("runCost() error = %v", err)
		}
	})
	if output != "OCR models: mistral\nExcluded from estimate: 1 content item(s) already managed by batch OCR\nNo documents pending OCR.\n" {
		t.Fatalf("output = %q", output)
	}
}

func TestBatchForgetIsLocalOnlyWithoutAPIKey(t *testing.T) {
	resetCommandState(t)
	t.Setenv("GEMINI_API_KEY", "")
	dbPath := filepath.Join(t.TempDir(), "forget-command.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	contentID := addCostContent(t, database, "forget-command", 1, true, "/docs/forget-command.png")
	batchID, err := database.CreateGeminiBatch(
		"forget-command", ocr.GeminiBatchModel, 375, 1875, nil,
		[]db.GeminiRequestPlan{{ContentID: contentID, RequestKey: "forget-command-key", FileType: "png", PageStart: 0, PageEnd: 1}},
		time.Now().UTC(),
	)
	if err != nil {
		t.Fatalf("CreateGeminiBatch() error = %v", err)
	}
	_ = database.Close()
	cmd := commandWithDatabaseFlag(t, dbPath)
	cmd.Flags().String("model", "", "")
	if err := cmd.Flags().Set("model", modelGemini); err != nil {
		t.Fatalf("set model: %v", err)
	}
	if err := runBatchForget(cmd, []string{strconv.FormatInt(batchID, 10)}); err != nil {
		t.Fatalf("runBatchForget() error = %v", err)
	}
}

func TestRollbackFreshBatchContentRemovesEverySealedSegment(t *testing.T) {
	prior, err := newFreshBatchGroup()
	if err != nil {
		t.Fatal(err)
	}
	defer prior.Close()
	current, err := newFreshBatchGroup()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := current.file.WriteString("prior-content\n"); err != nil {
		t.Fatal(err)
	}
	current.size = int64(len("prior-content\n"))
	current.plans = append(current.plans, db.GeminiRequestPlan{RequestKey: "prior"})
	startSize, startPlans := current.size, len(current.plans)

	if _, err := current.file.WriteString("first-segment\n"); err != nil {
		t.Fatal(err)
	}
	current.size += int64(len("first-segment\n"))
	current.plans = append(current.plans, db.GeminiRequestPlan{RequestKey: "first"})
	groups := []*freshBatchGroup{prior, current}

	middle, err := newFreshBatchGroup()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := middle.file.WriteString("middle-segment\n"); err != nil {
		t.Fatal(err)
	}
	middle.size = int64(len("middle-segment\n"))
	middle.plans = append(middle.plans, db.GeminiRequestPlan{RequestKey: "middle"})
	groups = append(groups, middle)

	active, err := newFreshBatchGroup()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := active.file.WriteString("tail-segment\n"); err != nil {
		t.Fatal(err)
	}
	active.size = int64(len("tail-segment\n"))
	active.plans = append(active.plans, db.GeminiRequestPlan{RequestKey: "tail"})

	if err := rollbackFreshBatchContent(&active, &groups, 1, startSize, startPlans); err != nil {
		t.Fatalf("rollbackFreshBatchContent() error = %v", err)
	}
	defer active.Close()
	if len(groups) != 1 || groups[0] != prior {
		t.Fatalf("groups = %+v, want only prior sealed group", groups)
	}
	if active != current || active.size != startSize || len(active.plans) != startPlans || active.plans[0].RequestKey != "prior" {
		t.Fatalf("restored group = %+v, want original pre-content state", active)
	}
	if middle.file != nil {
		t.Fatal("middle content group remained open")
	}
	buffer := make([]byte, startSize)
	if _, err := active.file.ReadAt(buffer, 0); err != nil {
		t.Fatal(err)
	}
	if string(buffer) != "prior-content\n" {
		t.Fatalf("restored bytes = %q, want prior content only", buffer)
	}
}

func TestBatchStartCancellationDoesNotPersistPreparedWork(t *testing.T) {
	resetCommandState(t)
	t.Setenv("GEMINI_API_KEY", "test-key")
	path := filepath.Join(t.TempDir(), "cancel.png")
	if err := os.WriteFile(path, []byte("image"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest, err := checksum.SHA256File(path)
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "cancel.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	contentID, err := database.InsertContent(digest, 1)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := database.InsertDocument(path, contentID, now, now); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	oldFactory := newGeminiBatchAPI
	newGeminiBatchAPI = func(string) geminiBatchAPI { return &fakeGeminiBatchAPI{} }
	t.Cleanup(func() { newGeminiBatchAPI = oldFactory })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd := commandWithDatabaseFlag(t, dbPath)
	cmd.SetContext(ctx)
	cmd.Flags().String("model", "", "")
	cmd.Flags().Int("limit", 0, "")
	if err := cmd.Flags().Set("model", modelGemini); err != nil {
		t.Fatal(err)
	}
	if err := runBatchStart(cmd, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("runBatchStart() error = %v, want context cancellation", err)
	}

	database, err = db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var batches, requests int
	if err := database.QueryRow(`SELECT COUNT(*) FROM gemini_batches`).Scan(&batches); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT COUNT(*) FROM gemini_batch_requests`).Scan(&requests); err != nil {
		t.Fatal(err)
	}
	if batches != 0 || requests != 0 {
		t.Fatalf("batches = %d, requests = %d; want no persisted canceled work", batches, requests)
	}
}

func TestBatchStartReportsExistingBlockedWorkWhenUntouchedFilesCannotBePrepared(t *testing.T) {
	resetCommandState(t)
	t.Setenv("GEMINI_API_KEY", "test-key")
	dbPath := filepath.Join(t.TempDir(), "start-blocked.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	blockedID := addCostContent(t, database, "blocked", 1, true, "/docs/blocked.png")
	if _, err := database.CreateBlockedGeminiRequest(
		db.GeminiRequestPlan{
			ContentID: blockedID, RequestKey: "blocked-key",
			FileType: "png", PageStart: 0, PageEnd: 1,
		},
		"failed",
		time.Now().UTC(),
	); err != nil {
		t.Fatal(err)
	}
	addCostContent(t, database, "missing", 1, true, "/docs/missing.png")
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	cmd := commandWithDatabaseFlag(t, dbPath)
	cmd.Flags().String("model", "", "")
	cmd.Flags().Int("limit", 0, "")
	if err := cmd.Flags().Set("model", modelGemini); err != nil {
		t.Fatal(err)
	}
	var runErr error
	var output string
	_ = captureStderr(t, func() {
		output = captureStdout(t, func() { runErr = runBatchStart(cmd, nil) })
	})
	if runErr != nil {
		t.Fatalf("runBatchStart() error = %v", runErr)
	}
	for _, want := range []string{
		"No valid pending page ranges could be prepared for Gemini batch OCR.",
		"1 blocked Gemini batch OCR page range across 1 content item requires attention.",
		"ringbinder batch failures",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("output = %q, want %q", output, want)
		}
	}
}

func TestBatchFailuresIsLocalOnlyNDJSON(t *testing.T) {
	resetCommandState(t)
	t.Setenv("GEMINI_API_KEY", "")
	dbPath := filepath.Join(t.TempDir(), "failures.db")
	database, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("db.Open() error = %v", err)
	}
	contentID := addCostContent(t, database, "failure", 2, true, "/docs/failure.pdf")
	requestID, err := database.CreateBlockedGeminiRequest(
		db.GeminiRequestPlan{ContentID: contentID, RequestKey: "failure-key", FileType: "pdf", PageStart: 0, PageEnd: 2},
		"invalid Gemini finish reason: RECITATION", time.Now().UTC(),
	)
	if err != nil {
		t.Fatalf("CreateBlockedGeminiRequest() error = %v", err)
	}
	if _, err := database.Exec(
		`UPDATE gemini_batch_requests SET attempt_count = 1 WHERE id = ?`, requestID,
	); err != nil {
		t.Fatalf("set automatic retry count: %v", err)
	}
	_ = database.Close()

	cmd := commandWithDatabaseFlag(t, dbPath)
	cmd.Flags().String("model", "", "")
	cmd.Flags().Bool("json", false, "")
	if err := cmd.Flags().Set("model", modelGemini); err != nil {
		t.Fatalf("set model: %v", err)
	}
	if err := cmd.Flags().Set("json", "true"); err != nil {
		t.Fatalf("set json: %v", err)
	}
	output := captureStdout(t, func() {
		if err := runBatchFailures(cmd, nil); err != nil {
			t.Fatalf("runBatchFailures() error = %v", err)
		}
	})
	if !strings.Contains(output, `"request_id":`+strconv.FormatInt(requestID, 10)) ||
		!strings.Contains(output, `"paths":["/docs/failure.pdf"]`) ||
		!strings.Contains(output, `"page_start":1`) || !strings.Contains(output, `"page_end":2`) ||
		!strings.Contains(output, `"attempt_count":1`) ||
		!strings.Contains(output, `"error":"Gemini stopped generation for potential recitation (RECITATION)"`) {
		t.Fatalf("NDJSON output = %q", output)
	}

	database, err = db.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(`UPDATE documents SET deleted = 1 WHERE content_id = ?`, contentID); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	output = captureStdout(t, func() {
		if err := runBatchFailures(cmd, nil); err != nil {
			t.Fatalf("runBatchFailures() error = %v", err)
		}
	})
	if !strings.Contains(output, `"paths":[]`) {
		t.Fatalf("NDJSON output = %q, want empty paths array", output)
	}

	if err := cmd.Flags().Set("json", "false"); err != nil {
		t.Fatalf("unset json: %v", err)
	}
	output = captureStdout(t, func() {
		if err := runBatchFailures(cmd, nil); err != nil {
			t.Fatalf("runBatchFailures() error = %v", err)
		}
	})
	for _, want := range []string{
		"automatic retries 1",
		"ringbinder batch retry <request-id> --mode direct",
		"ringbinder ocr --model mistral",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("human output = %q, want %q", output, want)
		}
	}
	if strings.Contains(output, "batch discard") {
		t.Fatalf("human output still recommends removed discard command: %q", output)
	}
}

func TestEstimateGeminiBatchCostUsesSparseUnownedMissingRanges(t *testing.T) {
	database := openCostTestDB(t)
	contentID := addCostContent(t, database, "sparse-batch-cost", 5, true, "/docs/sparse.pdf")
	model := "existing"
	if err := database.UpsertContentPages(contentID, []db.PageInput{
		{PageIndex: 0, Markdown: "done", Model: &model},
		{PageIndex: 2, Markdown: "done", Model: &model},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.CreateGeminiBatch(
		"sparse-owned", ocr.GeminiBatchModel, 375, 1875, nil,
		[]db.GeminiRequestPlan{{
			ContentID: contentID, RequestKey: "owned-page-4", FileType: "pdf",
			PageStart: 3, PageEnd: 4,
		}},
		time.Now().UTC(),
	); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	estimate, err := estimateGeminiBatchCost(database, 0, at)
	if err != nil {
		t.Fatal(err)
	}
	// Only page indexes 1 and 4 are missing and unowned; each is its own range.
	want := ocr.GeminiBatchCost(
		at,
		2*geminiPDFMediaTokens+2*geminiRequestOverhead,
		2*geminiOutputTokens,
	)
	if estimate.items != 1 || estimate.pages != 2 || estimate.cost != want {
		t.Fatalf("estimate = %+v, want 1 item, 2 pages, cost %d", estimate, want)
	}
}

func TestEstimateGeminiBatchCostUsesDiscountAndUntouchedSelection(t *testing.T) {
	database := openCostTestDB(t)
	ownedID := addCostContent(t, database, "owned-cost", 1, true, "/docs/owned.png")
	addCostContent(t, database, "untouched-cost", 1, true, "/docs/untouched.png")
	if _, err := database.CreateGeminiBatch(
		"cost-batch", ocr.GeminiBatchModel, 375, 1875, nil,
		[]db.GeminiRequestPlan{{ContentID: ownedID, RequestKey: "cost-key", FileType: "png", PageStart: 0, PageEnd: 1}},
		time.Now().UTC(),
	); err != nil {
		t.Fatalf("CreateGeminiBatch() error = %v", err)
	}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	estimate, err := estimateGeminiBatchCost(database, 0, at)
	if err != nil {
		t.Fatalf("estimateGeminiBatchCost() error = %v", err)
	}
	direct := ocr.GeminiCost(at, geminiImageMediaTokens+geminiRequestOverhead, geminiOutputTokens)
	if estimate.items != 1 || estimate.pages != 1 || estimate.cost != direct/2 {
		t.Fatalf("estimate = %+v, want one untouched item at half direct cost %d", estimate, direct/2)
	}
}
