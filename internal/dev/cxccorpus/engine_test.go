//go:build dev

package cxccorpus

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

func testSubstituter(t *testing.T) *Substituter {
	t.Helper()
	sub, err := LoadSubstitution(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	return sub
}

// The words after the resolved invocation, and after a bare cxc, follow the cli table (R27 and
// R33 notes); a [codexclaw ...] marker is not a command and keeps only its prefix rename.
func TestSubstitution_Expected_follows_the_cli_table(t *testing.T) {
	sub := testSubstituter(t)
	for _, row := range []struct{ in, want string }{
		{"node \"${PLUGIN_ROOT}/bin/cxc.mjs\" orchestrate P --session s", "{CRW} pabcd orchestrate P --session s"},
		{"run cxc orchestrate P", "run crw pabcd orchestrate P"},
		{"cxc config interview always", "crw pabcd config interview always"},
		{"cxc config list", "crw install config list"},
		{"cxc doctor --json", "crw doctor harness --json"},
		{"cxc hooks retrust", "crw doctor retrust"},
		{"cxc bg list", "crw relay job list"},
		{"cxc skill search q", "crw skill search q"},
		{"cxc -v then cxc uninstall", "crw -v then crw uninstall"},
		{"cxc statusline", "crw statusline"},
		{"[codexclaw bg] [codexclaw loop: x] [codexclaw freeze --dry-run", "[crw bg] [crw loop: x] [crw freeze --dry-run"},
	} {
		if got := sub.Expected(row.in); got != row.want {
			t.Errorf("Expected(%q) = %q, want %q", row.in, got, row.want)
		}
	}
}

func TestSubstitution_EnvName_follows_the_env_table(t *testing.T) {
	sub := testSubstituter(t)
	for _, row := range []struct {
		in, want string
		keep     bool
	}{
		{"CODEXCLAW_HOME", "CRW_HOME", true},
		{"CODEXCLAW_CXC", "CRW_BIN", true},
		{"CXC_BGWAKE", "CRW_BGWAKE", true},
		{"CODEXCLAW_TRUST_PROJECT_SUBAGENTS", "", false},
		{"CODEX_HOME", "CODEX_HOME", true},
	} {
		if got, keep := sub.EnvName(row.in); got != row.want || keep != row.keep {
			t.Errorf("EnvName(%q) = %q, %v; want %q, %v", row.in, got, keep, row.want, row.keep)
		}
	}
}

// A Go build names the grant store crw-subspawn-<uid>; its output must normalise to what the
// oracle's codexclaw-subspawn form becomes after the name substitution, in any case root.
func TestNormaliser_Renamed_matches_crw_grant_paths(t *testing.T) {
	n, sub := testNormaliser(t), testSubstituter(t)
	renamed, err := n.Renamed(sub)
	if err != nil {
		t.Fatal(err)
	}
	dir, file := strings.Repeat("a", 64), strings.Repeat("b", 64)
	oracle := "tmp/codexclaw-subspawn-1000/" + dir + "/" + file + ".json"
	crw := strings.ReplaceAll(oracle, "codexclaw-subspawn-", "crw-subspawn-")
	want := sub.Apply(n.NewSession(nil).Text(oracle))
	if want != "tmp/crw-subspawn-<UID>/<GRANTDIR_1>/<GRANTFILE_1>.json" {
		t.Fatalf("the oracle form became %q", want)
	}
	for _, root := range []string{t.TempDir(), t.TempDir()} {
		s := renamed.NewSession([]Binding{{"${TMP}", filepath.Join(root, "tmp")}})
		if got := s.Text(strings.Replace(crw, "tmp/", filepath.Join(root, "tmp")+"/", 1)); got != strings.Replace(want, "tmp/", "${TMP}/", 1) {
			t.Errorf("renamed rules read %q, want %q", got, want)
		}
	}
	for i, rule := range n.Rules.Text {
		if !strings.Contains(strings.ToLower(rule.Regex+rule.Replace), "codexclaw") && renamed.Rules.Text[i] != rule {
			t.Errorf("text rule %s changed though it names no CXC name", rule.Name)
		}
	}
}

// A form rebuilds the exact bytes it was classified from, so a replay compares compact against
// pretty and a final newline against none.
func TestStepResult_StdoutBytes_rebuilds_each_form(t *testing.T) {
	s := testNormaliser(t).NewSession(nil)
	seen := map[string]string{}
	for _, in := range []string{"", "{\"a\":1}", "{\"a\":1}\n", "{\n  \"a\": 1\n}\n", "{\n  \"a\": 1\n}", "{\"a\":1}\n{\"b\":2}\n", "plain\n"} {
		res := shapeStep(s, rawResult{stdout: in})
		if got := res.StdoutBytes(); got != in {
			t.Errorf("%s form of %q rebuilt as %q", res.StdoutForm, in, got)
		}
		seen[res.StdoutForm] = in
	}
	if len(seen) != 7 {
		t.Errorf("the rows reach %d forms, want 7: %v", len(seen), seen)
	}
	compact, pretty := shapeStep(s, rawResult{stdout: "{\"a\":1}\n"}), shapeStep(s, rawResult{stdout: "{\n  \"a\": 1\n}\n"})
	if compact.StdoutBytes() == pretty.StdoutBytes() || compact.StdoutForm == pretty.StdoutForm {
		t.Error("compact and pretty forms must stay distinguishable")
	}
	if got, ok := (Entry{Form: "sqlite", JSON: json.RawMessage("{\n  \"a\": 1\n}")}).Content(); !ok || got != "{\"a\":1}" {
		t.Errorf("a sqlite entry rebuilds its compact dump, got %q, %v", got, ok)
	}
	if _, ok := (Entry{Form: "binary", SHA256: "x"}).Content(); ok {
		t.Error("a binary entry has no text content")
	}
}

// shRuntime is a Runtime for a Go build's stand-in: /bin/sh scripts, stubs that log their calls.
type shRuntime struct{ git string }

func (rt shRuntime) Setup(c *Case, s Scenario) error {
	err := InstallStubs(c, s.Given, func(name string) error {
		script := "#!/bin/sh\nn=${0##*/}\nprintf '{\"cmd\":\"%s\",\"argv\":[\"%s\",\"%s\"],\"cwd\":\"%s\"}\\n' \"$n\" \"$1\" \"$2\" \"$PWD\" >> \"$CXC_REC_LOG\"\nexit 127\n"
		return os.WriteFile(filepath.Join(c.Root, "stubs", name), []byte(script), 0o755)
	})
	c.Env = append(c.Env, "CXC_REC_LOG="+filepath.Join(c.Root, ".rec", "calls.jsonl"))
	return err
}
func (shRuntime) Bindings(c *Case) []Binding { return c.Bindings() }
func (shRuntime) Command(c *Case, s Scenario, step Step) (Invocation, error) {
	return Invocation{Argv: []string{"/bin/sh", "-c", step.CLI[0]}}, nil
}
func (shRuntime) SeedSQLite(*Case, map[string][]string) error { return os.ErrInvalid }
func (shRuntime) DumpSQLite(string) (string, error)           { return "", os.ErrInvalid }
func (rt shRuntime) GitPath() string                          { return rt.git }
func (shRuntime) HookObservations() string                    { return HookObservations }

func shOptions(t *testing.T) RunOptions {
	t.Helper()
	return RunOptions{Scratch: t.TempDir(), HomeVar: "CRW_HOME", Rules: testNormaliser(t), Timeout: 20 * time.Second}
}

// A runtime that is not Node drives the same engine: exit, stdout form, stderr, the observed
// tree and the stub call log all come back normalised.
func TestRunScenario_drives_a_go_runtime(t *testing.T) {
	s := Scenario{
		ID:      "cli__sh__stands_in",
		Given:   Given{Files: map[string]string{"ws/a.txt": "x\n"}},
		Steps:   []Step{{CLI: []string{"echo hi; echo oops >&2; codex a b; exit 3"}}},
		Observe: []string{"ws"},
	}
	got, err := RunScenario(shRuntime{}, shOptions(t), s)
	if err != nil {
		t.Fatal(err)
	}
	if got.Exit != 3 || len(got.Steps) != 1 {
		t.Fatalf("exit %d, %d steps", got.Exit, len(got.Steps))
	}
	if r := got.Steps[0]; r.StdoutForm != "text" || r.StdoutBytes() != "hi\n" || r.Stderr != "oops\n" {
		t.Errorf("step = %+v", r)
	}
	if e := got.Tree["ws/a.txt"]; e.Type != "file" || e.Mode != "0644" || e.Form != "text" {
		t.Errorf("tree = %+v", got.Tree)
	}
	if len(got.Calls) != 1 || got.Calls[0].Cmd != "codex" || !slices.Equal(got.Calls[0].Argv, []string{"a", "b"}) || got.Calls[0].Cwd != "${WS}" {
		t.Errorf("calls = %+v", got.Calls)
	}
}

// What the engine writes for the given and for write steps, and the git work it runs, comes out
// with the modes of umask 022 whatever the process umask is; directories made on the way too.
func TestRunScenario_modes_do_not_depend_on_the_process_umask(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	s := Scenario{
		ID: "cli__sh__modes",
		Given: Given{
			Files: map[string]string{"ws/f/g.txt": "x"}, Dirs: []string{"ws/d/e"},
			JSON: map[string]json.RawMessage{"ws/j/k.json": json.RawMessage("{}")},
			Git:  &Git{Commit: "c", Worktrees: []GitWorktree{{Path: "tmp/wt", Branch: "b"}}},
		},
		Steps:   []Step{{Write: map[string]string{"ws/w/x.txt": "y"}}},
		Observe: []string{"ws", "tmp"},
	}
	got, err := RunScenario(shRuntime{git: git}, shOptions(t), s)
	if err != nil {
		t.Fatal(err)
	}
	for path, entry := range got.Tree {
		if strings.Contains(path, "/.git") || entry.Type == "symlink" {
			continue
		}
		want := "0644"
		if entry.Type == "dir" {
			want = "0755"
		}
		if entry.Mode != want {
			t.Errorf("%s has mode %s, want %s", path, entry.Mode, want)
		}
	}
	for _, path := range []string{"ws/f", "ws/f/g.txt", "ws/d/e", "ws/j/k.json", "ws/w", "ws/w/x.txt", "tmp/wt"} {
		if _, ok := got.Tree[path]; !ok {
			t.Errorf("%s is not in the tree", path)
		}
	}
}
