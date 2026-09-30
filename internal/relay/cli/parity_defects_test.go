package cli_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var evidenceTimestamp = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{6}\+00:00`)

type processResult struct {
	code     int
	out, err string
}

func runParityProcess(t *testing.T, env []string, path string, args ...string) processResult {
	t.Helper()
	return runParityProcessInput(t, env, "", path, args...)
}

func runParityProcessInput(t *testing.T, env []string, input, path string, args ...string) processResult {
	t.Helper()
	if filepath.Base(path) == "crw" || filepath.Base(path) == "codex-session-relay" {
		ownedArgs(t, "", args, "go")
	}
	cmd := exec.Command(path, args...)
	cmd.Env = env
	cmd.Stdin = strings.NewReader(input)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else {
			t.Fatal(err)
		}
	}
	return processResult{code, out.String(), stderr.String()}
}

// The real CLI and gh subprocess surface supplies every collector object shape:
// reviewThread/review/comment, workflow/check/status details, superseded runs,
// gates (including early failure), enumerations/pages, handoff, and restatement.
// Only independently sampled wall-clock timestamps are normalized; object bytes,
// query provenance, exit codes, and stderr are compared without reserialization.
func Test24BuiltBinaryEvidenceDefectBytes(t *testing.T) {
	t.Parallel()
	if !exhaustiveParity {
		t.Run("representative", func(t *testing.T) {
			root, _ := filepath.Abs("../../..")
			binary, alias := packageBinary(t)
			home := t.TempDir()
			env := append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+home+"/state", "XDG_CONFIG_HOME="+home+"/config", "XDG_CACHE_HOME="+home+"/cache", "XDG_DATA_HOME="+home+"/data", "CODEX_HOME="+home+"/codex", "CRW_MARKER_ROOT="+home+"/markers", "CRW_REFUSE_LIVE_STATE=", "PATH="+root+"/internal/relay/cli/testdata:"+os.Getenv("PATH"))
			assertEvidenceBytes(t, env, filepath.Join(root, ".venv/bin/python"), alias, binary, "rich", []string{"merge-evidence", "--repository", "owner/repo", "--pull-request", "7", "--page-size", "1", "--page-budget", "2"}, "")
		})
		return
	}
	root, _ := filepath.Abs("../../..")
	binary, alias := packageBinary(t)
	home := t.TempDir()
	env := append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+home+"/state", "XDG_CONFIG_HOME="+home+"/config", "XDG_CACHE_HOME="+home+"/cache", "XDG_DATA_HOME="+home+"/data", "CODEX_HOME="+home+"/codex", "CRW_MARKER_ROOT="+home+"/markers", "CRW_REFUSE_LIVE_STATE=", "PATH="+root+"/internal/relay/cli/testdata:"+os.Getenv("PATH"))
	python := filepath.Join(root, ".venv/bin/python")
	base := []string{"merge-evidence", "--repository", "owner/repo", "--pull-request", "7"}
	ready := pythonProcess(t, "ready", append(env, "CRW_FORGE_SCENARIO=ready"), "", python, append([]string{"-m", "codex_session_relay.cli"}, base...)...)
	if ready.code != 0 {
		t.Fatal(ready)
	}
	record := filepath.Join(home, "record.json")
	if err := os.WriteFile(record, []byte(ready.out), 0600); err != nil {
		t.Fatal(err)
	}
	t.Run("multi-rules", func(t *testing.T) {
		for _, shape := range []string{"two", "three", "duplicate"} {
			for _, outcome := range []string{"ready", "unresolved", "unknown"} {
				t.Run(shape+"-"+outcome, func(t *testing.T) {
					assertEvidenceBytes(t, env, python, alias, binary, "multi-"+shape+"-"+outcome, base, "")
				})
			}
		}
	})
	t.Run("budget", func(t *testing.T) {
		var tails [][]string
		for _, size := range []string{"0", "-1", "1"} {
			for _, budget := range []string{"1", "2", "6"} {
				tails = append(tails, []string{"--page-size", size, "--page-budget", budget})
			}
		}
		for budget := 0; budget <= 20; budget++ {
			tails = append(tails, []string{"--page-size", "1", "--call-budget", fmt.Sprint(budget)})
		}
		for _, tail := range tails {
			t.Run(strings.Join(tail, "_"), func(t *testing.T) {
				assertEvidenceBytes(t, env, python, alias, binary, "rich", append(append([]string{}, base...), tail...), "")
			})
		}
	})
	t.Run("providers", func(t *testing.T) {
		for _, providers := range []string{`{}`, `{"z-last":["98"],"external":["43"],"dev-gate":["42"],"a-first":["99"]}`, `{"dev-gate":{"a":1},"zeta":[null,9]}`} {
			var document map[string]json.RawMessage
			if err := json.Unmarshal([]byte(ready.out), &document); err != nil {
				t.Fatal(err)
			}
			var handoff map[string]json.RawMessage
			if err := json.Unmarshal(document["handoff"], &handoff); err != nil {
				t.Fatal(err)
			}
			handoff["requiredProviders"] = json.RawMessage(providers)
			handoff["requiredDeclared"] = json.RawMessage(`["dev-gate","zeta"]`)
			handoff["headSha"] = json.RawMessage(`"` + strings.Repeat("a", 40) + `"`)
			raw, err := json.Marshal(handoff)
			if err != nil {
				t.Fatal(err)
			}
			assertEvidenceBytes(t, env, python, alias, binary, "rich", append(append([]string{}, base...), "--restate", "-"), string(raw))
		}
	})
	t.Run("json", func(t *testing.T) {
		for _, input := range []string{"", "not json", "{} {}", "{\n", "[1,]", "{\r\n\"x\":}", `{"headSha":NaN}`, `{"headSha":Infinity}`, `{"headSha":-Infinity}`, `{"headSha":"NaN","nested":[NaN,Infinity,-Infinity,0,"NaN"]}`, "\ufeff{}", `{"n":` + strings.Repeat("9", 4300) + `}`, `{"n":` + strings.Repeat("9", 4301) + `}`} {
			t.Run(fmt.Sprintf("%q", input), func(t *testing.T) {
				assertEvidenceBytes(t, env, python, alias, binary, "ready", append(append([]string{}, base...), "--restate", "-"), input)
			})
		}
	})
	t.Run("utf8", func(t *testing.T) {
		for i, input := range []string{"\xff", "ok\xfe", "\xe2\x82", "\xf0\x90\x80x", "\xed\xa0\x80"} {
			file := filepath.Join(home, fmt.Sprintf("bad-utf8-%d", i))
			if err := os.WriteFile(file, []byte(input), 0600); err != nil {
				t.Fatal(err)
			}
			assertEvidenceBytes(t, env, python, alias, binary, "ready", append(append([]string{}, base...), "--restate", file), "")
		}
	})
	t.Run("pull-request", func(t *testing.T) {
		for _, value := range []string{"0", "-5", "00", "-0", "0_0", "9999999999999999999999999", "٣", "١٢", "_1", "1_", "1__0", " 2 "} {
			assertEvidenceBytes(t, env, python, alias, binary, "ready", []string{"merge-evidence", "--repository", "owner/repo", "--pull-request", value}, "")
		}
	})
	t.Run("big-options", func(t *testing.T) {
		for _, option := range []string{"page-size", "page-budget", "call-budget", "timeout"} {
			assertEvidenceBytes(t, env, python, alias, binary, "ready", append(append([]string{}, base...), "--"+option+"=9999999999999999999999999"), "")
		}
	})
	for _, tc := range []struct {
		name, scenario string
		restate        bool
	}{
		{"ready", "ready", false}, {"unresolved", "unresolved", false}, {"late", "late", true}, {"fresh-forbidden", "unknown", true}, {"fresh-unresolved", "unresolved", true}, {"seven-late", "seven", true}, {"all-object-types", "rich", false}, {"early-unreadable", "unreadable", false}, {"missing-restate", "ready", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{}, base...)
			if tc.restate {
				source := record
				if tc.name == "missing-restate" {
					source = filepath.Join(home, "absent.json")
				}
				args = append(args, "--restate", source)
			}
			runEnv := append(env, "CRW_FORGE_SCENARIO="+tc.scenario)
			want := pythonProcess(t, tc.name, runEnv, "", python, append([]string{"-m", "codex_session_relay.cli"}, args...)...)
			runEnv = append(runEnv, goForgePath(t))
			for _, shape := range []string{"alias", "multicall"} {
				path := alias
				argv := args
				if shape == "multicall" {
					path = binary
					argv = append([]string{"relay"}, args...)
				}
				got := runParityProcess(t, runEnv, path, argv...)
				got.out = evidenceTimestamp.ReplaceAllString(got.out, "<time>")
				want.out = evidenceTimestamp.ReplaceAllString(want.out, "<time>")
				if got != want {
					t.Fatalf("%s byte diff\nGo exit=%d stderr=%q\n%s\nPython exit=%d stderr=%q\n%s", shape, got.code, got.err, got.out, want.code, want.err, want.out)
				}
			}
		})
	}
	t.Run("invalid-settings", func(t *testing.T) {
		args := []string{"intent-declare", "--workspace", home, "--marker-root", home + "/markers", "--dispatch-request-id", "test", "--issue", "I-1", "--no-db-path", "--settings", "not JSON"}
		want := pythonProcess(t, "invalid settings", env, "", python, append([]string{"-m", "codex_session_relay.cli"}, args...)...)
		got := runParityProcess(t, append(env, goForgePath(t)), alias, args...)
		if got != want {
			t.Fatalf("settings byte diff\nGo=%+v\nPython=%+v", got, want)
		}
	})
}

// assertEvidenceBytes compares the built binary, in both shapes, with what the Python CLI
// answered (recorded: see pythonProcess) for args in env, the scripted forge scenario and input
// on stdin. Python ran against testdata/gh and Go runs against its Go twin (fakeGH).
func assertEvidenceBytes(t *testing.T, env []string, python, alias, binary, scenario string, args []string, input string) {
	t.Helper()
	runEnv := append(append([]string{}, env...), "CRW_FORGE_SCENARIO="+scenario)
	goEnv := append(append([]string{}, runEnv...), goForgePath(t))
	label := scenario + " " + oracleLabel(args...)
	// Match the installed console entry point's C stack depth, not runpy (-m),
	// which consumes another frame at CPython's JSON recursion boundary.
	oracle := `import sys; from codex_session_relay.cli import main; sys.exit(main(sys.argv[1:]))`
	want := pythonProcess(t, oracleKey(t, "console "+label), runEnv, input, python, append([]string{"-c", oracle}, args...)...)
	want.out = evidenceTimestamp.ReplaceAllString(want.out, "<time>")
	for _, shape := range []string{"alias", "multicall"} {
		path, argv := alias, args
		if shape == "multicall" {
			path, argv = binary, append([]string{"relay"}, args...)
			oracle := `import argparse,sys; from codex_session_relay import cli; p=cli.build_parser(); p.prog='crw relay'; children=next(a.choices for a in p._actions if isinstance(a,argparse._SubParsersAction)); [(setattr(c,'prog','crw relay '+n)) for n,c in children.items()]; cli.build_parser=lambda:p; sys.exit(cli.main(sys.argv[1:]))`
			want = pythonProcess(t, oracleKey(t, "crw relay "+label), runEnv, input, python, append([]string{"-c", oracle}, args...)...)
			want.out = evidenceTimestamp.ReplaceAllString(want.out, "<time>")
		}
		got := runParityProcessInput(t, goEnv, input, path, argv...)
		got.out = evidenceTimestamp.ReplaceAllString(got.out, "<time>")
		if got != want {
			t.Fatalf("%s %v byte diff\nGo exit=%d stderr=%q\n%s\nPython exit=%d stderr=%q\n%s", shape, args, got.code, got.err, got.out, want.code, want.err, want.out)
		}
	}
}
