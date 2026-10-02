//go:build dev

package cxccorpus

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("the module root is not three directories above the package: %v", err)
	}
	return root
}

// The committed corpus agrees with itself: every spec recorded, every fixture holding its spec,
// the rules and the rename table compiling, the coverage index complete.
func TestCorpus_lints_clean(t *testing.T) {
	if problems := Lint(repoRoot(t)); len(problems) > 0 {
		t.Fatalf("%d problem(s):\n%s", len(problems), strings.Join(problems, "\n"))
	}
}

// Every registered hook leg has at least one recorded fixture: the 32 legs are the floor of the
// corpus, whatever else is pending.
func TestCorpus_records_every_registered_leg(t *testing.T) {
	root := repoRoot(t)
	_, legs, err := LoadDeclarations(root)
	if err != nil {
		t.Fatal(err)
	}
	specs, err := LoadSpecs(root)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, spec := range specs {
		for _, s := range spec.Scenarios {
			for _, step := range s.Steps {
				seen[step.Hook] = true
			}
		}
	}
	for leg := range legs {
		if !seen[leg] {
			t.Errorf("hook leg %s is run by no scenario", leg)
		}
	}
}

func testNormaliser(t *testing.T) *Normaliser {
	t.Helper()
	n, err := LoadRules(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestNormalise_paths_longest_first_and_rules_then_aliases(t *testing.T) {
	n := testNormaliser(t)
	root := t.TempDir()
	s := n.NewSession([]Binding{{"${ROOT}", root}, {"${WS}", filepath.Join(root, "ws")}, {"${PLUGIN_ROOT}", "/opt/cxc/plugins/codexclaw"}, {"${CXC_ROOT}", "/opt/cxc"}})
	for _, row := range []struct{ in, want string }{
		{root + "/ws/.codexclaw/sessions/s.json", "${WS}/.codexclaw/sessions/s.json"},
		{root + "/other", "${ROOT}/other"},
		{`node "/opt/cxc/plugins/codexclaw/bin/cxc.mjs" orchestrate`, `node "${PLUGIN_ROOT}/bin/cxc.mjs" orchestrate`},
		{`{"updatedAt":"2026-01-01T00:00:00.003Z"}`, `{"updatedAt":"<TS>"}`},
		{`{"at":1767225600003,"list":[1767225600004]}`, `{"at":"<MS>","list":["<MS>"]}`},
		{"evidence-unrecordable/s-a-1767225600003.json", "evidence-unrecordable/s-a-<MS>.json"},
		{`{"pid": 4242}`, `{"pid": "<PID>"}`},
		{"0.2.40+codex.20260929183231", "<VERSION>"},
		{"c-20260101000000-a1b2c3 then c-20260101000000-a1b2c3 and e-20260101000000-ffffff", "<NONCE_1> then <NONCE_1> and <NONCE_2>"},
	} {
		if got := s.Text(row.in); got != row.want {
			t.Errorf("Text(%q) = %q, want %q", row.in, got, row.want)
		}
	}
}

func TestNormalise_drops_node_warnings_only(t *testing.T) {
	s := testNormaliser(t).NewSession(nil)
	in := "(node:123) ExperimentalWarning: SQLite is an experimental feature\n(Use `node --trace-warnings ...` to show where the warning was created)\nreal error\n"
	if got := s.Stderr(in); got != "real error\n" {
		t.Fatalf("Stderr = %q", got)
	}
}

func TestClassify_forms_rebuild_their_bytes(t *testing.T) {
	for _, row := range []struct{ in, form string }{
		{"", "empty"},
		{`{"a":1}`, "json"},
		{"{\"a\":1}\n", "json-line"},
		{"{\n  \"a\": 1\n}\n", "json-pretty"},
		{"{\n  \"a\": 1\n}", "json-pretty-nonl"},
		{"{\"a\":1}\n{\"b\":2}\n", "jsonl"},
		{"plain\n", "text"},
		{"{\"a\": 1}\n", "text"},
	} {
		if form, _, _ := classify(row.in); form != row.form {
			t.Errorf("classify(%q) = %s, want %s", row.in, form, row.form)
		}
	}
}

func TestSubstitution_rules_rename_what_the_table_says(t *testing.T) {
	sub, err := LoadSubstitution(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ in, want string }{
		{"Load $codexclaw:cxc-pabcd for P", "Load $crw:crw-pabcd for P"},
		{"[codexclaw: PLAN]", "[crw: PLAN]"},
		{"[codexclaw — refused: illegal transition IDLE->C]", "[crw — refused: illegal transition IDLE->C]"},
		{"read `.codexclaw/sessions/<id>.json`", "read `.crw/sessions/<id>.json`"},
		{"~/.codexclaw/config.json", "~/.crw/config.json"},
		{"cxc-loop,cxc-pabcd", "crw-loop,crw-pabcd"},
		{"(codexclaw) Guarding goal budget", "(crw) Guarding goal budget"},
		{"CODEXCLAW_HOME and CODEX_HOME", "CRW_HOME and CODEX_HOME"},
		{"# CodexClaw wrote this when it created .codexclaw;", "# CRW wrote this when it created .crw;"},
		{`node "${PLUGIN_ROOT}/bin/cxc.mjs" orchestrate P`, "{CRW} orchestrate P"},
		{"codexclaw.json", "crw.json"},
		{"[CXC-SUBAGENT-SCOPE] x", "[CRW-SUBAGENT-SCOPE] x"},
	} {
		if got := sub.Apply(row.in); got != row.want {
			t.Errorf("Apply(%q) = %q, want %q", row.in, got, row.want)
		}
	}
	for _, row := range []struct {
		in, want []string
		ok       bool
	}{
		{[]string{"orchestrate", "P", "--session", "s"}, []string{"pabcd", "orchestrate", "P", "--session", "s"}, true},
		{[]string{"memory", "allow-write", "--session", "s"}, []string{"pabcd", "memory", "allow-write", "--session", "s"}, true},
		{[]string{"memory", "search", "q"}, []string{"recall", "memory", "search", "q"}, true},
		{[]string{"config", "interview", "always"}, []string{"pabcd", "config", "interview", "always"}, true},
		{[]string{"config", "list"}, []string{"install", "config", "list"}, true},
		{[]string{"goalplan", "show"}, nil, false},
	} {
		got, ok := sub.MapArgv(row.in)
		if ok != row.ok || !slices.Equal(got, row.want) {
			t.Errorf("MapArgv(%q) = %q, %v; want %q, %v", row.in, got, ok, row.want, row.ok)
		}
	}
}

// The recorder refuses a scratch root inside a git checkout or the operator's live state, and
// an oracle that is not v0.2.40.
func TestRecorder_refuses_unsafe_roots(t *testing.T) {
	home := t.TempDir()
	restore := osGetenv
	osGetenv = func(key string) string {
		if key == "HOME" {
			return home
		}
		return restore(key)
	}
	defer func() { osGetenv = restore }()
	oracle := filepath.Join(t.TempDir(), "oracle")
	manifest := filepath.Join(oracle, "plugins/codexclaw/.codex-plugin/plugin.json")
	if err := os.MkdirAll(filepath.Dir(manifest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte(`{"version": "0.2.40+codex.20260929183231"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	checkout := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", checkout).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	live := filepath.Join(home, ".codex", "scratch")
	if err := os.MkdirAll(live, 0o755); err != nil {
		t.Fatal(err)
	}
	clean := t.TempDir()
	for _, row := range []struct {
		scratch, oracle string
		refused         *regexp.Regexp
	}{
		{filepath.Join(checkout), oracle, regexp.MustCompile(`inside the git checkout`)},
		{live, oracle, regexp.MustCompile(`inside live state`)},
		{home, oracle, regexp.MustCompile(`operator's home`)},
		{clean, t.TempDir(), regexp.MustCompile(`oracle`)},
		{clean, oracle, nil},
	} {
		r := &Recorder{Scratch: row.scratch, Oracle: row.oracle, Node: "/usr/bin/node", Git: "/usr/bin/git"}
		err := r.Check()
		switch {
		case row.refused == nil && err != nil:
			t.Errorf("scratch %s: refused: %v", row.scratch, err)
		case row.refused != nil && (err == nil || !row.refused.MatchString(err.Error())):
			t.Errorf("scratch %s oracle %s: err %v, want %s", row.scratch, row.oracle, err, row.refused)
		}
	}
}

// A given path outside the five roots is refused before anything is written.
func TestCasePath_stays_under_the_roots(t *testing.T) {
	c := &caseRoot{root: t.TempDir()}
	for _, rel := range []string{"ws/a", "codex/x/y", "cxc/z", "home/.config/k", "tmp/t"} {
		if _, err := casePath(c, rel); err != nil {
			t.Errorf("casePath(%q): %v", rel, err)
		}
	}
	for _, rel := range []string{"../x", "ws/../../x", "/etc/passwd", "stubs/codex", ".rec/calls.jsonl", "elsewhere"} {
		if _, err := casePath(c, rel); err == nil {
			t.Errorf("casePath(%q) accepted", rel)
		}
	}
}
