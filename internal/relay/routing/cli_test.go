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
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
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
		testBinary = filepath.Join(dir, "crw")
		cmd := exec.Command("go", "build", "-o", testBinary, "./cmd/crw")
		cmd.Dir = root
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &out
		if err = cmd.Run(); err != nil {
			binaryError = err
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
		clockBinary = filepath.Join(dir, "clock", "crw")
		cmd = exec.Command("go", "build", "-overlay", overlayPath, "-o", clockBinary, "./cmd/crw")
		cmd.Dir = root
		cmd.Stdout = &out
		cmd.Stderr = &out
		if err = cmd.Run(); err != nil {
			binaryError = err
			return
		}
		binaryError = os.Symlink(clockBinary, filepath.Join(filepath.Dir(clockBinary), "codex-session-relay"))
	})
	if binaryError != nil {
		t.Fatal(binaryError)
	}
	return testBinary
}

// oracleScript returns what the Python oracle script printed for mode against state: the
// recording by default, a live run under CRW_PYTHON_ORACLE=record or check (see pyoracle). Each
// observe runs after a live run and judges what Python did beyond its answer.
func oracleScript(t *testing.T, name, mode, state string, observe ...func() error) []byte {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	return pyoracle.AnswerInterned(t, name+" "+mode, func() ([]byte, error) {
		cmd := exec.Command("uv", "run", "--no-sync", "--no-project", "python3", filepath.Join(filepath.Dir(file), "testdata", name), mode, state)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		raw, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("oracle %v\n%s", err, stderr.String())
		}
		for _, check := range observe {
			if err := check(); err != nil {
				return nil, err
			}
		}
		return raw, nil
	}, pyoracle.Substitute(state, "<state>"), pyoracle.Substitute(filepath.Clean(filepath.Join(filepath.Dir(file), "../../..")), "<repo>"), pyoracle.Substitute(os.Getenv("TMPDIR"), "<tmpdir>"))
}

// stateFiles returns the regular files Python left in state once its store is gone, so a replay
// hands Go the same directory: the recording by default, the live directory under
// CRW_PYTHON_ORACLE=record or check.
func stateFiles(t *testing.T, key, state string) map[string]string {
	t.Helper()
	files := map[string]string{}
	pyoracle.JSON(t, key, &files, func() (any, error) {
		found := map[string]string{}
		err := filepath.WalkDir(state, func(path string, entry os.DirEntry, err error) error {
			if err != nil || !entry.Type().IsRegular() {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(state, path)
			found[filepath.ToSlash(rel)] = string(data)
			return err
		})
		return found, err
	}, pyoracle.Substitute(state, "<state>"))
	return files
}
func Test23_ArgparseWidthsBuiltBinary(t *testing.T) {
	binary := builtBinary(t)
	state := t.TempDir()
	raw := oracleScript(t, "cli_capture.py", "argv", state, func() error {
		if _, err := os.Stat(filepath.Join(state, "relay.sqlite3")); !os.IsNotExist(err) {
			return fmt.Errorf("parser-only oracle wrote store: %v", err)
		}
		return nil
	})
	var cases []struct {
		Command        string
		Args           []string
		Prog           string
		Width          *string
		Code           int
		Stdout, Stderr string
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	acceptedRaw := oracleScript(t, "cli_capture.py", "accepted-argv", state)
	accepted := cases[:0:0]
	if err := json.Unmarshal(acceptedRaw, &accepted); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(state); err != nil {
		t.Fatal(err)
	}
	parserCases := len(cases)
	cases = append(cases, accepted...)
	for index, tc := range cases {
		label := tc.Command + "/" + tc.Prog + "/" + strings.Join(tc.Args, " ")
		t.Run(label, func(t *testing.T) {
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
			if code != tc.Code || stdout.String() != tc.Stdout || stderr.String() != tc.Stderr {
				t.Fatalf("argparse bytes differ exit Python=%d Go=%d\nPython stdout=%q stderr=%q\nGo stdout=%q stderr=%q", tc.Code, code, tc.Stdout, tc.Stderr, stdout.String(), stderr.String())
			}
			if index < parserCases {
				if _, err := os.Stat(filepath.Join(state, "relay.sqlite3")); !os.IsNotExist(err) {
					t.Fatalf("parse error/help wrote store: %v", err)
				}
			}
		})
	}
}
func Test23_CLIWholeRepliesAndTables(t *testing.T) { cliReplay(t, "qa") }
func Test23_PRD_2_PolicyCLI(t *testing.T)          { cliReplay(t, "PRD-2") }
func Test23_PRD_10_RegistryCLI(t *testing.T)       { cliReplay(t, "PRD-10") }
func Test23_PRD_16_UnreadableJSON(t *testing.T)    { cliReplay(t, "PRD-16") }
func Test23_PRD_13_LedgerGateCLI(t *testing.T)     { cliReplay(t, "PRD-13") }
func cliReplay(t *testing.T, mode string) {
	pythonState := filepath.Join(t.TempDir(), "state")
	raw := oracleScript(t, "cli_capture.py", mode, pythonState)
	var records []struct {
		Args                   []string
		Code                   int
		Stdout, Stderr, Tables string
	}
	if err := json.Unmarshal(raw, &records); err != nil {
		t.Fatal(err)
	}
	// Remove Python's store whole, its fence (mirror, write gate, controller lock) included, so
	// the directory holds an absent store and Go's first command creates its own.
	for _, name := range []string{"relay.sqlite3", "relay.sqlite3-wal", "relay.sqlite3-shm", "events.jsonl", "takeover.json", "write-gate.lock", "takeover.lock"} {
		if err := os.Remove(filepath.Join(pythonState, name)); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	// What Python left beside its store (PRD-10's binding.json) is input to Go's commands.
	for name, data := range stateFiles(t, "files cli_capture.py "+mode+" left beside its store", pythonState) {
		path := filepath.Join(pythonState, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	clock := &integrationClock{stamp: "2023-11-14T22:13:20.000000+00:00", now: 1700000000}
	ctx := WithClock(context.Background(), clock)
	for _, record := range records {
		t.Run(strings.Join(record.Args[:1], ""), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if mode == "PRD-13" && strings.HasPrefix(record.Args[0], "route-") {
				ctx = context.WithValue(ctx, ledgerMissingKey{}, []string{"FaultLedger.adopt", "FaultLedger.record(adopt=)", "faults.UNASSIGNED"})
			}
			code := cli.ExecuteAs(ctx, "codex-session-relay", append([]string{"--state", pythonState}, record.Args...), &stdout, &stderr)
			if code != record.Code || stdout.String() != record.Stdout || stderr.String() != record.Stderr {
				t.Fatalf("CLI bytes differ exit Python=%d Go=%d\nPython: %s\nGo: %s\nstderr: %s", record.Code, code, record.Stdout, stdout.String(), stderr.String())
			}
			s, err := store.Open(ctx, filepath.Join(pythonState, "relay.sqlite3"), "")
			if err != nil {
				t.Fatal(err)
			}
			got, err := tablesJSON(ctx, s)
			closeErr := s.Close()
			if err != nil {
				t.Fatal(err)
			}
			if closeErr != nil {
				t.Fatal(closeErr)
			}
			if got != record.Tables {
				var want, actual any
				d := json.NewDecoder(strings.NewReader(record.Tables))
				d.UseNumber()
				if err := d.Decode(&want); err != nil {
					t.Fatal(err)
				}
				d = json.NewDecoder(strings.NewReader(got))
				d.UseNumber()
				if err := d.Decode(&actual); err != nil {
					t.Fatal(err)
				}
				t.Fatal(firstDifference("tables", want, actual))
			}
		})
		if t.Failed() {
			return
		}
	}
}
