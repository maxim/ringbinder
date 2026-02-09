package db

import (
	"encoding/json"
	"strings"
)

type PageRecord struct {
	ID          int64
	DocumentID  int64
	PageIndex   int
	Markdown    string
	Annotations json.RawMessage
}

type SearchResult struct {
	Path      string
	PageIndex int
	Snippet   string
}

func (db *DB) UpsertPage(documentID int64, pageIndex int, markdown string, annotations json.RawMessage) error {
	_, err := db.Exec(
		`INSERT INTO pages (document_id, page_index, markdown, annotations)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(document_id, page_index) DO UPDATE SET
		   markdown = excluded.markdown,
		   annotations = excluded.annotations`,
		documentID, pageIndex, markdown, string(annotations))
	return err
}

func (db *DB) DeletePagesForDocument(documentID int64) error {
	_, err := db.Exec("DELETE FROM pages WHERE document_id = ?", documentID)
	return err
}

func (db *DB) Search(query string) ([]SearchResult, error) {
	// Quote query as a plain-text phrase to avoid FTS5 syntax errors on punctuation
	quoted := `"` + strings.ReplaceAll(query, `"`, `""`) + `"`
	rows, err := db.Query(
		`SELECT d.path, p.page_index,
		        snippet(pages_fts, 0, '>>>', '<<<', '...', 48) as snippet
		 FROM pages_fts
		 JOIN pages p ON p.id = pages_fts.rowid
		 JOIN documents d ON d.id = p.document_id
		 WHERE pages_fts MATCH ?
		   AND d.deleted = 0
		 ORDER BY bm25(pages_fts)
		 LIMIT 50`, quoted)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(&r.Path, &r.PageIndex, &r.Snippet); err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return results, nil
}
