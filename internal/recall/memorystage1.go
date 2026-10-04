// CXC v0.2.40 (3c1459ac) recall/src/memory-search.ts:595-789.
package recall

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

const memoryStage1ChatExcerpt = 400

// The file collector's retained IDs suppress only rows already represented by
// a kept file hit. Both strict and relaxed passes use the same match plan.
func (s *memorySearchState) memoryStage1Collect(home string, active []QueryGroup, tally bool) ([]MemoryHit, map[string]bool, error) {
	hits, ids, err := s.memorySearchCollectFiles(active, tally)
	if err != nil {
		return nil, nil, err
	}
	err = s.memoryStage1Search(home, CompileMatchPlan(active, s.words, s.anyMode, s.relax), active, &hits, ids)
	return hits, ids, err
}

func memoryStage1Rows(path, query string, params ...any) ([]map[string]any, error) {
	db, err := openDbReadOnly(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	stmt, err := db.Prepare(query)
	if err != nil {
		return nil, err
	}
	return stmt.All(params...)
}

func memoryStage1Row(row map[string]any) (string, *float64) {
	value := func(v any) string {
		if v == nil {
			return ""
		}
		return memoryStatusString(v)
	}
	body := value(row["raw_memory"]) + "\n" + value(row["rollout_summary"])
	if sec, ok := row["source_updated_at"].(float64); ok {
		ms := sec * 1000
		return body, &ms
	}
	return body, nil
}

func (s *memorySearchState) memoryStage1Old(ms *float64) bool {
	return s.cutoffMs != nil && *s.cutoffMs != 0 && ms != nil && *ms < *s.cutoffMs
}

func (s *memorySearchState) memoryStage1Warning(err error) {
	s.warnings = append(s.warnings, "memories db unreadable ("+err.Error()+")")
}

func (s *memorySearchState) memoryStage1FillPresence(home string) error {
	allPresent := func() bool {
		for _, present := range s.present {
			if !present {
				return false
			}
		}
		return true
	}
	if allPresent() {
		return nil
	}
	path, err := memoriesDbPath(home) // Outside the oracle's caught DB operation.
	if err != nil || path == "" {
		return err
	}
	rows, err := memoryStage1Rows(path, "SELECT raw_memory, rollout_summary, source_updated_at FROM stage1_outputs")
	if err != nil {
		s.memoryStage1Warning(err)
		return nil
	}
	for _, row := range rows {
		body, ms := memoryStage1Row(row)
		if s.memoryStage1Old(ms) {
			continue
		}
		markGroupPresence(Lower(body), s.groups, s.present)
		if allPresent() {
			return nil
		}
	}
	return nil
}

// Terms are values of numbered bindings, never SQL text. SQLite LIKE is only
// a prefilter; PlanMatches below enforces boundaries and the optional quorum.
func memoryStage1Where(plan MatchPlan) (string, []any) {
	groups, join := plan.Required, " AND "
	if plan.AnyMode {
		groups, join = AllGroups(plan), " OR "
	}
	params, conditions := []any{}, []string{}
	for _, group := range groups {
		members := []string{}
		for _, word := range group {
			params = append(params, "%"+word.Text+"%")
			n := strconv.Itoa(len(params))
			members = append(members, "(lower(raw_memory) LIKE ?"+n+" OR lower(rollout_summary) LIKE ?"+n+")")
		}
		condition := "1"
		if len(members) != 0 {
			condition = "(" + strings.Join(members, " OR ") + ")"
		}
		conditions = append(conditions, condition)
	}
	where := strings.Join(conditions, join)
	if where == "" {
		where = "1"
	}
	return where, params
}

func memoryStage1ISO(ms float64) (string, error) {
	if math.IsNaN(ms) || math.IsInf(ms, 0) || math.Abs(ms) > 8_640_000_000_000_000 {
		//lint:ignore ST1005 Exact JavaScript RangeError diagnostic.
		return "", errors.New("Invalid time value")
	}
	d := time.UnixMilli(int64(math.Trunc(ms))).UTC()
	if d.Year() < 0 || d.Year() > 9999 {
		return fmt.Sprintf("%+07d", d.Year()) + d.Format("-01-02T15:04:05.000Z"), nil
	}
	return d.Format("2006-01-02T15:04:05.000Z"), nil
}

func (s *memorySearchState) memoryStage1Search(home string, plan MatchPlan, groups []QueryGroup, candidates *[]MemoryHit, matched map[string]bool) error {
	path, err := memoriesDbPath(home)
	if err != nil || path == "" {
		return err
	}
	where, params := memoryStage1Where(plan)
	rows, err := memoryStage1Rows(path, "SELECT thread_id, raw_memory, rollout_summary, source_updated_at FROM stage1_outputs\n      WHERE "+where+" ORDER BY source_updated_at DESC", params...)
	if err != nil {
		s.memoryStage1Warning(err)
		return nil
	}
	for _, row := range rows {
		var threadID, cwd, updatedAt *string
		id, isString := row["thread_id"].(string)
		if isString {
			threadID = &id
		}
		if id != "" && matched[id] {
			continue
		}
		body, ms := memoryStage1Row(row)
		lower := Lower(body)
		if s.memoryStage1Old(ms) || !PlanMatches(lower, plan) {
			continue
		}
		repoKey := ""
		if id != "" && s.scope != nil {
			if meta, found := s.scope.threadCwd[id]; found {
				cwd = &meta.Cwd
				if meta.GitOriginURL != nil {
					repoKey = normalizeRepoKey(*meta.GitOriginURL)
				}
			}
		}
		keep, bonus := memorySearchScopeAdjust(s.scope, cwd, lower, repoKey)
		if !keep {
			continue
		}
		if ms != nil {
			stamp, err := memoryStage1ISO(*ms)
			if err != nil {
				s.memoryStage1Warning(err)
				return nil // Earlier appended candidates survive the caught error.
			}
			updatedAt = &stamp
		}
		if !isString {
			id = "unknown"
		}
		*candidates = append(*candidates, MemoryHit{Origin: "stage1", Kind: MemoryStage1, Relpath: "stage1_outputs/" + id,
			ThreadID: threadID, UpdatedAt: updatedAt, Cwd: cwd, Excerpt: excerptAround(body, firstPresentMember(lower, groups), 400),
			Score: FinalScore(ScoreChunk(lower, groups, s.lowerPhrase), MemoryStage1, ms, s.nowMs) + bonus})
	}
	return nil
}

func (s *memorySearchState) memoryStage1Backfill(query string, hits []MemoryHit, opts MemorySearchOptions, home string, limit, days float64) []MemoryHit {
	threshold := 0.0
	if opts.ChatFallbackBelow != nil {
		threshold = math.Max(*opts.ChatFallbackBelow, 0)
	}
	if opts.SearchChat == nil || float64(len(hits)) > threshold {
		return hits
	}
	want, context, source := math.Min(limit, 5), 0.0, RolloutMain
	synonyms, tools := opts.Synonyms == nil || *opts.Synonyms, opts.ChatIncludeTools
	chatOpts := ChatSearchOptions{Home: &home, Days: &days, Limit: &want, Context: &context, Source: &source,
		NoRefresh: true, IncludeTools: &tools, ReadOriginUrl: opts.ReadOriginUrl, Synonyms: synonyms, Any: opts.Any}
	if s.scope != nil && s.scope.only {
		chatOpts.Cwd = &s.scope.prefix
	}
	r, err := opts.SearchChat(query, chatOpts)
	if err != nil {
		s.warnings = append(s.warnings, "chat fallback unavailable ("+err.Error()+")")
		return hits
	}
	n := 0 // JS slice truncates fractional bounds and maps NaN to zero.
	if !math.IsNaN(want) {
		n = min(len(r.Hits), int(math.Trunc(want)))
	}
	out := make([]MemoryHit, 0, n)
	for _, hit := range r.Hits[:n] {
		var ms *float64
		if stamp, valid := formatDateMs(hit.TS); valid {
			ms = &stamp
		}
		out = append(out, MemoryHit{Origin: "chat", Kind: MemoryChat, Relpath: hit.File, ThreadID: hit.ThreadID,
			UpdatedAt: &hit.TS, Cwd: hit.Cwd, Excerpt: memorySlice(hit.Text, 0, memoryStage1ChatExcerpt), Score: FinalScore(0, MemoryChat, ms, s.nowMs)})
	}
	if len(out) == 0 {
		return hits
	}
	s.warnings = append(s.warnings, fmt.Sprintf("no memory artifacts matched — %d raw session message(s) shown instead (tool logs excluded)", len(out)))
	return out
}
