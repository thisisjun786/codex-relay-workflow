package routing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

var binaryOnce sync.Once
var testBinary string
var binaryError error
var clockBinary string

func builtBinary(t *testing.T) string {
	t.Helper()
	binaryOnce.Do(func() {
		_, file, _, _ := runtime.Caller(0)
		root := filepath.Clean(filepath.Join(filepath.Dir(file), "../../.."))
		dir, err := os.MkdirTemp("/tmp", "routing-bin-")
		if err != nil {
			binaryError = err
			return
		}
		// A copy beside its codex-session-relay link, as an installation lays them out.
		testBinary = filepath.Join(dir, "crw")
		if binaryError = testsupport.CopyCRW(testBinary); binaryError != nil {
			return
		}
		binaryError = os.Symlink(testBinary, filepath.Join(dir, "codex-session-relay"))
		if binaryError != nil {
			return
		}
		// Compile the real entry point with only its context clock injected. The overlay
		// is test-only; product code never reads clock overrides from the environment.
		mainPath := filepath.Join(root, "cmd/crw/main.go")
		source, err := os.ReadFile(mainPath)
		if err != nil {
			binaryError = err
			return
		}
		text := strings.Replace(string(source), `_ "github.com/thisisjun786/codex-relay-workflow/internal/relay/routing"`, `routing "github.com/thisisjun786/codex-relay-workflow/internal/relay/routing"`, 1)
		text = strings.Replace(text, "import (", "import (\n \"bytes\"\n \"github.com/thisisjun786/codex-relay-workflow/internal/relay/faults\"", 1)
		text = strings.Replace(text, "cli.Version = version", "ctx = routing.WithClock(ctx, parityClock{})\n ctx = faults.WithInputs(ctx, parityClock{}, bytes.NewReader([]byte{0,1,2,3,4,5,6,7}))\n cli.Version = version", 1)
		text += "\ntype parityClock struct{}\nfunc(parityClock)Now()float64{return 1700000000}\nfunc(parityClock)ISO()string{return \"2023-11-14T22:13:20.000000+00:00\"}\n"
		patched := filepath.Join(dir, "main.go")
		if err = os.WriteFile(patched, []byte(text), 0600); err != nil {
			binaryError = err
			return
		}
		overlay, err := json.Marshal(map[string]any{"Replace": map[string]string{mainPath: patched}})
		if err != nil {
			binaryError = err
			return
		}
		overlayPath := filepath.Join(dir, "overlay.json")
		if err = os.WriteFile(overlayPath, overlay, 0600); err != nil {
			binaryError = err
			return
		}
		built, err := testsupport.BuildCRWPath("-overlay", overlayPath)
		if err != nil {
			binaryError = err
			return
		}
		clockBinary = filepath.Join(dir, "clock", "crw")
		if binaryError = testsupport.CopyBinary(built, clockBinary); binaryError != nil {
			return
		}
		binaryError = os.Symlink(clockBinary, filepath.Join(filepath.Dir(clockBinary), "codex-session-relay"))
	})
	if binaryError != nil {
		t.Fatal(binaryError)
	}
	return testBinary
}

// cliReply is what a command answered, as its golden holds it.
type cliReply struct {
	Exit   int    `json:"exit"`
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
}

// cliScenario is a CLI scenario's fixture: the commands in order and the files the state directory
// holds before the first (PRD-10's binding.json, which product-bind --record @... reads).
type cliScenario struct {
	Records []struct{ Args []string }
	Files   map[string]string
}

func Test23_ArgparseWidthsBuiltBinary(t *testing.T) {
	binary := builtBinary(t)
	state := filepath.Join(t.TempDir(), "state")
	var cases []struct {
		Command string
		Args    []string
		Prog    string
		Width   *string
	}
	scenarioInputs(t, "cli-argv.json", &cases, [2]string{state, "<state>"})
	var accepted []struct {
		Command string
		Args    []string
		Prog    string
		Width   *string
	}
	scenarioInputs(t, "cli-accepted-argv.json", &accepted, [2]string{state, "<state>"})
	parserCases := len(cases)
	cases = append(cases, accepted...)
	for index, tc := range cases {
		label := tc.Command + "/" + tc.Prog + "/" + strings.Join(tc.Args, " ")
		var reply cliReply
		if !t.Run(label, func(t *testing.T) {
			path := binary
			args := []string{"relay", "--state", state, tc.Command}
			if tc.Prog == "codex-session-relay" {
				path = filepath.Join(filepath.Dir(binary), tc.Prog)
				args = []string{"--state", state, tc.Command}
			}
			args = append(args, tc.Args...)
			cmd := exec.Command(path, args...)
			env := []string{}
			for _, v := range os.Environ() {
				if !strings.HasPrefix(v, "COLUMNS=") {
					env = append(env, v)
				}
			}
			if tc.Width != nil {
				env = append(env, "COLUMNS="+*tc.Width)
			}
			cmd.Env = env
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			code := 0
			if err := cmd.Run(); err != nil {
				if e, ok := err.(*exec.ExitError); ok {
					code = e.ExitCode()
				} else {
					t.Fatal(err)
				}
			}
			reply = cliReply{code, stdout.String(), stderr.String()}
			if index < parserCases {
				if _, err := os.Stat(filepath.Join(state, "relay.sqlite3")); !os.IsNotExist(err) {
					t.Fatalf("parse error/help wrote store: %v", err)
				}
			}
		}) {
			continue
		}
		golden.CheckJSON(t, fmt.Sprintf("%04d %s", index, label), reply, goldenPaths([2]string{state, "<state>"})...)
	}
}
func Test23_CLIWholeRepliesAndTables(t *testing.T) { cliReplay(t, "qa") }
func Test23_PRD_2_PolicyCLI(t *testing.T)          { cliReplay(t, "PRD-2") }
func Test23_PRD_10_RegistryCLI(t *testing.T)       { cliReplay(t, "PRD-10") }
func Test23_PRD_16_UnreadableJSON(t *testing.T)    { cliReplay(t, "PRD-16") }
func cliReplay(t *testing.T, mode string) {
	state := filepath.Join(t.TempDir(), "state")
	var scenario cliScenario
	scenarioInputs(t, "cli-"+mode+".json", &scenario, [2]string{state, "<state>"})
	for name, data := range scenario.Files {
		path := filepath.Join(state, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	clock := &integrationClock{stamp: "2023-11-14T22:13:20.000000+00:00", now: 1700000000}
	ctx := WithClock(context.Background(), clock)
	opts := goldenPaths([2]string{state, "<state>"})
	trail := &tableTrail{}
	for i, record := range scenario.Records {
		var reply cliReply
		var tables string
		if !t.Run(strings.Join(record.Args[:1], ""), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := cli.ExecuteAs(ctx, "codex-session-relay", append([]string{"--state", state}, record.Args...), &stdout, &stderr)
			reply = cliReply{code, stdout.String(), stderr.String()}
			s, err := store.Open(ctx, filepath.Join(state, "relay.sqlite3"), "")
			if err != nil {
				t.Fatal(err)
			}
			tables = tablesJSON(t, s)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
		}) {
			return
		}
		key := fmt.Sprintf("%02d %s", i, record.Args[0])
		golden.CheckJSON(t, key+" reply", reply, opts...)
		trail.check(t, key+" tables", tables, opts...)
	}
}
