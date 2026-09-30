package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// A path the settings or the Stop payload name reaches the system as os.fsencode's bytes: a
// surrogate escape (U+DCFF) is the byte it stands for, and a lone surrogate nothing encodes
// (U+D800) is the ValueError Python's adapter and status catch. Each answer is Python's own
// function over the same str: the status cell (completion._journal_cell), the transcript scan
// (stopadapter.event_identity), the claim (stopadapter.claim_event) and the journal row
// (stopadapter.journal).
func TestAPathFromJSONReachesTheSystemAsPythonEncodesIt(t *testing.T) {
	root := t.TempDir()
	journal := filepath.Join(root, "j\xff") // a directory whose name is not UTF-8
	if err := os.MkdirAll(filepath.Join(journal, "20260930"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTest(t, filepath.Join(journal, "20260930", strings.Repeat("a", 32)+".json"), []byte("{}\n"))
	writeTest(t, filepath.Join(journal, "transcript.jsonl"), nil)
	spelled := store.FSDecode(journal)      // the str Python holds for it, as JSON decodes "\udcff"
	unencodable := spelled + "\xed\xa0\x80" // ... followed by U+D800, which os.fsencode refuses
	script := `import json, os, sys
sys.path.insert(0, os.path.join(sys.argv[1], "scripts"))
from crw_runtime import completion
from codex_session_relay import stopadapter
journal = sys.argv[2]
unencodable = journal + "\ud800"
cells = []
for root in (journal, unencodable):
    cell = completion._journal_cell({"journalRoot": root, "journalPolicy": "every_invocation"})
    cells.append([cell["value"], cell["evidence"], cell.get("journalRoot")])
stop = {"session_id": "s", "turn_id": "t", "stop_hook_active": False, "last_assistant_message": "x"}
reasons = [stopadapter.event_identity(dict(stop, transcript_path=path))[1]["reason"]
           for path in (journal + "/transcript.jsonl", journal + "/missing.jsonl", unencodable)]
claim = stopadapter.claim_event({"journalRoot": unencodable}, "k" * 64, {"answerItem": "i"}, stop, ("20260930", "b" * 32))
row = stopadapter.journal({"journalRoot": unencodable}, {"adapterOutcome": "guard_answered"}, ("20260930", "c" * 32))
print(json.dumps({"cells": cells, "reasons": reasons, "claim": claim[0], "row": row}))`
	command := exec.Command(python(t), "-c", script, testRoot, journal)
	command.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	raw, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("python: %v\n%s", err, raw)
	}
	decoded, err := Decode(raw) // keeps each "\udcXX" a lone surrogate, as Python's json does
	if err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	want, _ := evidence.Object(decoded)
	before, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}

	cells := get(want, "cells").([]any)
	for i, spelling := range []string{spelled, unencodable} {
		got := journalCell(Object{{Key: "journalRoot", Value: spelling}, {Key: "journalPolicy", Value: "every_invocation"}})
		expected := cells[i].([]any)
		if got["value"] != expected[0] || got["evidence"] != expected[1] || got["journalRoot"] != expected[2] {
			t.Errorf("journal cell for %q: %q %q %q\npython %q", spelling, got["value"], got["evidence"], got["journalRoot"], expected)
		}
	}
	stop := Object{{Key: "session_id", Value: "s"}, {Key: "turn_id", Value: "t"}, {Key: "stop_hook_active", Value: false}, {Key: "last_assistant_message", Value: "x"}}
	reasons := get(want, "reasons").([]any)
	for i, path := range []string{spelled + "/transcript.jsonl", spelled + "/missing.jsonl", unencodable} {
		_, identity := EventIdentity(context.Background(), set(append(Object{}, stop...), "transcript_path", path))
		if got := get(identity, "reason"); got != reasons[i] {
			t.Errorf("transcript %q: %v, python %v", path, got, reasons[i])
		}
	}
	config := Object{{Key: "journalRoot", Value: unencodable}}
	slot := Slot{"20260930", strings.Repeat("b", 32)}
	if got, _ := ClaimEvent(context.Background(), config, strings.Repeat("k", 64), Object{{Key: "answerItem", Value: "i"}}, stop, slot, ""); got != get(want, "claim") {
		t.Errorf("claim under %q: %v, python %v", unencodable, got, get(want, "claim"))
	}
	if row, _ := Journal(context.Background(), config, Object{{Key: "adapterOutcome", Value: "guard_answered"}}, Slot{"20260930", strings.Repeat("c", 32)}); row != "" || get(want, "row") != nil {
		t.Errorf("journal row under %q: %q, python %v", unencodable, row, get(want, "row"))
	}
	after, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.EqualFunc(before, after, func(a, b os.DirEntry) bool { return a.Name() == b.Name() }) {
		t.Errorf("a root os.fsencode refuses was created: %v, then %v", before, after)
	}
}

// The settings path and the host ledger are the ones Python names: Path.home() when no Codex
// home is set (an empty HOME is the root, an unset one the passwd entry), and a relative path
// made absolute against the working directory the kernel names (os.path.abspath), not the $PWD
// spelling that reached it through a symbolic link.
func TestTheSettingsPathAndHostLedgerAreThePathsPythonNames(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(root, "real", "wd"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias", "wd")
	t.Chdir(alias)
	script := `import json, os, sys
sys.path.insert(0, os.path.join(sys.argv[1], "scripts"))
from crw_runtime import completion
from codex_session_relay import stopadapter
print(json.dumps([str(completion.configuration_path()), str(stopadapter.host_ledger())]))`
	unset := "(unset)"
	for _, c := range []struct{ name, home, codexHome, override string }{
		{"an empty HOME", "", unset, ""},
		{"HOME unset", unset, unset, ""},
		{"a relative CODEX_HOME", filepath.Join(root, "home"), "codex", ""},
		{"a relative override", filepath.Join(root, "home"), unset, "rel/settings.json"},
		{"a CODEX_HOME of two leading slashes", filepath.Join(root, "home"), "/" + filepath.Join(root, "codex"), ""},
		{"a CODEX_HOME of two leading slashes and a dot", filepath.Join(root, "home"), "/" + filepath.Join(root, ".", "codex") + "/./", ""},
		{"the root as CODEX_HOME", filepath.Join(root, "home"), "/", ""},
		{"a CODEX_HOME of two slashes alone", filepath.Join(root, "home"), "//", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			for key, value := range map[string]string{"HOME": c.home, "CODEX_HOME": c.codexHome, configEnv: c.override} {
				if value != unset {
					t.Setenv(key, value)
					continue
				}
				t.Setenv(key, "") // restored after the test
				if err := os.Unsetenv(key); err != nil {
					t.Fatal(err)
				}
			}
			command := exec.Command(python(t), "-c", script, testRoot)
			command.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
			raw, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("python: %v\n%s", err, raw)
			}
			var want []string
			if err = json.Unmarshal(raw, &want); err != nil {
				t.Fatalf("%v: %s", err, raw)
			}
			settings, err := configurationPath("", nil, "")
			if err != nil || settings != want[0] {
				t.Errorf("settings %q, python %q: %v", settings, want[0], err)
			}
			if host, err := hostLedger(); err != nil || host != want[1] {
				t.Errorf("host ledger %q, python %q: %v", host, want[1], err)
			}
		})
	}
}

// hook status reads a registration's settings where completion._settled puts them: Path.expanduser
// (an empty HOME is the root, ~ alone is the home itself) and then os.path.abspath.
func TestStatusSettlesARegistrationsSettingsAsPythonDoes(t *testing.T) {
	home := t.TempDir()
	entry := filepath.Join(home, entryPointName)
	writeTest(t, entry, []byte("# adapter"))
	script := `import json, os, sys
sys.path.insert(0, os.path.join(sys.argv[1], "scripts"))
from crw_runtime import completion
s = completion.status(environ=json.loads(sys.argv[2]))
print(json.dumps([s["configuration"]["configuration"], s["configuration"]["value"]]))`
	for _, settings := range []string{"~/s.json", "~"} {
		t.Run(settings, func(t *testing.T) {
			t.Setenv("HOME", "")
			writeStatusJSON(t, filepath.Join(home, "hooks.json"), map[string]any{"hooks": map[string]any{"Stop": []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "python3 " + entry + " " + settings, "timeout": 10}}}}}})
			env := map[string]string{"CODEX_HOME": home}
			raw, _ := json.Marshal(env)
			command := exec.Command(python(t), "-c", script, testRoot, string(raw))
			command.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
			out, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("python: %v\n%s", err, out)
			}
			var want []any
			if err = json.Unmarshal(out, &want); err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			got := Status(context.Background(), "", env, "Stop")["configuration"].(map[string]any)
			if got["configuration"] != want[0] || got["value"] != want[1] {
				t.Errorf("configuration %v (%v), python %v (%v)", got["configuration"], got["value"], want[0], want[1])
			}
		})
	}
}

// The owner's control socket is dialled in the directory each source names. A dbPath is the
// settings' str, so a surrogate escape in it (U+DCFF) is the byte it stands for, as Python's
// socket_guard connects to os.fsencode(str(state / "control.sock")). A directory the environment
// selects is already the bytes it names: a literal ED B3 BF run in XDG_STATE_HOME stays those
// bytes, as the owner that listens there spells it, and is not re-encoded to the byte 0xff.
func TestTheControlSocketIsDialledWhereItsSourceNamesIt(t *testing.T) {
	for _, c := range []struct {
		name   string
		config func(home string) (Object, string)
	}{
		{"a dbPath from the settings", func(home string) (Object, string) {
			state := filepath.Join(home, "s\xff")
			return Object{{Key: "dbPath", Value: store.FSDecode(state) + "/relay.sqlite3"}}, state
		}},
		{"a state directory from the environment", func(home string) (Object, string) {
			xdg := filepath.Join(home, "x\xed\xb3\xbf")
			t.Setenv("XDG_STATE_HOME", xdg)
			return nil, filepath.Join(xdg, "codex-session-relay", "default")
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			home := hookHome(t, 5)
			t.Setenv("CODEX_HOME", home)
			t.Setenv("HOME", home)
			t.Setenv("CODEX_SESSION_RELAY_STATE", "")
			t.Setenv("XDG_STATE_HOME", filepath.Join(home, "xdg"))
			pinned, state := c.config(home)
			config := Object{{Key: "configVersion", Value: int64(1)}, {Key: "mode", Value: "observe"}, {Key: "relayExecutable", Value: filepath.Join(home, "never-run")}, {Key: "markerRoot", Value: filepath.Join(home, "markers")}, {Key: "timeoutSeconds", Value: 5.0}, {Key: "journalRoot", Value: filepath.Join(home, "journal")}}
			writeTest(t, filepath.Join(home, ConfigName), []byte(evidence.Dumps(append(config, pinned...), false, false, true)))
			if err := os.MkdirAll(state, 0o700); err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("unix", filepath.Join(state, "control.sock"))
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			done := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err == nil {
					defer conn.Close()
					if _, err = readFrame(conn); err == nil {
						_, err = io.WriteString(conn, `{"decision":"release","state":"unmanaged","hook_output":{}}`+"\n")
					}
				}
				done <- err
			}()
			var out bytes.Buffer
			if code := runAdapter(context.Background(), nil, pipeInput(t, []byte(`{"session_id":"s","turn_id":"t"}`)), &out, time.Now(), nil); code != 0 || out.Len() != 0 {
				t.Fatalf("code=%d stdout=%s", code, &out)
			}
			if rows := rowsAt(t, home); len(rows) != 1 || rows[0]["adapterOutcome"] != "guard_answered" {
				t.Fatalf("the hook did not reach the owner listening at %q: %v", state, rows)
			}
			awaitHost(t, done)
		})
	}
}

// The status journal cell reads and names str(Path(root).expanduser()), as completion._journal_cell
// does: ~ expanded, "." and empty components and a trailing slash dropped, exactly two leading
// slashes kept. os.fsencode's refusal of a lone surrogate counts its position in that spelling.
func TestTheJournalCellNamesTheRootAsPathlibSpellsIt(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", root)
	if err = os.MkdirAll(filepath.Join(root, "d", "20260930"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeTest(t, filepath.Join(root, "d", "20260930", strings.Repeat("a", 32)+".json"), []byte("{}\n"))
	unencodable := "\xed\xa0\x80" // U+D800 as a JSON string decodes it
	var roots []any
	for _, spelled := range []string{
		root + "//a/./b" + unencodable, "/" + root + "/./a//b/" + unencodable + unencodable + "/c", "~/./a" + unencodable,
		root + "//a/./b", root + "/c/", "/" + root + "/c", "~/./c/",
		root + "//d/./", "/" + root + "/d", "~//d",
	} {
		roots = append(roots, spelled)
	}
	script := `import json, os, sys
sys.path.insert(0, os.path.join(sys.argv[1], "scripts"))
from crw_runtime import completion
cells = []
for root in json.loads(sys.argv[2]):
    cell = completion._journal_cell({"journalRoot": root, "journalPolicy": "every_invocation"})
    cells.append([cell["value"], cell["evidence"], cell.get("journalRoot")])
print(json.dumps(cells))`
	command := exec.Command(python(t), "-c", script, testRoot, evidence.Dumps(roots, false, false, true))
	command.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	raw, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("python: %v\n%s", err, raw)
	}
	decoded, err := Decode(raw)
	if err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
	for i, want := range decoded.([]any) {
		expected := want.([]any)
		got := journalCell(Object{{Key: "journalRoot", Value: roots[i]}, {Key: "journalPolicy", Value: "every_invocation"}})
		if got["value"] != expected[0] || got["evidence"] != expected[1] || got["journalRoot"] != expected[2] {
			t.Errorf("journal cell for %q:\n go     %q %q %q\n python %q", roots[i], got["value"], got["evidence"], got["journalRoot"], expected)
		}
	}
}

// hook status opens the paths a registration names as completion.status does: the hook file's
// strings keep a surrogate escape ("\udcff") as json.loads does, and the settings path and the
// adapter target reach the system as os.fsencode's bytes, so a settings file and an adapter under
// a name that is not UTF-8 are found and read, and each cell names the path as the str it is.
func TestStatusOpensARegistrationsPathsAsPythonEncodesThem(t *testing.T) {
	home := t.TempDir()
	adapter := filepath.Join(home, "a\xff", entryPointName)
	writeTest(t, adapter, []byte("# adapter"))
	writeTest(t, filepath.Join(home, "s\xff.json"), []byte(`{"configVersion": 1, "mode": "observe", "relayExecutable": "/x/r", "markerRoot": "/x/m", "timeoutSeconds": 5}`))
	spelled := func(name string) string { return strings.ReplaceAll(filepath.Join(home, name), "\xff", `\udcff`) }
	hooks := `{"hooks": {"Stop": [{"hooks": [{"type": "command", "command": "python3 ` + spelled("a\xff/"+entryPointName) + " " + spelled("s\xff.json") + `", "timeout": 10}]}]}}`
	writeTest(t, filepath.Join(home, "hooks.json"), []byte(hooks))
	t.Setenv("HOME", home)
	script := `import json, os, sys
sys.path.insert(0, os.path.join(sys.argv[1], "scripts"))
from crw_runtime import completion
s = completion.status(environ={"CODEX_HOME": sys.argv[2]})
named = s["configuration"]["namedSettings"]
print(json.dumps([s["configuration"]["value"], s["configuration"]["configuration"], s["registeredCommandTarget"]["value"],
    s["registeredCommandTarget"]["probes"][0]["path"], [[n["settings"], n["settingsState"], n["usable"]] for n in named]]))`
	command := exec.Command(python(t), "-c", script, testRoot, home)
	command.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("python: %v\n%s", err, out)
	}
	decoded, err := Decode(out) // keeps Python's surrogate escapes as the code points they are
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	want := evidence.Dumps(decoded, false, false, true)
	status := Status(context.Background(), home, map[string]string{"CODEX_HOME": home}, "Stop")
	configuration := status["configuration"].(map[string]any)
	target := status["registeredCommandTarget"].(map[string]any)
	named := []any{}
	for _, n := range configuration["namedSettings"].([]any) {
		entry := n.(map[string]any)
		named = append(named, []any{entry["settings"], entry["settingsState"], entry["usable"]})
	}
	got := evidence.Dumps([]any{configuration["value"], configuration["configuration"], target["value"], target["probes"].([]any)[0].(map[string]any)["path"], named}, false, false, true)
	if got != want {
		t.Errorf("status:\n go     %s\n python %s", got, want)
	}
	if !strings.Contains(want, `"PRESENT"`) {
		t.Fatalf("the fixture did not reach a present configuration in Python: %s", want)
	}
}

// A program found by PATH lookup is judged at the path the lookup found. shutil.which answers
// os.fsdecode of the bytes it found, so a directory whose name holds the bytes ED B3 BF (the
// encoding of a surrogate, which fsdecode escapes byte by byte) is the same directory when the
// cell encodes the str back, not the byte FF that a raw U+DCFF would stand for.
func TestAProgramFoundByPATHLookupIsJudgedWhereItWasFound(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "x\xed\xb3\xbf")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	program := filepath.Join(dir, "crw-lookup-probe")
	if err := os.WriteFile(program, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	cell := interpreterProbe(context.Background(), "crw-lookup-probe", false)
	if cell["value"] == absent {
		t.Fatalf("the program PATH lookup found is reported absent: %v", cell)
	}
	if want := store.FSDecode(program); cell["path"] != want {
		t.Fatalf("path %q, want shutil.which's str %q", cell["path"], want)
	}
}
