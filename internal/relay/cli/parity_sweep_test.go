package cli_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	_ "github.com/thisisjun786/codex-relay-workflow/internal/relay/capacity"
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
	binary, alias := packageBinary(t)
	home, err := os.MkdirTemp("", "crw-parity-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	env := []string{}
	for _, s := range os.Environ() {
		name, _, _ := strings.Cut(s, "=")
		if name == "HOME" || name == "CODEX_HOME" || strings.HasPrefix(name, "CODEX_SESSION_RELAY_") || name == "COLUMNS" || strings.HasPrefix(name, "XDG_") || strings.HasPrefix(name, "CRW_TEST_") || name == "CRW_REFUSE_LIVE_STATE" {
			continue
		}
		env = append(env, s)
	}
	env = append(env, "HOME="+home, "XDG_STATE_HOME="+home+"/state", "XDG_CONFIG_HOME="+home+"/config", "XDG_DATA_HOME="+home+"/data", "XDG_CACHE_HOME="+home+"/cache", "CODEX_HOME="+home+"/codex")
	// The sweep's cases: what testdata/argparse_sweep.py generated from the Python relay's
	// parsers for this sweep (testdata/fixtures/sweep-cases.json.gz), the same at every width.
	group := "argparse " + os.Getenv("CRW_SWEEP_COMMAND")
	if runtimeSweep {
		group = "runtime " + os.Getenv("CRW_SWEEP_COMMAND")
	} else if rootOnly {
		group = "root"
	}
	var fixture struct {
		Sweeps map[string][]sweepCase `json:"sweeps"`
	}
	fixtureJSON(t, "sweep-cases.json", &fixture, [2]string{"<home>", home})
	cases := fixture.Sweeps[group]
	if len(cases) == 0 {
		t.Fatalf("the sweep fixture holds no cases for %q", group)
	}
	// Each case meets the store state it was generated against. A root case (like a runtime
	// case) has a home of its own, empty. The per-command cases of an argparse sweep share the
	// default store, which a writer's accepted parse opens before the kind-module refusal: it
	// is created here, as the Python relay created it, and handed over, so every one of them
	// finds it.
	if !runtimeSweep && !rootOnly {
		testsupport.Create(t, filepath.Join(home, "state", "codex-session-relay", "default", "relay.sqlite3"), "", "python")
	}
	// Three wrap points cover narrow, default, and wide formatting. The formatter's
	// all-spec parity test covers every command independently of terminal width.
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
			runEnv = append(runEnv, "CRW_FORGE_SCENARIO=ready")
			runEnv = append(runEnv, goForgePath(t))
			// The shared default store goes to Go, as on a host after a takeover.
			ownedTree(t, home, "go")
			results := make([]map[string]any, len(cases))
			jobs := make(chan int)
			var wg sync.WaitGroup
			for worker := 0; worker < 4; worker++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for i := range jobs {
						c := cases[i]
						path := alias
						args := c.Args
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
						// A case's home starts empty: the Go runtime creates an absent store
						// itself (owner=go, epoch 1).
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
						gotOut := out.String()
						if runtimeSweep {
							gotOut = evidenceTimestamp.ReplaceAllString(gotOut, "<time>")
						}
						results[i] = map[string]any{"label": c.Label, "mode": c.Mode, "code": code, "stdout": gotOut, "stderr": stderr.String()}
					}
				}()
			}
			// No case creates a store another case reads: a root or runtime case has a home of
			// its own and the shared default store already exists, so the cases run in parallel.
			for i := range cases {
				jobs <- i
			}
			close(jobs)
			wg.Wait()
			t.Logf("columns=%q cases=%d", width, len(cases))
			expectJSON(t, "argparse_sweep.py", results, home)
		})
	}
}
