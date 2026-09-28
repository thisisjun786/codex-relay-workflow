package skill

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
)

var bracket = regexp.MustCompile(`^(\[([^\]]*)\])(\s*)`)
var separators = "·:-—|/"
var parentReasons = map[string]string{
	"malformed_role": "invalid", "role_out_of_scope": "unchanged", "binding_unverified": "withhold",
	"no_title_source": "invalid", "malformed_user_title": "invalid", "user_fixed_title": "unchanged",
	"no_family_label": "withhold", "ambiguous_family": "withhold", "family_unverified": "withhold",
	"already_prefixed": "unchanged", "foreign_prefix": "withhold", "prefix_replaced": "apply", "prefix_added": "apply",
}
var parentMatches = []string{"none", "bracket_family", "bracket_body", "bracket_replaced", "bare_label"}
var parentReadback = []string{"verified", "mismatch", "unread"}

type titleRequestError string

func (e titleRequestError) Error() string { return string(e) }

func pyString(value any) string {
	switch typed := value.(type) {
	case nil:
		return "None"
	case bool:
		if typed {
			return "True"
		}
		return "False"
	case string:
		return typed
	case []any:
		parts := make([]string, len(typed))
		for i, item := range typed {
			parts[i] = pyRepr(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, key := range keys {
			parts[i] = pyRepr(key) + ": " + pyRepr(typed[key])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	default:
		return fmt.Sprint(value)
	}
}

func titleResult(decision, reason string, title, prefix, body, stripped any, matched any, requires ...string) map[string]any {
	if requires == nil {
		requires = []string{}
	}
	return map[string]any{"decision": decision, "reason": reason, "title": title, "prefix": prefix, "body": body, "stripped": stripped, "matched": matched, "requires": requires}
}
func titleText(r map[string]any, key string) (any, error) {
	v, ok := r[key]
	if !ok || v == nil {
		return nil, nil
	}
	if _, ok := v.(string); !ok {
		return nil, titleRequestError(key + " must be a string or null")
	}
	return v, nil
}
func titleNames(r map[string]any, key string) ([]string, error) {
	v, ok := r[key]
	if !ok {
		return []string{}, nil
	}
	a, ok := v.([]any)
	if !ok {
		return nil, titleRequestError(key + " must be a list of strings")
	}
	out := make([]string, 0, len(a))
	for _, x := range a {
		s, ok := x.(string)
		if !ok {
			return nil, titleRequestError(key + " must be a list of strings")
		}
		out = append(out, s)
	}
	return out, nil
}
func titleDecide(request any) (map[string]any, error) {
	r, ok := request.(map[string]any)
	if !ok {
		return nil, titleRequestError("the request must be a JSON object")
	}
	rolev, e := titleText(r, "role")
	if e != nil {
		return nil, e
	}
	role, _ := rolev.(string)
	if role != "parent" && role != "supervisor" && role != "child" {
		return titleResult("invalid", "malformed_role", nil, nil, nil, nil, nil, "role as one of parent, supervisor, child"), nil
	}
	if role != "parent" {
		o, e := titleText(r, "observed_title")
		if e != nil {
			return nil, e
		}
		return titleResult("unchanged", "role_out_of_scope", o, nil, o, nil, nil), nil
	}
	if r["binding_verified"] != true {
		o, e := titleText(r, "observed_title")
		if e != nil {
			return nil, e
		}
		return titleResult("withhold", "binding_unverified", o, nil, nil, nil, nil, "management record matching this task ID to this project ID"), nil
	}
	ov, e := titleText(r, "observed_title")
	if e != nil {
		return nil, e
	}
	sv, e := titleText(r, "summary")
	if e != nil {
		return nil, e
	}
	source := ov
	if source == nil || source == "" {
		source = sv
	}
	if source == nil || source == "" {
		return titleResult("invalid", "no_title_source", nil, nil, nil, nil, nil, "observed_title read by task ID, or summary for a new task"), nil
	}
	src := source.(string)
	uv, ok := r["user_title"]
	if !ok {
		uv = "none"
	}
	user, ok := uv.(string)
	if !ok || user != "none" && user != "descriptive" && user != "fixed" {
		return titleResult("invalid", "malformed_user_title", nil, nil, nil, nil, nil, "user_title as one of none, descriptive, fixed"), nil
	}
	if user == "fixed" {
		return titleResult("unchanged", "user_fixed_title", src, nil, src, nil, nil), nil
	}
	families, e := titleNames(r, "family_candidates")
	if e != nil {
		return nil, e
	}
	labels, e := titleNames(r, "project_labels")
	if e != nil {
		return nil, e
	}
	if len(families) == 0 {
		return titleResult("withhold", "no_family_label", ov, nil, nil, nil, nil, "the project's product-family label, or confirmation it has none"), nil
	}
	if len(families) > 1 {
		return titleResult("withhold", "ambiguous_family", ov, nil, nil, nil, nil, "which of "+strings.Join(families, ", ")+" is the product family"), nil
	}
	family := families[0]
	found := false
	for _, s := range labels {
		found = found || s == family
	}
	if !found {
		return titleResult("withhold", "family_unverified", ov, nil, nil, nil, nil, "the candidate read back among this project's current labels"), nil
	}
	body, matched := src, "none"
	var stripped any
	own := "[" + family + "]"
	if src == own {
		return titleResult("unchanged", "already_prefixed", src, family, "", nil, "bracket_family"), nil
	}
	if strings.HasPrefix(src, own) && len(src) > len(own) && startsWithSpace(src[len(own):]) {
		body = strings.TrimLeftFunc(src[len(own):], isSpace)
		if body == "" {
			return titleResult("unchanged", "already_prefixed", src, family, "", nil, "bracket_family"), nil
		}
		matched = "bracket_family"
	} else if m := bracket.FindStringSubmatchIndex(src); m != nil {
		lex := src[m[2]:m[3]]
		removed := src[:m[1]]
		rest := src[m[1]:]
		disp, _ := r["bracket_disposition"].(map[string]any)
		named, _ := disp["bracket"].(string)
		if strings.HasPrefix(named, "[") && strings.HasSuffix(named, "]") && strings.HasPrefix(src, named) {
			lex = named
			rest = src[len(named):]
			gap := len(rest) - len(strings.TrimLeftFunc(rest, isSpace))
			removed = named + rest[:gap]
			rest = strings.TrimLeftFunc(rest, isSpace)
		}
		if strings.TrimFunc(rest, isSpace) == "" {
			return titleResult("withhold", "foreign_prefix", ov, nil, nil, nil, nil, "a title body beside "+lex), nil
		}
		action, _ := disp["action"].(string)
		if named != lex || action != "body" && action != "replace" {
			return titleResult("withhold", "foreign_prefix", ov, nil, nil, nil, nil, "bracket_disposition naming the leading bracket as it appears in the title, read here as "+lex+", as body or replace"), nil
		}
		if action == "replace" {
			stripped = removed
			body = rest
			matched = "bracket_replaced"
		} else {
			matched = "bracket_body"
		}
	} else if strings.HasPrefix(src, family) {
		rest := src[len(family):]
		if startsWithSpace(rest) {
			trim := strings.TrimLeftFunc(rest, isSpace)
			if trim != "" {
				first, width := utf8.DecodeRuneInString(trim)
				if strings.ContainsRune(separators, first) {
					tail := trim[width:]
					if startsWithSpace(tail) {
						b := strings.TrimLeftFunc(tail, isSpace)
						if b != "" {
							stripped = src[:len(src)-len(b)]
							body = b
							matched = "bare_label"
						}
					}
				}
			}
		}
	}
	title := own + " " + body
	if ov != nil && title == ov {
		return titleResult("unchanged", "already_prefixed", title, family, body, nil, matched), nil
	}
	if stripped != nil {
		return titleResult("apply", "prefix_replaced", title, family, body, stripped, matched), nil
	}
	return titleResult("apply", "prefix_added", title, family, body, nil, matched), nil
}
func isSpace(r rune) bool {
	return unicode.IsSpace(r) || r >= '\u001c' && r <= '\u001f'
}
func startsWithSpace(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	return isSpace(r)
}
func emitUnicode(w io.Writer, v any) error {
	var encoded bytes.Buffer
	e := json.NewEncoder(&encoded)
	e.SetEscapeHTML(false)
	e.SetIndent("", "  ")
	if err := e.Encode(v); err != nil {
		return err
	}
	text := encoded.String()
	var unescaped strings.Builder
	for i := 0; i < len(text); i++ {
		if text[i] == '\\' && i+1 < len(text) {
			if strings.HasPrefix(text[i:], `\u2028`) || strings.HasPrefix(text[i:], `\u2029`) {
				unescaped.WriteRune('\u2028' + rune(text[i+5]-'8'))
				i += 5
			} else {
				unescaped.WriteString(text[i : i+2])
				i++
			}
		} else {
			unescaped.WriteByte(text[i])
		}
	}
	_, err := io.WriteString(w, unescaped.String())
	return err
}
func titleRequest(stdin io.Reader, stderr io.Writer) (any, int) {
	raw, err := io.ReadAll(stdin)
	if err != nil {
		fmt.Fprintf(stderr, "Title check failed: %s. Nothing was written.\n", err)
		return nil, 3
	}
	decoded, err := pythonLoads(raw)
	var failure *evidence.PythonError
	if errors.As(err, &failure) && failure.Class == "UnicodeDecodeError" {
		fmt.Fprintln(stderr, err)
		return nil, 1
	}
	if err != nil {
		fmt.Fprintln(stderr, "Unreadable request: "+pythonValueDetail(err))
		return nil, 2
	}
	return orderedPlain(decoded), 0
}
func runParentTitle(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return argparseMissing(stderr, "crw skill parent-title", "command")
	}
	switch args[0] {
	case "decide":
		decoded, failed := titleRequest(stdin, stderr)
		if failed != 0 {
			return failed
		}
		v, e := titleDecide(decoded)
		if e != nil {
			fmt.Fprintln(stderr, "Unreadable request: "+e.Error())
			return 2
		}
		_ = emitUnicode(stdout, v)
		if v["decision"] == "invalid" {
			return 2
		}
		return 0
	case "readback":
		request, failed := titleRequest(stdin, stderr)
		if failed != 0 {
			return failed
		}
		r, _ := request.(map[string]any)
		req, ok := r["requested_title"].(string)
		if !ok {
			fmt.Fprintln(stderr, "Unreadable request: requested_title must be a string")
			return 2
		}
		obs, exists := r["observed_title"]
		if exists && obs != nil {
			if _, ok := obs.(string); !ok {
				fmt.Fprintln(stderr, "Unreadable request: observed_title must be a string or null")
				return 2
			}
		}
		state := "unread"
		if obs != nil {
			state = "mismatch"
			if obs == req {
				state = "verified"
			}
		}
		_ = emitUnicode(stdout, map[string]any{"readback": state})
		if state == "verified" {
			return 0
		}
		return 1
	case "replay":
		return replayTitles(args[1:], stdout, stderr)
	default:
		return invalidChoice(stderr, "crw skill parent-title", "command", args[0], "decide", "readback", "replay")
	}
}
func replayTitles(args []string, stdout, stderr io.Writer) int {
	dir := defaultFixture("titles")
	fixturesFS := bundledSkillFiles
	fixtureDir := dir
	allow := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--fixtures":
			i++
			if i >= len(args) {
				return argparseValue(stderr, "crw skill parent-title replay", "--fixtures")
			}
			dir = args[i]
			fixturesFS = os.DirFS(dir)
			fixtureDir = "."
		case "--allow-unreached":
			allow = true
		default:
			return invalidOption(stderr, "crw skill parent-title replay", args[i])
		}
	}
	paths, _ := fs.Glob(fixturesFS, fixtureDir+"/*.json")
	sort.Strings(paths)
	// parent_title.load_fixtures reads and decodes every fixture before any is
	// replayed: an OSError reaches main (exit 3), a decode error is uncaught.
	fixtures := make([]any, len(paths))
	for i, p := range paths {
		label := "/" + p
		if fixtureDir == "." {
			label = pythonPath(dir + "/" + p)
		}
		decoded, e := pythonReadJSON(fixturesFS, p, label)
		if e != nil {
			var osError *probeOSError
			if errors.As(e, &osError) {
				fmt.Fprintf(stderr, "Title check failed: %s. Nothing was written.\n", osError.detail)
				return 3
			}
			fmt.Fprintln(stderr, e)
			return 1
		}
		fixtures[i] = orderedPlain(decoded)
	}
	if len(paths) == 0 {
		fmt.Fprintf(stderr, "No fixtures under %s; nothing was checked\n", dir)
		return 1
	}
	fail := false
	reasons := map[string]bool{}
	reads := map[string]bool{}
	matches := map[string]bool{}
	for i, p := range paths {
		fixture := fixtures[i]
		f, ok := fixture.(map[string]any)
		if !ok {
			fmt.Fprintln(stderr, pythonAttribute(fixture, "get"))
			return 1
		}
		expected, ok := f["expected"].(map[string]any)
		if !ok {
			fmt.Fprintf(stderr, "%s: no recorded expectation\n", filepath.Base(p))
			fail = true
			continue
		}
		sub, exists := f["subcommand"]
		if !exists {
			sub = "decide"
		}
		if sub == "decide" {
			in := f["input"]
			got, e := titleDecide(in)
			if e != nil {
				fmt.Fprintf(stderr, "%s: request rejected: %s\n", filepath.Base(p), e)
				fail = true
				continue
			}
			reasons[got["reason"].(string)] = true
			if got["matched"] != nil {
				matches[got["matched"].(string)] = true
			}
			for k, want := range expected {
				if !jsonEqual(got[k], want) {
					fmt.Fprintf(stderr, "%s: %s expected %s, got %s\n", filepath.Base(p), k, pyRepr(want), pyRepr(got[k]))
					fail = true
				}
			}
		} else if sub == "readback" {
			in, _ := f["input"].(map[string]any)
			if evidence.Truthy(f["input"]) && in == nil {
				fmt.Fprintln(stderr, pythonAttribute(f["input"], "get"))
				return 1
			}
			state := "unread"
			if in["observed_title"] != nil {
				state = "mismatch"
				if in["observed_title"] == in["requested_title"] {
					state = "verified"
				}
			}
			reads[state] = true
			got := map[string]any{"readback": state}
			for _, key := range sortedKeys(expected) {
				if !jsonEqual(expected[key], got[key]) {
					fmt.Fprintf(stderr, "%s: %s expected %s, got %s\n", filepath.Base(p), key, evidence.Repr(expected[key]), evidence.Repr(got[key]))
					fail = true
				}
			}
		} else {
			fmt.Fprintf(stderr, "%s: unknown subcommand %v\n", filepath.Base(p), pyString(sub))
			fail = true
		}
	}
	fmt.Fprintf(stdout, "Replayed %d title fixtures against their recorded expectations.\n", len(paths))
	var missing []string
	for r := range parentReasons {
		if !reasons[r] {
			missing = append(missing, r)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		fmt.Fprintln(stderr, "No fixture reaches: "+strings.Join(missing, ", "))
	}
	var mr []string
	for _, r := range slices.Sorted(slices.Values(parentReadback)) {
		if !reads[r] {
			mr = append(mr, r)
		}
	}
	if len(mr) > 0 {
		fmt.Fprintln(stderr, "No fixture reaches readback: "+strings.Join(mr, ", "))
	}
	var mm []string
	sortedMatches := append([]string(nil), parentMatches...)
	sort.Strings(sortedMatches)
	for _, m := range sortedMatches {
		if !matches[m] {
			mm = append(mm, m)
		}
	}
	if len(mm) > 0 {
		fmt.Fprintln(stderr, "No fixture reaches the branch: "+strings.Join(mm, ", "))
	}
	fmt.Fprintln(stdout, "Replay compares this module with its fixtures. It is not evidence that any title was written, displayed or read back on a host.")
	if fail || (!allow && (len(missing) > 0 || len(mr) > 0 || len(mm) > 0)) {
		return 1
	}
	return 0
}
