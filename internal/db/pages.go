package db

import (
	"database/sql"
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
	Model     *string
}

type PageInput struct {
	PageIndex int
	Markdown  string
	Model     *string
}

type PageRange struct {
	Start int
	End   int
}

type ModelCount struct {
	Model     *string
	PageCount int
}

type SearchResult struct {
	Path         string
	PageIndex    int
	PageCount    int
	Snippet      string
	Rank         float64
	SearchSource string
	Model        *string
	OCRPending   bool
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
	return db.UpsertContentPages(contentID, []PageInput{{PageIndex: pageIndex, Markdown: markdown}})
}

// ReplaceContentPages remains as a compatibility name for callers that already
// have a complete set. Page writes are additive: partial retries must never
// erase successful pages from an earlier provider or run.
func (db *DB) ReplaceContentPages(contentID int64, pages []PageInput) error {
	return db.UpsertContentPages(contentID, pages)
}

func (db *DB) UpsertContentPages(contentID int64, pages []PageInput) (err error) {
	if len(pages) == 0 {
		return fmt.Errorf("no pages supplied for content %d", contentID)
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

	if err = upsertContentPagesTx(tx, contentID, pages); err != nil {
		return err
	}
	return tx.Commit()
}

func upsertContentPagesTx(tx *sql.Tx, contentID int64, pages []PageInput) error {
	if len(pages) == 0 {
		return fmt.Errorf("no pages supplied for content %d", contentID)
	}
	var pageCount int
	if err := tx.QueryRow(`SELECT page_count FROM contents WHERE id = ?`, contentID).Scan(&pageCount); err != nil {
		return err
	}
	seen := make(map[int]bool, len(pages))
	for _, page := range pages {
		if page.PageIndex < 0 || page.PageIndex >= pageCount {
			return fmt.Errorf("page index %d is outside content %d range 0-%d", page.PageIndex, contentID, pageCount-1)
		}
		if seen[page.PageIndex] {
			return fmt.Errorf("duplicate page index %d", page.PageIndex)
		}
		seen[page.PageIndex] = true
		if page.Model != nil && strings.TrimSpace(*page.Model) == "" {
			return fmt.Errorf("OCR model for page %d cannot be blank", page.PageIndex)
		}
		normalized := normalizeSearchText(page.Markdown)
		if _, err := tx.Exec(
			`INSERT INTO pages (content_id, page_index, markdown, search_text, model)
			 VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT(content_id, page_index) DO UPDATE SET
			   markdown = excluded.markdown,
			   search_text = excluded.search_text,
			   model = excluded.model`,
			contentID, page.PageIndex, page.Markdown, normalized, page.Model,
		); err != nil {
			return err
		}
	}
	return recomputeContentPendingTx(tx, contentID)
}

func recomputeContentPendingTx(tx *sql.Tx, contentID int64) error {
	result, err := tx.Exec(
		`UPDATE contents
		 SET ocr_pending = CASE WHEN (
		   SELECT COUNT(*) FROM pages p
		   WHERE p.content_id = contents.id
		     AND p.page_index >= 0
		     AND p.page_index < contents.page_count
		 ) = page_count THEN 0 ELSE 1 END
		 WHERE id = ?`,
		contentID,
	)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (db *DB) CopyContentPages(sourceContentID, targetContentID int64) (err error) {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	var targetPageCount int
	if err = tx.QueryRow(
		`SELECT page_count FROM contents WHERE id = ?`, targetContentID,
	).Scan(&targetPageCount); err != nil {
		return err
	}
	rows, err := tx.Query(
		`SELECT page_index, markdown, model
		 FROM pages
		 WHERE content_id = ? AND page_index >= 0 AND page_index < ?
		 ORDER BY page_index`,
		sourceContentID, targetPageCount,
	)
	if err != nil {
		return err
	}
	var pages []PageInput
	for rows.Next() {
		var page PageInput
		var model sql.NullString
		if err = rows.Scan(&page.PageIndex, &page.Markdown, &model); err != nil {
			_ = rows.Close()
			return err
		}
		if model.Valid {
			value := model.String
			page.Model = &value
		}
		pages = append(pages, page)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	if len(pages) > 0 {
		if err = upsertContentPagesTx(tx, targetContentID, pages); err != nil {
			return err
		}
	} else if err = recomputeContentPendingTx(tx, targetContentID); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) RecomputeContentPending(contentID int64) (err error) {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if err = recomputeContentPendingTx(tx, contentID); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) MissingPageIndexes(contentID int64) ([]int, error) {
	var pageCount int
	if err := db.QueryRow(`SELECT page_count FROM contents WHERE id = ?`, contentID).Scan(&pageCount); err != nil {
		return nil, err
	}
	rows, err := db.Query(
		`SELECT page_index FROM pages
		 WHERE content_id = ? AND page_index >= 0 AND page_index < ?
		 ORDER BY page_index`,
		contentID, pageCount,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	present := make(map[int]bool, pageCount)
	for rows.Next() {
		var index int
		if err := rows.Scan(&index); err != nil {
			return nil, err
		}
		present[index] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	missing := make([]int, 0, pageCount-len(present))
	for index := 0; index < pageCount; index++ {
		if !present[index] {
			missing = append(missing, index)
		}
	}
	return missing, nil
}

func (db *DB) MissingPageRanges(contentID int64) ([]PageRange, error) {
	indexes, err := db.MissingPageIndexes(contentID)
	if err != nil {
		return nil, err
	}
	return coalescePageIndexes(indexes), nil
}

func (db *DB) MissingPageRangesWithin(contentID int64, start, end int) ([]PageRange, error) {
	if start < 0 || end <= start {
		return nil, fmt.Errorf("invalid page range %d-%d", start, end)
	}
	indexes, err := db.MissingPageIndexes(contentID)
	if err != nil {
		return nil, err
	}
	within := make([]int, 0, len(indexes))
	for _, index := range indexes {
		if index >= start && index < end {
			within = append(within, index)
		}
	}
	return coalescePageIndexes(within), nil
}

func coalescePageIndexes(indexes []int) []PageRange {
	if len(indexes) == 0 {
		return nil
	}
	ranges := make([]PageRange, 0)
	start, previous := indexes[0], indexes[0]
	for _, index := range indexes[1:] {
		if index == previous+1 {
			previous = index
			continue
		}
		ranges = append(ranges, PageRange{Start: start, End: previous + 1})
		start, previous = index, index
	}
	return append(ranges, PageRange{Start: start, End: previous + 1})
}

func (db *DB) ContentOCRCoverage(contentID int64) (int, []ModelCount, error) {
	rows, err := db.Query(
		`SELECT p.model, COUNT(*)
		 FROM pages p
		 JOIN contents c ON c.id = p.content_id
		 WHERE p.content_id = ?
		   AND p.page_index >= 0 AND p.page_index < c.page_count
		 GROUP BY p.model
		 ORDER BY model IS NOT NULL, model`,
		contentID,
	)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()

	completed := 0
	var models []ModelCount
	for rows.Next() {
		var model sql.NullString
		var count int
		if err := rows.Scan(&model, &count); err != nil {
			return 0, nil, err
		}
		entry := ModelCount{PageCount: count}
		if model.Valid {
			value := model.String
			entry.Model = &value
		}
		models = append(models, entry)
		completed += count
	}
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}
	return completed, models, nil
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
	// Partial canonical rows stay in trigger-backed FTS for simple, durable
	// writes, but pending content is hidden so searches never expose incomplete
	// OCR evidence.
	query := fmt.Sprintf(
		`SELECT d.path, p.page_index, c.page_count,
		        snippet(%[1]s, 0, '>>>', '<<<', '...', 48) as snippet,
		        bm25(%[1]s) as rank, p.model, c.ocr_pending
		 FROM %[1]s
		 JOIN pages p ON p.id = %[1]s.rowid
		 JOIN documents d ON d.content_id = p.content_id
		 JOIN contents c ON c.id = d.content_id
		 WHERE %[1]s MATCH ?
		   AND d.deleted = 0
		   AND c.ocr_pending = 0
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
		var model sql.NullString
		var pending int
		if err := rows.Scan(
			&result.Path, &result.PageIndex, &result.PageCount, &result.Snippet,
			&result.Rank, &model, &pending,
		); err != nil {
			return nil, err
		}
		if model.Valid {
			value := model.String
			result.Model = &value
		}
		result.OCRPending = pending == 1
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
		`SELECT d.path, 0 as page_index, c.page_count, '' as snippet, 0.0 as rank,
		        NULL as model, c.ocr_pending
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
		var model sql.NullString
		var pending int
		if err := rows.Scan(
			&result.Path, &result.PageIndex, &result.PageCount, &result.Snippet,
			&result.Rank, &model, &pending,
		); err != nil {
			return nil, err
		}
		result.OCRPending = pending == 1
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

// OCR-backed reads follow the same all-pages-visible-at-once rule as FTS.
func (db *DB) GetPageMarkdownByPathAndIndex(path string, pageIndex int) (string, error) {
	var markdown string
	err := db.QueryRow(
		`SELECT p.markdown
		 FROM documents d
		 JOIN contents c ON c.id = d.content_id
		 JOIN pages p ON p.content_id = d.content_id
		 WHERE d.path = ?
		   AND d.deleted = 0
		   AND c.ocr_pending = 0
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
		`SELECT p.page_index, p.markdown, p.model
		 FROM documents d
		 JOIN contents c ON c.id = d.content_id
		 JOIN pages p ON p.content_id = d.content_id
		 WHERE d.path = ?
		   AND d.deleted = 0
		   AND c.ocr_pending = 0
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
		var model sql.NullString
		if err := rows.Scan(&pageRecord.PageIndex, &pageRecord.Markdown, &model); err != nil {
			return nil, err
		}
		if model.Valid {
			value := model.String
			pageRecord.Model = &value
		}
		pageRecords = append(pageRecords, pageRecord)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return pageRecords, nil
}

func backfillPageSearchTextTx(tx *sql.Tx) error {
	type pageText struct {
		ID       int64
		Markdown string
	}

	rows, err := tx.Query(`SELECT id, markdown FROM pages ORDER BY id`)
	if err != nil {
		return err
	}
	allPages := make([]pageText, 0)
	for rows.Next() {
		var row pageText
		if err := rows.Scan(&row.ID, &row.Markdown); err != nil {
			_ = rows.Close()
			return err
		}
		allPages = append(allPages, row)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	stmt, err := tx.Prepare(`UPDATE pages SET search_text = ? WHERE id = ?`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, page := range allPages {
		if _, err := stmt.Exec(normalizeSearchText(page.Markdown), page.ID); err != nil {
			return err
		}
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
