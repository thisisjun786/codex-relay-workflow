package cli_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// Decision 25 across the runtimes, in process: the Go producer and the Go drain against what the
// retained Python fence answered (one Python process per batch, recorded: see pythonCLI). The
// built-binary candidate drain is internal/relay/service Test31GoCandidateDrainsPythonQueuedEntriesBuiltCLI.

type answer struct {
	code   int
	stdout string
}

// goCLI runs the Go relay CLI in process, as codex-session-relay, without touching ownership.
func goCLI(t *testing.T, argv ...string) answer {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := cli.ExecuteAs(context.Background(), "codex-session-relay", argv, &stdout, &stderr)
	return answer{code, stdout.String()}
}

// pythonCLI is what the fence's cli.main answered for each argv, in one Python process, byte
// for byte (argv that is not UTF-8 arrives surrogate-escaped, as on a command line); recorded,
// see oracleRun.
func pythonCLI(t *testing.T, argvs ...[]string) []answer {
	t.Helper()
	var words []string
	for _, argv := range argvs {
		words = append(words, argv...)
	}
	label := "cli.main"
	if len(argvs) > 0 {
		label += fmt.Sprintf(" x%d %s", len(argvs), oracleLabel(argvs[0]...))
	}
	var recorded []recordedRun
	askOracle(t, oracleKey(t, label), &recorded, func() (any, error) {
		answers, err := livePythonCLI(argvs...)
		out := make([]recordedRun, len(answers))
		for i, a := range answers {
			out[i] = recordedRun{Code: a.code, Stdout: a.stdout}
		}
		return out, err
	}, placeholders(words...)...)
	if len(recorded) != len(argvs) {
		t.Fatalf("%d answers for %d argvs", len(recorded), len(argvs))
	}
	out := make([]answer, len(recorded))
	for i, r := range recorded {
		out[i] = answer{r.Code, r.Stdout}
	}
	return out
}

// livePythonCLI runs each argv through the fence's cli.main in one live Python process. Only a
// capture closure calls it.
func livePythonCLI(argvs ...[]string) ([]answer, error) {
	var encoded [][]string
	for _, argv := range argvs {
		var items []string
		for _, a := range argv {
			items = append(items, base64.StdEncoding.EncodeToString([]byte(a)))
		}
		encoded = append(encoded, items)
	}
	input, err := json.Marshal(encoded)
	if err != nil {
		return nil, err
	}
	script := `import base64, contextlib, io, json, os, sys
from codex_session_relay import cli
out = []
for argv in json.load(sys.stdin):
    printed = io.StringIO()
    with contextlib.redirect_stdout(printed):
        code = cli.main([os.fsdecode(base64.b64decode(a)) for a in argv])
    out.append([code, printed.getvalue()])
json.dump(out, sys.stdout)
`
	python := exec.Command(filepath.Join(repositoryRootPath(), ".venv", "bin", "python"), "-c", script)
	python.Stdin = bytes.NewReader(input)
	python.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	raw, err := python.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, fmt.Errorf("%v: %s", err, exit.Stderr)
		}
		return nil, err
	}
	var decoded [][2]any
	if err = json.Unmarshal(raw, &decoded); err != nil || len(decoded) != len(argvs) {
		return nil, fmt.Errorf("%v %s", err, raw)
	}
	out := make([]answer, len(decoded))
	for i, d := range decoded {
		out[i] = answer{int(d[0].(float64)), d[1].(string)}
	}
	return out, nil
}

func object(t *testing.T, text string) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal([]byte(text), &value); err != nil {
		t.Fatalf("%v: %s", err, text)
	}
	return value
}

// ownerOnlyState is an S the way both runtimes create one (0700).
func ownerOnlyState(t *testing.T, root, name string) string {
	t.Helper()
	state := filepath.Join(root, name)
	if err := os.MkdirAll(state, 0700); err != nil {
		t.Fatal(err)
	}
	return state
}

// setPhase republishes the mirror of a stopped store in phase draining (a transition in
// progress, as `takeover begin` publishes it) or back in phase active.
func setPhase(t *testing.T, dbPath, phase string) {
	t.Helper()
	r, err := ownership.ReadRecord(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	r.Phase, r.Transition = phase, nil
	if phase == "draining" {
		r.Transition = &ownership.Transition{ID: "t31-drain", From: r.Owner, To: map[string]string{"go": "python", "python": "go"}[r.Owner], TargetEpoch: r.Epoch + 1}
	}
	if err = ownership.Publish(dbPath, r, nil); err != nil {
		t.Fatal(err)
	}
}

// snapshotQuery reads a stopped or live store through a disposable copy, never beside it.
func snapshotQuery(t *testing.T, dbPath, query string, args ...any) []map[string]any {
	t.Helper()
	copyPath, cleanup, err := ownership.CopySnapshot(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	db, err := ownership.OpenExisting(t.Context(), copyPath, "ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err = rows.Scan(pointers...); err != nil {
			t.Fatal(err)
		}
		row := map[string]any{}
		for i, column := range columns {
			if b, ok := values[i].([]byte); ok {
				values[i] = string(b)
			}
			row[column] = values[i]
		}
		out = append(out, row)
	}
	return out
}

// cli.py main runs check_start for every command that is neither read-only nor answers without
// the selected store, before the selection refusal, --kind-module and the handler (backlog before
// todo 42, decision 31). On a store the other runtime owns, a write form therefore answers the
// ownership refusal, byte for byte in both runtimes, whether its --kind-module cannot be imported,
// it names no --socket (daemon and managed-start refuse that in their handler), or its --socket
// is not the one the store recorded. Under its own owner each runtime still answers the
// mismatched socket with the selection refusal. The Go side is the built binary, which registers
// every command family (sync-target included).
func Test31_check_start_precedes_the_selection_kind_module_and_handler_refusals(t *testing.T) {
	home := pythonHome(t)
	_, alias := packageBinary(t)
	bound := filepath.Join(home, "bound.sock")
	other := filepath.Join(home, "other.sock")
	statePython := ownerOnlyState(t, home, "python-owned")
	stateGo := ownerOnlyState(t, home, "go-owned")
	testsupport.Create(t, filepath.Join(statePython, "relay.sqlite3"), bound, "python")
	if got := goCLI(t, "--state", stateGo, "--socket", bound, "store-challenge", "--write"); got.code != 0 {
		t.Fatalf("%+v", got)
	}
	markers := filepath.Join(home, "markers")
	commands := [][]string{
		{"relationship-status", "--relationship", "r-1", "--status", "paused", "--actor", "a"},
		{"fault-target", "--product", "crw", "--team", "team"},
		{"store-challenge", "--write"},
		{"sync-target", "--relationship", "r-1", "--target-ref", "ISSUE-1"},
		{"merge-turn-request", "--repository", "repo", "--base-ref", "main", "--project", "P", "--task", "task", "--host", "host", "--head", "abc"},
		{"slot-reserve", "--kind", "k", "--subject", "s", "--parent-task", "parent", "--project", "P", "--actor", "a"},
		{"region-propose", "--repository", "repo", "--revision", "rev", "--path", "p", "--kind", "file", "--left-project", "L", "--right-project", "R", "--peer-link", "link", "--task", "t", "--constraint", "c"},
		{"daemon", "--max-ticks", "0"},
		{"managed-start", "--request", "{}", "--marker-root", markers},
		{"service", "start"},
		{"service", "stop"},
	}
	scenarios := []struct {
		name    string
		globals []string
	}{
		{"an unimportable --kind-module", []string{"--socket", bound, "--kind-module", "nosuch"}},
		{"no --socket", nil},
		{"a --socket the store did not record", []string{"--socket", other}},
	}
	var fence [][]string
	for _, scenario := range scenarios {
		for _, command := range commands {
			fence = append(fence, append(append([]string{"--state", stateGo}, scenario.globals...), command...))
		}
	}
	own := []string{"--socket", other, "store-challenge", "--write"}
	fence = append(fence, append([]string{"--state", statePython}, own...))
	want := pythonCLI(t, fence...)
	i := 0
	for _, scenario := range scenarios {
		for _, command := range commands {
			ran := binaryRun(t, alias, append(append([]string{"--state", statePython}, scenario.globals...), command...)...)
			got := answer{ran.code, ran.stdout}
			if got != want[i] || got.code != 2 || object(t, got.stdout)["reason"] != "store_owned_by_other" {
				t.Errorf("%s, %q:\n go     %d %s\n python %d %s", scenario.name, command, got.code, got.stdout, want[i].code, want[i].stdout)
			}
			i++
		}
	}
	refused := binaryRun(t, alias, append([]string{"--state", stateGo}, own...)...)
	if python := want[i]; refused.code != 2 || python.code != 2 || object(t, refused.stdout)["reason"] != "state_directory_serves_another_socket" || object(t, python.stdout)["reason"] != "state_directory_serves_another_socket" {
		t.Fatalf("own owner:\n go     %+v\n python %+v", refused, python)
	}
	if _, err := os.Stat(markers); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused managed-start wrote its marker root: %v", err)
	}
}
