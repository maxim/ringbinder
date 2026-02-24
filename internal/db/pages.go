package db

import (
	"fmt"
	"strings"
)

const defaultSearchLimit = 50

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
	Rank      float64
}

type SearchOptions struct {
	Query           string
	RawFTS          string
	Mode            string
	Limit           int
	Offset          int
	IncludePathLike bool
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
	return db.SearchWithOptions(SearchOptions{
		Query:           query,
		Mode:            "and",
		Limit:           defaultSearchLimit,
		Offset:          0,
		IncludePathLike: true,
	})
}

func (db *DB) SearchWithOptions(opts SearchOptions) ([]SearchResult, error) {
	searchLimit := opts.Limit
	if searchLimit <= 0 {
		searchLimit = defaultSearchLimit
	}

	searchOffset := opts.Offset
	if searchOffset < 0 {
		searchOffset = 0
	}

	searchMode, err := normalizeSearchMode(opts.Mode)
	if err != nil {
		return nil, err
	}

	queryTokens := strings.Fields(opts.Query)
	ftsQuery := strings.TrimSpace(opts.RawFTS)
	if ftsQuery == "" {
		if len(queryTokens) == 0 {
			return nil, nil
		}
		ftsQuery = buildFTSQueryTokens(queryTokens, searchMode)
	}

	rows, err := db.Query(
		// Stable tie-breakers keep results deterministic when bm25 scores are equal.
		`SELECT d.path, p.page_index, c.page_count,
		        snippet(pages_fts, 0, '>>>', '<<<', '...', 48) as snippet,
		        bm25(pages_fts) as rank
		 FROM pages_fts
		 JOIN pages p ON p.id = pages_fts.rowid
		 JOIN documents d ON d.content_id = p.content_id
		 JOIN contents c ON c.id = d.content_id
		 WHERE pages_fts MATCH ?
		   AND d.deleted = 0
		 ORDER BY rank ASC, d.path ASC, p.page_index ASC
		 LIMIT ? OFFSET ?`,
		ftsQuery, searchLimit, searchOffset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	results := make([]SearchResult, 0, searchLimit)
	seenPath := make(map[string]bool)
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(&r.Path, &r.PageIndex, &r.PageCount, &r.Snippet, &r.Rank); err != nil {
			return nil, err
		}
		results = append(results, r)
		seenPath[r.Path] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(results) >= searchLimit || !opts.IncludePathLike || len(queryTokens) == 0 {
		if len(results) > searchLimit {
			return results[:searchLimit], nil
		}
		return results, nil
	}

	likeClauses := make([]string, 0, len(queryTokens))
	likeArgs := make([]any, 0, len(queryTokens)+2)
	for _, token := range queryTokens {
		likeClauses = append(likeClauses, "d.path LIKE ?")
		likeArgs = append(likeArgs, "%"+token+"%")
	}

	pathTokenOperator := " AND "
	if searchMode == "or" {
		pathTokenOperator = " OR "
	}

	pathQuery := fmt.Sprintf(
		// Path-only matches do not come from FTS, so they have no bm25 score.
		// We set rank to 0.0 as a neutral sentinel so JSON shape stays stable.
		`SELECT d.path, 0 as page_index, c.page_count, '' as snippet, 0.0 as rank
		 FROM documents d
		 JOIN contents c ON c.id = d.content_id
		 WHERE d.deleted = 0
		   AND (%s)
		 ORDER BY d.path
		 LIMIT ? OFFSET ?`,
		strings.Join(likeClauses, pathTokenOperator),
	)
	likeArgs = append(likeArgs, searchLimit, searchOffset)

	pathRows, err := db.Query(pathQuery, likeArgs...)
	if err != nil {
		return nil, err
	}
	defer pathRows.Close()

	for pathRows.Next() {
		var r SearchResult
		if err := pathRows.Scan(&r.Path, &r.PageIndex, &r.PageCount, &r.Snippet, &r.Rank); err != nil {
			return nil, err
		}
		if seenPath[r.Path] {
			continue
		}
		results = append(results, r)
		seenPath[r.Path] = true
		if len(results) >= searchLimit {
			break
		}
	}
	if err := pathRows.Err(); err != nil {
		return nil, err
	}

	return results, nil
}

func normalizeSearchMode(mode string) (string, error) {
	normalizedMode := strings.ToLower(strings.TrimSpace(mode))
	if normalizedMode == "" {
		return "and", nil
	}

	switch normalizedMode {
	case "and", "or":
		return normalizedMode, nil
	default:
		return "", fmt.Errorf("invalid search mode %q (want and|or)", mode)
	}
}

func buildFTSQuery(query string) string {
	return buildFTSQueryTokens(strings.Fields(query), "and")
}

func buildFTSQueryTokens(tokens []string, mode string) string {
	if len(tokens) == 0 {
		return `""`
	}

	tokenOperator := " AND "
	if strings.EqualFold(mode, "or") {
		tokenOperator = " OR "
	}

	quotedTokens := make([]string, len(tokens))
	for i, token := range tokens {
		quotedTokens[i] = `"` + strings.ReplaceAll(token, `"`, `""`) + `"`
	}

	return strings.Join(quotedTokens, tokenOperator)
}

func (db *DB) GetPageMarkdownByPathAndIndex(path string, pageIndex int) (string, error) {
	var markdown string
	err := db.QueryRow(
		`SELECT p.markdown
		 FROM documents d
		 JOIN pages p ON p.content_id = d.content_id
		 WHERE d.path = ?
		   AND d.deleted = 0
		   AND p.page_index = ?`,
		path, pageIndex,
	).Scan(&markdown)
	if err != nil {
		return "", err
	}

	return markdown, nil
}

func (db *DB) GetPagesMarkdownByPathAndRange(path string, startInclusive, endInclusive int) ([]PageRecord, error) {
	rows, err := db.Query(
		`SELECT p.page_index, p.markdown
		 FROM documents d
		 JOIN pages p ON p.content_id = d.content_id
		 WHERE d.path = ?
		   AND d.deleted = 0
		   AND p.page_index BETWEEN ? AND ?
		 ORDER BY p.page_index`,
		path, startInclusive, endInclusive,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var pageRecords []PageRecord
	for rows.Next() {
		var pageRecord PageRecord
		if err := rows.Scan(&pageRecord.PageIndex, &pageRecord.Markdown); err != nil {
			return nil, err
		}
		pageRecords = append(pageRecords, pageRecord)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return pageRecords, nil
}
