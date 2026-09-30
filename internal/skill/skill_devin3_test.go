package skill

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// runSkill answers what `crw skill <family> args...` answered for stdin, and holds that answer,
// each UNREACHED line reduced to its function, to the golden kept under its arguments (first
// taken as the Python script's answer; decision 29a left Python's traceback frames out).
func runSkill(t *testing.T, binary, family string, args []string, stdin []byte) skillProcessResult {
	t.Helper()
	command := exec.Command(binary, append([]string{"skill", family}, args...)...)
	command.Env = oracleEnv("TMPDIR=/var/tmp")
	command.Stdin = bytes.NewReader(stdin)
	answer := captureSkillProcess(t, command)
	checkSkillAnswer(t, "", "", append([]string{"skill", family}, args...), normalizedAnswer(answer))
	return answer
}

func devin3Observation(t *testing.T, mutate func(o map[string]any, current, older map[string]any)) []byte {
	t.Helper()
	marker := func(dispatch, declared string) map[string]any {
		return map[string]any{
			"assignmentId": hashed(dispatch),
			"intent":       map[string]any{"declaredAt": declared, "dispatchRequestIdHash": hashed(dispatch), "issue": "JUN-000", "workspace": "/workspace/example"},
			"bound":        map[string]any{"at": "2026-01-01T00:05:00+00:00", "sessionId": "session-1111", "taskId": "task-1111"},
			"claims":       []any{map[string]any{"at": "2026-01-01T00:05:00+00:00", "dispatchRequestId": dispatch, "factId": "claims/session-1111/claim.json", "firstTurnId": "turn-0001", "sessionId": "session-1111"}},
			"relationship": map[string]any{"at": "2026-01-01T00:05:00+00:00", "relationshipId": "rel-example", "executionGeneration": 1},
		}
	}
	older, current := marker("dispatch-0001", "2026-01-01T00:00:00+00:00"), marker("dispatch-0002", "2026-01-02T00:00:00+00:00")
	o := map[string]any{
		"stop_input":  map[string]any{"cwd": "/workspace/example", "session_id": "session-1111", "turn_id": "turn-0001", "stop_hook_active": false},
		"workspace":   map[string]any{"assignments": []any{older, current}},
		"disposition": map[string]any{"outcome": "ready_for_review", "sessionId": "session-1111", "turnId": "turn-0001"},
		"now":         "2026-01-01T00:05:00+00:00",
	}
	mutate(o, current, older)
	raw, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func hashed(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestHookProbeRoundThreeSelectionLivePython(t *testing.T) {
	goldenRoot(t)
	binary := recordedCRW(t)
	cases := []struct {
		name, state string
		mutate      func(o, current, older map[string]any)
	}{
		// 4124181052: the workspace selects an assignment while marker is null.
		{"workspace selects while marker is null", "receipt_missing", func(o, current, older map[string]any) { o["marker"] = nil }},
		// 4124181412: an intent with an invalid dbPath cannot contradict the directory.
		{"unreadable intent keeps its candidate", "marker_malformed", func(o, current, older map[string]any) {
			intent := current["intent"].(map[string]any)
			intent["dbPath"], intent["dispatchRequestIdHash"] = 5, hashed("other")
		}},
		{"directory-addressed claim selects", "receipt_missing", func(o, current, older map[string]any) {
			older["claims"].([]any)[0].(map[string]any)["factId"] = "claims/session-1111"
			current["claims"] = []any{}
		}},
		{"no session never matches an unowned claim", "marker_claimed_by_other_session", func(o, current, older map[string]any) {
			delete(o["stop_input"].(map[string]any), "session_id")
			claim := older["claims"].([]any)[0].(map[string]any)
			claim["factId"] = "bogus"
			delete(claim, "sessionId")
			current["claims"] = []any{}
		}},
		{"no session never matches an unowned obstructed claim", "marker_claimed_by_other_session", func(o, current, older map[string]any) {
			delete(o["stop_input"].(map[string]any), "session_id")
			older["claims"] = []any{}
			claim := current["claims"].([]any)[0].(map[string]any)
			claim["factId"], claim["dispatchRequestId"] = "bogus", 7
			delete(claim, "sessionId")
			current["intent"].(map[string]any)["declaredAt"] = "2025-12-31T00:00:00+00:00"
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			// Given the observation, when the real command decides it, then the bytes are the golden's.
			path := filepath.Join(t.TempDir(), "observation.json")
			if err := os.WriteFile(path, devin3Observation(t, test.mutate), 0600); err != nil {
				t.Fatal(err)
			}
			answer := runHookProbeGo(t, binary, "decide", path)
			if answer.exit != 0 || !strings.Contains(answer.stdout, `"observation": "`+test.state+`"`) {
				t.Fatalf("decide did not observe %s: %+v", test.state, answer)
			}
		})
	}
}

func TestSkillUnreadableInputsLivePython(t *testing.T) {
	goldenRoot(t)
	binary := recordedCRW(t)
	dir := t.TempDir()
	write := func(name string, content []byte) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, content, 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	badJSON, badUTF8, trailing := write("bad.json", []byte("{x")), write("bad-utf8.json", []byte{0x72, 0xff, 0x0a}), write("trailing.json", []byte(`{"role":"child"} x`))
	directory := filepath.Join(dir, "directory.json")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	type run struct {
		family string
		args   []string
		stdin  string
	}
	var runs []run
	for _, path := range []string{badJSON, badUTF8, directory, filepath.Join(dir, "missing.json")} {
		runs = append(runs, run{"hook-probe", []string{"decide", path}, ""}, run{"start-policy", []string{"check", path}, ""})
	}
	for _, path := range []string{badJSON, badUTF8, trailing} {
		runs = append(runs, run{"parent-title", []string{"decide"}, path}, run{"parent-title", []string{"readback"}, path}, run{"start-policy", []string{"check"}, path})
	}
	for _, r := range runs {
		named := make([]string, len(r.args))
		for i, arg := range r.args {
			named[i] = strings.TrimPrefix(arg, dir+string(filepath.Separator))
		}
		t.Run(r.family+" "+strings.Join(named, " ")+" <"+filepath.Base(r.stdin), func(t *testing.T) {
			var stdin []byte
			if r.stdin != "" {
				stdin, _ = os.ReadFile(r.stdin)
			}
			// Given unreadable input, when the command reads it, then it refuses it as the golden holds.
			if answer := runSkill(t, binary, r.family, r.args, stdin); answer.exit == 0 {
				t.Fatalf("unreadable input accepted: %+v", answer)
			}
		})
	}
}

func TestSkillReplayUnreadableFixturesLivePython(t *testing.T) {
	goldenRoot(t)
	binary := recordedCRW(t)
	copyDir := func(t *testing.T, from string) string {
		t.Helper()
		to := filepath.Join(t.TempDir(), "fixtures")
		if output, err := exec.Command("cp", "-r", from, to).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, output)
		}
		return to
	}
	bundled := func(name string) string { return diskSkillPath(defaultFixture(name)) }
	unreadable := func(t *testing.T, path string) {
		t.Helper()
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(path, 0600) })
	}
	cases := []struct {
		name, family, fixtures, target string
		args                           func(dir string) []string
		content                        []byte
		unreadable                     bool
	}{
		// 4124181236: a title fixture that cannot be decoded or read must fail the run.
		{name: "title undecodable", family: "parent-title", fixtures: "titles", target: "zz.json", content: []byte("{x")},
		{name: "title invalid utf-8", family: "parent-title", fixtures: "titles", target: "zz.json", content: []byte{0xff}},
		{name: "title unreadable", family: "parent-title", fixtures: "titles", target: "zz.json", content: []byte("{}"), unreadable: true},
		{name: "hook undecodable", family: "hook-probe", fixtures: "decisions", target: "zz.json", content: []byte("{x")},
		{name: "host observation undecodable", family: "hook-probe", fixtures: "host", target: "host-observation-codex-0.154.0.json", content: []byte("{x")},
		{name: "host observation unreadable", family: "hook-probe", fixtures: "host", target: "host-observation-codex-0.154.0.json", unreadable: true},
		{name: "host capability undecodable", family: "hook-probe", fixtures: "host", target: "host-capability-codex-0.154.0.json", content: []byte("{x")},
		{name: "host capability unreadable", family: "hook-probe", fixtures: "host", target: "host-capability-codex-0.154.0.json", unreadable: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			// Given a copied fixture directory with one bad record.
			dir := copyDir(t, bundled(test.fixtures))
			target := filepath.Join(dir, test.target)
			if test.content != nil {
				if err := os.WriteFile(target, test.content, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if test.unreadable {
				unreadable(t, target)
			}
			args := []string{"replay", "--fixtures", dir}
			if test.fixtures == "host" {
				args = []string{"replay", "--host-fixtures", dir}
			}
			// When the real command replays it, every other path at the shipped default.
			answer := runSkill(t, binary, test.family, args, nil)
			// Then it fails, as the golden holds.
			if answer.exit == 0 {
				t.Fatalf("a bad fixture passed: %+v", answer)
			}
		})
	}
}

// 4124181621: the printed self-check is computed by a real oracle self-check.
func TestHookOracleSelfCheckLivePython(t *testing.T) {
	goldenRoot(t)
	without := func(key string) []string {
		return slices.DeleteFunc(slices.Clone(hookComparedKeys), func(k string) bool { return k == key })
	}
	cases := map[string][]string{"as shipped": hookComparedKeys, "reason dropped": without("reason"), "receiptDetail dropped": without("receiptDetail")}
	for name, compared := range cases {
		t.Run(name, func(t *testing.T) {
			// Given a compared-key set, when the self-check runs, then its failures and proven count
			// are the golden's (first taken as the reference's _oracle_self_check).
			failures, proven, err := hookOracleSelfCheckWith(compared, hookOracleRequiredKeys)
			if err != nil {
				t.Fatal(err)
			}
			if failures == nil {
				failures = []string{}
			}
			got, _ := json.Marshal([]any{failures, proven})
			checkSkillValue(t, "self-check", got)
		})
	}
	t.Run("an unenforced key is rejected per key", func(t *testing.T) {
		// Given both sets agree but the oracle cannot compare reason, then the self-check fails on reason.
		compared := without("reason")
		failures, _, err := hookOracleSelfCheckWith(compared, hookOracleRequiredKeys)
		if err != nil || len(failures) != 1 {
			t.Fatalf("set difference must be the only failure: %v %v", failures, err)
		}
		failures, proven, err := hookOracleSelfCheckWith(append(compared, "reasonx"), append(slices.Clone(compared), "reasonx"))
		if err != nil || proven != 6 || len(failures) != 1 || failures[0] != "the self-check observation never produces reasonx, so nothing here proves it is compared" {
			t.Fatalf("a key the answer never carries must fail: %v %d %v", failures, proven, err)
		}
		matched, err := checkHookOneKeys("probe", hook.Object{}, hook.Object{{Key: "reason", Value: "wrong"}}, &bytes.Buffer{}, nil, compared)
		if err != nil || !matched {
			t.Fatalf("an uncompared key must be accepted by checkHookOneKeys: %v %v", matched, err)
		}
	})
}

// 4124181823: every return site the Go replay records is a site Python's tracer reached, per
// fixture observation.
func TestHookReplayReachMatchesPythonTracer(t *testing.T) {
	fixtures := t.TempDir()
	if output, err := exec.Command("cp", "-r", diskSkillPath(defaultFixture("decisions"))+"/.", fixtures).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, output)
	}
	for name, mutate := range map[string]func(o, current, older map[string]any){
		"zz-empty-claim-owner": func(o, current, older map[string]any) {
			claim := current["claims"].([]any)[0].(map[string]any)
			claim["factId"], claim["sessionId"] = "claims//claim.json", ""
			delete(o, "workspace")
			o["marker"] = current
		},
		"zz-dot-claim-owner": func(o, current, older map[string]any) {
			current["claims"].([]any)[0].(map[string]any)["factId"] = "claims/../claim.json"
			delete(o, "workspace")
			o["marker"] = current
		},
	} {
		observation := devin3Observation(t, mutate)
		if err := os.WriteFile(filepath.Join(fixtures, name+".json"), []byte(`{"observation":`+string(observation)+`}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	paths, _ := filepath.Glob(filepath.Join(fixtures, "*.json"))
	got := map[string][]string{}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		value, err := hook.Decode(raw)
		if err != nil {
			t.Fatal(err)
		}
		fixture := asObject(value)
		observations := []any{objGet(fixture, "observation")}
		if steps, ok := objGet(fixture, "steps").([]any); ok && len(steps) > 0 {
			observations = observations[:0]
			for _, step := range steps {
				observations = append(observations, objGet(asObject(step), "observation"))
			}
		}
		for i, observation := range observations {
			if !truthyObservation(observation) {
				observation = hook.Object{}
			}
			reached := newHookReplayReach()
			if _, err := probeDecideTrace(observation, reached); err != nil {
				t.Fatalf("%s: %v", filepath.Base(path), err)
			}
			keys := make([]string, 0, len(reached.reached))
			for key := range reached.reached {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			got[fmt.Sprintf("%s#%d", filepath.Base(path), i)] = keys
		}
	}
	encoded, err := golden.Encode(got)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) < 150 {
		t.Fatalf("%d fixture observations", len(got))
	}
	// Given each fixture observation, then Go records exactly the return sites the golden holds
	// (first taken as the sites Python's tracer saw).
	checkSkillValue(t, "reached return sites per fixture observation", encoded)
}

// 4124181982: parentReasons, parentMatches and parentReadback are the denominators title replay
// counts, so each must be exactly what parent_title.go can answer: the reason and decision every
// titleResult call returns, every matched value it passes or assigns, and every state
// titleReadback returns. They are derived here from the Go source, as the reference derived its
// own from its module, so a reason added without its table entry fails.
func TestParentTitleVocabularyMatchesItsSource(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join(repositoryRoot(), "internal", "skill", "parent_title.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	reasons, matches, readback := map[string]string{}, map[string]bool{}, map[string]bool{}
	literal := func(expr ast.Expr, what string) string {
		value, ok := stringLiteral(expr)
		if !ok {
			t.Errorf("%s: %s is not a string literal", fset.Position(expr.Pos()), what)
		}
		return value
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.CallExpr:
			if name, ok := node.Fun.(*ast.Ident); !ok || name.Name != "titleResult" {
				return true
			}
			decision, reason := literal(node.Args[0], "decision"), literal(node.Args[1], "reason")
			if previous, seen := reasons[reason]; seen && previous != decision {
				t.Errorf("%s: %s is answered as both %s and %s", fset.Position(node.Pos()), reason, previous, decision)
			}
			reasons[reason] = decision
			if matched, ok := stringLiteral(node.Args[6]); ok {
				matches[matched] = true
			}
		case *ast.AssignStmt:
			for i, target := range node.Lhs {
				if name, ok := target.(*ast.Ident); ok && name.Name == "matched" && i < len(node.Rhs) {
					matches[literal(node.Rhs[i], "matched")] = true
				}
			}
		case *ast.FuncDecl:
			if node.Name.Name != "titleReadback" {
				return true
			}
			ast.Inspect(node.Body, func(inner ast.Node) bool {
				if ret, ok := inner.(*ast.ReturnStmt); ok && len(ret.Results) == 1 {
					readback[literal(ret.Results[0], "readback state")] = true
				}
				return true
			})
		}
		return true
	})
	set := func(values []string) map[string]bool {
		out := map[string]bool{}
		for _, value := range values {
			out[value] = true
		}
		return out
	}
	// Given parent_title.go, then every denominator equals what its source can answer.
	if !reflect.DeepEqual(parentReasons, reasons) {
		t.Errorf("parentReasons %v, source answers %v", parentReasons, reasons)
	}
	if !reflect.DeepEqual(set(parentMatches), matches) || len(parentMatches) != len(matches) {
		t.Errorf("parentMatches %v, source answers %v", parentMatches, matches)
	}
	if !reflect.DeepEqual(set(parentReadback), readback) || len(parentReadback) != len(readback) {
		t.Errorf("parentReadback %v, source answers %v", parentReadback, readback)
	}
}

func truthyObservation(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case hook.Object:
		return len(x) > 0
	}
	return true
}
