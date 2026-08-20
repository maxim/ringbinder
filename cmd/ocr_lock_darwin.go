//go:build darwin

package cmd

func ocrCoordinatorPath(databasePath string) string {
	// Darwin merges flock and SQLite's POSIX locks, so flocking the database
	// inode prevents Ringbinder's own SQLite connection from operating. A stable
	// adjacent inode preserves crash-safe flock semantics without racing SQLite.
	return databasePath + ".ringbinder-ocr.lock"
}
