package db

import (
	"strings"
)

type PageRecord struct {
	ID         int64
	DocumentID int64
	PageIndex  int
	Markdown   string
}

type PageInput struct {
	PageIndex int
	Markdown  string
}

type SearchResult struct {
	Path      string
	PageIndex int
	PageCount int
	Snippet   string
}

func (db *DB) UpsertPage(documentID int64, pageIndex int, markdown string) error {
	_, err := db.Exec(
		`INSERT INTO pages (document_id, page_index, markdown)
		 VALUES (?, ?, ?)
		 ON CONFLICT(document_id, page_index) DO UPDATE SET
		   markdown = excluded.markdown`,
		documentID, pageIndex, markdown)
	return err
}

func (db *DB) DeletePagesForDocument(documentID int64) error {
	_, err := db.Exec("DELETE FROM pages WHERE document_id = ?", documentID)
	return err
}

func (db *DB) ReplaceDocumentPages(documentID int64, pages []PageInput) (err error) {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	for _, page := range pages {
		if _, err = tx.Exec(
			`INSERT INTO pages (document_id, page_index, markdown)
			 VALUES (?, ?, ?)
			 ON CONFLICT(document_id, page_index) DO UPDATE SET
			   markdown = excluded.markdown`,
			documentID, page.PageIndex, page.Markdown); err != nil {
			return err
		}
	}

	if _, err = tx.Exec(
		"DELETE FROM pages WHERE document_id = ? AND page_index >= ?",
		documentID, len(pages)); err != nil {
		return err
	}

	if _, err = tx.Exec("UPDATE documents SET ocr_pending = 0 WHERE id = ?", documentID); err != nil {
		return err
	}

	if err = tx.Commit(); err != nil {
		return err
	}

	return nil
}

func (db *DB) Search(query string) ([]SearchResult, error) {
	ftsQuery := buildFTSQuery(query)
	rows, err := db.Query(
		`SELECT d.path, p.page_index, d.page_count,
		        snippet(pages_fts, 0, '>>>', '<<<', '...', 48) as snippet
		 FROM pages_fts
		 JOIN pages p ON p.id = pages_fts.rowid
		 JOIN documents d ON d.id = p.document_id
		 WHERE pages_fts MATCH ?
		   AND d.deleted = 0
		 ORDER BY bm25(pages_fts)
		 LIMIT 50`, ftsQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(&r.Path, &r.PageIndex, &r.PageCount, &r.Snippet); err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

func buildFTSQuery(query string) string {
	tokens := strings.Fields(query)
	if len(tokens) == 0 {
		return `""`
	}

	quoted := make([]string, len(tokens))
	for i, token := range tokens {
		quoted[i] = `"` + strings.ReplaceAll(token, `"`, `""`) + `"`
	}

	return strings.Join(quoted, " AND ")
}
