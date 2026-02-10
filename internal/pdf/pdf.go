package pdf

import (
	"os"

	pdfcpuapi "github.com/pdfcpu/pdfcpu/pkg/api"
)

func PageCount(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	// Use ReadContext + EnsurePageCount instead of pdfcpuapi.PageCount
	// to skip full validation. Some valid-in-practice PDFs (e.g. Stripe
	// invoices) have a bare Dict for /Annots instead of an Array, which
	// pdfcpu's validator rejects even in relaxed mode.
	ctx, err := pdfcpuapi.ReadContext(f, nil)
	if err != nil {
		return 0, err
	}
	if err := ctx.EnsurePageCount(); err != nil {
		return 0, err
	}
	return ctx.PageCount, nil
}
