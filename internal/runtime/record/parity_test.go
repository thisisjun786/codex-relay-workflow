//go:build parity

package record_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/pointer"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/staging"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/swapgate"
)

// These cross checks run live Python (scripts/crw_runtime and the relay package in .venv), so
// they are behind the parity tag; the default suite checks the committed goldens.

func python(t *testing.T, program string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(filepath.Join(golden.Root(), ".venv", "bin", "python"), append([]string{"-c", program}, args...)...)
	cmd.Dir = golden.Root()
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1", "PYTHONPATH="+filepath.Join(golden.Root(), "scripts"))
	return cmd
}

func output(t *testing.T, cmd *exec.Cmd) []byte {
	t.Helper()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%v: %v\n%s", cmd.Args, err, stderr.String())
	}
	return out
}

// The committed goldens are still what scripts/crw_runtime answers.
func TestParity_goldens_are_the_live_python_answers(t *testing.T) {
	cmd := exec.Command("python3", filepath.Join(golden.Dir(), "python_goldens.py"))
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	got := output(t, cmd)
	want, err := os.ReadFile(filepath.Join(golden.Dir(), "goldens.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("scripts/crw_runtime's answers changed; regenerate internal/runtime/testdata/goldens.json and re-read the Go ports")
	}
}

// The schema a Go candidate declares is the one runtime_install.py's candidate program derives
// from the Python relay's DDL and guard indexes, object for object.
func TestParity_declared_schema_is_the_python_candidates(t *testing.T) {
	out := output(t, python(t, `
import json, sqlite3
from crw_runtime import swapgate
from codex_session_relay import store
database = sqlite3.connect(":memory:")
database.executescript(store.DDL)
for _name, _statement in store.GUARD_INDEXES:
    database.execute(_statement)
print(json.dumps({row[0]: row[1] for row in database.execute(swapgate.SCHEMA_OBJECTS_QUERY)}, sort_keys=True))
`))
	value, err := reading.Decode(out)
	if err != nil {
		t.Fatal(err)
	}
	declared := swapgate.DeclaredSchema(context.Background())
	if golden.Canon(record.Get(declared, "objects")) != golden.Canon(value) {
		t.Fatal("the Go declared schema differs from the Python candidate's")
	}
}

// The promotion lock and the staging lock are one flock between the runtimes: a Python holder
// makes Go's probe HELD and its promotion Busy; a Go holder makes Python's Exclusive raise Busy
// and its owner_liveness LIVE.
func TestParity_locks_exclude_across_runtimes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, record.Name)
	holder := python(t, `
import sys
from crw_runtime import hostrecord
with hostrecord.Exclusive(sys.argv[1]):
    print("held", flush=True)
    sys.stdin.read()
`, path)
	stdin, _ := holder.StdinPipe()
	stdout, _ := holder.StdoutPipe()
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	if line, _ := bufio.NewReader(stdout).ReadString('\n'); strings.TrimSpace(line) != "held" {
		t.Fatalf("python holder: %q", line)
	}
	if state, _ := record.Probe(path + record.PromotionLockSuffix); state != record.Held {
		t.Fatalf("a Python-held promotion lock probed %s", state)
	}
	var busy *record.Busy
	if _, err := record.Promote(path, 150*time.Millisecond); !errors.As(err, &busy) {
		t.Fatalf("Go promoted while Python held the lock: %v", err)
	}
	_ = stdin.Close()
	_ = holder.Wait()

	held, err := record.Promote(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	env := filepath.Join(dir, "env")
	stage, err := staging.Take(env)
	if err != nil {
		t.Fatal(err)
	}
	out := output(t, python(t, `
import sys
from crw_runtime import hostrecord, staging
try:
    with hostrecord.Exclusive(sys.argv[1], timeout=0.2):
        print("promoted")
except hostrecord.Busy:
    print("busy")
print(staging.owner_liveness(sys.argv[2])[0])
`, path, env))
	held.Release()
	stage.Release()
	if got := strings.Fields(string(out)); len(got) != 2 || got[0] != "busy" || got[1] != staging.Live {
		t.Fatalf("python while Go held both locks: %v", got)
	}
}

// pointer.read and reading.read_json give the same state (and, for the pointer, target and
// detail) on the same filesystem in both runtimes.
func TestParity_pointer_and_reading_states(t *testing.T) {
	dir := t.TempDir()
	mk := func(p string) {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mk(filepath.Join(dir, "env"))
	mk(filepath.Join(dir, "realdir"))
	if err := os.Symlink(filepath.Join(dir, "env"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing", filepath.Join(dir, "dangling")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("loop", filepath.Join(dir, "loop")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"good.json": `{"a": 1}`, "broken.json": "{ x", "list.json": "[]", "bom.json": "\ufeff{}", "latin.json": "\xff"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	names := []string{"missing", "env", "realdir", "link", "dangling", "loop", "fifo", "good.json", "broken.json", "list.json", "bom.json", "latin.json"}
	out := output(t, python(t, `
import json, sys
from crw_runtime import pointer, reading
answers = {}
for name in sys.argv[2:]:
    path = sys.argv[1] + "/" + name
    answers[name] = {"pointer": pointer.read(path), "json": reading.read_json(path, "x").state,
                     "exception": reading.read_json(path, "x").exception}
print(json.dumps(answers))
`, append([]string{dir}, names...)...))
	var answers map[string]struct {
		Pointer struct {
			State, Detail string
			Target        *string
		}
		JSON      string `json:"json"`
		Exception *string
	}
	if err := json.Unmarshal(out, &answers); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		path := filepath.Join(dir, name)
		want := answers[name]
		got := pointer.Read(path)
		target := ""
		if want.Pointer.Target != nil {
			target = *want.Pointer.Target
		}
		if got.State != want.Pointer.State || got.Target != target || (got.State != pointer.Unreachable && got.Detail != want.Pointer.Detail) {
			t.Errorf("pointer %s: go %+v, python %+v", name, got, want.Pointer)
		}
		read := reading.ReadJSON(path, "x", nil, nil)
		exception := ""
		if want.Exception != nil {
			exception = *want.Exception
		}
		if read.State != want.JSON || read.Exception != exception {
			t.Errorf("read_json %s: go %s %s, python %s %s", name, read.State, read.Exception, want.JSON, exception)
		}
	}
}

// Python's fault sweep reads a record the Go installer wrote: still record version 1, and a Go
// entry at the copy's location is that copy's revision, exactly as the Go sweeper reads it.
func TestParity_python_faultsweep_reads_a_go_install_entry(t *testing.T) {
	dir := t.TempDir()
	environment := filepath.Join(dir, "bin-0.3.0-aaaaaaaaaaaa")
	if err := os.MkdirAll(filepath.Join(environment, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	restore := record.StampSourceTree(strings.Repeat("2", 40))
	defer restore()
	entry := record.GoInstall(environment, "codex-session-relay", strings.Repeat("a", 64), "crw install", true)
	source := record.Set(record.Get(entry, "source").(record.Object), "repositoryCommit", strings.Repeat("1", 40))
	source = record.Set(source, "workingTreeClean", true)
	entry = record.Set(entry, "source", source)
	fixtureRecord, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, record.Name)
	if err := os.WriteFile(path, fixtureRecord, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := record.Update(path, 1, record.Delta{Installs: []record.Named{{Component: "codex-session-relay", Entry: entry}}}); err != nil {
		t.Fatal(err)
	}
	out := output(t, python(t, `
import json, sys
from codex_session_relay import faultsweep
faultsweep.PACKAGE_DIRECTORY = sys.argv[2]
print(json.dumps(faultsweep.installed_revision(sys.argv[1]), sort_keys=True))
`, path, filepath.Join(environment, "bin")))
	value, err := reading.Decode(out)
	if err != nil {
		t.Fatal(err)
	}
	revision := golden.Obj(record.Get(value.(record.Object), "revision"))
	if record.Get(revision, "environment") != environment || record.Get(revision, "integrity") != strings.Repeat("a", 64) || record.Get(revision, "repositoryTree") != strings.Repeat("2", 40) {
		t.Fatalf("python read %s", out)
	}
}
