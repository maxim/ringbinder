package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const ocrCoordinatorBusyMessage = "another OCR or batch operation is already running for this database"

type ocrCoordinatorLock struct {
	file *os.File
}

func acquireOCRCoordinator(databasePath string) (*ocrCoordinatorLock, error) {
	path, err := filepath.Abs(databasePath)
	if err != nil {
		return nil, fmt.Errorf("resolve database path for OCR lock: %w", err)
	}
	path, err = canonicalOCRDatabasePath(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create database dir for OCR lock: %w", err)
	}
	lockPath := ocrCoordinatorPath(path)
	file, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open database for OCR lock: %w", err)
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, errors.New(ocrCoordinatorBusyMessage)
		}
		return nil, fmt.Errorf("lock database for OCR: %w", err)
	}
	return &ocrCoordinatorLock{file: file}, nil
}

func canonicalOCRDatabasePath(path string) (string, error) {
	return canonicalOCRDatabasePathSeen(path, make(map[string]bool))
}

func canonicalOCRDatabasePathSeen(path string, seen map[string]bool) (string, error) {
	if seen[path] {
		return "", fmt.Errorf("resolve database symlinks for OCR lock: symlink cycle at %s", path)
	}
	seen[path] = true
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return "", fmt.Errorf("read database symlink for OCR lock: %w", err)
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		target, err = filepath.Abs(target)
		if err != nil {
			return "", fmt.Errorf("resolve database symlink target for OCR lock: %w", err)
		}
		return canonicalOCRDatabasePathSeen(filepath.Clean(target), seen)
	} else if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("inspect database symlink for OCR lock: %w", err)
	}

	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !os.IsNotExist(err) {
		return "", fmt.Errorf("resolve database symlinks for OCR lock: %w", err)
	}

	missing := []string{filepath.Base(path)}
	parent := filepath.Dir(path)
	for {
		resolved, err := filepath.EvalSymlinks(parent)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved, nil
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("resolve database parent symlinks for OCR lock: %w", err)
		}
		next := filepath.Dir(parent)
		if next == parent {
			return "", fmt.Errorf("resolve database parent symlinks for OCR lock: %w", err)
		}
		missing = append(missing, filepath.Base(parent))
		parent = next
	}
}

func (lock *ocrCoordinatorLock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	// Closing releases flock even after abrupt command failures; an explicit
	// unlock keeps Close errors about the descriptor rather than lock state.
	unlockErr := unix.Flock(int(lock.file.Fd()), unix.LOCK_UN)
	closeErr := lock.file.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
