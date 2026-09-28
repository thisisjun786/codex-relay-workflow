package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/reception"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
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
	capture := exec.Command("uv", "run", "--no-sync", "python", root+"/internal/relay/sync/testdata/cli_capture.py", base)
	capture.Dir = root
	capture.Env = append(os.Environ(), "UV_PROJECT_ENVIRONMENT="+filepath.Join(root, ".venv"), "UV_CACHE_DIR="+base+"/uv")
	raw, e := capture.Output()
	if e != nil {
		t.Fatalf("capture: %v %s", e, raw)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	data, e := decodeValue(decoder)
	if e != nil {
		t.Fatal(e)
	}
	for _, item := range data.([]any) {
		args, _ := evidence.List(reception.Get(item, "argv"))
		argv := []string{}
		for _, arg := range args {
			argv = append(argv, text(arg))
		}
		for _, program := range []string{binary, alias} {
			before, e := os.ReadFile(text(reception.Get(item, "database")))
			if e != nil {
				t.Fatal(e)
			}
			db := base + "/state/relay.sqlite3"
			for _, p := range []string{db + "-wal", db + "-shm"} {
				if e = os.Remove(p); e != nil && !os.IsNotExist(e) {
					t.Fatal(e)
				}
			}
			if e = os.WriteFile(db, before, 0600); e != nil {
				t.Fatal(e)
			}
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
