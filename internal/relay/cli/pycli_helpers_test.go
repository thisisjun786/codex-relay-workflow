package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// Decision 25 across the runtimes, in process: the Go CLI's answers against the goldens (what
// the retained Python fence answered to the same forms, at first). The built-binary candidate
// drain is internal/relay/service Test31GoCandidateDrainsPythonQueuedEntriesBuiltCLI.

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

// batchKey is the golden key of a batch of answers to argvs: the label the fence's answers to
// the batch were recorded under, which names the batch's size and its first argv.
func batchKey(t *testing.T, argvs ...[]string) string {
	t.Helper()
	label := "cli.main"
	if len(argvs) > 0 {
		label += fmt.Sprintf(" x%d %s", len(argvs), keyLabel(argvs[0]...))
	}
	return goldenKey(t, label)
}

// expectAnswers checks a batch of answers (each one's exit and stdout) against the golden under
// key, with the run's directories the argvs name as placeholders.
func expectAnswers(t *testing.T, key string, answers []answer, argvs ...[]string) {
	t.Helper()
	var words []string
	for _, argv := range argvs {
		words = append(words, argv...)
	}
	values := make([]map[string]any, len(answers))
	for i, a := range answers {
		values[i] = map[string]any{"code": a.code, "stdout": a.stdout}
	}
	expectJSON(t, key, values, words...)
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
	home := tempHome(t)
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
	own := []string{"--socket", other, "store-challenge", "--write"}
	var argvs [][]string
	var answers []answer
	i := 0
	for _, scenario := range scenarios {
		for _, command := range commands {
			argv := append(append([]string{"--state", statePython}, scenario.globals...), command...)
			ran := binaryRun(t, alias, argv...)
			got := answer{ran.code, ran.stdout}
			argvs, answers = append(argvs, argv), append(answers, got)
			if got.code != 2 || object(t, got.stdout)["reason"] != "store_owned_by_other" {
				t.Errorf("%s, %q: %d %s", scenario.name, command, got.code, got.stdout)
			}
			i++
		}
	}
	argv := append([]string{"--state", stateGo}, own...)
	refused := binaryRun(t, alias, argv...)
	argvs, answers = append(argvs, argv), append(answers, answer{refused.code, refused.stdout})
	expectAnswers(t, batchKey(t, argvs...), answers, argvs...)
	if refused.code != 2 || object(t, refused.stdout)["reason"] != "state_directory_serves_another_socket" {
		t.Fatalf("own owner: %+v", refused)
	}
	if _, err := os.Stat(markers); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused managed-start wrote its marker root: %v", err)
	}
}
