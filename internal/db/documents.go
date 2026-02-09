package db

import (
	"database/sql"
	"fmt"
	"time"
)

type Document struct {
	ID         int64
	Path       string
	Checksum   string
	CreatedAt  time.Time
	ModifiedAt time.Time
	PageCount  int
	OCRPending bool
	Deleted    bool
}

func (db *DB) GetDocumentByPath(path string) (*Document, error) {
	row := db.QueryRow(
		`SELECT id, path, checksum, created_at, modified_at, page_count, ocr_pending, deleted
		 FROM documents WHERE path = ?`, path)

	var doc Document
	var createdAt, modifiedAt string
	var ocrPending, deleted int

	err := row.Scan(&doc.ID, &doc.Path, &doc.Checksum, &createdAt, &modifiedAt,
		&doc.PageCount, &ocrPending, &deleted)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	doc.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return nil, fmt.Errorf("parse created_at %q: %w", createdAt, err)
	}
	doc.ModifiedAt, err = time.Parse(time.RFC3339Nano, modifiedAt)
	if err != nil {
		return nil, fmt.Errorf("parse modified_at %q: %w", modifiedAt, err)
	}
	doc.OCRPending = ocrPending == 1
	doc.Deleted = deleted == 1
	return &doc, nil
}

func (db *DB) InsertDocument(path, checksum string, createdAt, modifiedAt time.Time, pageCount int) (int64, error) {
	res, err := db.Exec(
		`INSERT INTO documents (path, checksum, created_at, modified_at, page_count, ocr_pending, deleted)
		 VALUES (?, ?, ?, ?, ?, 1, 0)`,
		path, checksum, createdAt.Format(time.RFC3339Nano), modifiedAt.Format(time.RFC3339Nano), pageCount)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (db *DB) UpdateDocument(id int64, checksum string, modifiedAt time.Time, pageCount int) error {
	_, err := db.Exec(
		`UPDATE documents SET checksum = ?, modified_at = ?, page_count = ?, ocr_pending = 1
		 WHERE id = ?`,
		checksum, modifiedAt.Format(time.RFC3339Nano), pageCount, id)
	return err
}

func (db *DB) RestoreDocument(id int64, checksum string, modifiedAt time.Time, pageCount int, checksumChanged bool) error {
	ocrPending := 0
	if checksumChanged {
		ocrPending = 1
	}
	_, err := db.Exec(
		`UPDATE documents SET checksum = ?, modified_at = ?, page_count = ?, ocr_pending = ?, deleted = 0
		 WHERE id = ?`,
		checksum, modifiedAt.Format(time.RFC3339Nano), pageCount, ocrPending, id)
	return err
}

func (db *DB) SoftDeleteMissing(seenPaths map[string]bool) (int, error) {
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

func (db *DB) PendingDocuments() ([]Document, error) {
	rows, err := db.Query(
		`SELECT id, path, checksum, created_at, modified_at, page_count, ocr_pending, deleted
		 FROM documents WHERE ocr_pending = 1 AND deleted = 0 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var docs []Document
	for rows.Next() {
		var doc Document
		var createdAt, modifiedAt string
		var ocrPending, deleted int
		if err := rows.Scan(&doc.ID, &doc.Path, &doc.Checksum, &createdAt, &modifiedAt,
			&doc.PageCount, &ocrPending, &deleted); err != nil {
			return nil, err
		}
		doc.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			return nil, fmt.Errorf("parse created_at %q: %w", createdAt, err)
		}
		doc.ModifiedAt, err = time.Parse(time.RFC3339Nano, modifiedAt)
		if err != nil {
			return nil, fmt.Errorf("parse modified_at %q: %w", modifiedAt, err)
		}
		doc.OCRPending = ocrPending == 1
		doc.Deleted = deleted == 1
		docs = append(docs, doc)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return docs, nil
}

func (db *DB) MarkOCRDone(id int64) error {
	_, err := db.Exec("UPDATE documents SET ocr_pending = 0 WHERE id = ?", id)
	return err
}

func (db *DB) PendingStats() (docCount int, totalPages int, err error) {
	err = db.QueryRow(
		`SELECT COALESCE(COUNT(*), 0), COALESCE(SUM(page_count), 0)
		 FROM documents WHERE ocr_pending = 1 AND deleted = 0`).Scan(&docCount, &totalPages)
	return
}
