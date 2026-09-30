package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// cliFixture is what the Python sync command scenario left: each command line with the store it
// ran against (as a dump of the fixture's rows) and the tables its answer includes, and the files
// the commands read through an @path argument.
type cliFixture struct {
	Rows     storePool         `json:"rows"`
	Files    map[string]string `json:"files"`
	Captures []struct {
		Argv     []string        `json:"argv"`
		Database json.RawMessage `json:"database"`
		Tables   []string        `json:"tables"`
	} `json:"captures"`
}

// cliRun is what a command answered and the tables it left, as the Python dumper writes them.
type cliRun struct {
	code           int
	stdout, stderr string
	tables         []string
}

func Test23_SyncCommandsBuiltBinaryWholeBytes(t *testing.T) {
	base := t.TempDir()
	binary := builtBinary(t)
	alias := base + "/codex-session-relay"
	if e := os.Symlink(binary, alias); e != nil {
		t.Fatal(e)
	}
	// The Go side replays in base, from the stores and files Python left.
	var fixture cliFixture
	if e := json.Unmarshal(relocated(readFixture(t, "cli"), "<base>", base), &fixture); e != nil {
		t.Fatal(e)
	}
	for path, content := range fixture.Files {
		if e := os.WriteFile(path, []byte(content), 0o600); e != nil {
			t.Fatal(e)
		}
	}
	if e := os.MkdirAll(base+"/state", 0o700); e != nil {
		t.Fatal(e)
	}
	for index, item := range fixture.Captures {
		dump := fixture.Rows.dump(t, item.Database)
		var first *cliRun
		for _, program := range []string{binary, alias} {
			db := base + "/state/relay.sqlite3"
			// The previous store goes whole, its fence included; the state directory then holds
			// a copy of Python's store before this command, which gets its own identity and is
			// Go's after a takeover.
			for _, p := range []string{db, db + "-wal", db + "-shm", base + "/state/takeover.json", base + "/state/write-gate.lock", base + "/state/takeover.lock"} {
				if e := os.Remove(p); e != nil && !os.IsNotExist(e) {
					t.Fatal(e)
				}
			}
			restoreStore(t, dump, db)
			testsupport.Rehome(t, db)
			testsupport.HandOver(t, db, "go")
			commandArgs := item.Argv
			if program == binary {
				commandArgs = append([]string{"relay"}, item.Argv...)
			}
			run := exec.Command(program, commandArgs...)
			var out, errout bytes.Buffer
			run.Stdout, run.Stderr = &out, &errout
			answer := cliRun{code: processCode(run.Run()), stdout: out.String(), stderr: errout.String()}
			s, e := store.Open(context.Background(), db, "")
			if e != nil {
				t.Fatal(e)
			}
			for _, table := range item.Tables {
				rows, e := s.All(context.Background(), "SELECT * FROM "+table)
				if e != nil {
					t.Fatal(e)
				}
				values := []any{}
				for _, row := range rows {
					values = append(values, rowObject(row))
				}
				answer.tables = append(answer.tables, pyjson.Dumps(values, pyjson.Options{}))
			}
			if e = s.Close(); e != nil {
				t.Fatal(e)
			}
			if first != nil {
				// Both entry points are one command line interface.
				if !reflect.DeepEqual(answer, *first) {
					t.Fatalf("%s %v answers otherwise than %s\n%s: %+v\n%s: %+v", program, item.Argv, binary, binary, *first, program, answer)
				}
				continue
			}
			first = &answer
			key := fmt.Sprintf("%03d", index)
			checkProcess(t, key, answer.code, answer.stdout, answer.stderr, golden.Substitute(base, "<base>"))
			for i, table := range item.Tables {
				golden.Check(t, key+" table "+table, []byte(answer.tables[i]), golden.Substitute(base, "<base>"))
			}
		}
	}
}
