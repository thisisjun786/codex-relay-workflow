// CXC v0.2.40 recall/src/chat-search.ts:46-150,269-390: library scan path.
package recall

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
)

const (
	DefaultDays                = 7
	DefaultLimit               = 50
	MaxLimit                   = 200
	RolloutAll   RolloutSource = "all"
)

// ChatOrder is shared with the later index engine; scans always sort by recency.
type ChatOrder string

const (
	ChatRelevance ChatOrder = "relevance"
	ChatRecent    ChatOrder = "recent"
)

// ChatSearchOptions preserves absent numbers/strings separately from explicit zero/empty.
// NowMs, Order, IndexPath and NoRefresh are index options, unused by the scan.
type ChatSearchOptions struct {
	Days             *float64       `json:"days,omitempty"`
	Limit            *float64       `json:"limit,omitempty"`
	Context          *float64       `json:"context,omitempty"`
	Any              bool           `json:"any,omitempty"`
	Synonyms         bool           `json:"synonyms,omitempty"`
	Role             *string        `json:"role,omitempty"`
	Cwd              *string        `json:"cwd,omitempty"`
	Source           *RolloutSource `json:"source,omitempty"`
	IncludeSynthetic bool           `json:"includeSynthetic,omitempty"`
	IncludeTools     *bool          `json:"includeTools,omitempty"`
	Home             *string        `json:"home,omitempty"`
	Scan             bool           `json:"scan,omitempty"`
	NoRefresh        bool           `json:"noRefresh,omitempty"`
	Order            ChatOrder      `json:"order,omitempty"`
	NowMs            *float64       `json:"nowMs,omitempty"`
	IndexPath        *string        `json:"indexPath,omitempty"`
	ReadOriginUrl    ReadOriginUrl  `json:"-"`
}

type ChatContextEntry struct {
	TS      string `json:"ts"`
	Role    string `json:"role"`
	Text    string `json:"text"`
	IsMatch bool   `json:"isMatch"`
}

type ChatHit struct {
	TS         string             `json:"ts"`
	Role       string             `json:"role"`
	Text       string             `json:"text"`
	MatchField string             `json:"matchField"`
	ThreadID   *string            `json:"threadId"`
	Title      *string            `json:"title"`
	Cwd        *string            `json:"cwd"`
	GitBranch  *string            `json:"gitBranch"`
	Source     RolloutSource      `json:"source"`
	File       string             `json:"file"`
	Score      *float64           `json:"score,omitempty"`
	Context    []ChatContextEntry `json:"context"`
}

type ChatIndexInfo struct {
	LastIngestAt *string `json:"lastIngestAt"`
	Files        int     `json:"files"`
	SourceFiles  int     `json:"sourceFiles"`
	StaleFiles   int     `json:"staleFiles"`
	ReadOnly     bool    `json:"readOnly"`
}

type ChatSearchResult struct {
	Hits         []ChatHit      `json:"hits"`
	Warnings     []string       `json:"warnings"`
	ScannedFiles int            `json:"scannedFiles"`
	MatchedFiles int            `json:"matchedFiles"`
	TotalFiles   int            `json:"totalFiles"`
	ElapsedMs    int64          `json:"elapsedMs"`
	Mode         string         `json:"mode"`
	Index        *ChatIndexInfo `json:"index,omitempty"`
}

// chatScanShared is resolved by the future searchChat dispatcher, not by the scan.
type chatScanShared struct {
	Home                  string
	Days, Limit, ContextN float64
	Plan                  MatchPlan
	Source                RolloutSource
	RepoKey               string
}

func groupsForChat(raw []string, synonyms bool) []QueryGroup {
	if synonyms {
		return RelaxQueryGroups(ExpandQueryWords(raw))
	}
	groups := make([]QueryGroup, len(raw))
	for i, word := range raw {
		groups[i] = QueryGroup{{Text: Lower(word)}}
	}
	return groups
}

// ChatMatchPlan disables boundary gating on chat terms, including symbols.
// Relaxation uses the original token count before stopwords are removed.
func ChatMatchPlan(query string, anyMode, synonyms bool) MatchPlan {
	rawAll := SplitQueryWordsRaw(query)
	raw := DropStopwords(rawAll)
	return CompileMatchPlan(groupsForChat(raw, synonyms), raw, anyMode, len(rawAll) > MaxWords)
}

// searchViaScan consumes resolved shared inputs. The variadic clock replaces
// Date.now for deterministic callers; NowMs remains an index-only option.
// A listing error escapes; a file whose head or body cannot be read becomes a warning, and the rest is searched.
func searchViaScan(_ string, opts ChatSearchOptions, shared chatScanShared, clock ...time.Time) (ChatSearchResult, error) {
	now := time.Now
	if len(clock) != 0 {
		now = func() time.Time { return clock[0] }
	}
	started := now().UnixMilli()
	result := ChatSearchResult{Hits: []ChatHit{}, Warnings: []string{}, Mode: "scan"}
	finish := func() (ChatSearchResult, error) {
		result.ElapsedMs = now().UnixMilli() - started
		return result, nil
	}
	if PlanIsEmpty(shared.Plan) {
		result.Warnings = append(result.Warnings, "empty query")
		return finish()
	}
	dbPath, err := stateDbPath(shared.Home)
	if err != nil {
		return ChatSearchResult{}, err
	}
	threadMeta := loadThreadMeta(dbPath)
	if threadMeta.Warning != "" {
		result.Warnings = append(result.Warnings, threadMeta.Warning)
	}
	cutoff, err := chatScanCutoff(now(), shared.Days)
	if err != nil {
		return ChatSearchResult{}, err
	}
	files, err := ListRolloutFiles(shared.Home, shared.Days, now())
	if err != nil {
		return ChatSearchResult{}, err
	}
	result.TotalFiles = len(files)
	includeTools := opts.IncludeTools == nil || *opts.IncludeTools
	truncated := false
	for _, file := range files {
		if float64(len(result.Hits)) >= shared.Limit {
			truncated = true
			break
		}
		meta, err := ReadRolloutMeta(file.Path)
		if err != nil {
			// One file that cannot be read is skipped, with a warning; it does not end the search.
			result.Warnings = append(result.Warnings, "unreadable rollout: "+file.Path+" ("+err.Error()+")")
			continue
		}
		if shared.Source != RolloutAll && meta.Source != shared.Source {
			continue
		}
		if cwd := scanString(opts.Cwd); cwd != "" && !CwdMatches(scanString(meta.Cwd), cwd, FoldCwdCase()) && !repoKeysEqual(shared.RepoKey, scanString(meta.RepoKey)) {
			continue
		}
		result.ScannedFiles++
		content, err := readChatScanFile(file.Path)
		if err != nil {
			result.Warnings = append(result.Warnings, "unreadable rollout: "+file.Path+" ("+err.Error()+")")
			continue
		}
		// The raw text is a prefilter only while it spells what the decoded text would: a file with a
		// \u escape can hide a match from it, so such a file is parsed and judged on the decoded text.
		if !MatchesFilePrefilter(Lower(content), shared.Plan) && !strings.Contains(content, `\u`) {
			continue
		}
		entries, err := ParseRollout(content, includeTools)
		if err != nil {
			return ChatSearchResult{}, err
		}
		visible := make([]ChatEntry, 0, len(entries))
		for _, entry := range entries {
			if opts.IncludeSynthetic || !entry.Synthetic {
				visible = append(visible, entry)
			}
		}
		fileMatched := false
		for i, entry := range visible {
			if float64(len(result.Hits)) >= shared.Limit {
				truncated = true
				break
			}
			if role := scanString(opts.Role); role != "" && entry.Role != role {
				continue
			}
			if cutoff != "" && entry.TS != "" && rolloutCompare(entry.TS, cutoff) < 0 {
				continue
			}
			if !PlanMatches(Lower(entry.Text), shared.Plan) {
				continue
			}
			fileMatched = true
			hit := ChatHit{TS: entry.TS, Role: entry.Role, Text: entry.Text, MatchField: entry.MatchField,
				ThreadID: meta.ThreadID, Cwd: meta.Cwd, Source: meta.Source, File: file.Path, Context: []ChatContextEntry{}}
			if id := scanString(meta.ThreadID); id != "" {
				if tm, ok := threadMeta.ByID[id]; ok {
					if tm.Title != "" {
						hit.Title = &tm.Title
					}
					if hit.Cwd == nil {
						hit.Cwd = &tm.Cwd
					}
					hit.GitBranch = tm.GitBranch
				}
			}
			if shared.ContextN > 0 {
				hit.Context, err = contextWindow(visible, i, shared.ContextN)
				if err != nil {
					return ChatSearchResult{}, err
				}
			}
			result.Hits = append(result.Hits, hit)
		}
		if fileMatched {
			result.MatchedFiles++
		}
	}
	if truncated {
		result.Warnings = append(result.Warnings, "truncated at limit "+strconv.FormatFloat(shared.Limit, 'f', -1, 64)+" — raise --limit or narrow the query")
	}
	// Retain the oracle's cap-before-sort behavior and stable ties.
	sort.SliceStable(result.Hits, func(i, j int) bool { return rolloutCompare(result.Hits[i].TS, result.Hits[j].TS) > 0 })
	return finish()
}

func scanString(value *string) string {
	if value != nil {
		return *value
	}
	return ""
}

func contextWindow(entries []ChatEntry, index int, n float64) ([]ChatContextEntry, error) {
	from := math.Max(0, float64(index)-n)
	to := math.Min(float64(len(entries)-1), float64(index)+n)
	out := []ChatContextEntry{}
	for i := from; i <= to; i++ {
		if i != math.Trunc(i) || i < 0 || i >= float64(len(entries)) {
			//lint:ignore ST1005 Exact V8 property access error, pinned by the oracle.
			return nil, errors.New("Cannot read properties of undefined (reading 'ts')")
		}
		entry := entries[int(i)]
		out = append(out, ChatContextEntry{entry.TS, entry.Role, entry.Text, int(i) == index})
	}
	return out, nil
}

func chatScanCutoff(now time.Time, days float64) (string, error) {
	if !(days > 0) {
		return "", nil
	}
	ms := float64(now.UnixMilli()) - days*86_400_000
	if math.IsNaN(ms) || math.IsInf(ms, 0) || math.Abs(ms) > 8_640_000_000_000_000 {
		//lint:ignore ST1005 Exact JavaScript RangeError message.
		return "", errors.New("Invalid time value")
	}
	d := time.UnixMilli(int64(math.Trunc(ms))).UTC()
	if d.Year() < 0 || d.Year() > 9999 {
		return fmt.Sprintf("%+07d", d.Year()) + d.Format("-01-02T15:04:05.000Z"), nil
	}
	return d.Format("2006-01-02T15:04:05.000Z"), nil
}

// Separate Node's byte-read bound from V8's decoded UTF-16 string bound.
// The unit bound is source-reviewed; the sparse byte refusal is test-covered.
func readChatScanFile(path string) (string, error) {
	const maxBytes = 1<<31 - 1
	const maxUnits = 0x1fffffe8
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if info.Size() > maxBytes {
		return "", fmt.Errorf("file size (%d) is greater than 2 GiB", info.Size())
	}
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxBytes {
		return "", errors.New("file size is greater than 2 GiB")
	}
	decoded := source.DecodeUTF8(data)
	units := 0
	for _, r := range decoded {
		units++
		if r >= 0x10000 {
			units++
		}
		if units > maxUnits {
			//lint:ignore ST1005 Exact V8 string-length error.
			return "", errors.New("Cannot create a string longer than 0x1fffffe8 characters")
		}
	}
	return decoded, nil
}
