package db

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

type Document struct {
	ID         int64
	Path       string
	ContentID  int64
	Checksum   string
	CreatedAt  time.Time
	ModifiedAt time.Time
	PageCount  int
	OCRPending bool
	Deleted    bool
}

type documentScanner interface {
	Scan(dest ...any) error
}

func scanDocument(scanner documentScanner) (Document, error) {
	var doc Document
	var createdAt, modifiedAt string
	var ocrPending, deleted int

	if err := scanner.Scan(
		&doc.ID,
		&doc.Path,
		&doc.ContentID,
		&createdAt,
		&modifiedAt,
		&deleted,
		&doc.Checksum,
		&doc.PageCount,
		&ocrPending,
	); err != nil {
		return Document{}, err
	}

	created, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return Document{}, fmt.Errorf("parse created_at %q: %w", createdAt, err)
	}
	modified, err := time.Parse(time.RFC3339Nano, modifiedAt)
	if err != nil {
		return Document{}, fmt.Errorf("parse modified_at %q: %w", modifiedAt, err)
	}

	doc.CreatedAt = created
	doc.ModifiedAt = modified
	doc.OCRPending = ocrPending == 1
	doc.Deleted = deleted == 1
	return doc, nil
}

func (db *DB) GetDocumentByPath(path string) (*Document, error) {
	row := db.QueryRow(
		`SELECT d.id, d.path, d.content_id, d.created_at, d.modified_at, d.deleted,
		        c.checksum, c.page_count, c.ocr_pending
		 FROM documents d
		 JOIN contents c ON c.id = d.content_id
		 WHERE d.path = ?`,
		path,
	)

	doc, err := scanDocument(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &doc, nil
}

func (db *DB) InsertDocument(path string, contentID int64, createdAt, modifiedAt time.Time) (int64, error) {
	res, err := db.Exec(
		`INSERT INTO documents (path, content_id, created_at, modified_at, deleted)
		 VALUES (?, ?, ?, ?, 0)`,
		path, contentID, createdAt.Format(time.RFC3339Nano), modifiedAt.Format(time.RFC3339Nano),
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (db *DB) UpdateDocument(id int64, contentID int64, modifiedAt time.Time) error {
	_, err := db.Exec(
		`UPDATE documents SET content_id = ?, modified_at = ?
		 WHERE id = ?`,
		contentID, modifiedAt.Format(time.RFC3339Nano), id,
	)
	return err
}

func (db *DB) RestoreDocument(id int64, contentID int64, modifiedAt time.Time) error {
	_, err := db.Exec(
		`UPDATE documents
		 SET content_id = ?, modified_at = ?, deleted = 0
		 WHERE id = ?`,
		contentID, modifiedAt.Format(time.RFC3339Nano), id,
	)
	return err
}

func (db *DB) SoftDeleteMissing(seenPaths map[string]bool, roots []string) (int, error) {
	rows, err := db.Query("SELECT id, path FROM documents WHERE deleted = 0")
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var toDelete []int64
	for rows.Next() {
		var id int64
		var path string
		if err := rows.Scan(&id, &path); err != nil {
			return 0, err
		}
		if !pathWithinRoots(path, roots) {
			continue
		}
		if !seenPaths[path] {
			toDelete = append(toDelete, id)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for _, id := range toDelete {
		if _, err := db.Exec("UPDATE documents SET deleted = 1 WHERE id = ?", id); err != nil {
			return 0, err
		}
	}

	return len(toDelete), nil
}

func (db *DB) ResetAllDocuments() (int, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	var count int
	if err = tx.QueryRow("SELECT COUNT(*) FROM documents").Scan(&count); err != nil {
		return 0, err
	}
	if _, err = tx.Exec("DELETE FROM documents"); err != nil {
		return 0, err
	}
	if _, err = tx.Exec("DELETE FROM contents"); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}

	return count, nil
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

func (db *DB) PendingDocuments() ([]Document, error) {
	return db.queryDocuments(
		`SELECT d.id, d.path, d.content_id, d.created_at, d.modified_at, d.deleted,
		        c.checksum, c.page_count, c.ocr_pending
		 FROM documents d
		 JOIN contents c ON c.id = d.content_id
		 WHERE c.ocr_pending = 1
		   AND d.deleted = 0
		 ORDER BY d.id`,
	)
}

func (db *DB) AllDocuments() ([]Document, error) {
	return db.queryDocuments(
		`SELECT d.id, d.path, d.content_id, d.created_at, d.modified_at, d.deleted,
		        c.checksum, c.page_count, c.ocr_pending
		 FROM documents d
		 JOIN contents c ON c.id = d.content_id
		 WHERE d.deleted = 0
		 ORDER BY d.id`,
	)
}

func (db *DB) queryDocuments(query string, args ...any) ([]Document, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var docs []Document
	for rows.Next() {
		doc, err := scanDocument(rows)
		if err != nil {
			return nil, err
		}
		docs = append(docs, doc)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return docs, nil
}

func (db *DB) AllStats() (docCount int, totalPages int, err error) {
	err = db.QueryRow(
		`SELECT COALESCE(COUNT(*), 0), COALESCE(SUM(c.page_count), 0)
		 FROM documents d
		 JOIN contents c ON c.id = d.content_id
		 WHERE d.deleted = 0`,
	).Scan(&docCount, &totalPages)
	return
}

func (db *DB) PendingStats() (docCount int, totalPages int, err error) {
	err = db.QueryRow(
		`SELECT COALESCE(COUNT(*), 0), COALESCE(SUM(c.page_count), 0)
		 FROM contents c
		 WHERE c.ocr_pending = 1
		   AND EXISTS (
		     SELECT 1
		     FROM documents d
		     WHERE d.content_id = c.id AND d.deleted = 0
		   )`,
	).Scan(&docCount, &totalPages)
	return
}
