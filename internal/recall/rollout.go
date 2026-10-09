// CXC v0.2.40 recall/src/rollout.ts (3c1459ac): read-only discovery and parsing.
package recall

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// RolloutSource distinguishes a main session from a subagent session.
type RolloutSource string

const (
	RolloutMain     RolloutSource = "main"
	RolloutSubagent RolloutSource = "subagent"
	// RolloutUnknown is a file whose first line is too long to read: it cannot be said to be a main
	// or a subagent session, so a main-only search leaves it out.
	RolloutUnknown RolloutSource = "unknown"
)

// RolloutMeta preserves null versus empty string from the first session_meta line.
type RolloutMeta struct {
	ThreadID   *string       `json:"threadId"`
	Cwd        *string       `json:"cwd"`
	Source     RolloutSource `json:"source"`
	Nickname   *string       `json:"nickname"`
	Originator *string       `json:"originator"`
	RepoKey    *string       `json:"repoKey"`
}

// ChatEntry is one ordered message or tool-log entry. TS is the recorded timestamp.
type ChatEntry struct {
	TS         string `json:"ts"`
	Role       string `json:"role"`
	Text       string `json:"text"`
	MatchField string `json:"matchField"`
	Synthetic  bool   `json:"synthetic"`
}

// RolloutFile carries a directory date, or the archive filename's date.
type RolloutFile struct {
	Path string `json:"path"`
	Date string `json:"date"`
}

// SyntheticPrefixes returns the twelve harness-injected user-message prefixes.
func SyntheticPrefixes() []string {
	return []string{"<environment_context>", "<ENVIRONMENT_CONTEXT>", "<skill>", "<subagent_notification>",
		"<turn_aborted>", "<permissions instructions>", "<INSTRUCTIONS>", "<user_instructions>",
		"<system-reminder>", "# AGENTS.md instructions", "## Workspace Context", "[Recent Context]"}
}

// IsSyntheticUserText tests after JavaScript trimStart, without folding prefix case.
func IsSyntheticUserText(s string) bool {
	head := strings.TrimLeftFunc(s, isJSSpace)
	for _, prefix := range SyntheticPrefixes() {
		if strings.HasPrefix(head, prefix) {
			return true
		}
	}
	return false
}

// LocalDateString is YYYY-MM-DD in the process's local zone; years are not padded.
func LocalDateString(d time.Time) string {
	year, month, day := d.In(time.Local).Date()
	return fmt.Sprintf("%d-%02d-%02d", year, month, day)
}

// NormalizeCwd is lexical: it neither cleans dot segments nor resolves links.
func NormalizeCwd(cwd string) string {
	u := strings.TrimRight(strings.ReplaceAll(cwd, "\\", "/"), "/")
	if len(u) >= 8 && strings.ToLower(u[:8]) == "//?/unc/" {
		u = "//" + u[8:]
	} else if strings.HasPrefix(u, "//?/") {
		u = u[4:]
	}
	u = strings.TrimRight(u, "/")
	if len(u) >= 2 && isLower(u[0]) && u[1] == ':' {
		u = strings.ToUpper(u[:1]) + u[1:]
	}
	return u
}

// CanonicalCwdSQL builds the oracle's SQL twin. expr must be programmer-owned SQL,
// never a user value or bind placeholder; callers bind compared path values separately.
func CanonicalCwdSQL(expr string) string {
	slashed := `rtrim(replace(` + expr + `, '\', '/'), '/')`
	stripped := `(CASE WHEN substr(` + slashed + `, 1, 8) LIKE '//?/UNC/' THEN '//' || substr(` + slashed + `, 9) WHEN substr(` + slashed + `, 1, 4) = '//?/' THEN substr(` + slashed + `, 5) ELSE ` + slashed + ` END)`
	trimmed := `rtrim(` + stripped + `, '/')`
	return `(CASE WHEN ` + trimmed + ` GLOB '[a-z]:*' THEN upper(substr(` + trimmed + `, 1, 1)) || substr(` + trimmed + `, 2) ELSE ` + trimmed + ` END)`
}

// CwdMatches is separator-aware prefix matching, case-sensitive unless opted in.
func CwdMatches(sessionCwd, prefix string, caseInsensitive ...bool) bool {
	s, p := NormalizeCwd(sessionCwd), NormalizeCwd(prefix)
	if len(caseInsensitive) != 0 && caseInsensitive[0] {
		s, p = Lower(s), Lower(p)
	}
	return s == p || strings.HasPrefix(s, p+"/")
}

// FoldCwdCaseFor takes Node platform names, as does the oracle.
func FoldCwdCaseFor(platform string) bool { return platform == "darwin" || platform == "win32" }

// FoldCwdCase replaces the oracle's platform-computed module initializer.
func FoldCwdCase() bool { return runtime.GOOS == "darwin" || runtime.GOOS == "windows" }

// DateFromRolloutName checks the filename grammar, not calendar validity.
func DateFromRolloutName(name string) *string {
	if len(name) < 19 || !strings.HasPrefix(name, "rollout-") || name[12] != '-' || name[15] != '-' || name[18] != 'T' {
		return nil
	}
	for _, i := range []int{8, 9, 10, 11, 13, 14, 16, 17} {
		if !isDigit(name[i]) {
			return nil
		}
	}
	d := name[8:18]
	return &d
}

// rolloutCompare is JavaScript's relational string order (UTF-16 code units).
func rolloutCompare(a, b string) int {
	aa, bb := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < min(len(aa), len(bb)); i++ {
		if aa[i] < bb[i] {
			return -1
		}
		if aa[i] > bb[i] {
			return 1
		}
	}
	if len(aa) < len(bb) {
		return -1
	}
	if len(aa) > len(bb) {
		return 1
	}
	return 0
}

// ListRolloutFiles walks live date directories and the flat archive, newest first.
// now is an ordinary clock injection for deterministic callers, not a test-only mode.
//
// Only usable files are listed (a regular file, or a link that reaches one), and what cannot be read
// of one directory leaves that directory out, not the listing. A window that reaches before the
// representable calendar excludes nothing; a directory or archive file whose date is not a calendar
// date has no age to compare, so a window leaves it out, and no window lists it.
func ListRolloutFiles(home string, days float64, now ...time.Time) ([]RolloutFile, error) {
	clock := time.Now()
	if len(now) != 0 {
		clock = now[0]
	}
	cutoff := ""
	if days > 0 {
		ms := float64(clock.UnixMilli()) - days*86_400_000
		if !math.IsInf(ms, 0) && !math.IsNaN(ms) && math.Abs(ms) <= 8_640_000_000_000_000 {
			cutoff = LocalDateString(time.UnixMilli(int64(math.Trunc(ms))))
		}
	}
	inWindow := func(date string) bool {
		if cutoff == "" {
			return true
		}
		_, err := time.Parse("2006-01-02", date)
		return err == nil && rolloutCompare(date, cutoff) >= 0
	}
	out := []RolloutFile{}
	add := func(dir, date string) {
		if !inWindow(date) {
			return
		}
		names, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, entry := range names {
			name := source.DecodeUTF8([]byte(entry.Name()))
			if strings.HasSuffix(name, ".jsonl") && usableDatabase(filepath.Join(dir, entry.Name())) {
				out = append(out, RolloutFile{filepath.Join(dir, name), date})
			}
		}
	}
	root := sessionsDir(home)
	if _, err := os.Stat(root); err == nil {
		for _, year := range safeDirs(root) {
			for _, month := range safeDirs(filepath.Join(root, year)) {
				for _, day := range safeDirs(filepath.Join(root, year, month)) {
					add(filepath.Join(root, year, month, day), year+"-"+month+"-"+day)
				}
			}
		}
	}
	archive := filepath.Join(home, "archived_sessions")
	if _, err := os.Stat(archive); err == nil {
		if names, err := os.ReadDir(archive); err == nil {
			for _, entry := range names {
				name := source.DecodeUTF8([]byte(entry.Name()))
				date := DateFromRolloutName(name)
				if strings.HasSuffix(name, ".jsonl") && date != nil && inWindow(*date) && usableDatabase(filepath.Join(archive, entry.Name())) {
					out = append(out, RolloutFile{filepath.Join(archive, name), *date})
				}
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if c := rolloutCompare(out[i].Date, out[j].Date); c != 0 {
			return c > 0
		}
		return rolloutCompare(filepath.Base(out[i].Path), filepath.Base(out[j].Path)) > 0
	})
	return out, nil
}

func safeDirs(dir string) []string {
	entries, err := os.ReadDir(dir)
	out := []string{}
	if err != nil {
		return out
	}
	for _, entry := range entries {
		if entry.IsDir() {
			out = append(out, source.DecodeUTF8([]byte(entry.Name())))
		}
	}
	return out
}

func readFirstLine(path string) (string, error) {
	line, _, err := readFirstLineBounded(path)
	return line, err
}

// readFirstLineBounded also reports whether the line was cut at the read bound.
func readFirstLineBounded(path string) (line string, truncated bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	const cap = 1_048_576
	for size := 32_768; ; size = min(size*4, cap) {
		buf := make([]byte, size)
		n, err := f.ReadAt(buf, 0)
		if err != nil && err != io.EOF {
			return "", false, err
		}
		head := source.DecodeUTF8(buf[:n])
		if nl := strings.IndexByte(head, '\n'); nl >= 0 {
			return head[:nl], false, nil
		}
		if n < size {
			return head, false, nil
		}
		if size >= cap {
			more := make([]byte, 1)
			extra, _ := f.ReadAt(more, cap)
			return head, extra > 0, nil
		}
	}
}

func rolloutObject(v any) map[string]any { m, _ := v.(map[string]any); return m }
func rolloutStringPointer(v any) *string {
	if s, ok := v.(string); ok {
		return &s
	}
	return nil
}
func rolloutDefault(v, fallback any) any {
	if v == nil {
		return fallback
	}
	return v
}

func rolloutJSON(s string) map[string]any {
	data := []byte(s)
	if !json.Valid(data) {
		return nil
	}
	decoder := json.NewDecoder(strings.NewReader(s))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return nil
	}
	return rolloutObject(value)
}

// ReadRolloutMeta reads only the head. File errors escape; malformed metadata falls back.
func ReadRolloutMeta(path string) (RolloutMeta, error) {
	fallback := RolloutMeta{Source: RolloutMain}
	head, truncated, err := readFirstLineBounded(path)
	if err != nil {
		return fallback, err
	}
	if truncated {
		// The first line is longer than the bound and cannot be read: the file is neither known to be a
		// main session nor a subagent's.
		return RolloutMeta{Source: RolloutUnknown}, nil
	}
	j := rolloutJSON(head)
	if j["type"] != "session_meta" {
		return fallback, nil
	}
	p := rolloutObject(j["payload"])
	meta := RolloutMeta{ThreadID: rolloutStringPointer(p["id"]), Cwd: rolloutStringPointer(p["cwd"]), Source: RolloutMain,
		Nickname: rolloutStringPointer(p["agent_nickname"]), Originator: rolloutStringPointer(p["originator"])}
	_, hasSubagent := rolloutObject(p["source"])["subagent"]
	if p["thread_source"] == "subagent" || hasSubagent {
		meta.Source = RolloutSubagent
	}
	if url, ok := rolloutObject(p["git"])["repository_url"].(string); ok {
		if key := normalizeRepoKey(url); key != "" {
			meta.RepoKey = &key
		}
	}
	return meta, nil
}

// MatchesFilePrefilter shares the message predicate; callers lowercase the raw content.
// JSON-escaped text can still make the oracle prefilter miss a decoded message.
func MatchesFilePrefilter(lowerContent string, plan MatchPlan) bool {
	return !PlanIsEmpty(plan) && PlanMatches(lowerContent, plan)
}

// rolloutString adapts the unexported internal/role jsText: JS String of parsed JSON.
// It retains the oracle's coercion throw rather than silently dropping bad content.
func rolloutString(v any) (string, error) {
	switch v := v.(type) {
	case nil:
		return "null", nil
	case string:
		return v, nil
	case bool:
		return strconv.FormatBool(v), nil
	case json.Number:
		f, _ := strconv.ParseFloat(string(v), 64)
		if math.IsInf(f, 1) {
			return "Infinity", nil
		}
		if math.IsInf(f, -1) {
			return "-Infinity", nil
		}
		if f == 0 {
			return "0", nil
		}
		b, err := json.Marshal(f)
		return string(b), err
	case []any:
		parts := make([]string, len(v))
		for i, item := range v {
			if item == nil {
				continue
			}
			var err error
			if parts[i], err = rolloutString(item); err != nil {
				return "", err
			}
		}
		return strings.Join(parts, ","), nil
	case map[string]any:
		if _, ok := v["toString"]; ok {
			//lint:ignore ST1005 Exact JavaScript coercion error, pinned by the recorded oracle.
			return "", errors.New("Cannot convert object to primitive value")
		}
	}
	return "[object Object]", nil
}

func toolOutputText(output any) (string, error) {
	switch o := output.(type) {
	case string:
		return o, nil
	case []any:
		parts := make([]string, len(o))
		for i, item := range o {
			if s, ok := item.(string); ok {
				parts[i] = s
				continue
			}
			var err error
			if parts[i], err = rolloutString(rolloutDefault(rolloutObject(item)["text"], "")); err != nil {
				return "", err
			}
		}
		return strings.Join(parts, "\n"), nil
	case map[string]any:
		if s, ok := o["content"].(string); ok {
			return s, nil
		}
		if a, ok := o["content"].([]any); ok {
			return toolOutputText(a)
		}
		if s, ok := o["text"].(string); ok {
			return s, nil
		}
	}
	return "", nil
}

// ParseRollout skips unreadable JSON lines. An entry whose text, name, arguments or output is of a type
// that cannot be read as text (the oracle's String coercion throws, ending the whole parse) is dropped
// alone; the entries before and after it are kept.
func ParseRollout(content string, includeTools bool) ([]ChatEntry, error) {
	entries := []ChatEntry{}
lines:
	for _, line := range text.SplitLines(content) {
		// A line can spell its type with escapes (response_\u0069tem), so a line with an escape is parsed.
		if !strings.Contains(line, "response_item") && !strings.Contains(line, `\u`) {
			continue
		}
		j := rolloutJSON(line)
		if j["type"] != "response_item" {
			continue
		}
		p := rolloutObject(j["payload"])
		e := ChatEntry{Role: "tool", MatchField: "tool_log"}
		e.TS, _ = j["timestamp"].(string)
		kind, _ := p["type"].(string)
		if kind == "message" {
			var ok bool
			e.Role, ok = p["role"].(string)
			if !ok {
				e.Role = "unknown"
			}
			items, _ := p["content"].([]any)
			parts := []string{}
			for _, item := range items {
				c := rolloutObject(item)
				if c["type"] != "input_text" && c["type"] != "output_text" {
					continue
				}
				s, err := rolloutString(rolloutDefault(c["text"], ""))
				if err != nil {
					continue lines
				}
				parts = append(parts, s)
			}
			e.Text, e.MatchField = text.Trim(strings.Join(parts, "\n")), "content"
			if e.Text == "" {
				continue
			}
			e.Synthetic = e.Role == "developer" || e.Role == "user" && IsSyntheticUserText(e.Text)
		} else if includeTools && kind == "function_call" {
			name, err := rolloutString(rolloutDefault(p["name"], "tool"))
			if err != nil {
				continue lines
			}
			args, err := rolloutString(rolloutDefault(p["arguments"], ""))
			if err != nil {
				continue lines
			}
			e.Text = text.Trim(name + " " + args)
		} else if includeTools && kind == "function_call_output" {
			s, err := toolOutputText(p["output"])
			if err != nil {
				continue lines
			}
			e.Text = text.Trim(s)
			if e.Text == "" {
				continue
			}
		} else {
			continue
		}
		entries = append(entries, e)
	}
	return entries, nil
}
