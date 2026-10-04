// CXC v0.2.40 (3c1459ac) recall/src/format.ts:14-165.
package recall

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

const (
	excerptCap  = 300
	jsonTextCap = 500
	topicToken  = `\b\d+\.\d+(?:\.\d+)?\b|\b[\w.-]+\.(?:ts|tsx|js|mjs|json|md)\b|\b[A-Z][a-zA-Z]*[A-Z][A-Za-z0-9]*\b`
)

func clip(s string, n int) string {
	flat := strings.Join(strings.FieldsFunc(s, isJSSpace), " ")
	if len(utf16.Encode([]rune(flat))) <= n {
		return flat
	}
	return memorySlice(flat, 0, n) + "…"
}
func chatHitHeader(h ChatHit) string {
	s := fmt.Sprintf("[%s] (%s)", h.TS, h.Role)
	if h.MatchField == "tool_log" {
		s += " [tool_log]"
	}
	if h.Source == RolloutSubagent {
		s += " (subagent)"
	}
	if h.Title != nil && *h.Title != "" {
		s += " «" + clip(*h.Title, 60) + "»"
	}
	if h.Cwd != nil && *h.Cwd != "" {
		s += " {" + *h.Cwd + "}"
	}
	return s
}
func FormatChatResult(r ChatSearchResult) string {
	lines := []string{fmt.Sprintf("# %d hits (%d/%d files scanned, %dms)", len(r.Hits), r.ScannedFiles, r.TotalFiles, r.ElapsedMs)}
	if len(r.Hits) == 0 {
		lines = append(lines, "(no matches)")
	}
	for _, h := range r.Hits {
		lines = append(lines, "", chatHitHeader(h))
		if len(h.Context) > 0 {
			for _, c := range h.Context {
				prefix := "   "
				if c.IsMatch {
					prefix = ">> "
				}
				lines = append(lines, fmt.Sprintf("%s[%s] (%s) %s", prefix, c.TS, c.Role, clip(c.Text, 200)))
			}
		} else {
			lines = append(lines, clip(h.Text, excerptCap))
		}
		lines = append(lines, "---")
	}
	return strings.Join(appendWarnings(lines, r.Warnings), "\n")
}

func AgeDays(updatedAt *string, nowMs float64) *float64 {
	if updatedAt == nil || *updatedAt == "" {
		return nil
	}
	stamp, ok := formatDateMs(*updatedAt)
	if !ok {
		return nil
	}
	age := math.Max(0, math.Floor((nowMs-stamp)/86_400_000))
	return &age
}
func topicTokens(h MemoryHit) []string { return topicTokensWith(h, regexp.MustCompile(topicToken)) }
func topicTokensWith(h MemoryHit, re *regexp.Regexp) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, s := range re.FindAllString(h.Relpath+" "+h.Excerpt, -1) {
		s = Lower(s)
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
func sharesTopic(mine, theirs []string) bool {
	for _, a := range mine {
		for _, b := range theirs {
			if a == b {
				return true
			}
		}
	}
	return false
}
func NewerRelpath(h MemoryHit, hits []MemoryHit) *string {
	return newerRelpathWith(h, hits, regexp.MustCompile(topicToken))
}
func newerRelpathWith(h MemoryHit, hits []MemoryHit, re *regexp.Regexp) *string {
	if h.UpdatedAt == nil || *h.UpdatedAt == "" {
		return nil
	}
	stamp, ok := formatDateMs(*h.UpdatedAt)
	if !ok {
		return nil
	}
	mine := topicTokensWith(h, re)
	if len(mine) == 0 {
		return nil
	}
	var best *string
	for _, other := range hits {
		if other.Relpath == h.Relpath || other.UpdatedAt == nil || *other.UpdatedAt == "" {
			continue
		}
		next, ok := formatDateMs(*other.UpdatedAt)
		if !ok || next <= stamp {
			continue
		}
		if !sharesTopic(mine, topicTokensWith(other, re)) {
			continue
		}
		name := other.Relpath
		best = &name
		stamp = next
	}
	return best
}
func FormatMemoryResult(r MemorySearchResult, nowMs ...float64) string {
	now := float64(time.Now().UnixMilli())
	if len(nowMs) > 0 {
		now = nowMs[0]
	}
	lines := []string{fmt.Sprintf("# %d memory hits (%d files scanned, %sms)", len(r.Hits), r.ScannedFiles, memoryNumberText(r.ElapsedMs))}
	if len(r.Hits) == 0 {
		lines = append(lines, "(no matches)")
	}
	re := regexp.MustCompile(topicToken)
	for _, h := range r.Hits {
		loc := h.Relpath
		if h.StartLine != nil {
			loc += ":" + strconv.Itoa(*h.StartLine)
		}
		when, cwd, ageBit, newerBit := "", "", "", ""
		if h.UpdatedAt != nil && *h.UpdatedAt != "" {
			when = " [" + *h.UpdatedAt + "]"
		}
		if h.Cwd != nil && *h.Cwd != "" {
			cwd = " {" + *h.Cwd + "}"
		}
		if age := AgeDays(h.UpdatedAt, now); age != nil {
			ageBit = " [age: " + memoryNumberText(*age) + "d]"
		}
		if newer := newerRelpathWith(h, r.Hits, re); newer != nil {
			newerBit = " [newer: " + *newer + "]"
		}
		lines = append(lines, "", fmt.Sprintf("(%s/%s) %s%s%s%s%s", h.Origin, h.Kind, loc, when, cwd, ageBit, newerBit), clip(h.Excerpt, excerptCap), "---")
	}
	return strings.Join(appendWarnings(lines, r.Warnings), "\n")
}
func appendWarnings(lines, warnings []string) []string {
	if len(warnings) == 0 {
		return lines
	}
	return append(append(lines, "", "--- warnings ---"), warnings...)
}
func clipField(s string) (string, bool) {
	if len(utf16.Encode([]rune(s))) <= jsonTextCap {
		return s, false
	}
	return memorySlice(s, 0, jsonTextCap) + "…", true
}

type ClippedChatResult struct {
	ChatSearchResult
	Clipped bool `json:"clipped"`
}

func ClipChatResultForJson(r ChatSearchResult) ClippedChatResult {
	out := ClippedChatResult{ChatSearchResult: r}
	out.Hits = make([]ChatHit, len(r.Hits))
	for i, h := range r.Hits {
		var changed bool
		h.Text, changed = clipField(h.Text)
		out.Clipped = out.Clipped || changed
		if h.Title != nil {
			title, truncated := clipField(*h.Title)
			h.Title = &title
			out.Clipped = out.Clipped || truncated
		}
		context := make([]ChatContextEntry, len(h.Context))
		for j, c := range h.Context {
			c.Text, changed = clipField(c.Text)
			out.Clipped = out.Clipped || changed
			context[j] = c
		}
		h.Context = context
		out.Hits[i] = h
	}
	return out
}

// ISO dates use UTC; zone-less date-times and legacy dates use local time.
// Validate before time.Date's rollover and apply ECMAScript's TimeClip range.
func formatDateMs(s string) (float64, bool) {
	re := regexp.MustCompile(`^([+-]\d{6}|\d{4})(?:-(\d{2})(?:-(\d{2}))?)?(?:[Tt](\d{2}):(\d{2})(?::(\d{2})(?:\.(\d+))?)?([Zz]|[+-]\d{2}:?\d{2})?)?$`)
	if m := re.FindStringSubmatch(s); m != nil {
		if m[1] == "-000000" {
			return 0, false
		}
		number := func(i, fallback int) int {
			if m[i] == "" {
				return fallback
			}
			n, _ := strconv.Atoi(m[i])
			return n
		}
		year, month, day := number(1, 0), number(2, 1), number(3, 1)
		hour, minute, second := number(4, 0), number(5, 0), number(6, 0)
		if month < 1 || month > 12 || day < 1 || day > 31 || hour > 24 || minute > 59 || second > 59 {
			return 0, false
		}
		if hour == 24 && (minute != 0 || second != 0 || strings.Trim(m[7], "0") != "") {
			return 0, false
		}
		fraction := m[7] + "000"
		millis, _ := strconv.Atoi(fraction[:3])
		loc := time.UTC
		if m[4] != "" && m[8] == "" {
			loc = time.Local
		}
		if zone := m[8]; len(zone) > 1 {
			raw := strings.ReplaceAll(zone[1:], ":", "")
			hours, _ := strconv.Atoi(raw[:2])
			minutes, _ := strconv.Atoi(raw[2:])
			if hours > 23 || minutes > 59 {
				return 0, false
			}
			offset := (hours*60 + minutes) * 60
			if zone[0] == '-' {
				offset = -offset
			}
			loc = time.FixedZone("", offset)
		}
		return formatTimeClip(formatInLocation(time.Date(year, time.Month(month), day, hour, minute, second, millis*1_000_000, time.UTC), loc))
	}
	return formatLegacyDate(s)
}

// JavaScript picks the earlier occurrence at an overlap and advances a gap.
// Compare possible offsets around the civil date instead of time.Date's choice.
func formatInLocation(civil time.Time, loc *time.Location) time.Time {
	guess := time.Date(civil.Year(), civil.Month(), civil.Day(), civil.Hour(), civil.Minute(), civil.Second(), civil.Nanosecond(), loc)
	var match, after time.Time
	offsets := map[int]bool{}
	for _, delta := range []time.Duration{-48 * time.Hour, 0, 48 * time.Hour} {
		_, offset := guess.Add(delta).Zone()
		if offsets[offset] {
			continue
		}
		offsets[offset] = true
		candidate := civil.Add(-time.Duration(offset) * time.Second)
		local := candidate.In(loc)
		wall := time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), local.Minute(), local.Second(), local.Nanosecond(), time.UTC)
		if wall.Equal(civil) && (match.IsZero() || candidate.Before(match)) {
			match = candidate
		}
		if wall.After(civil) && (after.IsZero() || candidate.Before(after)) {
			after = candidate
		}
	}
	if !match.IsZero() {
		return match
	}
	if !after.IsZero() {
		return after
	}
	return guess
}

func formatTimeClip(at time.Time) (float64, bool) {
	ms := float64(at.Unix())*1000 + float64(at.Nanosecond()/1_000_000)
	return ms, math.Abs(ms) <= 8_640_000_000_000_000
}

func formatLegacyDate(raw string) (float64, bool) {
	s := text.Trim(raw)
	if s == "" {
		return 0, false
	}
	// A malformed ISO time must not turn into a permissive legacy date.
	if len(s) > 10 && s[4] == '-' && s[7] == '-' && (s[10] == 'T' || s[10] == 't') {
		return 0, false
	}
	if len(s) > 10 && s[4] == '-' && s[7] == '-' && s[10] == ' ' {
		if stamp, ok := formatDateMs(s[:10] + "T" + s[11:]); ok {
			return stamp, true
		}
	}
	if strings.HasSuffix(strings.ToUpper(s), "Z") && !strings.Contains(s, ":") {
		if at, e := time.Parse("2006-01-02", s[:len(s)-1]); e == nil {
			return formatTimeClip(at)
		}
	}
	if at := strings.Index(s, " ("); at >= 0 && strings.HasSuffix(s, ")") {
		s = text.Trim(s[:at])
	}
	loc := time.Local
	fields := strings.Fields(s)
	for i, field := range fields {
		for _, zone := range []struct {
			name  string
			hours int
		}{{"UT", 0}, {"UTC", 0}, {"GMT", 0}, {"EST", -5}, {"EDT", -4}, {"CST", -6}, {"CDT", -5}, {"MST", -7}, {"MDT", -6}, {"PST", -8}, {"PDT", -7}} {
			if strings.EqualFold(field, zone.name) {
				name := zone.name
				if name == "UT" {
					name = "GMT"
				}
				fields[i] = name
				loc = time.FixedZone(name, zone.hours*3600)
			}
		}
	}
	s = strings.Join(fields, " ")
	s = strings.ReplaceAll(s, "Sept ", "Sep ")
	for _, layout := range []string{time.RFC1123, time.RFC1123Z, time.RFC822, time.RFC822Z, time.ANSIC, time.UnixDate, time.RFC850, "Mon Jan 02 2006 15:04:05 GMT-0700", "Jan 2 2006", "January 2, 2006", "Jan 2, 2006", "January 2 2006", "2 January 2006", "2 Jan 2006", "Jan 2 2006 15:04", "2006/1/2 15:04:05", "2006/1/2 15:04", "Jan 2 2006 15:04:05 MST", "Jan 2 2006 15:04:05 -0700", "2006-1-2 15:04:05", "2006-1-2 15:04", "2006-01-02", "2006-01", "2006"} {
		if strings.Contains(layout, "MST") || strings.Contains(layout, "Z07") || strings.Contains(layout, "-0700") {
			if at, e := time.ParseInLocation(layout, s, loc); e == nil {
				return formatTimeClip(at)
			}
		} else if civil, e := time.Parse(layout, s); e == nil {
			return formatTimeClip(formatInLocation(civil, loc))
		}
	}
	nums := regexp.MustCompile(`^\d+(?:[-/,.]\d+){0,2}$`)
	if !nums.MatchString(s) {
		return 0, false
	}
	parts := strings.FieldsFunc(s, func(r rune) bool { return strings.ContainsRune("-/,.", r) })
	value := func(i int) int { n, _ := strconv.Atoi(parts[i]); return n }
	year, month, day := 0, 1, 1
	switch len(parts) {
	case 1:
		n := value(0)
		year = n
		switch {
		case n == 0:
			year = 2000
		case n <= 12:
			year = 2001
			month = n
		case n < 32:
			return 0, false
		case n < 50:
			year += 2000
		case n < 100:
			year += 1900
		}
	case 2:
		year = 2001
		month = value(0)
		day = value(1)
	case 3:
		if value(0) > 31 {
			year, month, day = value(0), value(1), value(2)
			if len(parts[0]) <= 2 {
				if year < 50 {
					year += 2000
				} else {
					year += 1900
				}
			}
		} else {
			month, day, year = value(0), value(1), value(2)
			if len(parts[2]) <= 2 {
				if year < 50 {
					year += 2000
				} else {
					year += 1900
				}
			}
		}
	}
	if month < 1 || month > 12 || day < 1 || day > 31 {
		return 0, false
	}
	return formatTimeClip(formatInLocation(time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC), time.Local))
}
