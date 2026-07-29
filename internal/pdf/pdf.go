package pdf

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	pdfcpuapi "github.com/pdfcpu/pdfcpu/pkg/api"
	pdfcpu "github.com/pdfcpu/pdfcpu/pkg/pdfcpu"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/types"
)

var (
	ErrRangeTooLarge = errors.New("extracted PDF page range exceeds byte limit")
	baseConfig       = model.NewDefaultConfiguration()
)

func PageCount(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	return PageCountContext(context.Background(), f)
}

// PageCountContext deliberately skips pdfcpu's full validation and optimization.
// Some valid-in-practice PDFs (for example, invoices with a bare /Annots Dict)
// are readable but rejected by the validator even in relaxed mode.
func PageCountContext(ctx context.Context, rs io.ReadSeeker) (int, error) {
	pdfCtx, err := readContext(ctx, rs)
	if err != nil {
		return 0, err
	}
	return pdfCtx.PageCount, nil
}

// ExtractRange creates a PDF containing the zero-based half-open page range
// [start, end). A fresh source context is parsed on every call because pdfcpu's
// extraction mutates or shares named-destination structures between contexts.
func ExtractRange(ctx context.Context, rs io.ReadSeeker, start, end, maxBytes int) ([]byte, error) {
	if start < 0 || end <= start {
		return nil, errors.New("invalid PDF page range")
	}
	if maxBytes < 0 {
		return nil, errors.New("invalid PDF byte limit")
	}

	pdfCtx, err := readContext(ctx, rs)
	if err != nil {
		return nil, err
	}
	if end > pdfCtx.PageCount {
		return nil, errors.New("PDF page range exceeds page count")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Some scanner/invoice generators emit a single annotation dictionary where
	// the PDF spec requires an array. Validation rejects it, while extraction
	// otherwise panics on pdfcpu's array assertion, so narrowly wrap that shape.
	if err := normalizeBareAnnotationDicts(ctx, pdfCtx, start, end); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// pdfcpu page numbers are one-based and inclusive. Ringbinder ranges are
	// zero-based and half-open, so [start, end) maps to start+1 through end.
	pages := pdfcpuapi.PagesForPageRange(start+1, end)
	extracted, err := pdfcpu.ExtractPages(pdfCtx, pages, false)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	w := newCappedBuffer(maxBytes)
	err = pdfcpuapi.WriteContext(extracted, w)
	if w.overflow || errors.Is(err, ErrRangeTooLarge) {
		return nil, ErrRangeTooLarge
	}
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return w.Bytes(), nil
}

func normalizeBareAnnotationDicts(opCtx context.Context, pdfCtx *model.Context, start, end int) error {
	for page := start + 1; page <= end; page++ {
		if err := opCtx.Err(); err != nil {
			return err
		}
		dict, _, _, err := pdfCtx.PageDict(page, false)
		if err != nil {
			return err
		}
		annots, found := dict.Find("Annots")
		if !found {
			continue
		}
		resolved, err := pdfCtx.Dereference(annots)
		if err != nil {
			return err
		}
		if resolved == nil {
			dict.Delete("Annots")
			continue
		}
		switch value := resolved.(type) {
		case types.Array:
			normalized, err := normalizeAnnotationArray(opCtx, pdfCtx, page, value)
			if err != nil {
				return err
			}
			dict["Annots"] = normalized
		case types.Dict:
			dict["Annots"] = types.Array{value}
		default:
			return fmt.Errorf("PDF page %d has unsupported /Annots type %T", page, resolved)
		}
	}
	return nil
}

func normalizeAnnotationArray(
	opCtx context.Context,
	pdfCtx *model.Context,
	page int,
	annots types.Array,
) (types.Array, error) {
	normalized := make(types.Array, 0, len(annots))
	for _, annot := range annots {
		if err := opCtx.Err(); err != nil {
			return nil, err
		}
		if annot == nil {
			continue
		}
		switch value := annot.(type) {
		case types.Dict:
			normalized = append(normalized, value)
		case types.IndirectRef:
			resolved, err := pdfCtx.Dereference(value)
			if err != nil {
				return nil, err
			}
			if resolved == nil {
				continue
			}
			if _, ok := resolved.(types.Dict); !ok {
				return nil, fmt.Errorf("PDF page %d has unsupported /Annots entry type %T", page, resolved)
			}
			// Keep valid references so pdfcpu can migrate each annotation object once.
			normalized = append(normalized, value)
		default:
			return nil, fmt.Errorf("PDF page %d has unsupported /Annots entry type %T", page, annot)
		}
	}
	return normalized, nil
}

func readContext(ctx context.Context, rs io.ReadSeeker) (*model.Context, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := rs.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	// pdfcpu lazily initializes package-global config without synchronization.
	// Initialize it once at package load, then clone it so concurrent document
	// workers neither race during initialization nor share mutable config.
	conf := *baseConfig
	pdfCtx, err := pdfcpu.ReadWithContext(ctx, rs, &conf)
	if err != nil {
		return nil, err
	}
	if err := pdfCtx.EnsurePageCount(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return pdfCtx, nil
}

type cappedBuffer struct {
	bytes.Buffer
	max      int
	overflow bool
}

func newCappedBuffer(maxBytes int) *cappedBuffer {
	return &cappedBuffer{max: maxBytes}
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	remaining := b.max - b.Len()
	if remaining <= 0 {
		b.overflow = true
		return 0, ErrRangeTooLarge
	}
	if len(p) <= remaining {
		return b.Buffer.Write(p)
	}

	n, _ := b.Buffer.Write(p[:remaining])
	b.overflow = true
	return n, ErrRangeTooLarge
}
