package skill

import (
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

var cell = regexp.MustCompile("`([^`]+)`")
var bullet = regexp.MustCompile(`^[-*+]\s+`)
var qualified = regexp.MustCompile(`^([a-z_]+)\s*\(([^)]*)\)$`)
var policyFields = []string{"run_mode", "observation_path"}

type vocabulary struct {
	modes, paths []string
	legal        map[[2]string]bool
}

func parseVocabulary(text string) (vocabulary, error) {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		trim := strings.TrimSpace(line)
		if !strings.HasPrefix(trim, "| |") {
			continue
		}
		parts := strings.Split(trim, "|")
		var paths []string
		good := true
		for _, p := range parts[2 : len(parts)-1] {
			m := cell.FindStringSubmatch(p)
			if m == nil {
				good = false
				break
			}
			paths = append(paths, m[1])
		}
		if !good || len(paths) == 0 {
			continue
		}
		v := vocabulary{paths: paths, legal: map[[2]string]bool{}}
		for _, row := range lines[i+2:] {
			row = strings.TrimSpace(row)
			if !strings.HasPrefix(row, "|") {
				break
			}
			cs := strings.Split(row, "|")[1:]
			cs = cs[:len(cs)-1]
			if len(cs) != len(paths)+1 {
				break
			}
			m := cell.FindStringSubmatch(cs[0])
			if m == nil {
				break
			}
			mode := m[1]
			v.modes = append(v.modes, mode)
			for j, p := range paths {
				v.legal[[2]string{mode, p}] = !strings.Contains(cs[j+1], "illegal")
			}
		}
		if len(v.modes) > 0 {
			return v, nil
		}
	}
	return vocabulary{}, fmt.Errorf("no pairing table found in %s", defaultContract("start-policy.md"))
}
func undecorate(s string) string {
	s = strings.TrimSpace(s)
	for {
		old := s
		s = strings.TrimSpace(strings.TrimRight(s, ".,;"))
		if len(s) > 2 && strings.ContainsRune("`\"'*_", rune(s[0])) && s[0] == s[len(s)-1] {
			s = strings.TrimSpace(s[1 : len(s)-1])
		}
		if s == old {
			return s
		}
	}
}
func readPolicy(lines []string) map[string][]string {
	out := map[string][]string{}
	for _, line := range lines {
		text := bullet.ReplaceAllString(strings.TrimSpace(line), "")
		name, value, ok := strings.Cut(text, ":")
		if !ok {
			continue
		}
		name = undecorate(name)
		if m := qualified.FindStringSubmatch(name); m != nil && (m[1] == "run_mode" || m[1] == "observation_path") {
			if name == m[1]+" (superseded)" {
				continue
			}
			out[m[1]] = append(out[m[1]], "unreadable qualifier ("+strings.TrimSpace(m[2])+")")
			continue
		}
		if name == "run_mode" || name == "observation_path" {
			out[name] = append(out[name], undecorate(value))
		}
	}
	return out
}
func checkPolicy(record map[string][]string, v vocabulary, out *[]string) bool {
	failed := false
	decl := map[string][]string{"run_mode": v.modes, "observation_path": v.paths}
	for _, field := range policyFields {
		values := record[field]
		var distinct []string
		seen := map[string]bool{}
		for _, x := range values {
			if !seen[x] {
				seen[x] = true
				distinct = append(distinct, x)
			}
		}
		allowed := strings.Join(decl[field], " | ")
		if len(values) == 0 {
			*out = append(*out, field+": missing -> declared: "+allowed)
			failed = true
		} else if len(distinct) > 1 {
			*out = append(*out, field+": recorded as "+strings.Join(distinct, " and ")+" -> two values, unreadable")
			failed = true
		} else {
			ok := false
			for _, x := range decl[field] {
				ok = ok || x == distinct[0]
			}
			if ok {
				repeat := ""
				if len(values) > 1 {
					repeat = fmt.Sprintf(" (stated %d times, same value)", len(values))
				}
				*out = append(*out, field+": "+distinct[0]+" -> ok"+repeat)
			} else {
				*out = append(*out, field+": "+distinct[0]+" -> not in set; declared: "+allowed)
				failed = true
			}
		}
	}
	if failed {
		*out = append(*out, "pairing: not checked, a field is unreadable", "An unreadable field is re-adjudicated exactly as a missing one is.")
		return false
	}
	pair := [2]string{record["run_mode"][0], record["observation_path"][0]}
	if v.legal[pair] {
		*out = append(*out, "pairing: "+pair[0]+" + "+pair[1]+" -> legal")
		return true
	}
	*out = append(*out, "pairing: "+pair[0]+" + "+pair[1]+" -> illegal; this is a record to repair")
	return false
}
func showVocabulary(v vocabulary, out *[]string) {
	*out = append(*out, "run_mode: "+strings.Join(v.modes, " | "), "observation_path: "+strings.Join(v.paths, " | "), "legal pairings, each one two lines to copy:")
	for _, m := range v.modes {
		for _, p := range v.paths {
			if v.legal[[2]string{m, p}] {
				*out = append(*out, "", "  run_mode: "+m, "  observation_path: "+p)
			}
		}
	}
}

type selfCase struct {
	name  string
	lines []string
	want  bool
}

var selfCases = []selfCase{{"the declared default", []string{"run_mode: goal-free-run", "observation_path: event-driven-idle"}, true}, {"a transitional record", []string{"run_mode: goal-free-run", "observation_path: blocked"}, true}, {"backticked and bulleted", []string{"- `run_mode`: `loop`", "- `observation_path`: `active-observation`"}, true}, {"decorated with trailing punctuation", []string{"`run_mode`: `loop`,", "`observation_path`: `blocked`."}, true}, {"bold markdown", []string{"- **run_mode**: **loop**", "- **observation_path**: **blocked**"}, true}, {"an unmatched marker", []string{"run_mode: loop_", "observation_path: blocked"}, false}, {"a mismatched pair", []string{"run_mode: *loop_", "observation_path: blocked"}, false}, {"a decorated field name", []string{"run_mode: loop", "observation_path_: blocked"}, false}, {"the same value stated twice", []string{"run_mode: loop", "observation_path: blocked", "run_mode: loop"}, true}, {"a stale pair above a current one", []string{"run_mode: goal-free-run", "observation_path: event-driven-idle", "run_mode: blocked", "observation_path: blocked"}, false}, {"parent G", []string{"run_mode: relay_only", "observation_path: relay"}, false}, {"parent H", []string{"run_mode: goal_free", "observation_path: relay"}, false}, {"parent I", []string{"run_mode: run_only", "observation_path: relay"}, false}, {"parent J", []string{"run_mode: relay_only", "observation_path: relay"}, false}, {"a parked parent given a running parent's path", []string{"run_mode: blocked", "observation_path: event-driven-idle"}, false}, {"nothing recorded", []string{"scope: this-run"}, false}, {"a re-adjudicated field keeping its history", []string{"run_mode: goal-free-run", "run_mode (superseded): loop", "observation_path: event-driven-idle"}, true}, {"history on both fields", []string{"run_mode: goal-free-run", "run_mode (superseded): loop", "observation_path: event-driven-idle", "observation_path (superseded): active-observation"}, true}, {"a qualifier this reader does not know", []string{"run_mode: goal-free-run", "run_mode (previous): loop", "observation_path: event-driven-idle"}, false}, {"history marked in the wrong case", []string{"run_mode: goal-free-run", "run_mode (SUPERSEDED): loop", "observation_path: event-driven-idle"}, false}, {"history marked with extra spacing", []string{"run_mode: goal-free-run", "run_mode ( superseded ): loop", "observation_path: event-driven-idle"}, false}}

// startPolicySelftest checks the vocabulary v was read as and replays selfCases against it,
// appending one line per result to out.
func startPolicySelftest(v vocabulary, out *[]string) bool {
	if strings.Join(v.modes, "|") != "goal-free-run|loop|blocked" || strings.Join(v.paths, "|") != "event-driven-idle|active-observation|blocked" {
		*out = append(*out, fmt.Sprintf("vocabulary drifted: parsed %v and %v, expected %v and %v", v.modes, v.paths, []string{"goal-free-run", "loop", "blocked"}, []string{"event-driven-idle", "active-observation", "blocked"}))
		return false
	}
	ok := true
	*out = append(*out, "vocabulary: 3 run modes and 3 observation paths, as declared")
	for _, c := range selfCases {
		var discard []string
		got := checkPolicy(readPolicy(c.lines), v, &discard)
		status := "ok"
		if got != c.want {
			status = "FAILED"
			ok = false
		}
		answer := "rejected"
		if got {
			answer = "accepted"
		}
		*out = append(*out, status+": "+c.name+" -> "+answer)
	}
	return ok
}

// StartPolicySelftest is `crw skill start-policy selftest` over contract, the bytes of a
// start-policy.md, instead of the copy built into crw: the repository's offline contract check
// (`crw-dev ci contracts`) judges the checkout's contract, not the one a binary was built with.
func StartPolicySelftest(contract []byte, stdout, stderr io.Writer) int {
	v, e := parseVocabulary(string(contract))
	if e != nil {
		fmt.Fprintln(stderr, e)
		return 2
	}
	var out []string
	ok := startPolicySelftest(v, &out)
	fmt.Fprintln(stdout, strings.TrimRight(strings.Join(out, "\n"), "\n"))
	if ok {
		return 0
	}
	return 1
}

func runStartPolicy(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return argparseMissing(stderr, "crw skill start-policy", "mode")
	}
	raw, e := fs.ReadFile(bundledSkillFiles, defaultContract("start-policy.md"))
	if e != nil {
		fmt.Fprintf(stderr, "cannot read %s: %s\n", defaultContract("start-policy.md"), e)
		return 2
	}
	v, e := parseVocabulary(string(raw))
	if e != nil {
		fmt.Fprintln(stderr, e)
		return 2
	}
	var out []string
	ok := true
	switch args[0] {
	case "vocabulary":
		showVocabulary(v, &out)
	case "check":
		path := ""
		if len(args) > 2 {
			return invalidOption(stderr, "crw skill start-policy check", strings.Join(args[2:], " "))
		}
		if len(args) == 2 {
			path = args[1]
		}
		data, e := readFileOrStdin(path, stdin)
		if e != nil {
			fmt.Fprintln(stderr, pythonOSErrorLine(e, pythonPath(path)))
			return 1
		}
		text, e := store.DecodeUTF8(data)
		if e != nil {
			fmt.Fprintln(stderr, "UnicodeDecodeError: "+e.Error())
			return 1
		}
		ok = checkPolicy(readPolicy(strings.Split(text, "\n")), v, &out)
	case "selftest":
		ok = startPolicySelftest(v, &out)
	default:
		return invalidChoice(stderr, "crw skill start-policy", "mode", args[0], "vocabulary", "check", "selftest")
	}
	fmt.Fprintln(stdout, strings.TrimRight(strings.Join(out, "\n"), "\n"))
	if ok {
		return 0
	}
	return 1
}
