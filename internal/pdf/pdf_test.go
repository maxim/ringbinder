package pdf

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	pdfcpu "github.com/pdfcpu/pdfcpu/pkg/pdfcpu"
)

func TestExtractRangePreservesBoundariesIdentityAndOrder(t *testing.T) {
	source := bytes.NewReader(makePDF("", "zero", "one", "two", "three"))

	data, err := ExtractRange(context.Background(), source, 1, 3, 1<<20)
	if err != nil {
		t.Fatalf("ExtractRange() error = %v", err)
	}

	extracted := bytes.NewReader(data)
	count, err := PageCountContext(context.Background(), extracted)
	if err != nil {
		t.Fatalf("PageCountContext() error = %v", err)
	}
	if count != 2 {
		t.Fatalf("page count = %d, want 2", count)
	}

	got := pageText(t, extracted, 2)
	if got[0] != "one" || got[1] != "two" {
		t.Fatalf("page text = %q, want [one two]", got)
	}
}

func TestExtractRangeUsesFreshContextForRepeatedRanges(t *testing.T) {
	source := bytes.NewReader(makePDF("", "zero", "one", "two"))

	first, err := ExtractRange(context.Background(), source, 2, 3, 1<<20)
	if err != nil {
		t.Fatalf("first ExtractRange() error = %v", err)
	}
	second, err := ExtractRange(context.Background(), source, 0, 2, 1<<20)
	if err != nil {
		t.Fatalf("second ExtractRange() error = %v", err)
	}

	if got := pageText(t, bytes.NewReader(first), 1); got[0] != "two" {
		t.Fatalf("first page text = %q, want two", got)
	}
	if got := pageText(t, bytes.NewReader(second), 2); got[0] != "zero" || got[1] != "one" {
		t.Fatalf("second page text = %q, want [zero one]", got)
	}
}

func TestExtractRangeStopsAtByteLimit(t *testing.T) {
	source := bytes.NewReader(makePDF("", "zero"))

	data, err := ExtractRange(context.Background(), source, 0, 1, 1<<20)
	if err != nil {
		t.Fatalf("unbounded ExtractRange() error = %v", err)
	}
	atLimit := newCappedBuffer(len(data))
	if _, err := atLimit.Write(data); err != nil {
		t.Fatalf("capped writer at exact limit error = %v", err)
	}
	if _, err := atLimit.Write([]byte{1}); !errors.Is(err, ErrRangeTooLarge) {
		t.Fatalf("capped writer above exact limit error = %v, want ErrRangeTooLarge", err)
	}
	belowLimit := newCappedBuffer(len(data) - 1)
	if _, err := belowLimit.Write(data); !errors.Is(err, ErrRangeTooLarge) {
		t.Fatalf("capped writer below exact limit error = %v, want ErrRangeTooLarge", err)
	}
	if _, err := ExtractRange(context.Background(), source, 0, 1, 10); !errors.Is(err, ErrRangeTooLarge) {
		t.Fatalf("ExtractRange() with tiny limit error = %v, want ErrRangeTooLarge", err)
	}
}

func TestExtractRangeToleratesBareDictionaryAnnots(t *testing.T) {
	for _, mode := range []string{"direct", "indirect", "null", "null-array", "missing-ref"} {
		t.Run(mode, func(t *testing.T) {
			source := bytes.NewReader(makePDF(mode, "annotation shape"))

			data, err := ExtractRange(context.Background(), source, 0, 1, 1<<20)
			if err != nil {
				t.Fatalf("ExtractRange() error = %v", err)
			}
			if got := pageText(t, bytes.NewReader(data), 1)[0]; got != "annotation shape" {
				t.Fatalf("page text = %q, want annotation shape", got)
			}
		})
	}
}

func TestExtractRangeRejectsMalformedAnnotationEntriesWithoutPanic(t *testing.T) {
	for _, mode := range []string{"bad-array"} {
		t.Run(mode, func(t *testing.T) {
			source := bytes.NewReader(makePDF(mode, "annotation shape"))
			if _, err := ExtractRange(context.Background(), source, 0, 1, 1<<20); err == nil {
				t.Fatalf("ExtractRange() error = nil")
			}
		})
	}
}

func TestPageCountContextHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := PageCountContext(ctx, bytes.NewReader(makePDF("", "zero")))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("PageCountContext() error = %v, want context.Canceled", err)
	}
}

func pageText(t *testing.T, source io.ReadSeeker, count int) []string {
	t.Helper()

	ctx, err := readContext(context.Background(), source)
	if err != nil {
		t.Fatalf("readContext() error = %v", err)
	}
	texts := make([]string, count)
	for i := 0; i < count; i++ {
		reader, err := pdfcpu.ExtractPageContent(ctx, i+1)
		if err != nil {
			t.Fatalf("ExtractPageContent(%d) error = %v", i+1, err)
		}
		content, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("read page content: %v", err)
		}
		start := strings.Index(string(content), "(")
		end := strings.Index(string(content), ") Tj")
		if start < 0 || end <= start {
			t.Fatalf("unexpected page content %q", content)
		}
		texts[i] = string(content[start+1 : end])
	}
	return texts
}

func makePDF(annotsMode string, labels ...string) []byte {
	var out bytes.Buffer
	out.WriteString("%PDF-1.4\n")
	offsets := []int{0}
	writeObject := func(number int, body string) {
		offsets = append(offsets, out.Len())
		fmt.Fprintf(&out, "%d 0 obj\n%s\nendobj\n", number, body)
	}

	kids := make([]string, len(labels))
	for i := range labels {
		kids[i] = fmt.Sprintf("%d 0 R", 4+i*2)
	}
	writeObject(1, "<< /Type /Catalog /Pages 2 0 R >>")
	writeObject(2, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(labels)))
	writeObject(3, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")
	annotsObject := 4 + len(labels)*2
	for i, label := range labels {
		pageObject := 4 + i*2
		contentObject := pageObject + 1
		annots := ""
		switch annotsMode {
		case "direct":
			annots = " /Annots <<>>"
		case "indirect":
			annots = fmt.Sprintf(" /Annots %d 0 R", annotsObject)
		case "null":
			annots = " /Annots null"
		case "null-array":
			annots = " /Annots [null]"
		case "bad-array":
			annots = " /Annots [42]"
		case "missing-ref":
			annots = " /Annots [99 0 R]"
		}
		writeObject(pageObject, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 3 0 R >> >> /Contents %d 0 R%s >>", contentObject, annots))
		stream := fmt.Sprintf("BT /F1 12 Tf 72 720 Td (%s) Tj ET", label)
		writeObject(contentObject, fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream))
	}
	if annotsMode == "indirect" {
		writeObject(annotsObject, "<<>>")
	}

	xref := out.Len()
	fmt.Fprintf(&out, "xref\n0 %d\n", len(offsets))
	out.WriteString("0000000000 65535 f \n")
	for _, offset := range offsets[1:] {
		fmt.Fprintf(&out, "%010d 00000 n \n", offset)
	}
	fmt.Fprintf(&out, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(offsets), xref)
	return out.Bytes()
}
