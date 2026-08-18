package db

import "database/sql"

type Content struct {
	ID         int64
	Checksum   string
	PageCount  int
	OCRPending bool
}

func scanContent(scanner interface{ Scan(dest ...any) error }) (Content, error) {
	var content Content
	var ocrPending int
	if err := scanner.Scan(&content.ID, &content.Checksum, &content.PageCount, &ocrPending); err != nil {
		return Content{}, err
	}
	content.OCRPending = ocrPending == 1
	return content, nil
}

func (db *DB) GetContentByChecksum(checksum string) (*Content, error) {
	row := db.QueryRow(
		`SELECT id, checksum, page_count, ocr_pending
		 FROM contents
		 WHERE checksum = ?`,
		checksum,
	)

	content, err := scanContent(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &content, nil
}

func (db *DB) InsertContent(checksum string, pageCount int) (int64, error) {
	res, err := db.Exec(
		`INSERT INTO contents (checksum, page_count, ocr_pending)
		 VALUES (?, ?, 1)`,
		checksum, pageCount,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (db *DB) MarkContentOCRDone(contentID int64) error {
	_, err := db.Exec("UPDATE contents SET ocr_pending = 0 WHERE id = ?", contentID)
	return err
}

func (db *DB) PendingContents() ([]Content, error) {
	rows, err := db.Query(
		`SELECT c.id, c.checksum, c.page_count, c.ocr_pending
		 FROM contents c
		 WHERE c.ocr_pending = 1
		   AND EXISTS (
		     SELECT 1
		     FROM documents d
		     WHERE d.content_id = c.id AND d.deleted = 0
		   )
		 ORDER BY c.id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var contents []Content
	for rows.Next() {
		content, err := scanContent(rows)
		if err != nil {
			return nil, err
		}
		contents = append(contents, content)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return contents, nil
}

func (db *DB) GetDocumentPathForContent(contentID int64) (string, error) {
	var path string
	err := db.QueryRow(
		`SELECT path
		 FROM documents
		 WHERE content_id = ? AND deleted = 0
		 ORDER BY id
		 LIMIT 1`,
		contentID,
	).Scan(&path)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return path, nil
}

func (db *DB) CleanupOrphanContents() (int, error) {
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	orphanSubquery := `SELECT c.id
	                   FROM contents c
	                   WHERE NOT EXISTS (
	                     SELECT 1
	                     FROM documents d
	                     WHERE d.content_id = c.id AND d.deleted = 0
	                   )`

	if _, err = tx.Exec(
		`DELETE FROM documents
		 WHERE deleted = 1
		   AND content_id IN (` + orphanSubquery + `)`,
	); err != nil {
		return 0, err
	}

	res, err := tx.Exec(
		`DELETE FROM contents
		 WHERE id IN (` + orphanSubquery + `)`,
	)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return int(n), nil
}
