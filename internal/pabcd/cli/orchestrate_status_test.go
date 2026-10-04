package cli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

func statusEnv(values map[string]string) host.LookupEnv {
	return func(key string) (string, bool) { value, ok := values[key]; return value, ok }
}

func statusRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"ws", "home", "codex", "crw"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("CRW_HOME", filepath.Join(root, "crw"))
	return root
}

func statusFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}

// Snapshot workspace bytes and entry kinds. Native SQLite sidecars are checked separately.
func statusTree(t *testing.T, root string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			tree[rel] = "dir"
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			tree[rel] = "link:" + target
			return err
		}
		b, err := os.ReadFile(path)
		tree[rel] = "file:" + string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func TestResolveSessionSelection(t *testing.T) {
	root := statusRoot(t)
	cwd := filepath.Join(root, "ws")
	got, err := ResolveSession(cwd, nil)
	if got != nil || err != nil {
		t.Fatalf("missing: %v %v", got, err)
	}
	dir := filepath.Join(cwd, ".crw", "sessions")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	got, err = ResolveSession(cwd, nil)
	if got != nil || err != nil {
		t.Fatalf("empty: %v %v", got, err)
	}
	for _, name := range []string{"old.json", "new.json", "ignored.json.tmp"} {
		statusFile(t, filepath.Join(dir, name), "{}")
	}
	for name, sec := range map[string]int64{"old.json": 100, "new.json": 200, "ignored.json.tmp": 300} {
		stamp := time.Unix(sec, 0)
		if err := os.Chtimes(filepath.Join(dir, name), stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	for _, explicit := range []*string{nil, statusPtr("")} {
		got, err = ResolveSession(cwd, explicit)
		if err != nil || got == nil || *got != "new" {
			t.Fatalf("latest: %v %v", got, err)
		}
	}
	got, err = ResolveSession(cwd, statusPtr("explicit"))
	if err != nil || got == nil || *got != "explicit" {
		t.Fatalf("explicit: %v %v", got, err)
	}
	if err := os.Symlink("absent", filepath.Join(dir, "broken.json")); err != nil {
		t.Fatal(err)
	}
	before := statusTree(t, cwd)
	got, err = ResolveSession(cwd, nil)
	if err != nil || got == nil || *got != "new" {
		t.Fatalf("stat failure: %v %v", got, err)
	}
	if !reflect.DeepEqual(before, statusTree(t, cwd)) {
		t.Fatal("discovery wrote workspace")
	}
}

func statusPtr(value string) *string { return &value }

func TestResolveSessionMillisecondRoundingAndEmptyID(t *testing.T) {
	root := statusRoot(t)
	cwd := filepath.Join(root, "ws")
	for name, nanos := range map[string]int{"a": 10, "z": 20} {
		path := filepath.Join(cwd, ".crw", "sessions", name+".json")
		statusFile(t, path, "{}")
		stamp := time.Unix(2000000000, int64(nanos))
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	// Node's double mtimeMs rounds these two distinct nanosecond stamps to a tie.
	got, err := ResolveSession(cwd, nil)
	if err != nil || got == nil || *got != "a" {
		t.Fatalf("rounded tie: %v %v", got, err)
	}
	empty := filepath.Join(root, "empty")
	statusFile(t, filepath.Join(empty, ".crw", "sessions", ".json"), "{}")
	got, err = ResolveSession(empty, nil)
	if err != nil || got == nil || *got != "" {
		t.Fatalf("empty file id: %v %v", got, err)
	}
	read, err := RunOrchestrateRead(ParseOrchestrateCliArgs([]string{"status", "--json"}, empty), ReadEnv{})
	if err != nil || read.Result == nil || *read.Result != (CliResult{Code: 0, Output: "no active session"}) {
		t.Fatalf("falsy discovered id: %+v %v", read, err)
	}
}

func TestResolveSessionOracleTieAndEntryKinds(t *testing.T) {
	for _, names := range [][]string{{"z", "a"}, {"\ue000", "𝒜"}} {
		t.Run(names[1], func(t *testing.T) {
			root := statusRoot(t)
			cwd := filepath.Join(root, "ws")
			for _, name := range names {
				path := filepath.Join(cwd, ".crw", "sessions", name+".json")
				statusFile(t, path, "{}")
				stamp := time.Unix(2000000000, 0)
				if err := os.Chtimes(path, stamp, stamp); err != nil {
					t.Fatal(err)
				}
			}
			got, err := ResolveSession(cwd, nil)
			if err != nil || got == nil || *got != names[1] {
				t.Fatalf("oracle tie: %v %v", got, err)
			}
		})
	}
	root := statusRoot(t)
	cwd := filepath.Join(root, "ws")
	dir := filepath.Join(cwd, ".crw", "sessions")
	if err := os.MkdirAll(filepath.Join(dir, "directory.json"), 0700); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveSession(cwd, nil)
	if err != nil || got == nil || *got != "directory" || !SessionFileExists(cwd, "directory") {
		t.Fatalf("directory candidate: %v %v", got, err)
	}
	bad := filepath.Join(root, "bad")
	statusFile(t, filepath.Join(bad, ".crw", "sessions"), "not a directory")
	if _, err := ResolveSession(bad, nil); err == nil {
		t.Fatal("list IO failure swallowed")
	}
	if _, err := RunOrchestrateRead(ParseOrchestrateCliArgs([]string{"status"}, bad), ReadEnv{}); err == nil {
		t.Fatal("runner swallowed discovery IO failure")
	}
}

func TestOrchestrateReadOracle(t *testing.T) {
	var fixture struct {
		Cases []struct {
			ID      string
			Argv    []string
			Files   map[string]string
			Dirs    []string
			Mtimes  map[string]int64
			Native  map[string]string
			SQLite  []string
			ExtraDB bool
			Want    CliResult
		}
	}
	b, err := os.ReadFile("testdata/orchestrate_status/oracle.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Cases) == 0 {
		t.Fatal("empty oracle")
	}
	for _, c := range fixture.Cases {
		t.Run(c.ID, func(t *testing.T) {
			root := statusRoot(t)
			cwd := filepath.Join(root, "ws")
			expand := func(s string) string { return crwNames(strings.ReplaceAll(s, "$R", root)) }
			for _, dir := range c.Dirs {
				if err := os.MkdirAll(filepath.Join(root, expand(dir)), 0700); err != nil {
					t.Fatal(err)
				}
			}
			for name, contents := range c.Files {
				statusFile(t, filepath.Join(root, expand(name)), expand(contents))
			}
			for name, sec := range c.Mtimes {
				stamp := time.Unix(sec, 0)
				if err := os.Chtimes(filepath.Join(root, expand(name)), stamp, stamp); err != nil {
					t.Fatal(err)
				}
			}
			if len(c.SQLite) > 0 {
				db, err := sql.Open("sqlite", filepath.Join(root, "codex", "state_5.sqlite"))
				if err != nil {
					t.Fatal(err)
				}
				for _, statement := range c.SQLite {
					if _, err := db.Exec(expand(statement)); err != nil {
						db.Close()
						t.Fatal(err)
					}
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if c.ExtraDB {
				db, err := sql.Open("sqlite", filepath.Join(root, "codex", "state_6.sqlite"))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec("CREATE TABLE threads (id TEXT)"); err != nil {
					db.Close()
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			}
			native := map[string]string{}
			for key, value := range c.Native {
				native[key] = expand(value)
			}
			argv := make([]string, len(c.Argv))
			for i, value := range c.Argv {
				argv[i] = expand(value)
			}
			before := statusTree(t, cwd)
			read, err := RunOrchestrateRead(ParseOrchestrateCliArgs(argv, cwd), ReadEnv{Native: statusEnv(native), Process: statusEnv(map[string]string{"HOME": filepath.Join(root, "home")})})
			if err != nil || read.Result == nil {
				t.Fatalf("not handled: %+v %v", read, err)
			}
			want := c.Want
			want.Output = expand(want.Output)
			if *read.Result != want {
				t.Fatalf("got %+v\nwant %+v", *read.Result, want)
			}
			if !reflect.DeepEqual(before, statusTree(t, cwd)) {
				t.Fatal("reader wrote workspace")
			}
		})
	}
}

func TestOrchestrateReadMutationBoundary(t *testing.T) {
	root := statusRoot(t)
	cwd := filepath.Join(root, "ws")
	statusFile(t, state.StatePath(cwd, "owner"), `{"phase":"P"}`)
	for _, argv := range [][]string{{"P", "--session", "cli"}, {"A", "--session", "owner"}, {"reset", "--session", "owner", "--attest", "bad"}} {
		before := statusTree(t, cwd)
		got, err := RunOrchestrateRead(ParseOrchestrateCliArgs(argv, cwd), ReadEnv{})
		if err != nil || got.Result != nil || got.SessionID != argv[2] {
			t.Fatalf("continuation: %+v %v", got, err)
		}
		if !reflect.DeepEqual(before, statusTree(t, cwd)) {
			t.Fatal("guard wrote state")
		}
	}
	for _, id := range []string{"cli", "owner", "", "CLI"} {
		if IsReservedSessionKey(id) != (id == "cli") {
			t.Fatalf("reserved %q", id)
		}
	}
	for _, argv := range [][]string{{"P"}, {"P", "--session", ""}, {"reset"}, {"A", "--session", "ghost"}} {
		got, err := RunOrchestrateRead(ParseOrchestrateCliArgs(argv, cwd), ReadEnv{})
		if err != nil || got.Result == nil || got.Result.Code != 1 {
			t.Fatalf("refusal: %+v %v", got, err)
		}
	}
}

func TestSiblingRootsAndStatusRendering(t *testing.T) {
	root := statusRoot(t)
	cwd := filepath.Join(root, "ws")
	home := filepath.Join(root, "home")
	for _, name := range []string{"tree", "AppData", "node_modules"} {
		if err := os.Mkdir(filepath.Join(home, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	statusFile(t, filepath.Join(home, "file"), "x")
	if err := os.Symlink(filepath.Join(home, "tree"), filepath.Join(home, "link")); err != nil {
		t.Fatal(err)
	}
	if got, want := SiblingRoots(cwd, statusEnv(map[string]string{"HOME": home})), []string{filepath.Join(home, "tree"), root}; !slices.Equal(got, want) {
		t.Fatalf("roots: %v want %v", got, want)
	}
	if got := SiblingRoots(cwd, statusEnv(map[string]string{"HOME": filepath.Join(root, "missing")})); !slices.Equal(got, []string{root}) {
		t.Fatalf("unreadable home: %v", got)
	}
	if got := SiblingRoots(string(filepath.Separator), statusEnv(map[string]string{"HOME": filepath.Join(root, "missing")})); len(got) != 0 {
		t.Fatalf("root parent: %v", got)
	}
	s := state.DefaultState("s", "")
	s.Phase = state.PhaseC
	s.Flags = state.Flags{Interview: true, AuditPassed: true, CheckPassed: true}
	text, err := RenderStatus(s, false, []string{"other"}, "latest-file")
	want := "session=s phase=C interview=true auditPassed=true checkPassed=true selection=latest-file (unverified terminal fallback)\nWARNING: this session id also has state in 1 other tree(s); the phase above describes THIS cwd only.\n  also at: other\n  Pass --cwd <path> to address a specific tree."
	if err != nil || text != want {
		t.Fatalf("render: %q %v", text, err)
	}
	text, err = RenderStatus(s, true, []string{"other"}, "native")
	if err != nil || text != `{"phase":"C","flags":{"interview":true,"auditPassed":true,"checkPassed":true},"sessionId":"s","selection":"native","alsoFoundAt":["other"]}` {
		t.Fatalf("JSON render: %q %v", text, err)
	}
}

func TestOrchestrateStatusNativeWALSidecars(t *testing.T) {
	root := statusRoot(t)
	cwd := filepath.Join(root, "ws")
	path := filepath.Join(root, "codex", "state_5.sqlite")
	id := "11111111-1111-4111-8111-111111111111"
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{"PRAGMA journal_mode=WAL", "CREATE TABLE threads (id TEXT, cwd TEXT, archived INTEGER, source TEXT)", "INSERT INTO threads VALUES ('" + id + "', '" + cwd + "', 0, 'vscode')"} {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	statusFile(t, state.StatePath(cwd, id), `{"phase":"P"}`)
	beforeDB, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	before := statusTree(t, cwd)
	got, err := RunOrchestrateRead(ParseOrchestrateCliArgs([]string{"status", "--json"}, cwd), ReadEnv{Native: statusEnv(map[string]string{"CODEX_THREAD_ID": id, "CODEX_HOME": filepath.Join(root, "codex")})})
	if err != nil || got.Result == nil || got.Result.Code != 0 || !strings.Contains(got.Result.Output, `"selection":"native"`) {
		t.Fatalf("WAL status: %+v %v", got, err)
	}
	afterDB, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeDB, afterDB) || !reflect.DeepEqual(before, statusTree(t, cwd)) {
		t.Fatal("native status changed main DB or workspace")
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); err != nil {
			t.Fatalf("expected SQLite sidecar %s: %v", suffix, err)
		}
	}
}
