package cli_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
)

// ownershipBlock is doctor's "ownership" object as printed, with runtime_build, which names
// the answering runtime (decisions.md 31), set to the Python build.
func ownershipBlock(t *testing.T, stdout string) string {
	t.Helper()
	start := strings.Index(stdout, "\n  \"ownership\": {")
	if start < 0 {
		t.Fatalf("no ownership block:\n%s", stdout)
	}
	end := start + strings.Index(stdout[start:], "\n  }")
	block := stdout[start : end+len("\n  }")]
	return regexp.MustCompile(`"runtime_build": "[^"]*"`).ReplaceAllString(block, `"runtime_build": "`+ownership.CompatibilityBuild+`"`)
}

// doctor's ownership block is ownership.report's all-or-nothing reading: a mirror or database
// that cannot be read nulls every key and the phase and names the failure in detail; a
// readable mirror's phase is echoed whatever its JSON type, as is each process record's
// python_compatibility_build. Both runtimes answer the same block for the same broken store.
// Where the record is refused whichever runtime reads it (the mirror or ownership.py
// validate refuses it before asking who owns the store), the access block is the same too:
// the probe's foreign branch, worded as check_start refuses (store.probe).
func TestDoctor_ownership_block_matches_python_on_a_broken_store(t *testing.T) {
	home := pythonHome(t)
	_, alias := packageBinary(t)
	write := func(path, text string) func(*testing.T) {
		return func(t *testing.T) {
			if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	durable := func(statement string) func(t *testing.T, state string) {
		return func(t *testing.T, state string) {
			db, err := sql.Open("sqlite", filepath.Join(state, "relay.sqlite3"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if _, err := db.Exec(statement); err != nil {
				t.Fatal(err)
			}
		}
	}
	stampOnly := map[string]bool{"empty mirror object": true, "incomplete record": true, "mistyped record": true, "numeric phase": true,
		"control socket of another state directory": true, "transition of another takeover": true, "invalid durable epoch": true, "record of another store": true}
	for _, c := range []struct {
		name      string
		break_    func(t *testing.T, state string)
		ownership string // a fragment of the ownership block both runtimes print
		access    string // when set, both access blocks are equal and their detail carries it
	}{
		{"malformed mirror", func(t *testing.T, state string) { write(filepath.Join(state, "takeover.json"), "{not json")(t) },
			`"detail": "store_owned_by_other: takeover record unreadable: JSONDecodeError: Expecting property name enclosed in double quotes: line 1 column 2 (char 1)"`,
			"store_owned_by_other: takeover record unreadable: JSONDecodeError: Expecting property name enclosed in double quotes: line 1 column 2 (char 1)"},
		{"mirror not an object", func(t *testing.T, state string) { write(filepath.Join(state, "takeover.json"), "[]")(t) },
			`"detail": "store_owned_by_other: takeover record is not an object"`, "store_owned_by_other: takeover record is not an object"},
		{"mirror not UTF-8", func(t *testing.T, state string) {
			write(filepath.Join(state, "takeover.json"), "{\"phase\": \"\xff\"}")(t)
		},
			`"detail": "store_owned_by_other: takeover record unreadable: UnicodeDecodeError: `,
			"store_owned_by_other: takeover record unreadable: UnicodeDecodeError: 'utf-8' codec can't decode byte 0xff in position 11: invalid start byte"},
		{"unreadable mirror", func(t *testing.T, state string) { chmod(t, filepath.Join(state, "takeover.json"), 0) },
			`"detail": "store_owned_by_other: takeover record unreadable: PermissionError: [Errno 13] Permission denied: `,
			"store_owned_by_other: takeover record unreadable: PermissionError: [Errno 13] Permission denied: "},
		{"unreadable database", func(t *testing.T, state string) { chmod(t, filepath.Join(state, "relay.sqlite3"), 0) },
			`"detail": "[Errno 13] Permission denied: `, ""},
		{"not a database", func(t *testing.T, state string) {
			write(filepath.Join(state, "relay.sqlite3"), strings.Repeat("not a database ", 512))(t)
		},
			`"detail": "file is not a database"`, ""},
		{"database without schema_meta", durable("DROP TABLE schema_meta"), `"owner": null,`, "store_owned_by_other: missing or unsupported writer protocol"},
		{"empty mirror object", func(t *testing.T, state string) { write(filepath.Join(state, "takeover.json"), "{}")(t) },
			`"phase": null,`, "store_owned_by_other: missing or unsupported writer protocol"},
		{"incomplete record", func(t *testing.T, state string) {
			editMirror(t, state, func(r map[string]any) { delete(r, "updatedAt") })
		},
			`"phase": "active",`, "store_owned_by_other: incomplete ownership record"},
		{"mistyped record", func(t *testing.T, state string) { editMirror(t, state, func(r map[string]any) { r["epoch"] = "1" }) },
			`"phase": "active",`, "store_owned_by_other: mistyped ownership record"},
		{"numeric phase", func(t *testing.T, state string) { rewriteMirror(t, state, `"phase":"active"`, `"phase":7`) },
			`"phase": 7,`, "store_owned_by_other: invalid takeover phase"},
		{"control socket of another state directory", func(t *testing.T, state string) {
			editMirror(t, state, func(r map[string]any) { r["relayRPCSocket"] = "/elsewhere/control.sock" })
		}, `"phase": "active",`, "store_owned_by_other: the control socket belongs to another state directory"},
		{"transition of another takeover", func(t *testing.T, state string) {
			editMirror(t, state, func(r map[string]any) { r["transition"] = map[string]any{"id": "elsewhere"} })
		}, `"phase": "active",`, "store_owned_by_other: ownership transition disagrees"},
		{"invalid durable epoch", durable("UPDATE schema_meta SET value='0' WHERE key='owner_epoch'"),
			`"owner_epoch": "0",`, "store_owned_by_other: invalid ownership epoch"},
		{"record of another store", func(t *testing.T, state string) {
			editMirror(t, state, func(r map[string]any) { r["storeId"] = "another" })
		},
			`"phase": "active",`, "store_owned_by_other: ownership record disagrees with the durable store"},
		{"non-string process builds", func(t *testing.T, state string) {
			write(filepath.Join(state, "daemon.json"), `{"python_compatibility_build": 7}`)(t)
		}, `"supervisor": 7,`, ""},
		// cutover.md Record: "initial stamp committed, mirror absent" keeps its six keys.
		{"stamp without its mirror", func(t *testing.T, state string) {
			if err := os.Remove(filepath.Join(state, "takeover.json")); err != nil {
				t.Fatal(err)
			}
		}, `"owner": "python",` + "\n" + `    "owner_epoch": "1",`, "store_owned_by_other: missing or unsupported writer protocol"},
	} {
		t.Run(c.name, func(t *testing.T) {
			state := filepath.Join(home, strings.ReplaceAll(c.name, " ", "-"))
			pythonCreates(t, state)
			c.break_(t, state)
			// Both runtimes diagnose the very same broken store: no handover (which refuses a
			// broken record) runs between them.
			py := fence(t, "--state", state, "doctor")
			got := binaryRun(t, alias, "--state", state, "doctor")
			pyBlock, goBlock := ownershipBlock(t, py.stdout), ownershipBlock(t, got.stdout)
			if pyBlock != goBlock || !strings.Contains(goBlock, c.ownership) {
				t.Fatalf("want %s\npython:%s\ngo:%s", c.ownership, pyBlock, goBlock)
			}
			if c.access == "" {
				return
			}
			pyAccess, goAccess := decode(t, py.stdout)["access"], decode(t, got.stdout)["access"]
			detail, _ := goAccess.(map[string]any)["detail"].(string)
			if stampOnly[c.name] {
				// The fence's validate refused this record for its mirror, which Go no longer
				// judges (decision 56): Go's probe reads the stamp, which names Python.
				if !strings.Contains(detail, "store_owned_by_other: the relay store belongs to another runtime") {
					t.Fatalf("go access detail %q", detail)
				}
				return
			}
			if !reflect.DeepEqual(pyAccess, goAccess) || !strings.Contains(detail, c.access) {
				t.Fatalf("access detail carrying %q\npython: %v\ngo:     %v", c.access, pyAccess, goAccess)
			}
		})
	}
}

// editMirror rewrites takeover.json through a JSON reading that keeps its numbers as written.
func editMirror(t *testing.T, state string, edit func(map[string]any)) {
	t.Helper()
	path := filepath.Join(state, "takeover.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var record map[string]any
	if err := decoder.Decode(&record); err != nil {
		t.Fatal(err)
	}
	edit(record)
	if raw, err = json.Marshal(record); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func chmod(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
}

func rewriteMirror(t *testing.T, state, from, to string) {
	t.Helper()
	path := filepath.Join(state, "takeover.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), from) {
		t.Fatalf("%s lacks %s: %s", path, from, raw)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(raw), from, to, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
}

// doctor's runtime_build names the answering runtime: the build the Go binary publishes as its
// holder identity (its version when no build is stamped), never the Python fence build, and
// the production binary links no test support to learn it.
func TestDoctor_runtime_build_is_the_answering_go_build(t *testing.T) {
	home := pythonHome(t)
	binary, alias := packageBinary(t)
	version := strings.TrimSpace(binaryRun(t, binary, "version").stdout)
	report := decode(t, binaryRun(t, alias, "--state", filepath.Join(home, "state"), "doctor").stdout)
	build := report["ownership"].(map[string]any)["runtime_build"]
	if build != version || build == ownership.CompatibilityBuild {
		t.Fatalf("runtime_build %v, crw version %q", build, version)
	}
	command := exec.Command("go", "list", "-deps", "./cmd/crw")
	command.Dir = repositoryRoot(t)
	deps, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, dep := range strings.Fields(string(deps)) {
		if dep == "testing" || strings.HasSuffix(dep, "/internal/testsupport") {
			t.Fatalf("./cmd/crw links %s", dep)
		}
	}
}
