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

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
	_ "github.com/thisisjun786/codex-relay-workflow/internal/relay/capacity"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
	_ "github.com/thisisjun786/codex-relay-workflow/internal/relay/managed"
	_ "github.com/thisisjun786/codex-relay-workflow/internal/relay/mergeturn"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
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
	for _, command := range []string{"intent-declare", "reporting-show", "supervisor-stage", "merge-evidence"} {
		t.Run(command, func(t *testing.T) {
			t.Setenv("CRW_SWEEP_COMMAND", command)
			t.Setenv("CRW_SWEEP_WIDTHS", "40,80,200")
			runBuiltBinarySweep(t, false, false)
		})
	}
}

func Test24BuiltBinaryRuntimeSweep(t *testing.T) {
	for _, command := range []string{"supervisor-stage", "merge-evidence"} {
		t.Run(command, func(t *testing.T) {
			t.Setenv("CRW_SWEEP_COMMAND", command)
			runBuiltBinarySweep(t, true, false)
		})
	}
}

func Test24BuiltBinaryRootParserParity(t *testing.T) {
	t.Parallel()
	runBuiltBinarySweep(t, false, true)
}

func runBuiltBinarySweep(t *testing.T, runtimeSweep, rootOnly bool) {
	t.Helper()
	root, _ := filepath.Abs("../../..")
	binary, alias := packageBinary(t)
	// A fixed tree: an intent-* case's answer names its workspace key, a digest of the case home's
	// path, which a recorded answer can only share with this run when the path is the same.
	home := fixedTree(t, t.Name())
	env := []string{}
	for _, s := range os.Environ() {
		name, _, _ := strings.Cut(s, "=")
		if name == "HOME" || name == "CODEX_HOME" || strings.HasPrefix(name, "CODEX_SESSION_RELAY_") || name == "COLUMNS" || strings.HasPrefix(name, "XDG_") || strings.HasPrefix(name, "CRW_TEST_") || name == "CRW_REFUSE_LIVE_STATE" {
			continue
		}
		env = append(env, s)
	}
	env = append(env, "HOME="+home, "XDG_STATE_HOME="+home+"/state", "XDG_CONFIG_HOME="+home+"/config", "XDG_DATA_HOME="+home+"/data", "XDG_CACHE_HOME="+home+"/cache", "CODEX_HOME="+home+"/codex")
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
	// Each case must meet the same store state in both runtimes. The oracle runs all its cases
	// before Go runs any, so a store one oracle case creates exists for every Go case, even for
	// those the oracle ran before it; since a reader never creates a store (decision 30), a
	// reader's answer depends on which. So a root case (like a runtime case) gets a home of its
	// own from the oracle, empty in both runtimes. The per-command cases of an argparse sweep
	// share the default store, which a writer's accepted parse opens before the kind-module
	// refusal: it is created here, owned by Python, so every one of them finds it in both
	// runtimes, not only those after the oracle's first writer.
	if !runtimeSweep && len(names) > 0 {
		testsupport.Create(t, filepath.Join(home, "state", "codex-session-relay", "default", "relay.sqlite3"), "", "python")
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
			// Each width runs the oracle on the stores the previous width's Go turn left:
			// Python takes them back first, as on a host.
			ownedTree(t, home, "python")
			// The oracle's cases are recorded (see pythonProcess); Python ran against
			// testdata/gh, Go runs against its Go twin (fakeGH).
			oracle := pythonProcess(t, "argparse_sweep.py", runEnv, string(request), filepath.Join(root, ".venv/bin/python"), filepath.Join(root, "internal/relay/cli/testdata/argparse_sweep.py"))
			if oracle.code != 0 {
				t.Fatalf("oracle: exit %d %s%s", oracle.code, oracle.out, oracle.err)
			}
			var cases []sweepCase
			if err := json.Unmarshal([]byte(oracle.out), &cases); err != nil {
				t.Fatalf("oracle decode: %v %s", err, oracle.out)
			}
			runEnv = append(runEnv, goForgePath(t))
			// The shared store the oracle's per-command cases used is owned by Python; Go runs
			// the same cases on it next, after a takeover, as on a host.
			ownedTree(t, home, "go")
			jobs := make(chan sweepCase)
			var wg sync.WaitGroup
			var mu sync.Mutex
			differences := map[string][]string{}
			counts := map[string]int{}
			for _, c := range cases {
				counts[c.Command]++
			}
			equal := 0
			focused := 0
			for worker := 0; worker < 4; worker++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for c := range jobs {
						path := alias
						args := c.Args
						wantOut, wantErr := c.Out, c.Err
						if c.Mode == 1 {
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
						// A case's home starts empty, as the oracle's did: the Go runtime creates
						// an absent store itself (owner=go, epoch 1) exactly where Python does.
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
							differences[c.Command] = append(differences[c.Command], fmt.Sprintf("%s mode=%d\nGo exit=%d stdout=%q stderr=%q\nPython exit=%d stdout=%q stderr=%q", c.Label, c.Mode, code, out.String(), stderr.String(), c.Code, wantOut, wantErr))
						}
						mu.Unlock()
					}
				}()
			}
			// No case creates a store another case reads: a root or runtime case has a home of
			// its own and the shared default store already exists, so the cases run in parallel.
			for _, c := range cases {
				jobs <- c
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
