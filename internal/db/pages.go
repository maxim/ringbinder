package db

import (
	"fmt"
	"strings"
)

type PageRecord struct {
	ID        int64
	ContentID int64
	PageIndex int
	Markdown  string
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

func (db *DB) UpsertPage(contentID int64, pageIndex int, markdown string) error {
	_, err := db.Exec(
		`INSERT INTO pages (content_id, page_index, markdown)
		 VALUES (?, ?, ?)
		 ON CONFLICT(content_id, page_index) DO UPDATE SET
		   markdown = excluded.markdown`,
		contentID, pageIndex, markdown,
	)
	return err
}

func (db *DB) DeletePagesForContent(contentID int64) error {
	_, err := db.Exec("DELETE FROM pages WHERE content_id = ?", contentID)
	return err
}

func (db *DB) ReplaceContentPages(contentID int64, pages []PageInput) (err error) {
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
			`INSERT INTO pages (content_id, page_index, markdown)
			 VALUES (?, ?, ?)
			 ON CONFLICT(content_id, page_index) DO UPDATE SET
			   markdown = excluded.markdown`,
			contentID, page.PageIndex, page.Markdown,
		); err != nil {
			return err
		}
	}

	if _, err = tx.Exec(
		"DELETE FROM pages WHERE content_id = ? AND page_index >= ?",
		contentID, len(pages),
	); err != nil {
		return err
	}

	if _, err = tx.Exec("UPDATE contents SET ocr_pending = 0 WHERE id = ?", contentID); err != nil {
		return err
	}

	if err = tx.Commit(); err != nil {
		return err
	}

	return nil
}

func (db *DB) Search(query string) ([]SearchResult, error) {
	tokens := strings.Fields(query)
	if len(tokens) == 0 {
		return nil, nil
	}

	ftsQuery := buildFTSQuery(query)
	rows, err := db.Query(
		`SELECT d.path, p.page_index, c.page_count,
		        snippet(pages_fts, 0, '>>>', '<<<', '...', 48) as snippet
		 FROM pages_fts
		 JOIN pages p ON p.id = pages_fts.rowid
		 JOIN documents d ON d.content_id = p.content_id
		 JOIN contents c ON c.id = d.content_id
		 WHERE pages_fts MATCH ?
		   AND d.deleted = 0
		 ORDER BY bm25(pages_fts)
		 LIMIT 50`, ftsQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []SearchResult
	seenPath := make(map[string]bool)
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(&r.Path, &r.PageIndex, &r.PageCount, &r.Snippet); err != nil {
			return nil, err
		}
		results = append(results, r)
		seenPath[r.Path] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	likeClauses := make([]string, 0, len(tokens))
	args := make([]any, 0, len(tokens))
	for _, token := range tokens {
		likeClauses = append(likeClauses, "d.path LIKE ?")
		args = append(args, "%"+token+"%")
	}

	pathQuery := fmt.Sprintf(
		`SELECT d.path, 0 as page_index, c.page_count, '' as snippet
		 FROM documents d
		 JOIN contents c ON c.id = d.content_id
		 WHERE d.deleted = 0
		   AND %s
		 ORDER BY d.path
		 LIMIT 50`,
		strings.Join(likeClauses, " AND "),
	)

	pathRows, err := db.Query(pathQuery, args...)
	if err != nil {
		return nil, err
	}
	defer pathRows.Close()

	for pathRows.Next() {
		var r SearchResult
		if err := pathRows.Scan(&r.Path, &r.PageIndex, &r.PageCount, &r.Snippet); err != nil {
			return nil, err
		}
		if seenPath[r.Path] {
			continue
		}
		results = append(results, r)
		seenPath[r.Path] = true
		if len(results) >= 50 {
			break
		}
	}
	if err := pathRows.Err(); err != nil {
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
