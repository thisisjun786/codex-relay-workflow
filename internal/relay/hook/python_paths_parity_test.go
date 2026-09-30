package hook

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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
			if host, err := abspath(filepath.Join(append([]string{codexHome()}, HostLedgerParts...)...)); err != nil || host != want[1] {
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
