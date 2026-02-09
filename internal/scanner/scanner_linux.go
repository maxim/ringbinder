//go:build linux

package scanner

import (
	"context"
	"io/fs"
	"path/filepath"
)

type linuxScanner struct{}

func NewScanner() Scanner {
	return &linuxScanner{}
}

func (s *linuxScanner) Scan(ctx context.Context, paths []string, results chan<- FileInfo) error {
	defer close(results)

	for _, root := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}

		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil // skip inaccessible entries
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}

			if d.IsDir() {
				return nil
			}

			ct, ok := classifyFile(path)
			if !ok {
				return nil
			}

			info, err := d.Info()
			if err != nil {
				return nil
			}

			results <- FileInfo{
				Path:        path,
				ModTime:     info.ModTime(),
				Size:        info.Size(),
				ContentType: ct,
			}
			return nil
		})
		if err != nil {
			return err
		}
	}

	return nil
}
