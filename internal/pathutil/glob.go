package pathutil

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/maxim/ringbinder/internal/config"
)

// ResolveGlobs expands sweep path patterns into absolute paths and roots.
func ResolveGlobs(patterns []string) (resolved []string, roots []string, warnings []string, err error) {
	resolvedSet := make(map[string]struct{}, len(patterns))
	rootCandidates := make([]string, 0, len(patterns))

	for _, original := range patterns {
		pattern := config.ExpandHome(original)
		if hasGlobMeta(pattern) {
			matches, matchErr := doublestar.FilepathGlob(pattern)
			if matchErr != nil {
				return nil, nil, nil, fmt.Errorf("resolve glob %q: %w", original, matchErr)
			}

			root, rootErr := GlobRoot(pattern)
			if rootErr != nil {
				return nil, nil, nil, fmt.Errorf("resolve glob root %q: %w", original, rootErr)
			}
			rootCandidates = append(rootCandidates, root)

			if len(matches) == 0 {
				warnings = append(warnings, fmt.Sprintf("path pattern %q matched no paths", original))
				continue
			}

			for _, match := range matches {
				abs, absErr := filepath.Abs(match)
				if absErr != nil {
					return nil, nil, nil, fmt.Errorf("resolve matched path %q: %w", match, absErr)
				}
				if _, ok := resolvedSet[abs]; ok {
					continue
				}
				resolvedSet[abs] = struct{}{}
				resolved = append(resolved, abs)
			}
			continue
		}

		abs, absErr := filepath.Abs(pattern)
		if absErr != nil {
			return nil, nil, nil, fmt.Errorf("resolve path %q: %w", original, absErr)
		}
		info, statErr := os.Stat(abs)
		if statErr != nil {
			return nil, nil, nil, fmt.Errorf("stat path %q: %w", original, statErr)
		}

		if _, ok := resolvedSet[abs]; !ok {
			resolvedSet[abs] = struct{}{}
			resolved = append(resolved, abs)
		}

		if info.IsDir() {
			rootCandidates = append(rootCandidates, abs)
		} else {
			rootCandidates = append(rootCandidates, filepath.Dir(abs))
		}
	}

	return resolved, dedupeRoots(rootCandidates), warnings, nil
}

// GlobRoot returns the absolute non-glob root for a glob pattern.
func GlobRoot(pattern string) (string, error) {
	pattern = config.ExpandHome(pattern)

	volume := filepath.VolumeName(pattern)
	rest := strings.TrimPrefix(pattern, volume)
	isAbs := strings.HasPrefix(rest, string(filepath.Separator))
	segments := strings.Split(rest, string(filepath.Separator))

	parts := make([]string, 0, len(segments))
	for _, segment := range segments {
		if segment == "" {
			continue
		}
		if hasGlobMeta(segment) {
			break
		}
		parts = append(parts, segment)
	}

	root := "."
	switch {
	case isAbs && volume != "":
		root = volume + string(filepath.Separator)
	case isAbs:
		root = string(filepath.Separator)
	case volume != "":
		root = volume
	}
	for _, segment := range parts {
		root = filepath.Join(root, segment)
	}

	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	return abs, nil
}

// MatchesAny reports whether path matches any glob pattern.
func MatchesAny(path string, patterns []string) (bool, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false, fmt.Errorf("resolve path %q: %w", path, err)
	}
	pathToMatch := filepath.ToSlash(filepath.Clean(absPath))

	for _, original := range patterns {
		pattern := config.ExpandHome(original)
		basenamePattern := !strings.Contains(pattern, "/") && !strings.Contains(pattern, `\`)
		if basenamePattern {
			pattern = filepath.Join("**", pattern)
		} else if !filepath.IsAbs(pattern) {
			absPattern, absErr := filepath.Abs(pattern)
			if absErr != nil {
				return false, fmt.Errorf("resolve pattern %q: %w", original, absErr)
			}
			pattern = absPattern
		}

		ok, matchErr := doublestar.Match(filepath.ToSlash(pattern), pathToMatch)
		if matchErr != nil {
			return false, fmt.Errorf("match pattern %q: %w", original, matchErr)
		}
		if ok {
			return true, nil
		}
	}

	return false, nil
}

func dedupeRoots(roots []string) []string {
	if len(roots) == 0 {
		return nil
	}

	unique := make([]string, 0, len(roots))
	seen := make(map[string]struct{}, len(roots))
	for _, root := range roots {
		cleanRoot := filepath.Clean(root)
		if _, ok := seen[cleanRoot]; ok {
			continue
		}
		seen[cleanRoot] = struct{}{}
		unique = append(unique, cleanRoot)
	}

	sort.Slice(unique, func(i, j int) bool {
		if len(unique[i]) == len(unique[j]) {
			return unique[i] < unique[j]
		}
		return len(unique[i]) < len(unique[j])
	})

	kept := make([]string, 0, len(unique))
	for _, root := range unique {
		if len(kept) > 0 && pathWithinRoots(root, kept) {
			continue
		}
		kept = append(kept, root)
	}
	return kept
}

func pathWithinRoots(path string, roots []string) bool {
	if len(roots) == 0 {
		return true
	}

	cleanPath := filepath.Clean(path)
	for _, root := range roots {
		cleanRoot := filepath.Clean(root)
		if cleanPath == cleanRoot {
			return true
		}

		rootWithSep := cleanRoot
		if !strings.HasSuffix(rootWithSep, string(filepath.Separator)) {
			rootWithSep += string(filepath.Separator)
		}
		if strings.HasPrefix(cleanPath, rootWithSep) {
			return true
		}
	}

	return false
}

func hasGlobMeta(path string) bool {
	return strings.ContainsAny(path, "*?[{")
}
