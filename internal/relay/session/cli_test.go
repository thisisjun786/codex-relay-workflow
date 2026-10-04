package session

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

const child = "019a0000-0000-7000-8000-000000000001"
const parent = "019a0000-0000-7000-8000-000000000002"

func TestMain(m *testing.M) { testsupport.Main(m) }

type fixture struct {
	root, cwd, home, path string
	env                   map[string]string
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func newFixture(t *testing.T, row map[string]any) fixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	f := fixture{root: root, cwd: filepath.Join(root, "work"), home: filepath.Join(root, "native")}
	for _, dir := range []string{f.cwd, f.home} {
		must(t, os.Mkdir(dir, 0o755))
	}
	f.path = state.StatePath(f.cwd, child)
	f.env = map[string]string{"CODEX_THREAD_ID": child, "CODEX_HOME": f.home}
	f.db(t, "5", row)
	return f
}

func (f fixture) db(t *testing.T, version string, override map[string]any) {
	t.Helper()
	row := map[string]any{"id": child, "cwd": f.cwd, "archived": 0, "source": "vscode"}
	for k, v := range override {
		row[k] = v
	}
	db, err := sql.Open("sqlite", filepath.Join(f.home, "state_"+version+".sqlite"))
	must(t, err)
	_, err = db.Exec("CREATE TABLE threads (id TEXT PRIMARY KEY, cwd TEXT, archived INTEGER, source TEXT, title TEXT)")
	must(t, err)
	_, err = db.Exec("INSERT INTO threads VALUES (?, ?, ?, ?, 'PRIVATE_TRANSCRIPT')", row["id"], row["cwd"], row["archived"], row["source"])
	must(t, err)
	must(t, db.Close())
}

func (f fixture) lookup(name string) (string, bool) { v, ok := f.env[name]; return v, ok }
func (f fixture) run(command string) Result {
	return Run(Options{Command: command, JSON: true}, f.cwd, f.lookup)
}

func field(t *testing.T, r Result, key string) any {
	t.Helper()
	object, ok := r.Out.(contract.OrderedObject)
	if !ok {
		t.Fatalf("not an object: %+v", r)
	}
	for _, f := range object {
		if f.Key == key {
			return f.Value
		}
	}
	return nil
}

func (f fixture) write(t *testing.T, path, body string) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(path), 0o755))
	must(t, os.WriteFile(path, []byte(body), 0o644))
}

func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	must(t, filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		value := fmt.Sprintf("%v/%d", info.Mode(), info.ModTime().UnixNano())
		if info.Mode().IsRegular() {
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			value += string(body)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			value += target
		}
		out[path] = value
		return nil
	}))
	return out
}

func TestCurrentIsReadOnlyAndNeverSelectsParent(t *testing.T) {
	for _, withParent := range []bool{false, true} {
		f := newFixture(t, nil)
		if withParent {
			f.write(t, state.StatePath(f.cwd, parent), `{"sessionId":"`+parent+`","phase":"B","loopArmSeen":true}`)
		}
		before := snapshot(t, f.root)
		r := f.run("current")
		if r.Code != 0 || field(t, r, "sessionId") != child || field(t, r, "statePath") != f.path || field(t, r, "stateExists") != false || field(t, r, "phase") != nil || field(t, r, "created") != false || field(t, r, "hooksVerified") != false {
			t.Fatalf("%+v", r)
		}
		if !reflect.DeepEqual(before, snapshot(t, f.root)) {
			t.Fatal("current changed bytes or directory listings/mtimes")
		}
		plain := Run(Options{Command: "current"}, f.cwd, f.lookup)
		if plain.Code != 0 || !strings.Contains(plain.Out.(string), "phase: null\ncreated: false\nhooksVerified: false") {
			t.Fatalf("plain: %+v", plain)
		}
	}
}

func TestBindCreatesOnlyChildAndPreservesResumedBytes(t *testing.T) {
	f := newFixture(t, nil)
	r := f.run("bind")
	if r.Code != 0 || field(t, r, "created") != true || field(t, r, "phase") != "IDLE" {
		t.Fatalf("%+v", r)
	}
	resumed := "{\n  \"sessionId\": \"" + child + "\", \"phase\": \"C\", \"orchestrationActive\": true, \"customEvidence\": \"preserve\"\n}\n"
	f.write(t, f.path, resumed)
	parentPath := state.StatePath(f.cwd, parent)
	parentBytes := `{"sessionId":"` + parent + `","phase":"B"}`
	f.write(t, parentPath, parentBytes)
	for _, command := range []string{"bind", "current", "bind"} {
		r = f.run(command)
		childAfter, err := os.ReadFile(f.path)
		must(t, err)
		parentAfter, err := os.ReadFile(parentPath)
		must(t, err)
		entries, err := os.ReadDir(filepath.Dir(f.path))
		must(t, err)
		if r.Code != 0 || field(t, r, "phase") != "C" || field(t, r, "created") != false || string(childAfter) != resumed || string(parentAfter) != parentBytes || len(entries) != 2 {
			t.Fatalf("resumed %s: %+v", command, r)
		}
	}
}

func TestNativeRefusalsFailClosedWithoutPrivateData(t *testing.T) {
	for _, row := range []map[string]any{{"id": parent}, {"archived": 1}, {"archived": nil}, {"cwd": "."}, {"cwd": "/synthetic-absent"}, {"source": "unknown"}, {"source": `{"subagent":"PRIVATE_TRANSCRIPT"}`}} {
		f := newFixture(t, row)
		before := snapshot(t, f.root)
		for _, command := range []string{"current", "bind"} {
			r := f.run(command)
			if r.Code != 1 || strings.Contains(fmt.Sprint(r.Out), "PRIVATE_TRANSCRIPT") || !reflect.DeepEqual(before, snapshot(t, f.root)) {
				t.Fatalf("%v %s: %+v", row, command, r)
			}
		}
	}
	for _, id := range []string{"", "../other", child + "\n", "PRIVATE_TRANSCRIPT"} {
		f := newFixture(t, nil)
		f.env["CODEX_THREAD_ID"] = id
		if id == "" {
			delete(f.env, "CODEX_THREAD_ID")
		}
		before := snapshot(t, f.root)
		if r := f.run("bind"); r.Code != 1 || !reflect.DeepEqual(before, snapshot(t, f.root)) || strings.Contains(fmt.Sprint(r.Out), "PRIVATE_TRANSCRIPT") {
			t.Fatalf("invalid id: %+v", r)
		}
	}
	f := newFixture(t, nil)
	f.write(t, filepath.Join(f.home, "state_10.sqlite"), "PRIVATE_TRANSCRIPT")
	if r := f.run("bind"); r.Code != 1 || field(t, r, "error") != "Cannot read the newest native state database or its threads schema. Check database access and Node SQLite support." {
		t.Fatalf("newest database fallback: %+v", r)
	}
}

func TestRawCorruptIdentityIsPreserved(t *testing.T) {
	for _, body := range []string{"PRIVATE_TRANSCRIPT {", "null", "[]", "{}", `{"sessionId":"` + parent + `","phase":"B"}`, `{"sessionId":"` + child + `","phase":"INVALID"}`, `{"sessionId":"` + child + `","phase":null}`} {
		f := newFixture(t, nil)
		f.write(t, f.path, body)
		before := snapshot(t, f.root)
		for _, command := range []string{"current", "bind"} {
			r := f.run(command)
			if r.Code != 1 || strings.Contains(fmt.Sprint(r.Out), "PRIVATE_TRANSCRIPT") || !reflect.DeepEqual(before, snapshot(t, f.root)) {
				t.Fatalf("%s: %+v", body, r)
			}
		}
	}
}

func TestRedirectedOrNonregularStateFailsClosed(t *testing.T) {
	for _, kind := range []string{"root-link", "sessions-link", "file-link", "dangling-link", "root-file", "sessions-file", "file-dir", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t, nil)
			external := filepath.Join(f.root, "external", "private.json")
			f.write(t, external, `{"sessionId":"`+child+`","phase":"B"}`)
			root, dir := filepath.Dir(filepath.Dir(f.path)), filepath.Dir(f.path)
			switch kind {
			case "root-link":
				must(t, os.Symlink(filepath.Dir(external), root))
			case "root-file":
				f.write(t, root, "private")
			case "sessions-link", "sessions-file":
				must(t, os.Mkdir(root, 0o755))
				if kind == "sessions-link" {
					must(t, os.Symlink(filepath.Dir(external), dir))
				} else {
					f.write(t, dir, "private")
				}
			default:
				must(t, os.MkdirAll(dir, 0o755))
				switch kind {
				case "file-dir":
					must(t, os.Mkdir(f.path, 0o755))
				case "fifo":
					must(t, syscall.Mkfifo(f.path, 0o600))
				default:
					if kind == "dangling-link" {
						external += "-absent"
					}
					must(t, os.Symlink(external, f.path))
				}
			}
			before := snapshot(t, f.root)
			for _, command := range []string{"current", "bind"} {
				if r := f.run(command); r.Code != 1 || !reflect.DeepEqual(before, snapshot(t, f.root)) {
					t.Fatalf("%s: %+v", command, r)
				}
			}
		})
	}
}

func TestBindRechecksConcurrentWinnerAndCreationFailure(t *testing.T) {
	for _, kind := range []string{"corrupt", "wrong-id", "symlink", "disappeared", "error"} {
		f := newFixture(t, nil)
		winner := snapshot(t, f.root)
		ensure := func(cwd, id string) (bool, error) {
			switch kind {
			case "error":
				return false, errors.New("private failure")
			case "disappeared":
				return false, nil
			case "symlink":
				f.write(t, filepath.Join(f.root, "external.json"), `{"sessionId":"`+child+`","phase":"B"}`)
				must(t, os.MkdirAll(filepath.Dir(f.path), 0o755))
				must(t, os.Symlink(filepath.Join(f.root, "external.json"), f.path))
			case "wrong-id":
				f.write(t, f.path, `{"sessionId":"`+parent+`","phase":"B"}`)
			default:
				f.write(t, f.path, "PRIVATE_TRANSCRIPT {")
			}
			winner = snapshot(t, f.root)
			return false, nil
		}
		r := run(Options{Command: "bind", JSON: true}, f.cwd, f.lookup, ensure)
		if r.Code != 1 || field(t, r, "hooksVerified") != false || strings.Contains(fmt.Sprint(r.Out), "private failure") || !reflect.DeepEqual(winner, snapshot(t, f.root)) {
			t.Fatalf("%s: %+v", kind, r)
		}
	}
}

func TestConcurrentBindPublishesExactlyOnce(t *testing.T) {
	f := newFixture(t, nil)
	var wg sync.WaitGroup
	gate := make(chan struct{})
	results := make(chan Result, 4)
	for range 4 {
		wg.Go(func() { <-gate; results <- f.run("bind") })
	}
	close(gate)
	wg.Wait()
	close(results)
	created := 0
	for r := range results {
		if r.Code != 0 || field(t, r, "phase") != "IDLE" {
			t.Fatalf("%+v", r)
		}
		if field(t, r, "created") == true {
			created++
		}
	}
	entries, err := os.ReadDir(filepath.Dir(f.path))
	must(t, err)
	if created != 1 || len(entries) != 1 || entries[0].Name() != child+".json" {
		t.Fatalf("created=%d entries=%v", created, entries)
	}
}

func TestOrderedAnswerAndLargeUnrelatedNumber(t *testing.T) {
	f := newFixture(t, nil)
	f.write(t, f.path, `{"sessionId":"`+child+`","phase":"P","large":1e999}`)
	r := f.run("current")
	var out bytes.Buffer
	must(t, contract.Emit(&out, r.Out))
	if r.Code != 0 || !strings.Contains(out.String(), "\"ok\": true,\n  \"sessionId\":") || !strings.HasSuffix(out.String(), "\"hooksVerified\": false\n}\n") {
		t.Fatalf("%+v %s", r, &out)
	}
}
