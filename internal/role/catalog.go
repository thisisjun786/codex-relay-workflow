package role

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// NativeOpenAIModels is the oracle's NATIVE_OPENAI_MODELS, with no package initializer.
// It is a declaration seam, not an allowlist: configured arbitrary IDs remain selectable.
func NativeOpenAIModels() []string {
	return []string{"gpt-5.5", "gpt-5.4", "gpt-5.4-mini", "gpt-5.6-luna"}
}

type ModelSource string
type CatalogState string

const (
	ModelNative        ModelSource  = "native"
	ModelOcx           ModelSource  = "ocx"
	CatalogNative      CatalogState = "native-catalog"
	CatalogOcx         CatalogState = "ocx-active"
	CatalogUnsupported CatalogState = "unsupported-ocx-catalog"
	CatalogUnavailable CatalogState = "unavailable"
)

// A nil pointer omits efforts; a pointer to a nil slice prints null, to an empty slice [].
type CatalogEntry struct {
	ID               string      `json:"id"`
	Source           ModelSource `json:"source"`
	Label            string      `json:"label"`
	ReasoningEfforts *[]string   `json:"reasoningEfforts,omitempty"`
}
type Catalog struct {
	State   CatalogState   `json:"state"`
	Entries []CatalogEntry `json:"entries"`
}
type ProviderCatalogInput struct {
	Mode      string    `json:"mode"`
	OcxModels *[]string `json:"ocxModels,omitempty"`
}
type CatalogDeps struct {
	// ReadNativeEntries produces the native entries with their labels and effort ladders; when set it
	// replaces ReadNativeCache. Neither set reads the process environment's native catalog.
	ReadNativeEntries func() []CatalogEntry
	ReadNativeCache   func() []string
	ProviderStatus    *ProviderCatalogInput
}

func entryKey(raw json.RawMessage) string {
	if s, ok := stringOf(raw); ok {
		return s
	}
	m, _ := members(raw)
	for _, k := range []string{"id", "slug"} {
		if s, ok := stringOf(m[k]); ok && s != "" {
			return s
		}
	}
	return ""
}
func isRoutedSlug(key string) bool { return strings.Contains(key, "/") }

// NativeCatalogPath scans only root-level model_catalog_json. A basic string is read with TOML's
// escapes (tomlBasicString), not JSON's (CRW-1132; the oracle decoded it with JSON.parse); a malformed
// selected key fails closed.
func NativeCatalogPath(env host.LookupEnv) string {
	if env == nil {
		env = os.LookupEnv
	}
	if p, _ := env("CODEX_MODELS_CACHE_PATH"); text.Trim(p) != "" {
		return p
	}
	home, _ := env("CODEX_HOME")
	home = text.Trim(home)
	if home == "" {
		h, err := host.Home(env)
		if err != nil {
			return ""
		}
		home = filepath.Join(h, ".codex")
	}
	b, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return ""
	}
	for _, line := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		if strings.HasPrefix(text.Trim(line), "[") {
			break
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = text.Trim(key)
		if key != "model_catalog_json" && key != "\"model_catalog_json\"" && key != "'model_catalog_json'" {
			continue
		}
		originalValue := value
		value = text.Trim(value)
		if len(value) < 2 || (value[0] != '\'' && value[0] != '"') {
			return ""
		}
		end := 1
		for end < len(value) {
			if value[end] == value[0] {
				break
			}
			if value[0] == '"' && value[end] == '\\' {
				end++
			}
			end++
		}
		if end >= len(value) {
			return ""
		}
		tailRaw := originalValue[strings.Index(originalValue, value)+end+1:]
		tail := text.Trim(tailRaw)
		if tail != "" && tail[0] != '#' {
			return ""
		}
		if tail != "" && strings.ContainsAny(tailRaw[strings.Index(tailRaw, "#")+1:], "\r\u2028\u2029") {
			return "" // JavaScript's comment .* does not consume these line terminators.
		}
		var selected string
		if value[0] == '\'' {
			selected = value[1:end]
		} else if s, ok := tomlBasicString(value[1:end]); ok {
			selected = s
		} else {
			return ""
		}
		if text.Trim(selected) == "" {
			return ""
		}
		if strings.HasPrefix(selected, "~/") || strings.HasPrefix(selected, "~\\") {
			h, e := host.Home(env)
			if e != nil {
				return ""
			}
			rest := selected[2:]
			last := rest
			if last == "" {
				last = h
			}
			trailingSlash := strings.HasSuffix(last, "/")
			selected = filepath.Join(h, rest)
			if selected == "" {
				selected = "."
			}
			if trailingSlash && !strings.HasSuffix(selected, "/") {
				selected += "/"
			}
		}
		if !filepath.IsAbs(selected) {
			selected = filepath.Join(home, selected)
			selected, err = filepath.Abs(selected)
			if err != nil {
				return ""
			}
		}
		return selected
	}
	return filepath.Join(home, "models_cache.json")
}

// newCatalogEntry is the one construction of a catalog entry, for every reader: the ladder is read
// the same way whichever source gave the row (CRW-1132).
func newCatalogEntry(id string, source ModelSource, label string, efforts json.RawMessage) CatalogEntry {
	return CatalogEntry{ID: id, Source: source, Label: label, ReasoningEfforts: reasoningEfforts(efforts)}
}

// nativeEntry is the entry of a native catalog row: a routed slug is OCX-backed, and the label is the ID.
func nativeEntry(id string, efforts json.RawMessage) CatalogEntry {
	source := ModelNative
	if isRoutedSlug(id) {
		source = ModelOcx
	}
	return newCatalogEntry(id, source, id, efforts)
}

func reasoningEfforts(raw json.RawMessage) *[]string {
	var efforts []string
	if len(raw) == 0 || raw[0] != '[' {
		return &efforts
	}
	var rows []json.RawMessage
	if json.Unmarshal(raw, &rows) != nil {
		return &efforts
	}
	efforts = make([]string, 0)
	seen := map[string]bool{}
	for _, v := range rows {
		s, ok := stringOf(v)
		if !ok {
			m, _ := members(v)
			s, ok = stringOf(m["effort"])
		}
		if ok && s != "" && !seen[s] {
			seen[s] = true
			efforts = append(efforts, s)
		}
	}
	return &efforts
}

// ReadNativeCatalog returns nil for unavailable and a non-nil empty slice for an empty catalog.
func ReadNativeCatalog(env host.LookupEnv) []CatalogEntry {
	p := NativeCatalogPath(env)
	if p == "" {
		return nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	b = bytes.Trim(b, " \t\r\n") // JSON.parse accepts JSON whitespace, not all Unicode space.
	if len(b) == 0 {
		return nil
	}
	if b[0] != '[' {
		m, ok := members(b)
		if !ok {
			return nil
		}
		b = m["models"]
	}
	if len(b) == 0 || b[0] != '[' {
		return nil
	}
	var rows []json.RawMessage
	if json.Unmarshal(b, &rows) != nil {
		return nil
	}
	entries := make([]CatalogEntry, 0)
	seen := map[string]bool{}
	for _, raw := range rows {
		id := entryKey(raw)
		m, _ := members(raw)
		visibility, _ := stringOf(m["visibility"])
		if text.Trim(id) == "" || seen[id] || string(m["disabled"]) == "true" || visibility == "hide" {
			continue
		}
		seen[id] = true
		efforts := m["reasoningEfforts"]
		if len(efforts) == 0 || string(efforts) == "null" {
			efforts = m["supported_reasoning_levels"]
		}
		entries = append(entries, nativeEntry(id, efforts))
	}
	return entries
}
func ReadNativeCacheDefault(env host.LookupEnv) []string {
	entries := ReadNativeCatalog(env)
	if entries == nil {
		return nil
	}
	ids := make([]string, len(entries))
	for i, e := range entries {
		ids[i] = e.ID
	}
	return ids
}

// nativeEntries are the native entries of the library catalog: the injected entries, else the injected IDs,
// else the process environment's native catalog. They are deduplicated by exact ID and a blank ID is dropped
// where it is produced, whichever way they arrived (CRW-1132; the oracle trusted an injected list).
func nativeEntries(deps CatalogDeps) []CatalogEntry {
	var in []CatalogEntry
	switch {
	case deps.ReadNativeEntries != nil:
		in = deps.ReadNativeEntries()
	case deps.ReadNativeCache != nil:
		for _, id := range deps.ReadNativeCache() {
			in = append(in, nativeEntry(id, nil))
		}
	default:
		in = ReadNativeCatalog(os.LookupEnv)
	}
	entries := make([]CatalogEntry, 0, len(in))
	seen := map[string]bool{}
	for _, e := range in {
		if text.Trim(e.ID) == "" || seen[e.ID] {
			continue
		}
		seen[e.ID] = true
		entries = append(entries, e)
	}
	return entries
}
func BuildCatalog(deps CatalogDeps) Catalog {
	entries := nativeEntries(deps)
	state := CatalogNative
	if len(entries) == 0 {
		state = CatalogUnavailable
	}
	p := deps.ProviderStatus
	if p == nil || p.Mode != "provider" {
		return Catalog{state, entries}
	}
	if p.OcxModels == nil {
		state = CatalogUnsupported
		for _, e := range entries {
			if e.Source == ModelOcx {
				state = CatalogOcx
				break
			}
		}
		return Catalog{state, entries}
	}
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.ID] = true
	}
	for _, id := range *p.OcxModels {
		if text.Trim(id) == "" || seen[id] {
			continue
		}
		seen[id] = true
		entries = append(entries, newCatalogEntry(id, ModelOcx, id, nil))
	}
	return Catalog{CatalogOcx, entries}
}

// tomlBasicString is the text of a TOML basic string's body (the characters between its quotes): the
// escapes \b \t \n \f \r \" \\ and \e, \uXXXX and \UXXXXXXXX for a Unicode scalar value. A surrogate or
// out-of-range code point, any other escape (JSON's \/ among them) and a control character other than
// a tab are refused, as TOML refuses them. It needs no dependency: only this one string syntax is read.
func tomlBasicString(body string) (string, bool) {
	var out strings.Builder
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c != '\\' {
			if c < 0x20 && c != '\t' || c == 0x7f {
				return "", false
			}
			out.WriteByte(c)
			continue
		}
		i++
		if i >= len(body) {
			return "", false
		}
		switch body[i] {
		case 'b':
			out.WriteByte('\b')
		case 't':
			out.WriteByte('\t')
		case 'n':
			out.WriteByte('\n')
		case 'f':
			out.WriteByte('\f')
		case 'r':
			out.WriteByte('\r')
		case 'e':
			out.WriteByte(0x1b)
		case '"':
			out.WriteByte('"')
		case '\\':
			out.WriteByte('\\')
		case 'u', 'U':
			digits := 4
			if body[i] == 'U' {
				digits = 8
			}
			if i+1+digits > len(body) {
				return "", false
			}
			n, err := strconv.ParseUint(body[i+1:i+1+digits], 16, 32)
			if err != nil || !utf8.ValidRune(rune(n)) {
				return "", false
			}
			out.WriteRune(rune(n))
			i += digits
		default:
			return "", false
		}
	}
	return out.String(), true
}
