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

	return pdfcpuapi.PageCount(f, nil)
}
