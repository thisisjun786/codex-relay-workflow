package cli_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
	_ "github.com/thisisjun786/codex-relay-workflow/internal/relay/capacity"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
	_ "github.com/thisisjun786/codex-relay-workflow/internal/relay/managed"
	_ "github.com/thisisjun786/codex-relay-workflow/internal/relay/mergeturn"
)

type sweepCase struct {
	Command, Label, Home string
	Args                 []string
	Code, Mode           int
	Out, Err             string
}

func todo24(name string) bool {
	return strings.HasPrefix(name, "intent-") || strings.HasPrefix(name, "reporting-") || strings.HasPrefix(name, "supervisor-") || name == "linkage-directive" || name == "merge-evidence"
}

// Every accepted parse hits the real pre-handler kind-module refusal. This keeps
// repeats deterministic, proves dispatch reached the same boundary, and prevents
// mutating handlers or host-dependent clocks from obscuring argument parity.
func Test24BuiltBinaryArgparseSweep(t *testing.T) {
	if exhaustiveParity {
		runBuiltBinarySweep(t, false, false)
		return
	}
	for _, command := range []string{"intent-declare", "reporting-show", "supervisor-stage", "merge-evidence"} {
		t.Run(command, func(t *testing.T) {
			t.Setenv("CRW_SWEEP_COMMAND", command)
			t.Setenv("CRW_SWEEP_WIDTHS", "40,80,200")
			runBuiltBinarySweep(t, false, false)
		})
	}
}

func Test24BuiltBinaryRuntimeSweep(t *testing.T) {
	t.Parallel()
	runBuiltBinarySweep(t, true, false)
}

func Test24BuiltBinaryRootParserParity(t *testing.T) {
	t.Parallel()
	runBuiltBinarySweep(t, false, true)
}

func runBuiltBinarySweep(t *testing.T, runtimeSweep, rootOnly bool) {
	t.Helper()
	root, _ := filepath.Abs("../../..")
	binary, alias := packageBinary(t)
	home, err := os.MkdirTemp("", "crw-parity-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	env := []string{}
	for _, s := range os.Environ() {
		name, _, _ := strings.Cut(s, "=")
		if name == "HOME" || name == "CODEX_HOME" || strings.HasPrefix(name, "CODEX_SESSION_RELAY_") || name == "COLUMNS" || strings.HasPrefix(name, "XDG_") || strings.HasPrefix(name, "CRW_TEST_") || name == "CRW_ALLOW_LIVE_STATE" {
			continue
		}
		env = append(env, s)
	}
	env = append(env, "HOME="+home, "XDG_STATE_HOME="+home+"/state", "XDG_CONFIG_HOME="+home+"/config", "XDG_DATA_HOME="+home+"/data", "XDG_CACHE_HOME="+home+"/cache", "CODEX_HOME="+home+"/codex", "CRW_ALLOW_LIVE_STATE=1")
	var names []string
	for name := range argparse.Specs {
		if name != "" && cli.Registered(name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if runtimeSweep {
		filtered := names[:0]
		for _, name := range names {
			if todo24(name) {
				filtered = append(filtered, name)
			}
		}
		names = filtered
	}
	if only := os.Getenv("CRW_SWEEP_COMMAND"); only != "" {
		names = strings.Split(only, ",")
	}
	if rootOnly {
		names = []string{}
	}
	// Three wrap points cover narrow, default, and wide formatting. The formatter's
	// all-spec Python parity test covers every command independently of terminal width.
	widths := []string{"40", "80", "200"}
	if os.Getenv("CRW_SWEEP_WIDTHS") != "" {
		widths = strings.Split(os.Getenv("CRW_SWEEP_WIDTHS"), ",")
	}
	for _, width := range widths {
		t.Run("columns="+width, func(t *testing.T) {
			runEnv := append([]string{}, env...)
			if width != "" {
				runEnv = append(runEnv, "COLUMNS="+width)
			}
			runEnv = append(runEnv, "PATH="+root+"/internal/relay/cli/testdata:"+os.Getenv("PATH"), "CRW_FORGE_SCENARIO=ready")
			request, _ := json.Marshal(map[string]any{"commands": names, "runtime": runtimeSweep, "home": home, "root": !runtimeSweep && os.Getenv("CRW_SWEEP_COMMAND") == ""})
			oracle := exec.Command(filepath.Join(root, ".venv/bin/python"), "testdata/argparse_sweep.py")
			oracle.Env = runEnv
			oracle.Stdin = bytes.NewReader(request)
			raw, err := oracle.CombinedOutput()
			if err != nil {
				t.Fatalf("oracle: %v %s", err, raw)
			}
			var cases []sweepCase
			if err = json.Unmarshal(raw, &cases); err != nil {
				t.Fatalf("oracle decode: %v %s", err, raw)
			}
			type job struct {
				c    sweepCase
				mode int
				done chan struct{}
			}
			jobs := make(chan job)
			var wg sync.WaitGroup
			var mu sync.Mutex
			differences := map[string][]string{}
			counts := map[string]int{}
			for _, c := range cases {
				counts[c.Command]++
			}
			equal := 0
			focused := 0
			for worker := 0; worker < 8; worker++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for j := range jobs {
						c := j.c
						path := alias
						args := c.Args
						wantOut, wantErr := c.Out, c.Err
						if j.mode == 1 {
							path = binary
							args = append([]string{"relay"}, args...)
						}
						cmd := exec.Command(path, args...)
						cmd.Env = runEnv
						if c.Home != "" {
							if err := os.MkdirAll(c.Home, 0700); err != nil {
								t.Error(err)
								continue
							}
							cmd.Dir = c.Home
							cmd.Env = append(append([]string{}, runEnv...), "HOME="+c.Home, "XDG_STATE_HOME="+c.Home+"/state", "XDG_CONFIG_HOME="+c.Home+"/config", "XDG_DATA_HOME="+c.Home+"/data", "XDG_CACHE_HOME="+c.Home+"/cache", "CODEX_HOME="+c.Home+"/codex")
						}
						var out, stderr bytes.Buffer
						cmd.Stdout = &out
						cmd.Stderr = &stderr
						code := 0
						if err := cmd.Run(); err != nil {
							if e, ok := err.(*exec.ExitError); ok {
								code = e.ExitCode()
							} else {
								code = -1
								stderr.WriteString(err.Error())
							}
						}
						if c.Home != "" {
							os.RemoveAll(c.Home)
						}
						mu.Lock()
						if todo24(c.Command) {
							focused++
						}
						gotOut := out.String()
						if runtimeSweep {
							gotOut = evidenceTimestamp.ReplaceAllString(gotOut, "<time>")
							wantOut = evidenceTimestamp.ReplaceAllString(wantOut, "<time>")
						}
						if code == c.Code && gotOut == wantOut && stderr.String() == wantErr {
							equal++
						} else {
							differences[c.Command] = append(differences[c.Command], fmt.Sprintf("%s mode=%d\nGo exit=%d stdout=%q stderr=%q\nPython exit=%d stdout=%q stderr=%q", c.Label, j.mode, code, out.String(), stderr.String(), c.Code, wantOut, wantErr))
						}
						mu.Unlock()
						if j.done != nil {
							close(j.done)
						}
					}
				}()
			}
			for _, c := range cases {
				// The Python oracle executes root cases sequentially. They can
				// initialize the same scratch store; preserve that order rather
				// than racing PRAGMA journal_mode during concurrent first opens.
				if c.Command == "<root>" {
					done := make(chan struct{})
					jobs <- job{c: c, mode: c.Mode, done: done}
					select {
					case <-done:
					case <-time.After(30 * time.Second):
						t.Fatal("root case did not finish: " + c.Label)
					}
				} else {
					jobs <- job{c: c, mode: c.Mode}
				}
			}
			close(jobs)
			wg.Wait()
			// The alternate invocation is checked with Python's prog set before formatting.
			report := map[string]any{"columns": width, "cases": len(cases), "equal": equal, "focused": focused, "commands": names, "per_command": counts, "diffs": differences}
			if directory := os.Getenv("CRW_SWEEP_REPORT"); directory != "" {
				b, _ := json.MarshalIndent(report, "", "  ")
				prefix := "sweep-"
				if runtimeSweep {
					prefix = "runtime-"
				} else if rootOnly {
					prefix = "root-"
				}
				if err := os.WriteFile(filepath.Join(directory, prefix+width+".json"), b, 0600); err != nil {
					t.Fatal(err)
				}
			}
			t.Logf("columns=%q cases=%d equal=%d commands=%d", width, len(cases), equal, len(names))
			for name, diffs := range differences {
				if runtimeSweep && name == "supervisor-report-recorded" {
					known := true
					for _, d := range diffs {
						known = known && strings.HasPrefix(d, "empty-event mode=")
					}
					if known {
						t.Logf("documented Python --event= TypeError: %d differences", len(diffs))
						continue
					}
				}
				if todo24(name) || (!strings.HasPrefix(name, "region-") && !strings.HasPrefix(name, "slot-") && name != "capacity-show" && name != "limit-declare" && name != "usage-observe") {
					t.Errorf("%s: %d differences; first:\n%s", name, len(diffs), diffs[0])
				} else {
					t.Logf("separate capacity parser %s: %d differences; first:\n%s", name, len(diffs), diffs[0])
				}
			}
		})
	}
}
