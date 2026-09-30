package cli_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func Test24FixXBytes(t *testing.T) {
	t.Parallel()
	if !exhaustiveParity {
		root, _ := filepath.Abs("../../..")
		binary, alias := packageBinary(t)
		home := t.TempDir()
		env := append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+home+"/xdg", "XDG_CONFIG_HOME="+home+"/config", "XDG_CACHE_HOME="+home+"/cache", "XDG_DATA_HOME="+home+"/data", "CODEX_HOME="+home+"/codex", "CRW_REFUSE_LIVE_STATE=", "PATH="+root+"/internal/relay/cli/testdata:"+os.Getenv("PATH"), `CRW_FORGE_RULES_JSON=[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":{"z":1,"a":false}}]}}]`)
		assertEvidenceBytes(t, env, filepath.Join(root, ".venv/bin/python"), alias, binary, "ready", []string{"merge-evidence", "--repository", "owner/repo", "--pull-request", "7"}, "")
		return
	}
	root, _ := filepath.Abs("../../..")
	binary, alias := packageBinary(t)
	home := t.TempDir()
	env := append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+home+"/xdg", "XDG_CONFIG_HOME="+home+"/config", "XDG_CACHE_HOME="+home+"/cache", "XDG_DATA_HOME="+home+"/data", "CODEX_HOME="+home+"/codex", "CRW_REFUSE_LIVE_STATE=", "PATH="+root+"/internal/relay/cli/testdata:"+os.Getenv("PATH"))
	python := filepath.Join(root, ".venv/bin/python")
	base := []string{"merge-evidence", "--repository", "owner/repo", "--pull-request", "7"}
	rule := func(parameters string) string {
		return `[{"type":"required_status_checks","parameters":` + parameters + `}]`
	}
	assertRules := func(t *testing.T, rules string) {
		t.Helper()
		assertEvidenceBytes(t, append(env, "CRW_FORGE_RULES_JSON="+rules), python, alias, binary, "ready", base, "")
	}
	t.Run("1-contexts", func(t *testing.T) {
		for i, v := range []string{`5`, `["a"]`, `true`, `1.0`, `1e0`, `1e20`, `{"z":1,"a":false}`, `false`, `0`, `[]`, `null`} {
			t.Run(fmt.Sprint(i), func(t *testing.T) { assertRules(t, rule(`{"required_status_checks":[{"context":`+v+`}]}`)) })
		}
	})
	t.Run("2-malformed", func(t *testing.T) {
		for i, parameters := range []string{`{"required_status_checks":["x"]}`, `["x"]`, `{"required_status_checks":"x"}`, `{"required_status_checks":5}`, `{"required_status_checks":[true]}`, `{"required_status_checks":[[],false,0,null]}`, `[]`} {
			t.Run(fmt.Sprint(i), func(t *testing.T) { assertRules(t, rule(parameters)) })
		}
		assertRules(t, `[null,3,"x",[],{"type":"deletion","parameters":["ignored"]}]`)
		for _, checks := range []string{`5`, `"x"`, `{"a":1}`} {
			assertEvidenceBytes(t, env, python, alias, binary, "ready", append(append([]string{}, base...), "--restate=-"), `{"headSha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","checks":`+checks+`}`)
		}
	})
	t.Run("3-truthiness", func(t *testing.T) {
		for i, v := range []string{`"no"`, `0`, `[]`, `[1]`, `{}`, `{"a":0}`, `1.0`, `false`, `null`} {
			t.Run(fmt.Sprint(i), func(t *testing.T) {
				assertRules(t, `[{"type":"required_status_checks","parameters":{"strict_required_status_checks_policy":`+v+`,"required_status_checks":[{"context":"dev-gate"}]}},{"type":"pull_request","parameters":{"required_review_thread_resolution":`+v+`}}]`)
			})
		}
	})
	t.Run("4-timeout", func(t *testing.T) {
		values := []string{"2147483", "2147484", "9223372036", "9223372037", "1" + strings.Repeat("0", 305), "1" + strings.Repeat("0", 306), "1" + strings.Repeat("0", 308), "18" + strings.Repeat("0", 307), "-1" + strings.Repeat("0", 308), "-18" + strings.Repeat("0", 307)}
		for i, v := range values {
			t.Run(fmt.Sprint(i), func(t *testing.T) {
				assertEvidenceBytes(t, env, python, alias, binary, "ready", append(append([]string{}, base...), "--timeout="+v), "")
			})
		}
		assertEvidenceBytes(t, env, python, alias, binary, "ready", append(append([]string{}, base...), "--timeout="+values[7], "--call-budget=0"), "")
	})
	t.Run("5-blank-head", func(t *testing.T) {
		for _, head := range []string{" ", "\t", "\n", "\r\n"} {
			assertEvidenceBytes(t, env, python, alias, binary, "ready", append(append([]string{}, base...), "--restate=-", "--restate-head="+head), `{}`)
		}
	})
	t.Run("6-depth", func(t *testing.T) {
		for _, depth := range []int{9990, 9997, 9998, 9999, 10000, 50000} {
			for _, shape := range []struct{ name, open, close string }{{"array", "[", "]"}, {"object", `{"a":`, "}"}} {
				t.Run(fmt.Sprintf("%s-%d", shape.name, depth), func(t *testing.T) {
					data := `{"headSha":"` + strings.Repeat("a", 40) + `","x":` + strings.Repeat(shape.open, depth) + "0" + strings.Repeat(shape.close, depth) + "}"
					assertEvidenceBytes(t, env, python, alias, binary, "ready", append(append([]string{}, base...), "--restate=-"), data)
					file := filepath.Join(home, "deep.json")
					if err := os.WriteFile(file, []byte(data), 0600); err != nil {
						t.Fatal(err)
					}
					assertEvidenceBytes(t, env, python, alias, binary, "ready", append(append([]string{}, base...), "--restate", file), "")
				})
			}
		}
	})
	t.Run("7-nonlist", func(t *testing.T) {
		for _, v := range []string{`{"a":1}`, `"text"`, `5`, `false`, `null`} {
			assertRules(t, v)
		}
	})
	// Compare filesystem effects as well as wire bytes. Recreate the same home
	// for each invocation so neither implementation inherits the other's store.
	assertState := func(t *testing.T, args []string) {
		t.Helper()
		state := filepath.Join(home, "state")
		args = append([]string{"--state", state}, args...)
		for _, invocation := range []struct {
			path   string
			prefix []string
		}{{alias, nil}, {binary, []string{"relay"}}} {
			if err := os.RemoveAll(state); err != nil {
				t.Fatal(err)
			}
			// Python's answer and whether it left a store (recorded: see pythonProcess).
			want := pythonProcess(t, oracleKey(t, oracleLabel(args...)), env, "", python, append([]string{"-c", `import os, sys
from codex_session_relay.cli import main
code = main(sys.argv[2:])
print("store" if os.path.exists(sys.argv[1]) else "none", file=sys.stderr)
sys.exit(code)`, filepath.Join(state, "relay.sqlite3")}, args...)...)
			var pyErr error
			if lines := strings.Split(strings.TrimSuffix(want.err, "\n"), "\n"); lines[len(lines)-1] != "store" {
				pyErr = os.ErrNotExist
			}
			want.err = strings.TrimSuffix(strings.TrimSuffix(want.err, "store\n"), "none\n")
			if err := os.RemoveAll(state); err != nil {
				t.Fatal(err)
			}
			got := runParityProcess(t, append(env, goForgePath(t)), invocation.path, append(append([]string{}, invocation.prefix...), args...)...)
			_, goErr := os.Stat(filepath.Join(state, "relay.sqlite3"))
			if goErr != nil && !os.IsNotExist(goErr) {
				t.Fatal(goErr)
			}
			if got != want || (pyErr == nil) != (goErr == nil) {
				t.Fatalf("state/byte diff %v\nGo=%+v store=%v\nPython=%+v store=%v", args, got, goErr == nil, want, pyErr == nil)
			}
		}
	}
	t.Run("8-release", func(t *testing.T) {
		for _, field := range []string{"request-id", "fingerprint", "reason"} {
			for _, value := range []string{"", " ", "\t", "\r\n"} {
				for _, revision := range []string{"0", "-1"} {
					args := []string{"managed-release", "--request-id=missing", "--fingerprint=f", "--reason=r", "--revision=" + revision, "--" + field + "=" + value}
					assertState(t, args)
				}
			}
		}
	})
	t.Run("9-validate-before-store", func(t *testing.T) {
		for _, args := range [][]string{{"fault-show", "--limit=+0"}, {"fault-show", "--limit=-0"}, {"fault-show", "--limit=-99999999999999999999999"}, {"fault-show", "--fault=x", "--product=crw"}, {"fault-show", "--publication=x", "--after=0"}, {"fault-sweep", "--readings-after=-1"}} {
			assertState(t, args)
		}
		for _, value := range []string{"\t", "\n", "a\tb", "a\nb", " x ", "\t\n", "\r\n", "x\v"} {
			assertState(t, []string{"fault-sweep", "--readings=" + value})
		}
		// These shape checks are in the library, after Python opens the store.
		assertState(t, []string{"fault-sweep", "--readings={}"})
		assertState(t, []string{"fault-show", "--limit=99999999999999999999999"})
	})
}
