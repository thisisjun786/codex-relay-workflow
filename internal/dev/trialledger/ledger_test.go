//go:build dev

package trialledger

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/pyload"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// The goldens (testdata/golden, internal/testsupport/golden) were first captured from the Python
// ledger, `python3 scripts/trial_startup.py ledger --start <start.json>` under CPython 3.14.4
// (the relay host's interpreter), over the cases in testdata/cases.json materialized as
// materialize does here; the same grader reproduced every ledger-grade.json on the relay host
// byte for byte. TestFromISOFormatIsCPythons's golden was first datetime.datetime.fromisoformat's
// answer to each string of testdata/fixtures/fromisoformat-inputs.json, from the same interpreter.

type ledgerCase struct {
	Name   string   `json:"name"`
	Start  *string  `json:"start"`
	Ledger *string  `json:"ledger"`
	Setup  []string `json:"setup"`
	Arg    *string  `json:"arg"`
}

func loadCases(t *testing.T) []ledgerCase {
	t.Helper()
	raw, err := os.ReadFile("testdata/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []ledgerCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	// encoding/json turns a lone surrogate into U+FFFD; the cases need the one Python wrote.
	decoded, err := hook.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	for i, item := range decoded.([]any) {
		c := item.(hook.Object)
		for key, into := range map[string]**string{"start": &cases[i].Start, "ledger": &cases[i].Ledger, "arg": &cases[i].Arg} {
			if s, ok := evidence.Get(c, key).(string); ok {
				*into = &s
			}
		}
	}
	return cases
}

// fsEncode is how Python's surrogateescape writes a str to a file: U+DC80..U+DCFF are the bytes
// they stand for.
func fsEncode(s string) []byte {
	var b []byte
	for i := 0; i < len(s); {
		if surrogateAt(s, i) {
			r := rune(s[i]&0x0f)<<12 | rune(s[i+1]&0x3f)<<6 | rune(s[i+2]&0x3f)
			if r >= 0xdc80 && r <= 0xdcff {
				b = append(b, byte(r-0xdc00))
				i += 3
				continue
			}
		}
		b = append(b, s[i])
		i++
	}
	return b
}

// materialize lays a case out under base as the capture did and returns the --start argument.
func materialize(t *testing.T, c ledgerCase, base string) string {
	t.Helper()
	trial := base + "/trial"
	sub := strings.NewReplacer("${TRIAL}", trial, "${BASE}", base).Replace
	has := func(op string) bool {
		for _, s := range c.Setup {
			if s == op {
				return true
			}
		}
		return false
	}
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.Mkdir(trial, 0o755))
	start := trial + "/start.json"
	if has("start-outside") {
		start = base + "/start.json"
	}
	switch {
	case has("start-dir"):
		must(os.Mkdir(start, 0o755))
	case c.Start != nil:
		must(os.WriteFile(start, fsEncode(sub(*c.Start)), 0o600))
	}
	if has("git-above") {
		must(os.Mkdir(base+"/.git", 0o755))
	}
	if c.Ledger != nil {
		text := fsEncode(sub(*c.Ledger))
		switch {
		case has("ledger-link-outside"):
			must(os.WriteFile(base+"/outside.jsonl", text, 0o600))
			must(os.Symlink(base+"/outside.jsonl", trial+"/ledger.jsonl"))
		case has("ledger-link-worktree"):
			must(os.MkdirAll(trial+"/nested/.git", 0o755))
			must(os.WriteFile(trial+"/nested/ledger.jsonl", text, 0o600))
			must(os.Symlink(trial+"/nested/ledger.jsonl", trial+"/ledger.jsonl"))
		default:
			must(os.WriteFile(trial+"/ledger.jsonl", text, 0o600))
		}
	}
	if has("ledger-dir") {
		must(os.Mkdir(trial+"/ledger.jsonl", 0o755))
	}
	if c.Arg != nil {
		return sub(*c.Arg)
	}
	return start
}

var nowField = regexp.MustCompile(`"now": "[^"]*"`)

func grade(t *testing.T, c ledgerCase) (int, string, map[string]any) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	code := Run([]string{"--start", materialize(t, c, base)}, &out, &errs)
	text := nowField.ReplaceAllString(strings.ReplaceAll(out.String(), base, "${BASE}"), `"now": "<now>"`)
	var document map[string]any
	if err := json.Unmarshal(out.Bytes(), &document); err != nil {
		t.Fatalf("%s: no document (exit %d): %v\n%s", c.Name, code, err, errs.String())
	}
	return code, text, document
}

// TestLedgerGradesAsThePythonLedgerDid: every case's exit status and document are the golden's
// (first the Python ledger's), byte for byte, refusals included.
func TestLedgerGradesAsThePythonLedgerDid(t *testing.T) {
	cases := loadCases(t)
	graded := map[string]gradeAnswer{}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			code, text, _ := grade(t, c)
			graded[c.Name] = gradeAnswer{code, text}
		})
	}
	// The goldens are kept in one file for the whole table, one key per case.
	for _, c := range cases {
		answer, ok := graded[c.Name]
		if !ok {
			continue
		}
		golden.Check(t, c.Name+" exit", []byte(strconv.Itoa(answer.exit)))
		golden.Check(t, c.Name+" stdout", []byte(answer.stdout))
	}
}

// gradeAnswer is one case's exit status and report, as grade normalizes it.
type gradeAnswer struct {
	exit   int
	stdout string
}

func caseNamed(t *testing.T, name string) ledgerCase {
	t.Helper()
	for _, c := range loadCases(t) {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no case %q", name)
	return ledgerCase{}
}

func windowOf(document map[string]any) map[string]any { return document["window"].(map[string]any) }

// TSU-13 and TSU-37 (the ledger's half): a window that has closed is graded; interventions are
// counted apart by timestamp; one inside the window fails window.passed and exits 1; a finished
// trial under the older record version is still gradable; the bounds say whether they were
// corroborated.
func TestTSU13_PreparationAndTheWindowAreCountedApart(t *testing.T) {
	code, _, document := grade(t, caseNamed(t, "clean window after a failed preparation segment"))
	preparation, window := document["preparation"].(map[string]any), windowOf(document)
	if code != 0 || preparation["interventions"] != 1.0 || window["interventions"] != 0.0 || window["windowIsClean"] != true || len(document["judgmentsThatFailed"].([]any)) != 0 {
		t.Fatal(document)
	}
	if failed := preparation["failedSegments"].([]any); len(failed) != 1 || failed[0] != "P1" || preparation["segments"].([]any)[0].(map[string]any)["interventions"] != 1.0 {
		t.Fatal(preparation)
	}
	if window["provenance"] != "declared" {
		t.Fatal(window)
	}
	code, _, document = grade(t, caseNamed(t, "an intervention inside the window fails its judgment"))
	if failed := document["judgmentsThatFailed"].([]any); code != 1 || windowOf(document)["passed"] != false || windowOf(document)["interventions"] != 1.0 || len(failed) != 1 || failed[0] != "window.passed" {
		t.Fatal(code, document)
	}
	if code, _, document = grade(t, caseNamed(t, "a record of version 1 is still gradable")); code != 0 || windowOf(document)["windowIsClean"] != true {
		t.Fatal(code, document)
	}
	if code, _, document = grade(t, caseNamed(t, "corroborated opening")); code != 0 || windowOf(document)["provenance"] != "corroborated" {
		t.Fatal(code, document)
	}
}

// TSU-71: a caller's corroboration carries only the times that were compared; a passed or met
// inside it is dropped and never counted as a judgment.
func TestTSU71_CorroborationCarriesOnlyComparedTimes(t *testing.T) {
	_, _, document := grade(t, caseNamed(t, "corroborated opening"))
	if kept := windowOf(document)["corroboration"].(map[string]any); len(kept) != 1 || kept["opensAt"] == nil || document["judgmentsCounted"] != 1.0 {
		t.Fatal(document)
	}
}

// TSU-14, TSU-24, TSU-70, TSU-90 and the record checks: a ledger or record this cannot grade is
// refused with its reason, and exit 2.
func TestLedgerRefusals(t *testing.T) {
	for name, reason := range map[string]string{
		"a claimed class disagreeing with its time": "claimed class disagrees",
		"two windows":                               "a trial has one window",
		"overlapping segments":                      "two segments overlap in time",
		"a line without a time":                     "is not a timestamp",
		"corroboration that disagrees":              "corroborating time disagrees",
		"corroboration naming no time":              "names no time to compare",
		"a window the record declares otherwise":    "does not match the one the record declares",
		"a segment never closing":                   "never closes",
		"a segment closing without an outcome":      "failed or succeeded",
		"a line dated after grading":                "dated after the time it is being graded",
		"open and close name different segments":    "different segments",
		"a preparation segment through the window":  "overlaps the trial window",
		"a zero-length window":                      "no duration",
		"a ledger linked out of the trial root":     "outside the trial root",
		"a ledger linked into a nested worktree":    "inside a git worktree",
		"a dispatch before the window":              "does not open at the dispatch",
		"no dispatch":                               "one dispatch",
		"a structured actor":                        "written as text",
		"a structured segment":                      "written as text",
		"a time without an offset":                  "does not name a UTC offset",
		"a start record of an unknown version":      "unsupported record version",
		"a trial root inside a git worktree":        "inside a git worktree",
		"a start record outside the trial root":     "outside the trial root",
		"a trial root with a NUL":                   "NUL byte",
		"no ledger":                                 "the ledger could not be read",
		"a start record that is not JSON":           "the start record could not be read",
		"a relative start path":                     "must be an absolute path",
		"a line splitting on a raw line separator":  "a ledger line is not JSON",
		"corroboration that is not an object":       "not an object of times",
		"a window the record does not declare":      "does not declare window.opensAt",
		"a segment boundary naming no segment":      "names no segment",
		"a segment closing before it opens":         "closes before it opens",
		"a segment ending without starting":         "ends without starting",
		"a start record of a version given as text": "unsupported record version",
		"a start record that is not UTF-8":          "the start record could not be read",
		"a ledger that is not UTF-8":                "the ledger could not be read",
		"a time that is no timestamp":               "not an ISO-8601 timestamp",
		"a line of no known kind":                   "no known kind",
		"a trial root that is not a directory":      "not a directory",
		"a start record that is not an object":      "not a JSON object",
		"a start record of another source":          "does not stamp itself",
		"a missing target":                          "target has to be written as text",
		"a declared bound that is not a time":       "window.opensAt is not an ISO-8601 timestamp",
		"corroboration that is not a time":          "window.corroboration.opensAt",
		"a start record that is a directory":        "the start record could not be read",
		"a ledger that is a directory":              "the ledger could not be read",
		"a relative trial root":                     "trialRoot must be an absolute path",
		"a segment starting twice":                  "starts twice",
		"a segment ending twice":                    "ends twice",
		"no window close":                           "not bounded",
		"a line that is not JSON":                   "not JSON",
		"a line that is JSON but no object":         "no known kind",
		"a time of the wrong type":                  "is not a timestamp",
		"a blank time":                              "is not a timestamp",
		"no start record":                           "the start record could not be read",
	} {
		t.Run(name, func(t *testing.T) {
			code, _, document := grade(t, caseNamed(t, name))
			refused, _ := document["refused"].(string)
			if code != 2 || !strings.Contains(refused, reason) || document["source"] != source {
				t.Fatalf("exit %d, refused %q, want %q", code, refused, reason)
			}
		})
	}
}

// TSU-30 (the ledger's half): grading reads the trial root and nothing else, so a finished trial
// is gradable with no installation at all, from any working directory.
func TestTSU30_AFinishedTrialIsGradableWithoutAnInstallation(t *testing.T) {
	c := caseNamed(t, "a redacted host window after a succeeded preparation")
	t.Chdir(t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", "")
	if code, _, document := grade(t, c); code != 0 || windowOf(document)["windowIsClean"] != true {
		t.Fatal(code, document)
	}
}

// fromISOFormat accepts, places and refuses every string as the golden holds, first CPython's
// datetime.fromisoformat's answers: line n is repr() of input n and its answer, ["OK", the UTC
// time, aware] or ["ERR", the message].
func TestFromISOFormatIsCPythons(t *testing.T) {
	inputs, err := hook.Decode(golden.Fixture(t, "fromisoformat-inputs.json"))
	if err != nil {
		t.Fatal(err)
	}
	var answers strings.Builder
	for _, item := range inputs.([]any) {
		input := item.(string)
		at, aware, err := fromISOFormat(input)
		var got []any
		if err != nil {
			got = []any{"ERR", err.Error()}
		} else {
			got = []any{"OK", at.UTC().Format("2006-01-02T15:04:05.000000"), aware}
		}
		answers.WriteString(pyvalue.StrRepr(input) + " " + pyjson.Dumps(got, pyjson.Options{}) + "\n")
	}
	golden.Check(t, "answers", []byte(answers.String()))
}

// A start record or a ledger line reads as deep as CPython 3.14's json nests (past
// encoding/json's 10000) and grades as the same trial does without the nesting; one container past
// the interpreter's edge, the RecursionError neither reader catches is the run that raised before
// it could report, with the message and not the class as its detail. An integer longer than
// int() converts is the ValueError it is, not a JSONDecodeError.
func TestLedgerReadsRecordsAsDeepAsPython(t *testing.T) {
	const name = "clean window after a failed preparation segment"
	wantExit, wantText, _ := grade(t, caseNamed(t, name))
	nestedIn := func(where, value string) ledgerCase {
		c := caseNamed(t, name)
		if where == "start" {
			s := strings.Replace(*c.Start, `{"source"`, `{"note": `+value+`, "source"`, 1)
			c.Start = &s
		} else {
			s := strings.Replace(*c.Ledger, `"kind": "segment_start"`, `"kind": "segment_start", "note": `+value, 1)
			c.Ledger = &s
		}
		return c
	}
	arrays := func(depth int) string { return strings.Repeat("[", depth) + strings.Repeat("]", depth) }
	for _, where := range []string{"start", "ledger"} {
		for _, depth := range []int{20000, pyload.Nesting - 1} {
			code, text, _ := grade(t, nestedIn(where, arrays(depth)))
			if code != wantExit || text != wantText {
				t.Fatalf("%s %d deep: exit %d\n%s", where, depth, code, text)
			}
		}
		code, _, document := grade(t, nestedIn(where, arrays(pyload.Nesting)))
		detail, _ := document["detail"].(map[string]any)
		if code != 2 || document["refused"] != "this run raised before it could report" || detail["exception"] != "RecursionError" ||
			detail["detail"] != "maximum recursion depth exceeded while decoding a JSON array from a unicode string" || detail["raisedAt"] != nil {
			t.Fatalf("%s past the edge: exit %d %v", where, code, document)
		}
	}
	code, _, document := grade(t, nestedIn("start", "1"+strings.Repeat("0", 4300)))
	detail, _ := document["detail"].(map[string]any)
	if code != 2 || detail["detail"] != "could not read the start record (ValueError: Exceeds the limit (4300 digits) for integer string conversion: value has 4301 digits; use sys.set_int_max_str_digits() to increase the limit)" {
		t.Fatalf("exit %d %v", code, document)
	}
}

// The start record's path comes from the command line, which Python holds surrogateescaped: a
// refusal names it that way (a byte that is not UTF-8 is written \udcXX), and an OSError's text
// as repr() of it, escaping every character str.isprintable refuses. Each line is what
// trial_startup.py ledger printed for the same bytes under CPython 3.14.4.
func TestLedgerNamesTheStartPathAsPythonHoldsIt(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root is never refused a directory's search permission")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	locked := base + "/locked"
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	run := func(start string, lines ...string) {
		t.Helper()
		var out, errs bytes.Buffer
		if code := Run([]string{"--start", start}, &out, &errs); code != 2 {
			t.Fatalf("exit %d: %s", code, errs.String())
		}
		for _, line := range lines {
			if !strings.Contains(out.String(), line) {
				t.Fatalf("no %s in\n%s", line, out.String())
			}
		}
	}
	for _, c := range []struct{ name, held, repr string }{
		{"a\u00a0b", `a\u00a0b`, `a\\xa0b`},
		{"a\u2028b", `a\u2028b`, `a\\u2028b`},
		{"a\xffb", `a\udcffb`, `a\\udcffb`},
		{"a\xed\xa0\x80b", `a\udced\udca0\udc80b`, `a\\udced\\udca0\\udc80b`},
	} {
		run(locked+"/"+c.name+"/start.json",
			`"detail": "whether anything exists at this path could not be established (PermissionError: [Errno 13] Permission denied: '`+locked+"/"+c.repr+`/start.json')",`,
			`"path": "`+locked+"/"+c.held+`/start.json",`)
	}
	run(base+"/r\xffx/start.json", `"detail": "nothing exists at `+base+`/r\udcffx/start.json",`, `"path": "`+base+`/r\udcffx/start.json",`)
	run("r\xffx", `"value": "r\udcffx"`)
}

// The trial root the start record names is opened as Python opens Path(trialRoot): os.fsencode
// turns a surrogate escape back into the byte it stands for, so a root whose name is not UTF-8
// (written "\udcff" in the record, as Python's json writes it) grades, and the report names its
// ledger that way: the report is the one the same case grades to under a UTF-8 root (which
// trial_startup.py ledger printed for the same layout under CPython 3.14.4). A lone surrogate
// os.fsencode refuses names no directory.
func TestLedgerOpensTheTrialRootAsPythonEncodesIt(t *testing.T) {
	const name = "clean window after a failed preparation segment"
	c := caseNamed(t, name)
	wantExit, wantText, _ := grade(t, c)
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base, written := parent+"/b\xff", parent+"/b\\udcff" // on disk, and as a JSON string holds it
	if err := os.MkdirAll(base+"/trial", 0o755); err != nil {
		t.Fatal(err)
	}
	sub := strings.NewReplacer("${TRIAL}", written+"/trial", "${BASE}", written).Replace
	if err := os.WriteFile(base+"/trial/start.json", []byte(sub(*c.Start)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base+"/trial/ledger.jsonl", []byte(sub(*c.Ledger)), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	code := Run([]string{"--start", base + "/trial/start.json"}, &out, &errs)
	text := nowField.ReplaceAllString(strings.ReplaceAll(out.String(), written, "${BASE}"), `"now": "<now>"`)
	if code != wantExit || text != wantText {
		t.Fatalf("exit %d, under a UTF-8 root %d\n%s\nunder a UTF-8 root\n%s%s", code, wantExit, text, wantText, errs.String())
	}

	bad := parent + "/bad"
	if err := os.Mkdir(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	record := `{"source": "live-trial-start", "recordVersion": 2, "trialRoot": "` + bad + `\ud800"}`
	if err := os.WriteFile(bad+"/start.json", []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := Run([]string{"--start", bad + "/start.json"}, &out, &errs); code != 2 || !strings.Contains(out.String(), `"refused": "the trial root is not a directory"`) || !strings.Contains(out.String(), `"trialRoot": "`+bad+`\ud800"`) {
		t.Fatalf("exit %d\n%s", code, out.String())
	}
}

// An operator field the report writes back as words (a segment, an actor) that nests 1000
// containers deep is refused by name, as it is at any depth: the field's own refusal, exit 2.
// CPython 3.14.4's recursive shown() passes the interpreter's recursion limit at 997 and answers
// "this run raised before it could report" instead; that is a Python defect not carried over
// (docs/port/known-defects.md), and this pins the refusal the Python comment promises.
func TestALedgerFieldNestedPastPythonsRecursionLimitIsRefusedByName(t *testing.T) {
	nested := strings.Repeat("[", 1000) + strings.Repeat("]", 1000)
	for _, c := range []struct {
		field, from, to string
		line            float64
	}{
		{"segment", `"segment": "P1"}`, `"segment": ` + nested + `}`, 1},
		{"actor", `"actor": "operator"`, `"actor": ` + nested, 2},
	} {
		lines := caseNamed(t, "clean window after a failed preparation segment")
		ledger := strings.Replace(*lines.Ledger, c.from, c.to, 1)
		lines.Ledger = &ledger
		code, _, document := grade(t, lines)
		detail, _ := document["detail"].(map[string]any)
		if code != 2 || document["refused"] != "a ledger line's "+c.field+" has to be written as text" || detail["line"] != c.line || detail["found"] != nested {
			t.Fatalf("%s 1000 deep: exit %d %v", c.field, code, document)
		}
	}
}
