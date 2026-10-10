// CXC v0.2.40 (3c1459ac) recall/src/index-search.ts:144-153,272-454.
// Query conditions, storage, metadata and result types remain with their owners.
package recall

import (
	"errors"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

// RRFScore fuses zero-based lane ranks; nil is the oracle's undefined rank.
func RRFScore(rank *float64, weight float64) float64 {
	if rank == nil {
		return 0
	}
	return weight / (RRFK + *rank + 1)
}

// RecencyScore is bounded by one adjacent head-rank gap; future stamps clamp.
func RecencyScore(tsMs *float64, nowMs float64) float64 {
	if tsMs == nil || math.IsNaN(*tsMs) || math.IsInf(*tsMs, 0) {
		return 0
	}
	ageHours := math.Max(0, (nowMs-*tsMs)/3_600_000)
	return RecencyWeight * math.Exp(-math.Ln2*ageHours/RecencyHalfLifeHours)
}

const indexRankRowColumns = `SELECT m.id, m.path, m.ord, m.ts, m.role, m.match_field, m.text,
 f.thread_id, f.cwd, f.source FROM msgs m JOIN files f ON f.path = m.path WHERE `

type indexRankRow = map[string]any

func indexRankRead(db *RwDb, sql string, params ...any) ([]indexRankRow, error) {
	stmt, err := db.Prepare(sql)
	if err != nil {
		return nil, err
	}
	return stmt.All(params...)
}

// indexRankLaneRanks ranks one lane within the rows the query admits: the eligibility predicate
// (role, source, cwd, date, synthetic, tool) is part of the lane's own query, ahead of its LIMIT,
// so rows outside the scope cannot fill the pool and push an older relevant row out of it.
// A missing or errored lane contributes nothing. Other query errors escape.
func indexRankLaneRanks(db *RwDb, table string, words []string, anyMode bool, k float64, eligible string, eligibleParams []any) map[float64]float64 {
	ranks := map[float64]float64{}
	if len(words) == 0 {
		return ranks
	}
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = ftsQuote(w)
	}
	joiner := " AND "
	if anyMode {
		joiner = " OR "
	}
	// table is supplied only by the two fixed lane calls below, never by input; eligible is the
	// fixed-fragment predicate of candidateFilter and its values are bound.
	params := append(append([]any{strings.Join(quoted, joiner)}, eligibleParams...), k)
	rows, err := indexRankRead(db, "SELECT "+table+".rowid AS id FROM "+table+" JOIN msgs m ON m.id = "+table+".rowid JOIN files f ON f.path = m.path WHERE "+table+" MATCH ? AND "+eligible+" ORDER BY bm25("+table+") LIMIT ?", params...)
	if err != nil {
		return ranks
	}
	for i, row := range rows {
		ranks[hitCountNumber(row["id"])] = float64(i)
	}
	return ranks
}

func indexRankLaneScore(ranks map[float64]float64, id, weight float64) float64 {
	rank, ok := ranks[id]
	if !ok {
		return 0
	}
	return RRFScore(&rank, weight)
}

// Array.slice(0,end): truncation toward zero, negative ends and nonfinite values.
func indexRankSliceEnd(length int, end float64) int {
	if math.IsNaN(end) {
		return 0
	}
	end = math.Trunc(end)
	if end < 0 {
		end = math.Max(float64(length)+end, 0)
	}
	return int(math.Min(math.Max(end, 0), float64(length)))
}

type indexRankScoredRow struct {
	id, score float64
	row       indexRankRow
}

func indexRankRows(db *RwDb, opts resolvedQuery) ([]indexRankRow, bool, error) {
	k := planPoolSize(opts.Plan, opts.Limit)
	nowMs := float64(time.Now().UnixMilli())
	if opts.NowMs != nil {
		nowMs = *opts.NowMs
	}
	words, anyMode := laneQuery(opts.Plan)
	triWords := []string{}
	for _, w := range words {
		if utf8.RuneCountInString(w) >= 3 {
			triWords = append(triWords, w)
		}
	}
	eligible, eligibleParams := candidateFilter(opts, false)
	fts := indexRankLaneRanks(db, "msgs_fts", words, anyMode, k, eligible, eligibleParams)
	tri := indexRankLaneRanks(db, "msgs_tri", triWords, anyMode, k, eligible, eligibleParams)
	byID := map[float64]indexRankRow{}
	ids := []float64{}
	laneIDs := map[float64]bool{}
	for id := range fts {
		laneIDs[id] = true
	}
	for id := range tri {
		laneIDs[id] = true
	}
	if len(laneIDs) > 0 {
		where, params := candidateFilter(opts, false)
		for id := range laneIDs {
			params = append(params, id)
		}
		holes := strings.TrimSuffix(strings.Repeat("?,", len(laneIDs)), ",")
		rows, err := indexRankRead(db, indexRankRowColumns+where+" AND m.id IN ("+holes+")", params...)
		if err != nil {
			return nil, false, err
		}
		for _, row := range rows {
			if textMatches(memoryStatusString(row["text"]), opts.Plan) {
				id := hitCountNumber(row["id"])
				if _, exists := byID[id]; !exists {
					ids = append(ids, id)
				}
				byID[id] = row
			}
		}
	}
	// Preserve the oracle's conditional top-up and bounded pool, not an exhaustive scan.
	if float64(len(byID)) < opts.Limit+1 {
		where, params := candidateFilter(opts, true)
		params = append(params, k)
		rows, err := indexRankRead(db, indexRankRowColumns+where+" ORDER BY m.ts DESC LIMIT ?", params...)
		if err != nil {
			return nil, false, err
		}
		for _, row := range rows {
			id := hitCountNumber(row["id"])
			if _, ok := byID[id]; !ok && textMatches(memoryStatusString(row["text"]), opts.Plan) {
				byID[id] = row
				ids = append(ids, id)
			}
		}
	}
	scored := make([]indexRankScoredRow, 0, len(byID))
	for _, id := range ids {
		row := byID[id]
		var ts *float64
		if n, ok := formatDateMs(memoryStatusString(row["ts"])); ok {
			ts = &n
		}
		score := indexRankLaneScore(fts, id, LaneWeightFTS) + indexRankLaneScore(tri, id, LaneWeightTri) + RecencyScore(ts, nowMs)
		scored = append(scored, indexRankScoredRow{id, score, row})
	}
	JSSort(scored, func(a, b indexRankScoredRow) float64 {
		delta := b.score - a.score
		if delta != 0 && !math.IsNaN(delta) {
			return delta
		}
		if c := rolloutCompare(memoryStatusString(a.row["ts"]), memoryStatusString(b.row["ts"])); c != 0 {
			return -float64(c)
		}
		return a.id - b.id
	})
	truncated := float64(len(scored)) > opts.Limit
	rows := make([]indexRankRow, indexRankSliceEnd(len(scored), opts.Limit))
	for i := range rows {
		rows[i] = scored[i].row
		rows[i]["score"] = scored[i].score
	}
	return rows, truncated, nil
}

func indexRankRecentRows(db *RwDb, opts resolvedQuery) ([]indexRankRow, bool, error) {
	where, params := candidateFilter(opts, true)
	params = append(params, planPoolSize(opts.Plan, opts.Limit))
	pool, err := indexRankRead(db, indexRankRowColumns+where+" ORDER BY m.ts DESC LIMIT ?", params...)
	if err != nil {
		return nil, false, err
	}
	rows := []indexRankRow{}
	for _, row := range pool {
		if textMatches(memoryStatusString(row["text"]), opts.Plan) {
			rows = append(rows, row)
		}
	}
	rows = rows[:indexRankSliceEnd(len(rows), opts.Limit+1)]
	truncated := float64(len(rows)) > opts.Limit
	if truncated {
		if opts.Limit < 0 || opts.Limit != math.Trunc(opts.Limit) {
			//lint:ignore ST1005 Exact V8 Array.length error, pinned by the Node oracle.
			return nil, false, errors.New("Invalid array length")
		}
		rows = rows[:int(opts.Limit)]
	}
	return rows, truncated, nil
}

// queryIndex resolves metadata once, chooses ordering, then enriches shared hits.
func queryIndex(db *RwDb, opts IndexQueryOptions) (ChatSearchResult, error) {
	started := time.Now()
	statePath, err := stateDbPath(opts.Home)
	if err != nil {
		return ChatSearchResult{}, err
	}
	meta := loadThreadMeta(statePath)
	query := resolvedQuery{IndexQueryOptions: opts, repoThreadIDs: indexSameOriginThreadIDs(meta, opts.RepoKey), hasRepoKeyColumn: filesHasColumn(db, "repo_key")}
	var rows []indexRankRow
	var truncated bool
	if opts.Order == ChatRecent {
		rows, truncated, err = indexRankRecentRows(db, query)
	} else {
		rows, truncated, err = indexRankRows(db, query)
	}
	if err != nil {
		return ChatSearchResult{}, err
	}
	result := ChatSearchResult{Hits: []ChatHit{}, Warnings: []string{}, Mode: "index"}
	if truncated {
		result.Warnings = append(result.Warnings, "truncated at limit "+memoryNumberText(opts.Limit)+" — raise --limit or narrow the query")
	}
	if meta.Warning != "" {
		result.Warnings = append(result.Warnings, meta.Warning)
	}
	matched := map[string]bool{}
	for _, row := range rows {
		hit := ChatHit{TS: memoryStatusString(row["ts"]), Role: memoryStatusString(row["role"]), Text: memoryStatusString(row["text"]), MatchField: "content", ThreadID: indexRankStringPointer(row["thread_id"]), Cwd: indexRankStringPointer(row["cwd"]), Source: RolloutMain, File: memoryStatusString(row["path"]), Context: []ChatContextEntry{}}
		if row["match_field"] == "tool_log" {
			hit.MatchField = "tool_log"
		}
		switch row["source"] {
		case "subagent":
			hit.Source = RolloutSubagent
		case "unknown":
			hit.Source = RolloutUnknown
		}
		if hit.ThreadID != nil && *hit.ThreadID != "" {
			if tm, ok := meta.ByID[*hit.ThreadID]; ok {
				if tm.Title != "" {
					hit.Title = &tm.Title
				}
				hit.GitBranch = tm.GitBranch
			}
		}
		if score, ok := row["score"].(float64); ok {
			hit.Score = &score
		}
		if opts.ContextN > 0 {
			hit.Context, err = queryIndexContext(db, hit.File, hitCountNumber(row["ord"]), opts.ContextN, opts.IncludeSynthetic)
			if err != nil {
				return ChatSearchResult{}, err
			}
		}
		result.Hits = append(result.Hits, hit)
		matched[hit.File] = true
	}
	counts, err := indexRankRead(db, "SELECT COUNT(*) AS n FROM files")
	if err != nil {
		return ChatSearchResult{}, err
	}
	result.TotalFiles = int(hitCountNumber(counts[0]["n"]))
	result.ScannedFiles = result.TotalFiles
	result.MatchedFiles = len(matched)
	result.ElapsedMs = time.Since(started).Milliseconds()
	return result, nil
}

func indexRankStringPointer(value any) *string {
	if s, ok := value.(string); ok {
		return &s
	}
	return nil
}

// Neighbors filter synthetic users, but neither tool logs nor hit role/cutoff.
func queryIndexContext(db *RwDb, path string, ord, n float64, includeSynthetic bool) ([]ChatContextEntry, error) {
	synth := ""
	if !includeSynthetic {
		synth = " AND synthetic = 0"
	}
	before, err := indexRankRead(db, "SELECT ts, role, text, ord FROM msgs WHERE path = ? AND ord < ?"+synth+" ORDER BY ord DESC LIMIT ?", path, ord, n)
	if err != nil {
		return nil, err
	}
	at, err := indexRankRead(db, "SELECT ts, role, text, ord FROM msgs WHERE path = ? AND ord = ?", path, ord)
	if err != nil {
		return nil, err
	}
	after, err := indexRankRead(db, "SELECT ts, role, text, ord FROM msgs WHERE path = ? AND ord > ?"+synth+" ORDER BY ord ASC LIMIT ?", path, ord, n)
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(before)-1; i < j; i, j = i+1, j-1 {
		before[i], before[j] = before[j], before[i]
	}
	rows := append(append(before, at...), after...)
	out := make([]ChatContextEntry, len(rows))
	for i, row := range rows {
		out[i] = ChatContextEntry{TS: memoryStatusString(row["ts"]), Role: memoryStatusString(row["role"]), Text: memoryStatusString(row["text"]), IsMatch: hitCountNumber(row["ord"]) == ord}
	}
	return out, nil
}
