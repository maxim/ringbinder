//go:build darwin

package scanner

import (
	"context"
	"io/fs"
	"path/filepath"
	"strings"
)

type darwinScanner struct{}

func NewScanner() Scanner {
	return &darwinScanner{}
}

func (s *darwinScanner) Scan(ctx context.Context, paths []string, results chan<- FileInfo) error {
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

			// Skip macOS metadata
			name := d.Name()
			if d.IsDir() {
				if name == ".Trash" || name == ".Spotlight-V100" || name == ".fseventsd" {
					return filepath.SkipDir
				}
				return nil
			}
			if name == ".DS_Store" || strings.HasPrefix(name, "._") {
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
