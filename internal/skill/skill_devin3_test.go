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
)

func runSkillPair(t *testing.T, binary, family string, args []string, stdin []byte) (skillProcessResult, skillProcessResult) {
	t.Helper()
	command := pythonSkillCommand(family, args)
	python := exec.Command(command[0], command[1:]...)
	python.Env = oracleEnv("PYTHONDONTWRITEBYTECODE=1")
	python.Stdin = bytes.NewReader(stdin)
	gocli := exec.Command(binary, append([]string{"skill", family}, args...)...)
	gocli.Env = oracleEnv("TMPDIR=/var/tmp")
	gocli.Stdin = bytes.NewReader(stdin)
	return pythonProcess(t, "", python), captureSkillProcess(t, gocli)
}

func requireSkillPairParity(t *testing.T, python, gocli skillProcessResult) {
	t.Helper()
	if !skillProcessParity(python, gocli) {
		t.Fatalf("live Python mismatch (decision 29a)\npython exit=%d stdout=%q stderr=%q\ngo exit=%d stdout=%q stderr=%q", python.exit, python.stdout, python.stderr, gocli.exit, gocli.stdout, gocli.stderr)
	}
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
	pythonOracleRoot(t)
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
			// Given the observation, when both real commands decide it, then the bytes agree.
			path := filepath.Join(t.TempDir(), "observation.json")
			if err := os.WriteFile(path, devin3Observation(t, test.mutate), 0600); err != nil {
				t.Fatal(err)
			}
			python := runHookProbePython(t, "decide", path)
			if python.exit != 0 || !strings.Contains(python.stdout, `"observation": "`+test.state+`"`) {
				t.Fatalf("Python oracle did not observe %s: %+v", test.state, python)
			}
			requireHookProbeParity(t, python, runHookProbeGo(t, binary, "decide", path))
		})
	}
}

func TestSkillUnreadableInputsLivePython(t *testing.T) {
	pythonOracleRoot(t)
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
			// Given unreadable input, when both commands read it, then exit, stdout and the final stderr line agree.
			python, gocli := runSkillPair(t, binary, r.family, r.args, stdin)
			if python.exit == 0 {
				t.Fatalf("Python accepted unreadable input: %+v", python)
			}
			requireSkillPairParity(t, python, gocli)
		})
	}
}

func TestSkillReplayUnreadableFixturesLivePython(t *testing.T) {
	pythonOracleRoot(t)
	binary := recordedCRW(t)
	copyDir := func(t *testing.T, from string) string {
		t.Helper()
		to := filepath.Join(t.TempDir(), "fixtures")
		if output, err := exec.Command("cp", "-r", from, to).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, output)
		}
		return to
	}
	inputs := pythonInputs(t)
	bundled := func(name string) string { return filepath.Join(inputs, name) }
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
			// When both real commands replay it, every other path at the frozen inputs.
			python, gocli := runSkillPair(t, binary, test.family, frozenReplayArgs(inputs, test.family, args), nil)
			// Then neither passes, and both fail identically.
			if python.exit == 0 {
				t.Fatalf("Python passed a bad fixture: %+v", python)
			}
			requireSkillPairParity(t, python, gocli)
		})
	}
}

// 4124181621: the printed self-check is computed by a real oracle self-check.
func TestHookOracleSelfCheckLivePython(t *testing.T) {
	pythonOracleRoot(t)
	root := repositoryRoot()
	script := filepath.Join(root, "plugins", "crw", "skills", "crw-run", "scripts", "hook_probe.py")
	pythonSelfCheck := func(t *testing.T, compared []string) string {
		t.Helper()
		keys, _ := json.Marshal(compared)
		code := fmt.Sprintf(`import importlib.util,json
s=importlib.util.spec_from_file_location("hook_probe",%q);m=importlib.util.module_from_spec(s);s.loader.exec_module(m)
m.COMPARED_KEYS=tuple(json.loads(%q))
print(json.dumps(m._oracle_self_check(),separators=(",",":")))`, script, string(keys))
		command := exec.Command(filepath.Join(root, ".venv", "bin", "python"), "-c", code)
		command.Env = oracleEnv("PYTHONDONTWRITEBYTECODE=1")
		return strings.TrimSpace(string(pythonOutput(t, "self-check", command)))
	}
	without := func(key string) []string {
		return slices.DeleteFunc(slices.Clone(hookComparedKeys), func(k string) bool { return k == key })
	}
	cases := map[string][]string{"as shipped": hookComparedKeys, "reason dropped": without("reason"), "receiptDetail dropped": without("receiptDetail")}
	for name, compared := range cases {
		t.Run(name, func(t *testing.T) {
			// Given a compared-key set, when both self-checks run, then failures and the proven count agree.
			failures, proven, err := hookOracleSelfCheckWith(compared, hookOracleRequiredKeys)
			if err != nil {
				t.Fatal(err)
			}
			if failures == nil {
				failures = []string{}
			}
			got, _ := json.Marshal([]any{failures, proven})
			if want := pythonSelfCheck(t, compared); string(got) != want {
				t.Fatalf("self-check drifted from Python\nGo:     %s\nPython: %s", got, want)
			}
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

// 4124181823: every return site the Go replay records is a site Python's tracer reaches, per fixture.
func TestHookReplayReachMatchesPythonTracer(t *testing.T) {
	root := repositoryRoot()
	scripts := filepath.Join(root, "plugins", "crw", "skills", "crw-run", "scripts")
	fixtures := t.TempDir()
	if output, err := exec.Command("cp", "-r", filepath.Join(pythonInputs(t), "decisions")+"/.", fixtures).CombinedOutput(); err != nil {
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
	code := fmt.Sprintf(`import importlib.util,json,sys
s=importlib.util.spec_from_file_location("hook_probe",%q);m=importlib.util.module_from_spec(s);s.loader.exec_module(m)
sites=m.return_sites();order={}
for fn,line in sites: order.setdefault(fn,[]).append(line)
ordinal={(fn,l):"%%s:%%d"%%(fn,i+1) for fn,ls in order.items() for i,l in enumerate(sorted(ls))}
out={}
for path in m.load_fixtures(%q):
    f=json.loads(path.read_text(encoding="utf-8"));steps=f.get("steps")
    for i,o in enumerate([st.get("observation") for st in steps] if steps else [f.get("observation")]):
        r=set();sys.settrace(m._trace_returns(r))
        try: m.decide(o or {})
        finally: sys.settrace(None)
        out["%%s#%%d"%%(path.name,i)]=sorted(ordinal[k] for k in r)
print(json.dumps(out,sort_keys=True))`, filepath.Join(scripts, "hook_probe.py"), fixtures)
	command := exec.Command(filepath.Join(root, ".venv", "bin", "python"), "-c", code)
	command.Env = oracleEnv("PYTHONDONTWRITEBYTECODE=1")
	output := pythonOutput(t, "reached return sites per fixture observation", command)
	var python map[string][]string
	if err := json.Unmarshal(output, &python); err != nil {
		t.Fatal(err)
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
	if len(got) != len(python) || len(got) < 150 {
		t.Fatalf("observation count Go=%d Python=%d", len(got), len(python))
	}
	for key, want := range python {
		// Given one fixture observation, then Go records exactly the return sites Python's tracer saw.
		if !reflect.DeepEqual(got[key], want) {
			t.Errorf("%s\nGo:     %v\nPython: %v", key, got[key], want)
		}
	}
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
