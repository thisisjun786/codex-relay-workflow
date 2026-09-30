package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/reception"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
	"golang.org/x/sys/unix"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	stdsync "sync"
	"testing"
	"time"
)

var binaryOnce stdsync.Once
var binaryPath string
var binaryError error

func builtBinary(t *testing.T) string {
	t.Helper()
	binaryOnce.Do(func() {
		dir, e := os.MkdirTemp("", "t23b-binary-")
		if e != nil {
			binaryError = e
			return
		}
		binaryPath = filepath.Join(dir, "crw")
		root, e := filepath.Abs("../../..")
		if e != nil {
			binaryError = e
			return
		}
		goBinary, e := exec.LookPath("go")
		if e != nil {
			binaryError = e
			return
		}
		cmd := exec.Command(goBinary, "build", "-tags", "contracttest", "-o", binaryPath, "./cmd/crw")
		cmd.Dir = root
		output, e := cmd.CombinedOutput()
		if e != nil {
			binaryError = errors.New(string(output))
		}
	})
	if binaryError != nil {
		t.Fatal(binaryError)
	}
	return binaryPath
}

type storeCapture struct {
	Argv    []string
	Library string
	Kwargs  json.RawMessage
	Policy  *string
	Files   map[string]*string
	Special map[string]struct {
		Symlink string
		FIFO    bool
	}
	// Database is a path in Python's capture and the store's dump in the recording.
	Database       json.RawMessage
	Stdout, Stderr string
	Exit           int
	After          map[string]*string
	// PathDigests, in the recording only, names each packet whose content digest depends on
	// the temporary path it carries: Python's digest, by the packet file it digests.
	PathDigests map[string]string `json:",omitempty"`
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
func storeReplay(t *testing.T, names ...string) {
	t.Helper()
	binary := builtBinary(t)
	root, e := filepath.Abs("../../..")
	if e != nil {
		t.Fatal(e)
	}
	raw, e := json.Marshal(names)
	if e != nil {
		t.Fatal(e)
	}
	out := pythonAnswer(t, "store_capture.py", raw, func() ([]byte, error) {
		keep := t.TempDir()
		pyTmp := filepath.Join(keep, "tmp")
		if e := os.Mkdir(pyTmp, 0o700); e != nil {
			return nil, e
		}
		cmd := exec.Command("uv", "run", "--no-sync", "python", filepath.Join(root, "internal/relay/sync/testdata/store_capture.py"), keep, string(raw))
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "UV_PROJECT_ENVIRONMENT="+filepath.Join(root, ".venv"), "UV_CACHE_DIR="+keep+"/uv", "TMPDIR="+pyTmp)
		out, e := cmd.Output()
		if e != nil {
			var exit *exec.ExitError
			if errors.As(e, &exit) {
				return nil, fmt.Errorf("python store scenario %v: %v\n%s", names, e, exit.Stderr)
			}
			return nil, e
		}
		return storeCapturesRecorded(out, pyTmp)
	}, pyoracle.SameWhen(sameUpToIdentifiers))
	// The scenarios' temporary directories, and the files the calls read there, are this run's.
	out, pool := openStores(t, relocated(out, "<pytmp>", t.TempDir()))
	out = relocatedDigests(t, out)
	var captures []storeCapture
	if e = json.Unmarshal(out, &captures); e != nil {
		t.Fatal(e)
	}
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
			if got := evidence.Dumps(result, false, false, true); got != c.Stdout {
				t.Fatalf("library ladder call %d\nPython:%s\nGo:%s", index, c.Stdout, got)
			}
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
			data, e := recordedFileBytes(*encoded)
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
		if code != c.Exit || stdout.String() != c.Stdout || stderr.String() != c.Stderr {
			t.Fatalf("call %d %v\nexit Python=%d Go=%d\nPython stdout: %s\nGo stdout: %s\nPython stderr: %s\nGo stderr: %s", index, c.Argv, c.Exit, code, c.Stdout, stdout.String(), c.Stderr, stderr.String())
		}
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
		for path, encoded := range c.After {
			data, e := os.ReadFile(path)
			if encoded == nil {
				if !errors.Is(e, os.ErrNotExist) {
					t.Fatalf("ledger should be absent: %s %v", path, e)
				}
				continue
			}
			if e != nil {
				t.Fatal(e)
			}
			want, e := recordedFileBytes(*encoded)
			if e != nil {
				t.Fatal(e)
			}
			if !bytes.Equal(data, want) {
				t.Fatalf("ledger bytes differ\nPython: %s\nGo: %s", want, data)
			}
		}
	}
}
