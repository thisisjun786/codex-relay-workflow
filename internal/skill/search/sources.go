package search

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

const (
	JAWRegistryURL   = "https://raw.githubusercontent.com/lidge-jun/cli-jaw-skills/main/registry.json"
	JAWRawBase       = "https://raw.githubusercontent.com/lidge-jun/cli-jaw-skills/main"
	HermesCatalogURL = "https://raw.githubusercontent.com/NousResearch/hermes-agent/main/website/docs/reference/skills-catalog.md"
	HermesRawBase    = "https://raw.githubusercontent.com/NousResearch/hermes-agent/main/skills"
	ClawhubAPIBase   = "https://clawhub.ai/api/v1"
)

func catalogText(fetch FetchText, url string) (string, error) {
	body, err := fetch(url)
	if err != nil {
		return "", err
	}
	if len(body) > MaxBodyBytes {
		return "", fmt.Errorf("catalog body exceeds %d bytes", MaxBodyBytes)
	}
	if !utf8.ValidString(body) {
		return "", fmt.Errorf("catalog body is not UTF-8")
	}
	return body, nil
}
func catalogJSON(fetch FetchText, url string) (pyjson.Object, error) {
	body, err := catalogText(fetch, url)
	if err != nil {
		return nil, err
	}
	value, err := pyjson.Loads(body, pyjson.LoadOptions{})
	if err != nil {
		return nil, err
	}
	obj, ok := value.(pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("catalog must be an object")
	}
	return obj, nil
}
func stringPointer(value any) *string {
	s, ok := value.(string)
	if !ok {
		return nil
	}
	return &s
}
func stringDefault(value any, fallback string) string {
	if s, ok := value.(string); ok {
		return s
	}
	return fallback
}

// NormalizeRequires matches the oracle's string-only array filtering, including
// present empty arrays. Other metadata is ignored, not executed or interpreted.
func NormalizeRequires(raw any) *Requires {
	obj, ok := raw.(pyjson.Object)
	if !ok {
		return nil
	}
	pick := func(key string) []string {
		items, ok := obj.Get(key).([]any)
		if !ok {
			return nil
		}
		out := []string{}
		for _, x := range items {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	r := Requires{Bins: pick("bins"), Env: pick("env"), System: pick("system")}
	if r.Bins == nil && r.Env == nil && r.System == nil {
		return nil
	}
	return &r
}

// FetchJawRows keeps Object.entries order: canonical uint32 indices first, then
// insertion order. Duplicate JSON keys retain the last value, first position.
func FetchJawRows(fetch FetchText) ([]SkillRow, error) {
	doc, err := catalogJSON(fetch, JAWRegistryURL)
	if err != nil {
		return nil, err
	}
	rows := []SkillRow{}
	raw := doc.Get("skills")
	if raw == nil {
		return rows, nil
	}
	entries, ok := raw.(pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("jaw skills must be an object")
	}
	entries = slices.Clone(entries)
	slices.SortStableFunc(entries, func(a, b pyjson.Field) int {
		ai, aok := arrayIndex(a.Key)
		bi, bok := arrayIndex(b.Key)
		if aok && bok {
			return int(int64(ai) - int64(bi))
		}
		if aok {
			return -1
		}
		if bok {
			return 1
		}
		return 0
	})
	for _, field := range entries {
		if !safeComponent(field.Key) {
			return nil, fmt.Errorf("invalid jaw skill id")
		}
		meta, ok := field.Value.(pyjson.Object)
		if !ok {
			return nil, fmt.Errorf("jaw skill metadata must be an object")
		}
		entry := stringDefault(meta.Get("entry"), field.Key+"/SKILL.md")
		if !safePath(entry) {
			return nil, fmt.Errorf("invalid jaw entry path")
		}
		rows = append(rows, SkillRow{ID: field.Key, Source: SourceJaw, Name: stringDefault(meta.Get("name"), field.Key), Description: stringDefault(meta.Get("description"), ""), DescriptionKo: stringPointer(meta.Get("desc_ko")), Category: stringPointer(meta.Get("category")), RawURL: JAWRawBase + "/" + entry, SupersededBy: stringPointer(meta.Get("superseded_by")), Status: stringPointer(meta.Get("status")), Requires: NormalizeRequires(meta.Get("requires"))})
	}
	return rows, nil
}
func arrayIndex(s string) (uint64, bool) {
	n, err := strconv.ParseUint(s, 10, 32)
	return n, err == nil && n < 0xffffffff && strconv.FormatUint(n, 10) == s
}

// JS whitespace is wider than RE2's \s. Compile here to do no work at startup.
func FetchHermesRows(fetch FetchText) ([]SkillRow, error) {
	body, err := catalogText(fetch, HermesCatalogURL)
	if err != nil {
		return nil, err
	}
	const space = `[\t\n\v\f\r \x{00A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}\x{FEFF}]`
	line := regexp.MustCompile(`^\|` + space + "*\\[`([^`]+)`\\]\\([^)]*\\)" + space + `*\|` + space + "*(.+?)" + space + `*\|` + space + "*`([^`]+)`" + space + `*\|` + space + `*$`)
	rows := []SkillRow{}
	for _, raw := range strings.Split(body, "\n") {
		m := line.FindStringSubmatch(text.Trim(raw))
		if m == nil {
			continue
		}
		if !safeComponent(m[1]) || !safePath(m[3]) {
			return nil, fmt.Errorf("invalid hermes skill id or path")
		}
		row := SkillRow{ID: m[1], Source: SourceHermes, Name: m[1], Description: m[2], RawURL: HermesRawBase + "/" + m[3] + "/SKILL.md"}
		if category, _, ok := strings.Cut(m[3], "/"); ok {
			row.Category = &category
		}
		rows = append(rows, row)
	}
	return rows, nil
}
func SearchClawhubRows(fetch FetchText, query string) ([]SkillRow, error) {
	doc, err := catalogJSON(fetch, ClawhubAPIBase+"/search?q="+encodeURIComponent(query)+"&limit=20")
	if err != nil {
		return nil, err
	}
	rows := []SkillRow{}
	raw := doc.Get("results")
	if raw == nil {
		return rows, nil
	}
	results, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("clawhub results must be an array")
	}
	seen := map[string]bool{}
	for _, value := range results {
		meta, ok := value.(pyjson.Object)
		if !ok {
			return nil, fmt.Errorf("clawhub result must be an object")
		}
		slug, ok := meta.Get("slug").(string)
		if !ok || slug == "" || seen[slug] {
			continue
		}
		if !safeComponent(slug) {
			return nil, fmt.Errorf("invalid clawhub slug")
		}
		seen[slug] = true
		name, description := slug, ""
		for _, key := range []string{"displayName", "summary"} {
			if value := meta.Get(key); value != nil {
				s, ok := value.(string)
				if !ok {
					return nil, fmt.Errorf("clawhub %s must be text", key)
				}
				if key == "displayName" {
					name = s
				} else {
					description = s
				}
			}
		}
		rows = append(rows, SkillRow{ID: slug, Source: SourceClawhub, Name: name, Description: description, RawURL: ClawhubAPIBase + "/packages/" + encodeURIComponent(slug) + "/file?path=SKILL.md"})
	}
	return rows, nil
}
func safeComponent(s string) bool {
	if s == "" || s == "." || s == ".." || strings.HasPrefix(s, "-") {
		return false
	}
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-' && r != '.' {
			return false
		}
	}
	return true
}
func safePath(s string) bool {
	for _, part := range strings.Split(s, "/") {
		if !safeComponent(part) {
			return false
		}
	}
	return true
}
func encodeURIComponent(s string) string {
	const hex = "0123456789ABCDEF"
	var out strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-_.!~*'()", rune(c)) {
			out.WriteByte(c)
		} else {
			out.WriteByte('%')
			out.WriteByte(hex[c>>4])
			out.WriteByte(hex[c&15])
		}
	}
	return out.String()
}
