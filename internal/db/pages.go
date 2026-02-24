package db

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
)

const (
	defaultSearchLimit = 50

	searchSourceFTS     = "fts"
	searchSourceTrigram = "trigram"
	searchSourcePath    = "path"
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
	Path         string
	PageIndex    int
	PageCount    int
	Snippet      string
	Rank         float64
	SearchSource string
}

type SearchOptions struct {
	Query           string
	RawFTS          string
	Mode            string
	Limit           int
	Offset          int
	IncludePathLike bool
	UseTrigram      bool
}

func (db *DB) UpsertPage(contentID int64, pageIndex int, markdown string) error {
	normalizedSearchText := normalizeSearchText(markdown)
	_, err := db.Exec(
		`INSERT INTO pages (content_id, page_index, markdown, search_text)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(content_id, page_index) DO UPDATE SET
		   markdown = excluded.markdown,
		   search_text = excluded.search_text`,
		contentID, pageIndex, markdown, normalizedSearchText,
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
		normalizedSearchText := normalizeSearchText(page.Markdown)
		if _, err = tx.Exec(
			`INSERT INTO pages (content_id, page_index, markdown, search_text)
			 VALUES (?, ?, ?, ?)
			 ON CONFLICT(content_id, page_index) DO UPDATE SET
			   markdown = excluded.markdown,
			   search_text = excluded.search_text`,
			contentID, page.PageIndex, page.Markdown, normalizedSearchText,
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
		UseTrigram:      false,
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

	fetchLimit := searchLimit + searchOffset
	if fetchLimit < searchLimit {
		fetchLimit = searchLimit
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

	primaryResults, err := db.queryFTSResults("pages_fts", ftsQuery, fetchLimit, 0, searchSourceFTS)
	if err != nil {
		return nil, err
	}

	combinedResults := primaryResults
	if opts.UseTrigram {
		trigramResults, err := db.queryFTSResults("pages_fts_trigram", ftsQuery, fetchLimit, 0, searchSourceTrigram)
		if err != nil {
			return nil, err
		}
		combinedResults = mergeSearchResults(primaryResults, trigramResults)
	}

	if opts.IncludePathLike && len(queryTokens) > 0 && len(combinedResults) < fetchLimit {
		pathMatches, err := db.queryPathMatches(queryTokens, searchMode, fetchLimit, 0)
		if err != nil {
			return nil, err
		}
		combinedResults = appendMissingPathResults(combinedResults, pathMatches)
	}

	return paginateSearchResults(combinedResults, searchLimit, searchOffset), nil
}

func (db *DB) queryFTSResults(indexName string, ftsQuery string, limit int, offset int, searchSource string) ([]SearchResult, error) {
	query := fmt.Sprintf(
		`SELECT d.path, p.page_index, c.page_count,
		        snippet(%[1]s, 0, '>>>', '<<<', '...', 48) as snippet,
		        bm25(%[1]s) as rank
		 FROM %[1]s
		 JOIN pages p ON p.id = %[1]s.rowid
		 JOIN documents d ON d.content_id = p.content_id
		 JOIN contents c ON c.id = d.content_id
		 WHERE %[1]s MATCH ?
		   AND d.deleted = 0
		 ORDER BY rank ASC, d.path ASC, p.page_index ASC
		 LIMIT ? OFFSET ?`,
		indexName,
	)

	rows, err := db.Query(query, ftsQuery, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	results := make([]SearchResult, 0, limit)
	for rows.Next() {
		var result SearchResult
		if err := rows.Scan(&result.Path, &result.PageIndex, &result.PageCount, &result.Snippet, &result.Rank); err != nil {
			return nil, err
		}
		result.SearchSource = searchSource
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return results, nil
}

func (db *DB) queryPathMatches(tokens []string, mode string, limit int, offset int) ([]SearchResult, error) {
	if len(tokens) == 0 {
		return nil, nil
	}

	likeClauses := make([]string, 0, len(tokens))
	pathQueryArgs := make([]any, 0, len(tokens)+2)
	for _, token := range tokens {
		likeClauses = append(likeClauses, "d.path LIKE ?")
		pathQueryArgs = append(pathQueryArgs, "%"+token+"%")
	}

	pathTokenOperator := " AND "
	if mode == "or" {
		pathTokenOperator = " OR "
	}

	pathQuery := fmt.Sprintf(
		// Path-only matches are a fallback and intentionally have a neutral rank.
		`SELECT d.path, 0 as page_index, c.page_count, '' as snippet, 0.0 as rank
		 FROM documents d
		 JOIN contents c ON c.id = d.content_id
		 WHERE d.deleted = 0
		   AND (%s)
		 ORDER BY d.path ASC
		 LIMIT ? OFFSET ?`,
		strings.Join(likeClauses, pathTokenOperator),
	)

	pathQueryArgs = append(pathQueryArgs, limit, offset)
	rows, err := db.Query(pathQuery, pathQueryArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	results := make([]SearchResult, 0, limit)
	for rows.Next() {
		var result SearchResult
		if err := rows.Scan(&result.Path, &result.PageIndex, &result.PageCount, &result.Snippet, &result.Rank); err != nil {
			return nil, err
		}
		result.SearchSource = searchSourcePath
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return results, nil
}

func mergeSearchResults(primary []SearchResult, secondary []SearchResult) []SearchResult {
	if len(secondary) == 0 {
		merged := make([]SearchResult, len(primary))
		copy(merged, primary)
		return merged
	}

	type resultKey struct {
		Path      string
		PageIndex int
	}

	byPage := make(map[resultKey]SearchResult, len(primary)+len(secondary))
	for _, result := range primary {
		byPage[resultKey{Path: result.Path, PageIndex: result.PageIndex}] = result
	}

	for _, result := range secondary {
		key := resultKey{Path: result.Path, PageIndex: result.PageIndex}
		existing, exists := byPage[key]
		if !exists {
			byPage[key] = result
			continue
		}

		if result.Rank < existing.Rank {
			byPage[key] = result
			continue
		}

		if result.Rank == existing.Rank && existing.Snippet == "" && result.Snippet != "" {
			existing.Snippet = result.Snippet
			byPage[key] = existing
		}
	}

	merged := make([]SearchResult, 0, len(byPage))
	for _, result := range byPage {
		merged = append(merged, result)
	}

	sort.Slice(merged, func(i, j int) bool {
		if merged[i].Rank != merged[j].Rank {
			return merged[i].Rank < merged[j].Rank
		}
		if merged[i].Path != merged[j].Path {
			return merged[i].Path < merged[j].Path
		}
		return merged[i].PageIndex < merged[j].PageIndex
	})

	return merged
}

func appendMissingPathResults(existing []SearchResult, pathMatches []SearchResult) []SearchResult {
	if len(pathMatches) == 0 {
		return existing
	}

	seenPaths := make(map[string]bool, len(existing))
	for _, result := range existing {
		seenPaths[result.Path] = true
	}

	combined := make([]SearchResult, len(existing), len(existing)+len(pathMatches))
	copy(combined, existing)

	for _, result := range pathMatches {
		if seenPaths[result.Path] {
			continue
		}
		combined = append(combined, result)
		seenPaths[result.Path] = true
	}

	return combined
}

func paginateSearchResults(results []SearchResult, limit int, offset int) []SearchResult {
	if offset >= len(results) {
		return nil
	}

	end := offset + limit
	if end > len(results) {
		end = len(results)
	}

	paged := make([]SearchResult, end-offset)
	copy(paged, results[offset:end])
	return paged
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

func (db *DB) backfillPageSearchText() error {
	type pageText struct {
		ID       int64
		Markdown string
	}

	rows, err := db.Query(`SELECT id, markdown FROM pages ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()

	allPages := make([]pageText, 0)
	for rows.Next() {
		var row pageText
		if err := rows.Scan(&row.ID, &row.Markdown); err != nil {
			return err
		}
		allPages = append(allPages, row)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	stmt, err := tx.Prepare(`UPDATE pages SET search_text = ? WHERE id = ?`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, page := range allPages {
		normalizedSearchText := normalizeSearchText(page.Markdown)
		if _, err = stmt.Exec(normalizedSearchText, page.ID); err != nil {
			return err
		}
	}

	if err = tx.Commit(); err != nil {
		return err
	}

	return nil
}

func normalizeSearchText(markdown string) string {
	// We normalize into lowercase alphanumeric terms so formatting noise
	// (markdown punctuation, OCR symbols, decorative glyphs) does not fragment
	// the searchable token stream.
	var normalized strings.Builder
	normalized.Grow(len(markdown))

	previousWasSpace := true
	for _, char := range markdown {
		switch {
		case unicode.IsLetter(char) || unicode.IsDigit(char):
			normalized.WriteRune(unicode.ToLower(char))
			previousWasSpace = false
		case unicode.IsSpace(char):
			if !previousWasSpace {
				normalized.WriteByte(' ')
				previousWasSpace = true
			}
		default:
			if !previousWasSpace {
				normalized.WriteByte(' ')
				previousWasSpace = true
			}
		}
	}

	return strings.TrimSpace(normalized.String())
}
