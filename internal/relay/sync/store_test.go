package sync

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/reception"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
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
	Database       *string
	Stdout, Stderr string
	Exit           int
	After          map[string]*string
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
	keep := t.TempDir()
	cmd := exec.Command("uv", "run", "--no-sync", "python", filepath.Join(root, "internal/relay/sync/testdata/store_capture.py"), keep, string(raw))
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "UV_PROJECT_ENVIRONMENT="+filepath.Join(root, ".venv"), "UV_CACHE_DIR="+keep+"/uv")
	out, e := cmd.Output()
	if e != nil {
		var exit *exec.ExitError
		if errors.As(e, &exit) {
			t.Fatalf("Python store scenario %v: %v\n%s", names, e, exit.Stderr)
		}
		t.Fatal(e)
	}
	var captures []storeCapture
	if e = json.Unmarshal(out, &captures); e != nil {
		t.Fatal(e)
	}
	if len(captures) == 0 {
		t.Fatal("scenario executed no packet-check calls")
	}
	for index, c := range captures {
		if c.Library == "ladder" {
			d := json.NewDecoder(bytes.NewReader(c.Kwargs))
			d.UseNumber()
			kwargs, e := decodeValue(d)
			if e != nil {
				t.Fatal(e)
			}
			ctx := context.Background()
			ro, e := store.OpenReadOnly(ctx, *c.Database, time.Second)
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
		if c.Database == nil {
			if e = os.Remove(db); e != nil && !os.IsNotExist(e) {
				t.Fatal(e)
			}
		}
		if c.Database != nil {
			data, e := os.ReadFile(*c.Database)
			if e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(db, data, 0600); e != nil {
				t.Fatal(e)
			}
		}
		var beforeRows []store.Row
		tableRows := map[string][]store.Row{}
		var beforeDB *store.ReadOnly
		if c.Database != nil {
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
			data, e := hex.DecodeString(*encoded)
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
		if len(names) == 1 && names[0] == "test_the_store_backed_check_writes_nothing_in_the_state_directory" {
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
		if c.Database == nil {
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
			want, e := hex.DecodeString(*encoded)
			if e != nil {
				t.Fatal(e)
			}
			if !bytes.Equal(data, want) {
				t.Fatalf("ledger bytes differ\nPython: %s\nGo: %s", want, data)
			}
		}
	}
}
