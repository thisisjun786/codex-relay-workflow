package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/reception"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
	"golang.org/x/sys/unix"
)

// builtBinary is crw built with the contracttest tag, whose outbox runs on a fake clock and a
// fixed token (cli_clock_contract.go).
func builtBinary(t *testing.T) string {
	t.Helper()
	return testsupport.BuildCRW(t, "-tags", "contracttest")
}

// storeCapture is one call of a Python store-reception scenario: a packet-check command line or a
// direct call of the library's ladder, and what it read.
type storeCapture struct {
	Argv    []string
	Library string
	Kwargs  json.RawMessage
	Policy  *string
	// Files are what the call read, as fileBytes decodes them; null removes the file.
	Files   map[string]*string
	Special map[string]struct {
		Symlink string
		FIFO    bool
	}
	// Database is the store Python backed up before the call, as a dump of the fixture's rows.
	Database json.RawMessage
	// After names the files (ledgers) whose content after the call belongs to its outcome.
	After []string `json:",omitempty"`
	// PathDigests names each packet whose content digest depends on the temporary path it
	// carries: the placeholder the fixture and the goldens hold for that digest, by the packet
	// file it digests.
	PathDigests map[string]string `json:",omitempty"`
}

// checkProcess checks what a command answered, its exit, stdout and stderr, against the goldens
// under key.
func checkProcess(t *testing.T, key string, code int, stdout, stderr string, options ...golden.Option) {
	t.Helper()
	golden.Check(t, key+" exit", []byte(strconv.Itoa(code)), options...)
	golden.Check(t, key+" stdout", []byte(stdout), options...)
	golden.Check(t, key+" stderr", []byte(stderr), options...)
}

func processCode(e error) int {
	if e == nil {
		return 0
	}
	var exit *exec.ExitError
	if errors.As(e, &exit) {
		return exit.ExitCode()
	}
	return -1
}

// storeFixture is what a Python store-reception scenario left: the scenarios it ran, the rows of
// the stores it backed up, and each packet-check call with what it read.
type storeFixture struct {
	Scenarios []string       `json:"scenarios"`
	Rows      storePool      `json:"rows"`
	Captures  []storeCapture `json:"captures"`
}

// storeReplay runs each packet-check call of a Python store-reception scenario through the built
// binary (or, for a direct ladder call, the library) on what Python's call read, and checks its
// exit, output and the ledgers it left against the golden.
func storeReplay(t *testing.T, names ...string) {
	t.Helper()
	binary := builtBinary(t)
	// The scenarios' temporary directories, and the files the calls read there, are this run's.
	pyDir := t.TempDir()
	raw := relocated(readFixture(t, "store"), "<pytmp>", pyDir)
	var fixture storeFixture
	if e := json.Unmarshal(raw, &fixture); e != nil {
		t.Fatal(e)
	}
	sameScenarios(t, fixture.Scenarios, names)
	options := []golden.Option{golden.Substitute(pyDir, "<pytmp>")}
	if pairs := packetDigests(t, fixture.Captures); len(pairs) > 0 {
		if e := json.Unmarshal([]byte(strings.NewReplacer(pairs...).Replace(string(raw))), &fixture); e != nil {
			t.Fatal(e)
		}
		for i := 0; i < len(pairs); i += 2 {
			options = append(options, golden.Substitute(pairs[i+1], pairs[i]))
		}
	}
	captures, pool := fixture.Captures, fixture.Rows
	var e error
	if len(captures) == 0 {
		t.Fatal("scenario executed no packet-check calls")
	}
	// Python ran this scenario with a live Store connection; Go's replay holds its store the same way.
	holdsLive := len(names) == 1 && names[0] == "test_the_store_backed_check_writes_nothing_in_the_state_directory"
	for index, c := range captures {
		if c.Library == "ladder" {
			d := json.NewDecoder(bytes.NewReader(c.Kwargs))
			d.UseNumber()
			kwargs, e := decodeValue(d)
			if e != nil {
				t.Fatal(e)
			}
			ctx := context.Background()
			database := filepath.Join(t.TempDir(), "relay.sqlite3")
			restoreStore(t, pool.dump(t, c.Database), database)
			ro, e := store.OpenReadOnly(ctx, database, time.Second)
			if e != nil {
				t.Fatal(e)
			}
			var result Obj
			e = ro.ReadSnapshot(ctx, func(ctx context.Context, s *store.Store) error {
				var e error
				result, e = reception.Ladder(ctx, s, text(reception.Get(kwargs, "relationship_id")), text(reception.Get(kwargs, "subject")), reception.Get(kwargs, "observation"))
				return e
			})
			if closeErr := ro.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			if e != nil {
				t.Fatal(e)
			}
			golden.Check(t, fmt.Sprintf("%03d stdout", index), []byte(pyjson.Dumps(result, pyjson.Options{})), options...)
			continue
		}
		// The Python test has finished and removed these fixture roots; the independent Go
		// replay reconstructs only captured inputs, and owns that reconstruction's cleanup.
		for path := range c.Files {
			for fixture := filepath.Clean(path); fixture != filepath.Dir(fixture); fixture = filepath.Dir(fixture) {
				if strings.HasPrefix(filepath.Base(fixture), "relay-test-") {
					fixture := fixture
					t.Cleanup(func() {
						if err := os.RemoveAll(fixture); err != nil {
							t.Error(err)
						}
					})
					break
				}
			}
		}
		state := ""
		for i, v := range c.Argv {
			if v == "--state" {
				state = c.Argv[i+1]
			}
		}
		if e = os.MkdirAll(state, 0700); e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() {
			if e := os.RemoveAll(state); e != nil {
				t.Error(e)
			}
		})
		db := filepath.Join(state, "relay.sqlite3")
		for _, path := range []string{db, db + "-wal", db + "-shm"} {
			if e = os.Remove(path); e != nil && !errors.Is(e, os.ErrNotExist) {
				t.Fatal(e)
			}
		}
		hasDatabase := len(c.Database) > 0 && string(c.Database) != "null"
		if !hasDatabase {
			if e = os.Remove(db); e != nil && !os.IsNotExist(e) {
				t.Fatal(e)
			}
		}
		if hasDatabase {
			restoreStore(t, pool.dump(t, c.Database), db)
			if holdsLive {
				// The store Go holds live is a copy of Python's: it gets its own identity and
				// is Go's after a takeover, as a Go host holding it would have it.
				testsupport.Rehome(t, db)
				testsupport.HandOver(t, db, "go")
			}
		}
		var beforeRows []store.Row
		tableRows := map[string][]store.Row{}
		var beforeDB *store.ReadOnly
		if hasDatabase {
			beforeDB, e = store.OpenReadOnly(context.Background(), db, time.Second)
			if e != nil {
				t.Fatal(e)
			}
			e = beforeDB.ReadSnapshot(context.Background(), func(ctx context.Context, s *store.Store) error {
				var e error
				beforeRows, e = s.All(ctx, "SELECT name, sql FROM sqlite_master WHERE type='table' ORDER BY name")
				if e != nil {
					return e
				}
				for _, table := range beforeRows {
					rows, err := s.All(ctx, "SELECT * FROM "+text(table.Get("name")))
					if err != nil {
						return err
					}
					tableRows[text(table.Get("name"))] = rows
				}
				return nil
			})
			if e != nil {
				t.Fatal(e)
			}
			if e = beforeDB.Close(); e != nil {
				t.Fatal(e)
			}
		}
		for path, encoded := range c.Files {
			if encoded == nil {
				if e = os.Remove(path); e != nil && !errors.Is(e, os.ErrNotExist) {
					t.Fatal(e)
				}
				continue
			}
			data, e := fileBytes(*encoded)
			if e != nil {
				t.Fatal(e)
			}
			if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(path, data, 0600); e != nil {
				t.Fatal(e)
			}
		}
		for path, special := range c.Special {
			if e = os.Remove(path); e != nil && !errors.Is(e, os.ErrNotExist) {
				t.Fatal(e)
			}
			if special.FIFO {
				e = unix.Mkfifo(path, 0600)
			} else {
				e = os.Symlink(special.Symlink, path)
			}
			if e != nil {
				t.Fatal(e)
			}
		}
		var stateBefore []os.DirEntry
		var stateInfo os.FileInfo
		if holdsLive {
			// Python ran with a live Store connection, so its WAL sidecars already existed.
			// Keep that same precondition instead of charging a read-only SQLite open for
			// sidecars the fixture backup deliberately did not copy.
			held, err := store.Open(context.Background(), db, "")
			if err != nil {
				t.Fatal(err)
			}
			defer held.Close()
			stateBefore, err = os.ReadDir(state)
			if err != nil {
				t.Fatal(err)
			}
			stateInfo, err = os.Stat(state)
			if err != nil {
				t.Fatal(err)
			}
		}
		invoke := exec.Command(binary, append([]string{"relay"}, c.Argv...)...)
		policy := ""
		if c.Policy != nil {
			policy = *c.Policy
		}
		invoke.Env = append(os.Environ(), "CODEX_THREAD_BRIDGE_EXECUTION_POLICY="+policy)
		var stdout, stderr bytes.Buffer
		invoke.Stdout, invoke.Stderr = &stdout, &stderr
		e = invoke.Run()
		code := processCode(e)
		if stateInfo != nil {
			after, err := os.ReadDir(state)
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(state)
			if err != nil {
				t.Fatal(err)
			}
			namesOf := func(entries []os.DirEntry) []string {
				out := []string{}
				for _, entry := range entries {
					out = append(out, entry.Name())
				}
				return out
			}
			if !reflect.DeepEqual(namesOf(stateBefore), namesOf(after)) || !stateInfo.ModTime().Equal(info.ModTime()) {
				t.Fatalf("state directory changed: before=%v at %v after=%v at %v", namesOf(stateBefore), stateInfo.ModTime(), namesOf(after), info.ModTime())
			}
		}
		checkProcess(t, fmt.Sprintf("%03d", index), code, stdout.String(), stderr.String(), options...)
		if !hasDatabase {
			if _, err := os.Stat(db); !os.IsNotExist(err) {
				t.Fatalf("packet-check created absent store: %v", err)
			}
		} else {
			ro, err := store.OpenReadOnly(context.Background(), db, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			err = ro.ReadSnapshot(context.Background(), func(ctx context.Context, s *store.Store) error {
				after, err := s.All(ctx, "SELECT name, sql FROM sqlite_master WHERE type='table' ORDER BY name")
				if err != nil {
					return err
				}
				if !reflect.DeepEqual(beforeRows, after) {
					t.Fatal("read-only packet-check changed schema")
				}
				for name, before := range tableRows {
					after, err := s.All(ctx, "SELECT * FROM "+name)
					if err != nil {
						return err
					}
					if !reflect.DeepEqual(before, after) {
						t.Fatalf("read-only packet-check changed table %s", name)
					}
				}
				return nil
			})
			if closeErr := ro.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
		after := map[string]*string{}
		for _, path := range c.After {
			data, e := os.ReadFile(path)
			switch {
			case e == nil:
				after[path] = fileContent(data)
			case errors.Is(e, os.ErrNotExist):
				after[path] = nil
			default:
				t.Fatal(e)
			}
		}
		if len(after) > 0 {
			golden.CheckJSON(t, fmt.Sprintf("%03d after", index), after, options...)
		}
	}
}
