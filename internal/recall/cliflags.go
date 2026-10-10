// CXC v0.2.40 (3c1459ac) recall/src/cli.ts:29-145; command dispatch is separate.
package recall

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

func Usage() string {
	return strings.Join([]string{
		`crw recall chat search "<query>" [--days N] [--cwd PATH] [--role r] [--source main|subagent|all]`,
		"                           [--limit N] [--context N] [--any] [--all] [--no-tools]",
		"                           [--recent] [--scan] [--no-refresh] [--synonyms] [--json]",
		"crw recall chat index [--rebuild] [--status] [--verify] [--json]",
		`crw recall memory search "<query>" [--days N] [--limit N] [--any] [--no-synonyms]`,
		"                             [--cwd PATH] [--cwd-only PATH] [--no-chat] [--json]",
		"crw recall memory status [--json] [--home PATH]",
		"crw recall memory requeue [--apply] [--include-context-window] [--kind K] [--limit N]",
		"                   [--retries N] [--json] [--home PATH]",
		"",
		fmt.Sprintf("  --days N     restrict to the last N days (chat default %d, 0 = full history)", DefaultDays),
		"  --cwd PATH   scope to PATH and to other checkouts of the same git origin",
		"               (chat: only those sessions; memory: rank those hits first)",
		"  --cwd-only PATH  memory search: drop hits recorded outside that project",
		"  --role r     only messages with this role (user|assistant|tool)",
		"  --source s   main (default) | subagent | all",
		fmt.Sprintf("  --limit N    max hits (chat default %d, memory default %d)", DefaultLimit, DefaultMemoryLimit),
		"  --context N  include N neighbouring messages around each hit",
		"  --any        OR the query words (default: AND)",
		"  --all        include harness-injected synthetic messages",
		"  --no-tools   skip tool call/output (tool_log) matching",
		"  --recent     order by time instead of relevance (index engine default: relevance)",
		"  --rank       order by relevance (the default; accepted for explicitness)",
		"  --no-chat    memory search: do not fall back to raw chat when nothing matches",
		"  --scan       force the raw JSONL scan path (skip the sidecar index)",
		"  --no-refresh skip refresh-on-query ingest (fastest, index may be stale)",
		"  --no-synonyms memory search: raw words only — no ko/en synonyms, no korean stem",
		"  --synonyms   chat search: expand ko/en synonyms + korean stems (default off)",
		"  --json       machine-readable output (text fields clipped at 500 chars)",
		"  --full       with --json: emit unclipped text fields",
		"  --verify     chat index: decide freshness from file content, not only size and mtime (with --status, report without writing)",
		"  --home PATH  search an alternate Codex home (default $CODEX_HOME ?? ~/.codex)",
	}, "\n")
}

type ParsedFlags struct {
	Values      map[string]any `json:"values"`
	Positionals []string       `json:"positionals"`
}

const boolFlagNames = "any all no-tools rank recent scan no-refresh no-synonyms synonyms no-chat full rebuild status json"

// portBoolFlagNames are boolean flags of the port that the oracle does not have. They are accepted,
// but a parse that did not see one does not list it, so the parsed shape of every oracle case is unchanged.
const portBoolFlagNames = "verify"

func WantsHelp(args []string) bool {
	for _, s := range args {
		if s == "--help" || s == "-h" {
			return true
		}
	}
	return false
}
func FlagLikePathError(values map[string]any) error {
	for _, key := range []string{"cwd", "cwd-only", "home", "index-path"} {
		if raw, ok := values[key].(string); ok && strings.HasPrefix(raw, "-") {
			return fmt.Errorf("--%s path must not start with '-': got %s", key, flagJSONQuote(raw))
		}
	}
	return nil
}

// ReadFlags returns the oracle's diagnostic without its terminal newline.
// The command caller owns stderr and exit status; help is checked before this.
func ReadFlags(args []string) (ParsedFlags, error) {
	parsed, err := ParseFlags(args)
	if err != nil {
		return ParsedFlags{}, err
	}
	if err = FlagLikePathError(parsed.Values); err != nil {
		return ParsedFlags{}, err
	}
	return parsed, nil
}
func ExplicitHome(values map[string]any) (*string, error) {
	raw, ok := values["home"].(string)
	if !ok {
		return nil, nil
	}
	if _, err := os.Stat(raw); err != nil {
		return nil, fmt.Errorf("--home not found: %s", raw)
	}
	return &raw, nil
}
func NumFlag(values map[string]any, key string) *float64 {
	raw, ok := values[key].(string)
	if !ok || text.Trim(raw) == "" {
		return nil
	}
	n := hitCountNumber(raw)
	if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
		return nil
	}
	n = math.Floor(n)
	return &n
}
func flagOption(name string) (string, bool) {
	switch name {
	case "d":
		return "days", false
	case "l":
		return "limit", false
	case "c":
		return "context", false
	case "days", "limit", "context", "role", "cwd", "cwd-only", "source", "home", "index-path":
		return name, false
	}
	for _, key := range strings.Fields(boolFlagNames + " " + portBoolFlagNames) {
		if key == name {
			return name, true
		}
	}
	return "", false
}
func missingFlagError(key string) error {
	option := "--" + key
	switch key {
	case "days":
		option = "-d, " + option
	case "limit":
		option = "-l, " + option
	case "context":
		option = "-c, " + option
	}
	//lint:ignore ST1005 Preserve the exact Node parseArgs diagnostic.
	return fmt.Errorf("Option '%s <value>' argument missing", option)
}
func unknownFlagError(raw string, quoted ...string) error {
	example := flagJSONQuote(raw)
	if len(quoted) > 0 {
		example = quoted[0]
	}
	//lint:ignore ST1005 Preserve the exact Node parseArgs diagnostic.
	return fmt.Errorf("Unknown option '%s'. To specify a positional argument starting with a '-', place it at the end of the command after '--', as in '-- %s", raw, example)
}
func ambiguousFlagError(raw, key string) error {
	hint := "'--" + key + "=-XYZ'"
	if !strings.HasPrefix(raw, "--") {
		hint += " or '" + raw + "-XYZ'"
	}
	//lint:ignore ST1005 Preserve the exact Node parseArgs diagnostic.
	return fmt.Errorf("Option '%s' argument is ambiguous.\nDid you forget to specify the option argument for '%s'?\nTo specify an option argument starting with a dash use %s.", raw, raw, hint)
}
func ParseFlags(args []string) (ParsedFlags, error) {
	out := ParsedFlags{Values: map[string]any{}, Positionals: []string{}}
	for i := 0; i < len(args); i++ {
		raw := args[i]
		if raw == "--" {
			out.Positionals = append(out.Positionals, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(raw, "-") || raw == "-" {
			out.Positionals = append(out.Positionals, raw)
			continue
		}
		name, value, attached := "", "", false
		if strings.HasPrefix(raw, "--") {
			name, value, attached = strings.Cut(raw[2:], "=")
			if name != "" {
				raw = "--" + name
			}
		} else {
			name = memorySlice(raw, 1, 2)
			if len(raw) > 2 {
				value = raw[2:]
				attached = true
			}
			raw = memorySlice(raw, 0, 2)
		}
		key, boolean := flagOption(name)
		// Long flags cannot use the single-letter aliases.
		if key == "" || strings.HasPrefix(raw, "--") && (name == "d" || name == "l" || name == "c") {
			if !strings.HasPrefix(args[i], "--") {
				first, _ := utf8.DecodeRuneInString(args[i][1:])
				if first > 0xffff {
					high := utf16.Encode([]rune{first})[0]
					return ParsedFlags{}, unknownFlagError(raw, fmt.Sprintf("\"-\\u%04x\"", high))
				}
			}
			return ParsedFlags{}, unknownFlagError(raw)
		}
		if boolean {
			if attached {
				//lint:ignore ST1005 Preserve the exact Node parseArgs diagnostic.
				return ParsedFlags{}, fmt.Errorf("Option '%s' does not take an argument", raw)
			}
			out.Values[key] = true
			continue
		}
		if !attached {
			if i+1 >= len(args) {
				return ParsedFlags{}, missingFlagError(key)
			}
			i++
			value = args[i]
			if value != "-" && strings.HasPrefix(value, "-") {
				return ParsedFlags{}, ambiguousFlagError(raw, key)
			}
		}
		out.Values[key] = value
	}
	for _, key := range strings.Fields(boolFlagNames) {
		if _, ok := out.Values[key]; !ok {
			out.Values[key] = false
		}
	}
	return out, nil
}

// JSON.stringify does not HTML-escape or escape Unicode line separators.
func flagJSONQuote(s string) string {
	var b bytes.Buffer
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(s)
	in := bytes.TrimSuffix(b.Bytes(), []byte{'\n'})
	out := make([]byte, 0, len(in))
	for i := 0; i < len(in); i++ {
		switch {
		case bytes.HasPrefix(in[i:], []byte(`\u2028`)):
			out = append(out, "\u2028"...)
			i += 5
		case bytes.HasPrefix(in[i:], []byte(`\u2029`)):
			out = append(out, "\u2029"...)
			i += 5
		default:
			out = append(out, in[i])
			if in[i] == '\\' && i+1 < len(in) {
				i++
				out = append(out, in[i])
			}
		}
	}
	return string(out)
}
