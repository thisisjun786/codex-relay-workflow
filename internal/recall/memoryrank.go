package recall

import (
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf16"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// CXC v0.2.40 recall/src/memory-search.ts:47-218,238-285,363-424 (3c1459ac).
// Search orchestration and sorting are separate ports. These readers operate on
// well-formed UTF-8; a UTF-16 slice splitting an astral pair yields U+FFFD in Go.
const (
	DefaultMemoryLimit = 20
	perFileCap         = 2
	relaxedPenalty     = 2
	CwdBoost           = 2
)

type MemoryKind string

const (
	MemorySummary   MemoryKind = "summary"
	MemoryHandbook  MemoryKind = "handbook"
	MemorySkill     MemoryKind = "skill"
	MemoryExtension MemoryKind = "extension"
	MemoryRaw       MemoryKind = "raw"
	MemoryRollout   MemoryKind = "rollout"
	MemoryStage1    MemoryKind = "stage1"
	MemoryChat      MemoryKind = "chat"
	MemoryOther     MemoryKind = "other"
)

type MemoryHit struct {
	Origin    string     `json:"origin"`
	Kind      MemoryKind `json:"kind"`
	Relpath   string     `json:"relpath"`
	ThreadID  *string    `json:"threadId"`
	UpdatedAt *string    `json:"updatedAt"`
	Excerpt   string     `json:"excerpt"`
	StartLine *int       `json:"startLine"`
	Cwd       *string    `json:"cwd"`
	Score     float64    `json:"score"`
}

type MemorySearchResult struct {
	Hits         []MemoryHit `json:"hits"`
	Warnings     []string    `json:"warnings"`
	ScannedFiles int         `json:"scannedFiles"`
	ElapsedMs    float64     `json:"elapsedMs"`
}

func KindOfRelpath(relpath string, origin ...string) MemoryKind {
	if len(origin) > 0 {
		switch origin[0] {
		case "stage1":
			return MemoryStage1
		case "chat":
			return MemoryChat
		}
	}
	switch {
	case relpath == "memory_summary.md":
		return MemorySummary
	case relpath == "MEMORY.md":
		return MemoryHandbook
	case relpath == "raw_memories.md":
		return MemoryRaw
	case strings.HasPrefix(relpath, "skills/"):
		return MemorySkill
	case strings.HasPrefix(relpath, "extensions/"):
		return MemoryExtension
	case strings.HasPrefix(relpath, "rollout_summaries/"):
		return MemoryRollout
	default:
		return MemoryOther
	}
}

// Accessors replace the oracle's exported tables without startup work or mutable maps.
func KindPriority(kind MemoryKind) float64 {
	switch kind {
	case MemorySummary:
		return 4
	case MemoryHandbook:
		return 3
	case MemorySkill:
		return 2.5
	case MemoryExtension:
		return 2
	case MemoryRaw:
		return 0.5
	case MemoryChat:
		return -1
	case MemoryRollout, MemoryStage1, MemoryOther:
		return 0
	default:
		return math.NaN()
	}
}

func HalfLifeHours(kind MemoryKind) float64 {
	switch kind {
	case MemorySummary, MemoryHandbook, MemorySkill:
		return math.Inf(1)
	case MemoryRollout, MemoryStage1, MemoryOther, MemoryChat:
		return 24 * 7
	case MemoryRaw:
		return 24 * 30
	case MemoryExtension:
		return 24 * 90
	default:
		return math.NaN()
	}
}

func RecencyBoost(kind MemoryKind, updatedAtMs *float64, nowMs float64) float64 {
	halfLife := HalfLifeHours(kind)
	if math.IsInf(halfLife, 1) || updatedAtMs == nil || math.IsNaN(*updatedAtMs) || math.IsInf(*updatedAtMs, 0) {
		return 0
	}
	ageHours := math.Max(0, (nowMs-*updatedAtMs)/3_600_000)
	boost := 1.5 * math.Exp((-math.Ln2*ageHours)/halfLife)
	if (kind == MemoryRollout || kind == MemoryStage1) && ageHours > halfLife*2 {
		return boost - math.Min(2, (ageHours-halfLife*2)/(halfLife*2))
	}
	return boost
}

func FinalScore(textScore float64, kind MemoryKind, updatedAtMs *float64, nowMs float64) float64 {
	return textScore + KindPriority(kind) + RecencyBoost(kind, updatedAtMs, nowMs)
}

func ScoreChunk(lowerText string, groups []QueryGroup, lowerPhrase string) float64 {
	score := 0
	for _, group := range groups {
		bestOcc := 0
		for _, member := range group {
			bestOcc = max(bestOcc, CountTermOccurrences(lowerText, member, 5))
		}
		if bestOcc > 0 {
			score += 2 + bestOcc - 1
		}
	}
	if len(groups) > 1 && lowerPhrase != "" && strings.Contains(lowerText, lowerPhrase) {
		score += 5
	}
	if strings.HasPrefix(lowerText, "#") {
		score++
	}
	return float64(score)
}

func listMarkdownFiles(root string) ([]string, error) {
	out := []string{}
	if _, err := os.Stat(root); err != nil { // existsSync: any failed existence check is false
		return out, nil
	}
	var walk func(string) error
	walk = func(dir string) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			full := filepath.Join(dir, entry.Name())
			if entry.IsDir() {
				if err := walk(full); err != nil {
					return err
				}
			} else if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".md") {
				out = append(out, full)
			}
		}
		return nil
	}
	err := walk(root)
	return out, err
}

// memorySlice is String.slice in UTF-16 units; lone surrogates cross Go's UTF-8 boundary as U+FFFD.
func memorySlice(s string, from, end int) string {
	units := utf16.Encode([]rune(s))
	index := func(n int) int {
		if n < 0 {
			n += len(units)
		}
		return max(0, min(n, len(units)))
	}
	from, end = index(from), index(end)
	if end <= from {
		return ""
	}
	return string(utf16.Decode(units[from:end]))
}

// frontmatterValue is the value on the rest of a frontmatter line: its ends trimmed with the JavaScript whitespace set, one pair of
// matching quotes taken off. A value that is empty is none (known-defects.md :592).
func frontmatterValue(s string) *string {
	if end := strings.IndexFunc(s, isJSLineTerminator); end >= 0 {
		s = s[:end]
	}
	s = strings.TrimFunc(s, isJSSpace)
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		s = strings.TrimFunc(s[1:len(s)-1], isJSSpace)
	}
	if s == "" {
		return nil
	}
	return &s
}

func isJSLineTerminator(r rune) bool { return r == '\n' || r == '\r' || r == '\u2028' || r == '\u2029' }

// frontmatterThreadID is the value of a thread_id key that starts a line within the first 2,000 UTF-16 units. The value is on the line of
// its key: a key with nothing after it does not take the next line (a heading, another key) for its id (known-defects.md :591).
func frontmatterThreadID(content string) *string {
	prefix := memorySlice(content, 0, 2000)
	start := true
	for i, r := range prefix {
		if start && strings.HasPrefix(prefix[i:], "thread_id:") {
			if value := frontmatterValue(prefix[i+len("thread_id:"):]); value != nil {
				return value
			}
		}
		start = isJSLineTerminator(r)
	}
	return nil
}

func frontmatterCwd(content string) *string {
	for _, line := range text.SplitLines(memorySlice(content, 0, 2000)) {
		key, rest, colon := strings.Cut(line, ":")
		if !colon || key == "" || !allBytes(key, func(b byte) bool { return isLower(b) || b == '_' }) {
			return nil
		}
		value := frontmatterValue(rest)
		if value == nil {
			return nil
		}
		if key == "cwd" {
			return value
		}
	}
	return nil
}

type ParagraphChunk struct {
	Text      string `json:"text"`
	StartLine int    `json:"startLine"`
}

func ParagraphChunks(content string) []ParagraphChunk {
	chunks := []ParagraphChunk{}
	lines, buf, start := text.SplitLines(content), []string{}, 1
	for i := 0; i <= len(lines); i++ {
		line := ""
		if i < len(lines) {
			line = lines[i]
		}
		if text.Trim(line) == "" {
			if len(buf) > 0 {
				chunks = append(chunks, ParagraphChunk{strings.Join(buf, "\n"), start})
				buf = []string{}
			}
			start = i + 2
		} else {
			if len(buf) == 0 {
				start = i + 1
			}
			buf = append(buf, line)
		}
	}
	return chunks
}

func groupHit(lowerText string, group QueryGroup) bool {
	return slices.ContainsFunc(group, func(term QueryTerm) bool { return TermIncludes(lowerText, term) })
}

func firstMatchStartLine(content string, groups []QueryGroup) int {
	for i, line := range text.SplitLines(content) {
		lower := Lower(line)
		if slices.ContainsFunc(groups, func(group QueryGroup) bool { return groupHit(lower, group) }) {
			return i + 1
		}
	}
	return 1
}

func markGroupPresence(lowerText string, groups []QueryGroup, present []bool) {
	for i, group := range groups {
		if !present[i] && groupHit(lowerText, group) {
			present[i] = true
		}
	}
}

func firstPresentMember(lowerText string, groups []QueryGroup) QueryTerm {
	for _, group := range groups {
		for _, member := range group {
			if TermIncludes(lowerText, member) {
				return member
			}
		}
	}
	return groups[0][0] // upstream caller guarantees nonempty groups/members
}

// originalUnits maps a position counted in UTF-16 units of the lowercased text to the same position in the original: İ lowercases to two
// units, so every one before the position moves it by one (known-defects.md :593).
func originalUnits(original string, lowerUnits int) int {
	lowered, origin := 0, 0
	for _, r := range original {
		step, own := 1, 1
		if r >= 0x10000 {
			step, own = 2, 2
		} else if r == 0x130 {
			step = 2
		}
		if lowered+step > lowerUnits {
			break
		}
		lowered += step
		origin += own
	}
	return origin
}

func excerptAround(content string, term QueryTerm, span int) string {
	lower := Lower(content)
	at := TermIndexOf(lower, term, 0)
	if at < 0 {
		return memorySlice(content, 0, span)
	}
	at = originalUnits(content, len(utf16.Encode([]rune(lower[:at]))))
	from := max(0, at-int(math.Floor(float64(span)/2)))
	return memorySlice(content, from, from+span)
}
