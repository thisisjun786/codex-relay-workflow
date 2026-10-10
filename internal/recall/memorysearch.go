package recall

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// ChatSearchFn uses the already-landed chat result and option contracts.
type ChatSearchFn func(string, ChatSearchOptions) (ChatSearchResult, error)

// MemorySearchOptions ports recall/src/memory-search.ts:49-77. A nil SearchChat
// retains memory-only behavior, the CLI's --no-chat integration seam.
type MemorySearchOptions struct {
	Limit             *float64      `json:"limit,omitempty"`
	Days              *float64      `json:"days,omitempty"`
	Any               bool          `json:"any,omitempty"`
	Home              *string       `json:"home,omitempty"`
	Synonyms          *bool         `json:"synonyms,omitempty"`
	NowMs             *float64      `json:"nowMs,omitempty"`
	Cwd               *string       `json:"cwd,omitempty"`
	CwdOnly           bool          `json:"cwdOnly,omitempty"`
	ReadOriginUrl     ReadOriginUrl `json:"-"`
	SearchChat        ChatSearchFn  `json:"-"`
	ChatFallbackBelow *float64      `json:"chatFallbackBelow,omitempty"`
	ChatIncludeTools  bool          `json:"chatIncludeTools,omitempty"`
}

// CwdScope is the per-search path/origin scope from memory-search.ts:292-304.
type CwdScope struct {
	prefix        string
	lowerPrefixes [2]string
	only          bool
	threadCwd     map[string]ThreadMeta
	repoKey       string
}

func memorySearchIsAbsolute(raw string) bool {
	return strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "\\") ||
		len(raw) >= 3 && (raw[0] >= 'A' && raw[0] <= 'Z' || raw[0] >= 'a' && raw[0] <= 'z') && raw[1] == ':' && (raw[2] == '/' || raw[2] == '\\')
}

// Relative paths resolve against the process cwd, not the Codex home. Absolute
// paths keep their recording platform's spelling until NormalizeCwd runs.
func memorySearchBuildCwdScope(home string, opts MemorySearchOptions, warnings *[]string) (*CwdScope, error) {
	if opts.Cwd == nil || text.Trim(*opts.Cwd) == "" {
		return nil, nil
	}
	prefix := *opts.Cwd
	if !memorySearchIsAbsolute(prefix) {
		abs, err := RecallPhysicalAbs(prefix)
		if err != nil {
			return nil, err
		}
		prefix = abs
	}
	prefix = NormalizeCwd(prefix)
	path, err := stateDbPath(home)
	if err != nil {
		return nil, err
	}
	meta := loadThreadMeta(path)
	if meta.Warning != "" {
		*warnings = append(*warnings, meta.Warning)
	}
	lower := Lower(prefix)
	return &CwdScope{prefix, [2]string{lower, strings.ReplaceAll(lower, "/", "\\")}, opts.CwdOnly, meta.ByID, repoKeyForCwd(prefix, opts.ReadOriginUrl)}, nil
}

func memorySearchScopeAdjust(scope *CwdScope, hitCwd *string, lowerText, hitRepoKey string) (bool, float64) {
	if scope == nil {
		return true, 0
	}
	if hitCwd != nil && *hitCwd != "" && CwdMatches(*hitCwd, scope.prefix, FoldCwdCase()) || repoKeysEqual(scope.repoKey, hitRepoKey) {
		return true, CwdBoost
	}
	for _, prefix := range scope.lowerPrefixes {
		if mentionsPath(lowerText, prefix) {
			return true, CwdBoost / 2
		}
	}
	return !scope.only, 0
}

// isPathRune is a character that continues a path name; a mention of a path ends before one that is not.
func isPathRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("_-.~%+@$#/\\", r)
}

// mentionsPath reports whether lowerText names the path prefix as a whole path: the mention does not start inside a longer path, and
// it ends at the end of the text, at a separator (the path's own subpaths), or at a character that cannot continue a name, so
// /proj/here-adjacent and /x/proj/here are not /proj/here, while "/proj/here." at the end of a sentence is (known-defects.md :762).
func mentionsPath(lowerText, prefix string) bool {
	if prefix == "" {
		return false
	}
	for from := 0; from <= len(lowerText); {
		i := strings.Index(lowerText[from:], prefix)
		if i < 0 {
			return false
		}
		start, end := from+i, from+i+len(prefix)
		from = start + 1
		before, _ := utf8.DecodeLastRuneInString(lowerText[:start])
		if start > 0 && isPathRune(before) {
			continue
		}
		if quoted, closed := quotedPath(lowerText, start, before); closed {
			// A quoted path is the text to its closing quote, spaces included: it is this directory, or inside it.
			if quoted == prefix || strings.HasPrefix(quoted, prefix) && (strings.HasSuffix(prefix, "/") || strings.HasSuffix(prefix, "\\") ||
				quoted[len(prefix)] == '/' || quoted[len(prefix)] == '\\') {
				return true
			}
			continue
		}
		if end == len(lowerText) {
			return true
		}
		next, size := utf8.DecodeRuneInString(lowerText[end:])
		switch {
		case next == '/' || next == '\\':
			return true
		case next == '.':
			// A dot ends a sentence when nothing that continues a name follows it.
			if after, _ := utf8.DecodeRuneInString(lowerText[end+size:]); end+size >= len(lowerText) || !isPathRune(after) {
				return true
			}
		case !isPathRune(next):
			return true
		}
	}
	return false
}

// quotedPath is the text from start to the quote that closes the one just before it, on the same line; closed is false when the path is
// not quoted or the quote is never closed.
func quotedPath(lowerText string, start int, before rune) (quoted string, closed bool) {
	if start == 0 || before != '"' && before != '\'' && before != '`' {
		return "", false
	}
	rest := lowerText[start:]
	end := strings.IndexRune(rest, before)
	if end < 0 || strings.ContainsAny(rest[:end], "\r\n") {
		return "", false
	}
	return rest[:end], true
}

type memorySearchState struct {
	root, lowerPhrase string
	files, words      []string
	groups            []QueryGroup
	present           []bool
	anyMode, relax    bool
	cutoffMs          *float64
	nowMs             float64
	scope             *CwdScope
	warnings          []string
	scannedFiles      int
}

func memorySearchISO(ms float64) string {
	return time.UnixMilli(int64(ms)).UTC().Format("2006-01-02T15:04:05.000Z")
}

// Collect recompiles the plan on each pass and returns the retained file thread
// IDs for stage1 deduplication (memory-search.ts:544-559).
func (s *memorySearchState) memorySearchCollectFiles(active []QueryGroup, tally bool) ([]MemoryHit, map[string]bool, error) {
	plan := CompileMatchPlan(active, s.words, s.anyMode, s.relax)
	candidates, matchedThreadIDs := []MemoryHit{}, map[string]bool{}
	s.scannedFiles = 0
	for _, file := range s.files {
		data, readErr := os.ReadFile(file)
		var info os.FileInfo
		var statErr error
		if readErr == nil {
			info, statErr = os.Stat(file)
		}
		if readErr != nil || statErr != nil {
			s.warnings = append(s.warnings, "unreadable memory file: "+file)
			continue
		}
		stamp := float64(info.ModTime().Unix())*1000 + float64(info.ModTime().Nanosecond())/1e6
		if s.cutoffMs != nil && stamp < *s.cutoffMs { // a cutoff of exactly zero still filters (known-defects.md :763)
			continue
		}
		s.scannedFiles++
		content := source.DecodeUTF8(data)
		lowerFile := Lower(content)
		threadID, fileCwd := frontmatterThreadID(content), frontmatterCwd(content)
		repoKey := ""
		if threadID != nil && s.scope != nil {
			if meta, found := s.scope.threadCwd[*threadID]; found {
				if fileCwd == nil {
					fileCwd = &meta.Cwd
				}
				if meta.GitOriginURL != nil {
					repoKey = normalizeRepoKey(*meta.GitOriginURL)
				}
			}
		}
		if tally {
			// Presence is evidence the scoped search could return, so it is judged the way the hits
			// are: paragraph by paragraph. A term in a paragraph the scope rejects adds none, even
			// when another paragraph of the same file mentions the cwd.
			if s.scope == nil {
				markGroupPresence(lowerFile, s.groups, s.present)
			} else {
				for _, chunk := range ParagraphChunks(content) {
					lower := Lower(chunk.Text)
					if keep, _ := memorySearchScopeAdjust(s.scope, fileCwd, lower, repoKey); keep {
						markGroupPresence(lower, s.groups, s.present)
					}
				}
			}
		}
		if !PlanMatches(lowerFile, plan) {
			continue
		}
		relpath, err := filepath.Rel(s.root, file)
		if err != nil {
			return nil, nil, err
		}
		kind := KindOfRelpath(filepath.ToSlash(relpath), "file")
		updatedAt := memorySearchISO(stamp)
		base := MemoryHit{Origin: "file", Kind: kind, Relpath: filepath.ToSlash(relpath), ThreadID: threadID, UpdatedAt: &updatedAt, Cwd: fileCwd}
		keptBefore, paragraphMatches := len(candidates), 0
		for _, chunk := range ParagraphChunks(content) {
			lower := Lower(chunk.Text)
			if !PlanMatches(lower, plan) {
				continue
			}
			paragraphMatches++
			keep, bonus := memorySearchScopeAdjust(s.scope, fileCwd, lower, repoKey)
			if !keep {
				continue
			}
			hit := base
			hit.Excerpt = excerptAround(chunk.Text, firstPresentMember(lower, active), 400)
			hit.StartLine = &chunk.StartLine
			hit.Score = FinalScore(ScoreChunk(lower, active, s.lowerPhrase), kind, &stamp, s.nowMs) + bonus
			candidates = append(candidates, hit)
		}
		// A paragraph that matched and was rejected by scope must not cause a
		// file-span fallback, even if another paragraph mentions the cwd.
		if paragraphMatches == 0 {
			keep, bonus := memorySearchScopeAdjust(s.scope, fileCwd, lowerFile, repoKey)
			if keep {
				hit := base
				hit.Excerpt = excerptAround(strings.Join(text.SplitLines(content), "\n"), firstPresentMember(lowerFile, active), 400)
				line := firstMatchStartLine(content, active)
				hit.StartLine = &line
				hit.Score = FinalScore(ScoreChunk(lowerFile, active, s.lowerPhrase), kind, &stamp, s.nowMs) + bonus
				candidates = append(candidates, hit)
			}
		}
		if threadID != nil && *threadID != "" && len(candidates) > keptBefore {
			matchedThreadIDs[*threadID] = true
		}
	}
	return candidates, matchedThreadIDs, nil
}

// SearchMemory ports memory-search.ts:426-789 at CXC v0.2.40 (3c1459ac).
// Thrown IO errors become Go errors; caught file, database and chat failures
// remain warnings. Stage1 and fallback helpers are in memorystage1.go.
func SearchMemory(query string, opts MemorySearchOptions) (MemorySearchResult, error) {
	started := time.Now().UnixMilli()
	home := ""
	if opts.Home != nil {
		home = *opts.Home
	} else {
		var err error
		home, err = codexHome()
		if err != nil {
			return MemorySearchResult{}, err
		}
	}
	limit, days, nowMs := float64(DefaultMemoryLimit), 0.0, float64(time.Now().UnixMilli())
	if opts.Limit != nil {
		limit = *opts.Limit
	}
	limit = math.Max(limit, 1)
	if opts.Days != nil {
		days = *opts.Days
	}
	if opts.NowMs != nil {
		if err := checkFiniteNowMs(*opts.NowMs); err != nil {
			return MemorySearchResult{}, err
		}
		nowMs = *opts.NowMs
	}
	raw := SplitQueryWordsRaw(query)
	words := DropStopwords(raw)
	groups := ExpandQueryWords(words)
	if opts.Synonyms != nil && !*opts.Synonyms {
		for i, group := range groups {
			groups[i] = group[:1]
		}
	}
	s := memorySearchState{root: memoriesDir(home), words: words, groups: groups, present: make([]bool, len(groups)), anyMode: opts.Any, relax: len(raw) > MaxWords, nowMs: nowMs, lowerPhrase: flatten(Lower(query)), warnings: []string{}}
	if days > 0 {
		cutoff := nowMs - days*86_400_000
		s.cutoffMs = &cutoff
	}
	if len(words) == 0 {
		return MemorySearchResult{Hits: []MemoryHit{}, Warnings: []string{"empty query"}, ElapsedMs: float64(time.Now().UnixMilli() - started)}, nil
	}
	if _, err := os.Stat(s.root); err != nil {
		s.warnings = append(s.warnings, "memories root not found (file search off)")
	}
	dbPath, err := memoriesDbPath(home)
	if err != nil {
		return MemorySearchResult{}, err
	}
	if dbPath == "" {
		s.warnings = append(s.warnings, "memories db not found (stage1 search off)")
	}
	s.files, err = listMarkdownFiles(s.root)
	if err != nil {
		return MemorySearchResult{}, err
	}
	s.scope, err = memorySearchBuildCwdScope(home, opts, &s.warnings)
	if err != nil {
		return MemorySearchResult{}, err
	}
	candidates, _, err := s.memoryStage1Collect(home, groups, true)
	if err != nil {
		return MemorySearchResult{}, err
	}
	if len(candidates) == 0 && HasBoundaryTerm(groups) {
		if err := s.memoryStage1FillPresence(home); err != nil {
			return MemorySearchResult{}, err
		}
		miss := map[int]bool{}
		for i, group := range groups {
			if HasBoundaryTerm([]QueryGroup{group}) && !s.present[i] {
				miss[i] = true
			}
		}
		if len(miss) > 0 {
			candidates, _, err = s.memoryStage1Collect(home, RelaxGroupsAt(groups, miss), false)
			if err != nil {
				return MemorySearchResult{}, err
			}
			if len(candidates) > 0 {
				for i := range candidates {
					candidates[i].Score -= relaxedPenalty
				}
				s.warnings = append(s.warnings, "no word-boundary matches — showing substring matches (lower confidence)")
			}
		}
	}
	hits := RankAndTrim(candidates, limit)
	hits = s.memoryStage1Backfill(query, hits, opts, home, limit, days)
	if len(hits) == 0 && s.scope != nil && s.scope.only {
		s.warnings = append(s.warnings, "no matches inside --cwd-only "+s.scope.prefix+" — retry with --cwd to rank it first instead")
	}
	return MemorySearchResult{Hits: hits, Warnings: s.warnings, ScannedFiles: s.scannedFiles, ElapsedMs: float64(time.Now().UnixMilli() - started)}, nil
}
