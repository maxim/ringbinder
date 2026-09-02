package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/maxim/ringbinder/internal/db"
	"github.com/maxim/ringbinder/internal/ocr"
	"github.com/spf13/cobra"
)

type partialErrorReadSeeker struct {
	data  []byte
	err   error
	read  bool
	seeks int
}

type panicUploadAPI struct {
	*fakeGeminiBatchAPI
}

type httpUploadAPI struct {
	*fakeGeminiBatchAPI
	endpoint string
}

func (api *httpUploadAPI) UploadJSONL(
	ctx context.Context,
	_ string,
	source io.ReadSeeker,
	size int64,
) (ocr.GeminiRemoteFile, error) {
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return ocr.GeminiRemoteFile{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, api.endpoint, source)
	if err != nil {
		return ocr.GeminiRemoteFile{}, err
	}
	request.ContentLength = size
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return ocr.GeminiRemoteFile{}, err
	}
	defer response.Body.Close()
	return ocr.GeminiRemoteFile{Name: "files/input"}, nil
}

func (api *panicUploadAPI) UploadJSONL(
	context.Context,
	string,
	io.ReadSeeker,
	int64,
) (ocr.GeminiRemoteFile, error) {
	panic("upload panic")
}

func (reader *partialErrorReadSeeker) Read(buffer []byte) (int, error) {
	if reader.read {
		return 0, io.EOF
	}
	reader.read = true
	return copy(buffer, reader.data), reader.err
}

func (reader *partialErrorReadSeeker) Seek(offset int64, _ int) (int64, error) {
	reader.seeks++
	return offset, nil
}

func TestCountingReadSeekerCountsPartialErrorReadsButNotSeeks(t *testing.T) {
	t.Parallel()

	readErr := errors.New("read failed")
	source := &partialErrorReadSeeker{data: []byte("abc"), err: readErr}
	counted := 0
	reader := &countingReadSeeker{
		source: source,
		onRead: func(count int) { counted += count },
	}

	buffer := make([]byte, 8)
	count, err := reader.Read(buffer)
	if count != 3 || !errors.Is(err, readErr) || string(buffer[:count]) != "abc" {
		t.Fatalf("Read() = (%d, %v, %q), want (3, read failed, abc)", count, err, buffer[:count])
	}
	if counted != 3 {
		t.Fatalf("counted bytes = %d, want 3", counted)
	}
	position, err := reader.Seek(7, io.SeekStart)
	if err != nil || position != 7 || source.seeks != 1 {
		t.Fatalf("Seek() = (%d, %v), seeks = %d; want (7, nil), 1", position, err, source.seeks)
	}
	if counted != 3 {
		t.Fatalf("Seek changed counted bytes to %d", counted)
	}
}

func TestWrappedUploadPreservesHTTPBodyAndContentLength(t *testing.T) {
	database, batch, request := createPreparedImageTestBatch(t, "http-boundary")
	line := []byte(fmt.Sprintf(`{"key":%q}`+"\n", request.RequestKey))
	var received []byte
	var contentLength int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		contentLength = request.ContentLength
		received, _ = io.ReadAll(request.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	api := &httpUploadAPI{fakeGeminiBatchAPI: &fakeGeminiBatchAPI{}, endpoint: server.URL}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())

	var uploadErr error
	_ = captureStdout(t, func() {
		uploadErr = uploadAndSubmitGeminiBatch(
			cmd, database, api, batch.ID, bytes.NewReader(line), int64(len(line)),
		)
	})
	if uploadErr != nil {
		t.Fatalf("uploadAndSubmitGeminiBatch() error = %v", uploadErr)
	}
	if contentLength != int64(len(line)) {
		t.Fatalf("ContentLength = %d, want %d", contentLength, len(line))
	}
	if !bytes.Equal(received, line) {
		t.Fatalf("received body = %q, want %q", received, line)
	}
}

func TestResumePreparedBatchShowsUploadLifecycle(t *testing.T) {
	database, batch, _ := createPreparedImageTestBatch(t, "resume-progress")
	api := &fakeGeminiBatchAPI{}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var resumeErr error

	output := captureStdout(t, func() {
		resumeErr = resumePreparedGeminiBatch(
			cmd, database, api, ocr.NewGeminiClient("", time.Now().UTC()), batch,
		)
	})
	if resumeErr != nil {
		t.Fatalf("resumePreparedGeminiBatch() error = %v", resumeErr)
	}
	assertInOrder(t, output,
		fmt.Sprintf("Uploading Gemini batch %d:", batch.ID),
		fmt.Sprintf("Gemini batch %d upload complete:", batch.ID),
	)
}

func TestUploadStoppedOutputPreservesFailureState(t *testing.T) {
	tests := []struct {
		name      string
		uploadErr error
		wantState string
	}{
		{name: "deterministic", uploadErr: errors.New("upload rejected"), wantState: db.GeminiBatchPrepared},
		{
			name: "ambiguous",
			uploadErr: &ocr.GeminiAmbiguousOperationError{
				Operation: "file upload",
				Err:       errors.New("connection reset"),
			},
			wantState: db.GeminiBatchUploadUnknown,
		},
		{name: "canceled", uploadErr: context.Canceled, wantState: db.GeminiBatchUploadUnknown},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			database, batch, request := createPreparedImageTestBatch(t, "stopped-"+test.name)
			line := []byte(fmt.Sprintf(`{"key":%q}`+"\n", request.RequestKey))
			api := &fakeGeminiBatchAPI{uploadError: test.uploadErr, uploadReadBeforeError: 3}
			cmd := &cobra.Command{}
			cmd.SetContext(context.Background())
			var uploadErr error

			output := captureStdout(t, func() {
				uploadErr = uploadAndSubmitGeminiBatch(
					cmd, database, api, batch.ID, bytes.NewReader(line), int64(len(line)),
				)
			})
			if !errors.Is(uploadErr, test.uploadErr) {
				t.Fatalf("uploadAndSubmitGeminiBatch() error = %v, want %v", uploadErr, test.uploadErr)
			}
			assertInOrder(t, output,
				fmt.Sprintf("Uploading Gemini batch %d:", batch.ID),
				fmt.Sprintf("Gemini batch %d upload stopped at 3 B /", batch.ID),
			)
			if strings.Contains(output, test.uploadErr.Error()) {
				t.Fatalf("progress output repeats detailed error: %q", output)
			}
			stored, err := database.GetGeminiBatch(batch.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored == nil || stored.State != test.wantState {
				t.Fatalf("stored batch = %+v, want state %q", stored, test.wantState)
			}
		})
	}
}

func TestCanceledUploadDoesNotEnterUnknownState(t *testing.T) {
	database, batch, request := createPreparedImageTestBatch(t, "canceled-upload")
	line := []byte(fmt.Sprintf(`{"key":%q}`+"\n", request.RequestKey))
	api := &fakeGeminiBatchAPI{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	var uploadErr error

	output := captureStdout(t, func() {
		uploadErr = uploadAndSubmitGeminiBatch(
			cmd, database, api, batch.ID, bytes.NewReader(line), int64(len(line)),
		)
	})
	if !errors.Is(uploadErr, context.Canceled) {
		t.Fatalf("uploadAndSubmitGeminiBatch() error = %v, want context cancellation", uploadErr)
	}
	if output != "" || api.uploadCalls != 0 {
		t.Fatalf("output = %q, upload calls = %d; want no attempted upload", output, api.uploadCalls)
	}
	stored, err := database.GetGeminiBatch(batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored == nil || stored.State != db.GeminiBatchPrepared {
		t.Fatalf("stored batch = %+v, want prepared", stored)
	}
}

func TestCanceledSubmissionStaysUploaded(t *testing.T) {
	database, batch, _ := createPreparedImageTestBatch(t, "canceled-submit")
	if err := database.SetGeminiBatchUploaded(batch.ID, "files/input", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	api := &fakeGeminiBatchAPI{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)

	err := submitUploadedGeminiBatch(cmd, database, api, batch.ID)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("submitUploadedGeminiBatch() error = %v, want context cancellation", err)
	}
	if api.createCalls != 0 {
		t.Fatalf("create calls = %d, want 0", api.createCalls)
	}
	stored, err := database.GetGeminiBatch(batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored == nil || stored.State != db.GeminiBatchUploaded {
		t.Fatalf("stored batch = %+v, want uploaded", stored)
	}
}

func TestLateSuccessfulUploadResumesWithoutUploadingAgain(t *testing.T) {
	database, batch, request := createPreparedImageTestBatch(t, "late-upload")
	line := []byte(fmt.Sprintf(`{"key":%q}`+"\n", request.RequestKey))
	ctx, cancel := context.WithCancel(context.Background())
	api := &fakeGeminiBatchAPI{
		uploadFunc: func(context.Context, string, io.ReadSeeker, int64) (ocr.GeminiRemoteFile, error) {
			cancel()
			return ocr.GeminiRemoteFile{Name: "files/late-upload"}, nil
		},
	}
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)

	err := uploadAndSubmitGeminiBatch(
		cmd, database, api, batch.ID, bytes.NewReader(line), int64(len(line)),
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("uploadAndSubmitGeminiBatch() error = %v, want context cancellation", err)
	}
	stored, err := database.GetGeminiBatch(batch.ID)
	if err != nil || stored == nil || stored.State != db.GeminiBatchUploaded ||
		stored.InputFileName != "files/late-upload" {
		t.Fatalf("batch after late upload = %+v, %v; want uploaded remote file", stored, err)
	}
	if api.uploadCalls != 1 || api.createCalls != 0 {
		t.Fatalf("remote calls after cancellation = %d uploads, %d creates; want 1, 0", api.uploadCalls, api.createCalls)
	}

	resume := &cobra.Command{}
	resume.SetContext(context.Background())
	resume.SetOut(io.Discard)
	resume.SetErr(io.Discard)
	var totals batchContinueTotals
	if _, err := advanceGeminiBatch(resume, database, api, nil, *stored, &totals); err != nil {
		t.Fatalf("advanceGeminiBatch() error = %v", err)
	}
	if api.uploadCalls != 1 || api.createCalls != 1 || len(api.createInputFiles) != 1 ||
		api.createInputFiles[0] != "files/late-upload" {
		t.Fatalf(
			"resumed remote calls = %d uploads, %d creates with inputs %v; want saved upload submitted once",
			api.uploadCalls, api.createCalls, api.createInputFiles,
		)
	}
	resumed, err := database.GetGeminiBatch(batch.ID)
	if err != nil || resumed == nil || resumed.State != db.GeminiBatchPending ||
		resumed.InputFileName != "files/late-upload" || resumed.RemoteName != "batches/1" {
		t.Fatalf("batch after resumed submission = %+v, %v; want persisted upload and remote batch", resumed, err)
	}
}

func TestLateSuccessfulSubmissionResumesByPollingRemoteBatch(t *testing.T) {
	database, batch, _ := createPreparedImageTestBatch(t, "late-submission")
	if err := database.SetGeminiBatchUploaded(batch.ID, "files/late-submission", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	api := &fakeGeminiBatchAPI{
		createFunc: func(context.Context, string, string, string) (ocr.GeminiRemoteBatch, error) {
			cancel()
			return ocr.GeminiRemoteBatch{Name: "batches/late-submission", State: "JOB_STATE_PENDING"}, nil
		},
	}
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)

	if err := submitUploadedGeminiBatch(cmd, database, api, batch.ID); err != nil {
		t.Fatalf("submitUploadedGeminiBatch() error = %v", err)
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("command context error = %v, want cancellation", ctx.Err())
	}
	stored, err := database.GetGeminiBatch(batch.ID)
	if err != nil || stored == nil || stored.State != db.GeminiBatchPending ||
		stored.RemoteName != "batches/late-submission" {
		t.Fatalf("batch after late submission = %+v, %v; want persisted remote batch", stored, err)
	}

	var polledName string
	api.getFunc = func(_ context.Context, name string) (ocr.GeminiRemoteBatch, error) {
		polledName = name
		return ocr.GeminiRemoteBatch{Name: "batches/late-submission", State: "JOB_STATE_RUNNING"}, nil
	}
	resume := &cobra.Command{}
	resume.SetContext(context.Background())
	resume.SetOut(io.Discard)
	resume.SetErr(io.Discard)
	var totals batchContinueTotals
	if _, err := advanceGeminiBatch(resume, database, api, nil, *stored, &totals); err != nil {
		t.Fatalf("advanceGeminiBatch() error = %v", err)
	}
	if api.createCalls != 1 || api.getCalls != 1 || polledName != "batches/late-submission" {
		t.Fatalf(
			"resume calls = %d creates, %d polls of %q; want no resubmission and one poll of saved remote",
			api.createCalls, api.getCalls, polledName,
		)
	}
}

func TestSerialUploadsHaveSeparateProgressLifecycles(t *testing.T) {
	database, firstBatch, firstRequest := createPreparedImageTestBatch(t, "serial-one")
	secondBatch, secondRequest := addPreparedImageTestBatch(t, database, "serial-two")
	firstLine := []byte(fmt.Sprintf(`{"key":%q}`+"\n", firstRequest.RequestKey))
	secondLine := []byte(fmt.Sprintf(`{"key":%q}`+"\n", secondRequest.RequestKey))
	api := &fakeGeminiBatchAPI{}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var uploadErr error

	output := captureStdout(t, func() {
		if err := uploadAndSubmitGeminiBatch(
			cmd, database, api, firstBatch.ID, bytes.NewReader(firstLine), int64(len(firstLine)),
		); err != nil {
			uploadErr = err
			return
		}
		uploadErr = uploadAndSubmitGeminiBatch(
			cmd, database, api, secondBatch.ID, bytes.NewReader(secondLine), int64(len(secondLine)),
		)
	})
	if uploadErr != nil {
		t.Fatalf("serial upload error = %v", uploadErr)
	}
	assertInOrder(t, output,
		fmt.Sprintf("Uploading Gemini batch %d:", firstBatch.ID),
		fmt.Sprintf("Gemini batch %d upload complete:", firstBatch.ID),
		fmt.Sprintf("Uploading Gemini batch %d:", secondBatch.ID),
		fmt.Sprintf("Gemini batch %d upload complete:", secondBatch.ID),
	)
}

func TestUploadSourceReadErrorReportsConsumedBytes(t *testing.T) {
	database, batch, _ := createPreparedImageTestBatch(t, "read-error")
	readErr := errors.New("source failed")
	source := &partialErrorReadSeeker{data: []byte("abc"), err: readErr}
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var uploadErr error

	output := captureStdout(t, func() {
		uploadErr = uploadAndSubmitGeminiBatch(
			cmd, database, &fakeGeminiBatchAPI{}, batch.ID, source, 10,
		)
	})
	if !errors.Is(uploadErr, readErr) {
		t.Fatalf("uploadAndSubmitGeminiBatch() error = %v, want source error", uploadErr)
	}
	if !strings.Contains(output, fmt.Sprintf("Gemini batch %d upload stopped at 3 B / 10 B.", batch.ID)) {
		t.Fatalf("output = %q, want exact partial source byte count", output)
	}
}

func TestUploadProgressChecksCommandOutputWriter(t *testing.T) {
	database, batch, request := createPreparedImageTestBatch(t, "redirected-upload")
	line := []byte(fmt.Sprintf(`{"key":%q}`+"\n", request.RequestKey))
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&out)

	oldTerminalCheck := progressWriterIsTerminal
	var checked io.Writer
	progressWriterIsTerminal = func(writer io.Writer) bool {
		checked = writer
		return false
	}
	t.Cleanup(func() { progressWriterIsTerminal = oldTerminalCheck })

	if err := uploadAndSubmitGeminiBatch(
		cmd, database, &fakeGeminiBatchAPI{}, batch.ID,
		bytes.NewReader(line), int64(len(line)),
	); err != nil {
		t.Fatalf("uploadAndSubmitGeminiBatch() error = %v", err)
	}
	if checked != &out {
		t.Fatalf("terminal check writer = %T, want command output writer", checked)
	}
	if strings.Contains(out.String(), "\x1b[") {
		t.Fatalf("redirected upload output contains ANSI controls: %q", out.String())
	}
}

func TestUploadHelperRestoresTTYAfterTransportPanic(t *testing.T) {
	database, batch, request := createPreparedImageTestBatch(t, "panic")
	line := []byte(fmt.Sprintf(`{"key":%q}`+"\n", request.RequestKey))
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	oldTerminalCheck := progressWriterIsTerminal
	progressWriterIsTerminal = func(io.Writer) bool { return true }
	t.Cleanup(func() { progressWriterIsTerminal = oldTerminalCheck })
	var recovered any

	output := captureStdout(t, func() {
		func() {
			defer func() { recovered = recover() }()
			_ = uploadAndSubmitGeminiBatch(
				cmd,
				database,
				&panicUploadAPI{fakeGeminiBatchAPI: &fakeGeminiBatchAPI{}},
				batch.ID,
				bytes.NewReader(line),
				int64(len(line)),
			)
		}()
	})
	if recovered == nil {
		t.Fatal("uploadAndSubmitGeminiBatch() did not propagate panic")
	}
	assertInOrder(t, output, "\x1b[?25l", "\x1b[?25h")
}

func assertInOrder(t *testing.T, output string, parts ...string) {
	t.Helper()
	position := 0
	for _, part := range parts {
		index := strings.Index(output[position:], part)
		if index < 0 {
			t.Fatalf("output %q does not contain %q after byte %d", output, part, position)
		}
		position += index + len(part)
	}
}
