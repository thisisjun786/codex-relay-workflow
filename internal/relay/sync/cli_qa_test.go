package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/reception"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

func Test23_SyncCommandsBuiltBinaryWholeBytes(t *testing.T) {
	root, e := filepath.Abs("../../..")
	if e != nil {
		t.Fatal(e)
	}
	base := t.TempDir()
	binary := builtBinary(t)
	alias := base + "/codex-session-relay"
	if e = os.Symlink(binary, alias); e != nil {
		t.Fatal(e)
	}
	// Python works in a directory of its own; the Go side replays in base, which Python never
	// touched, from the stores and files Python left.
	answer := pyoracle.AnswerInterned(t, "cli_capture.py", func() ([]byte, error) {
		base := t.TempDir()
		capture := exec.Command("uv", "run", "--no-sync", "python", root+"/internal/relay/sync/testdata/cli_capture.py", base)
		capture.Dir = root
		capture.Env = append(os.Environ(), "UV_PROJECT_ENVIRONMENT="+filepath.Join(root, ".venv"), "UV_CACHE_DIR="+base+"/uv")
		raw, e := capture.Output()
		if e != nil {
			return nil, fmt.Errorf("capture: %v %s", e, raw)
		}
		return cliCapturesRecorded(raw, base)
	})
	answer, pool := openStores(t, relocated(answer, "<base>", base))
	var recorded struct {
		Captures json.RawMessage
		Files    map[string]string
	}
	if e = json.Unmarshal(answer, &recorded); e != nil {
		t.Fatal(e)
	}
	for path, content := range recorded.Files {
		if e = os.WriteFile(path, []byte(content), 0o600); e != nil {
			t.Fatal(e)
		}
	}
	var stores []struct{ Database json.RawMessage }
	if e = json.Unmarshal(recorded.Captures, &stores); e != nil {
		t.Fatal(e)
	}
	decoder := json.NewDecoder(bytes.NewReader(recorded.Captures))
	decoder.UseNumber()
	data, e := decodeValue(decoder)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.MkdirAll(base+"/state", 0o700); e != nil {
		t.Fatal(e)
	}
	for index, item := range data.([]any) {
		dump := pool.dump(t, stores[index].Database)
		args, _ := evidence.List(reception.Get(item, "argv"))
		argv := []string{}
		for _, arg := range args {
			argv = append(argv, text(arg))
		}
		for _, program := range []string{binary, alias} {
			db := base + "/state/relay.sqlite3"
			// The previous store goes whole, its fence included; the state directory then holds
			// a copy of Python's store before this command, which gets its own identity and is
			// Go's after a takeover.
			for _, p := range []string{db, db + "-wal", db + "-shm", base + "/state/takeover.json", base + "/state/write-gate.lock", base + "/state/takeover.lock"} {
				if e = os.Remove(p); e != nil && !os.IsNotExist(e) {
					t.Fatal(e)
				}
			}
			restoreStore(t, dump, db)
			testsupport.Rehome(t, db)
			testsupport.HandOver(t, db, "go")
			commandArgs := argv
			if program == binary {
				commandArgs = append([]string{"relay"}, argv...)
			}
			run := exec.Command(program, commandArgs...)
			var out, errout bytes.Buffer
			run.Stdout, run.Stderr = &out, &errout
			code := processCode(run.Run())
			expected, _ := evidence.PyInt(reception.Get(item, "exit"))
			if code != int(expected) || out.String() != text(reception.Get(item, "stdout")) || errout.String() != text(reception.Get(item, "stderr")) {
				t.Fatalf("%s %v exit Python=%d Go=%d\nPython:%s\nGo:%s\nPython stderr:%s\nGo stderr:%s", program, argv, expected, code, reception.Get(item, "stdout"), out.String(), reception.Get(item, "stderr"), errout.String())
			}
			s, e := store.Open(context.Background(), db, "")
			if e != nil {
				t.Fatal(e)
			}
			tables, _ := evidence.Object(reception.Get(item, "tables"))
			for _, table := range tables {
				rows, e := s.All(context.Background(), "SELECT * FROM "+table.Key)
				if e != nil {
					t.Fatal(e)
				}
				values := []any{}
				for _, row := range rows {
					values = append(values, rowObject(row))
				}
				if got, want := evidence.Dumps(values, false, false, true), evidence.Dumps(table.Value, false, false, true); got != want {
					t.Fatalf("%v table %s bytes differ\nPython:%s\nGo:%s", argv, table.Key, want, got)
				}
			}
			if e = s.Close(); e != nil {
				t.Fatal(e)
			}
		}
	}
}
