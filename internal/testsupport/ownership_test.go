package testsupport_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

// Each fixture state is judged by Go's real admission, store.Open. A refusal of a store the other
// runtime owns is proven by its last check, the one that runs only after the mirror, the durable
// stamp and the physical identity all agreed. The retained Python fence's own admission of these
// stores was checked beside it until todo 44: rollback to Python closed at todo 43
// (rollback_allowed=0), and the Python runtime leaves in todo 44.
const (
	admitted    = "admitted"
	goRefusesPy = "refused: store_owned_by_other: the relay store belongs to another runtime"
	// pythonCreates is the Python initializer creating an absent store; its schema_meta rows and
	// its mirror are recorded (pythonCreated).
	pythonCreates = "import json, sqlite3, sys\nfrom codex_session_relay.store import Store\nStore(sys.argv[1], sys.argv[2] or None).close()\ndb = sqlite3.connect(sys.argv[1])\nprint(json.dumps(dict(db.execute('SELECT key, value FROM schema_meta'))))\ndb.close()"
)

// fixedSocket is the App Server socket the initializer comparisons name. It is never connected;
// it is fixed so that the scope key derived from it is the same in a recording and on replay.
const fixedSocket = "/crw-test/app.sock"

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, current, _, _ := runtime.Caller(0)
	return filepath.Clean(filepath.Join(filepath.Dir(current), "../.."))
}

func python(t *testing.T, script string, args ...string) string {
	t.Helper()
	root := repositoryRoot(t)
	command := exec.Command(filepath.Join(root, ".venv/bin/python"), append([]string{"-c", script}, args...)...)
	command.Dir = root
	command.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("Python: %v\n%s", err, output)
	}
	return strings.TrimSpace(string(output))
}

// pythonCreated is what the Python initializer left creating an absent store at root/python: its
// schema_meta rows but store_id and store_created_at, and its comparableMirror, recorded
// (pyoracle).
func pythonCreated(t *testing.T, root string) (map[string]string, map[string]any) {
	t.Helper()
	var answer struct {
		Meta   map[string]string `json:"meta"`
		Mirror map[string]any    `json:"mirror"`
	}
	pyoracle.JSON(t, "created", &answer, func() (any, error) {
		real := filepath.Join(root, "python", "relay.sqlite3")
		var created map[string]string
		if err := json.Unmarshal([]byte(python(t, pythonCreates, real, fixedSocket)), &created); err != nil {
			return nil, err
		}
		// Only what the comparisons read: which store and when never agree between two runs.
		delete(created, "store_id")
		delete(created, "store_created_at")
		return map[string]any{"meta": created, "mirror": comparableMirror(mirror(t, real))}, nil
	}, pyoracle.Substitute(root, "<ROOT>"))
	return answer.Meta, answer.Mirror
}

func goAdmission(t *testing.T, path, socket string) string {
	t.Helper()
	s, err := store.Open(context.Background(), path, socket)
	if err != nil {
		return "refused: " + err.Error()
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	return admitted
}

// requireOwnedBy proves Go admits a store Go owns and refuses one Python owns only because of the
// owner.
func requireOwnedBy(t *testing.T, path, socket, owner string) {
	t.Helper()
	want := map[string]string{"go": admitted, "python": goRefusesPy}[owner]
	if got := goAdmission(t, path, socket); got != want {
		t.Fatalf("Go admission of a %s store: %s", owner, got)
	}
}

func meta(t *testing.T, path string) map[string]string {
	t.Helper()
	db, err := ownership.OpenExisting(context.Background(), path, "ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT key, value FROM schema_meta")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var key, value string
		if err = rows.Scan(&key, &value); err != nil {
			t.Fatal(err)
		}
		out[key] = value
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func mirror(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(path), "takeover.json"))
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err = json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	return record
}

// ownershipKeys is the six-key stamp, the part of schema_meta the fence adds.
func ownershipKeys(values map[string]string) map[string]string {
	out := map[string]string{}
	for _, key := range ownership.Keys {
		if value, ok := values[key]; ok {
			out[key] = value
		}
	}
	return out
}

// comparableMirror drops what differs between any two stores: which file, which store, when.
func comparableMirror(record map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range record {
		switch key {
		case "database", "storeId", "updatedAt", "relayRPCSocket":
		default:
			out[key] = value
		}
	}
	return out
}

// writeFixture writes the frozen Python-produced store by hand, as a contract fixture does: a
// complete schema with no ownership fence at all.
func writeFixture(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repositoryRoot(t), "contract/fixtures/sqlite-ddl/python-store.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	raw, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(to, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// fatalRecorder stands in for the test inside a helper that must fail: it records the message
// and ends the helper's goroutine the way t.Fatal ends the test's.
type fatalRecorder struct {
	testing.TB
	message string
}

func (r *fatalRecorder) Helper() {}
func (r *fatalRecorder) Fatalf(format string, args ...any) {
	r.message = fmt.Sprintf(format, args...)
	runtime.Goexit()
}
func (r *fatalRecorder) Fatal(args ...any) {
	r.message = fmt.Sprint(args...)
	runtime.Goexit()
}

func fatalOf(t *testing.T, helper func(testing.TB)) string {
	t.Helper()
	recorder := &fatalRecorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		helper(recorder)
	}()
	<-done
	return recorder.message
}

func requireFatal(t *testing.T, want string, helper func(testing.TB)) {
	t.Helper()
	if got := fatalOf(t, helper); !strings.Contains(got, want) {
		t.Fatalf("helper failure %q, want one naming %q", got, want)
	}
}

func TestCreate_is_admitted_by_the_owner_alone(t *testing.T) {
	for _, owner := range []string{"python", "go"} {
		for _, withSocket := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/socket=%t", owner, withSocket), func(t *testing.T) {
				root := t.TempDir()
				path, socket := filepath.Join(root, "state", "relay.sqlite3"), ""
				if withSocket {
					socket = filepath.Join(root, "app.sock")
				}
				testsupport.Create(t, path, socket, owner)
				requireOwnedBy(t, path, socket, owner)
				record := mirror(t, path)
				if withSocket && record["appServerSocket"] != socket || !withSocket && record["appServerSocket"] != nil {
					t.Fatalf("mirror socket %v, want %q", record["appServerSocket"], socket)
				}
			})
		}
	}
}

// An empty file is no database to SQLite and to the Python initializer; Create fills it in place,
// so a test that compares device and inode values keeps the inode it truncated.
func TestCreate_fills_an_empty_file_in_place(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.sqlite3")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := ownership.Physical(path)
	if err != nil {
		t.Fatal(err)
	}
	testsupport.Create(t, path, "", "go")
	after, err := ownership.Physical(path)
	if err != nil || after.Inode != before.Inode || after.Device != before.Device {
		t.Fatalf("inode %d became %d: %v", before.Inode, after.Inode, err)
	}
	requireOwnedBy(t, path, "", "go")
}

// Fence and Create stamp exactly what each runtime's own absent-store initializer stamps: the
// six schema_meta keys and the mirror agree with a store Python's Store (recorded) and Go's
// store.Open created from nothing, apart from which file and which store they are.
func TestFence_matches_each_runtimes_absent_store_initializer(t *testing.T) {
	for _, owner := range []string{"python", "go"} {
		t.Run(owner, func(t *testing.T) {
			root := t.TempDir()
			socket := fixedSocket
			var created map[string]string
			var createdMirror map[string]any
			if owner == "python" {
				created, createdMirror = pythonCreated(t, root)
			} else {
				real := filepath.Join(root, "go", "relay.sqlite3")
				s, err := store.Open(context.Background(), real, socket)
				if err != nil {
					t.Fatal(err)
				}
				if err = s.Close(); err != nil {
					t.Fatal(err)
				}
				created, createdMirror = meta(t, real), mirror(t, real)
			}
			fixture := filepath.Join(root, "fixture", "relay.sqlite3")
			testsupport.Create(t, fixture, socket, owner)
			if got, want := ownershipKeys(meta(t, fixture)), ownershipKeys(created); fmt.Sprint(got) != fmt.Sprint(want) || len(got) != 6 {
				t.Fatalf("stamp %v, %s initializer %v", got, owner, want)
			}
			if got, want := comparableMirror(mirror(t, fixture)), comparableMirror(createdMirror); fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("mirror %v, %s initializer %v", got, owner, want)
			}
			for _, name := range []string{"write-gate.lock", "takeover.lock"} {
				info, err := os.Stat(filepath.Join(filepath.Dir(fixture), name))
				if err != nil || info.Mode().Perm() != 0o600 {
					t.Fatalf("%s: %v %v", name, info, err)
				}
			}
		})
	}
}

// A hand-written fixture is refused by Go until it is fenced; Fence then gives the owner the
// store, and fencing again for the same owner changes nothing.
func TestFence_gives_a_fixture_to_one_runtime(t *testing.T) {
	for _, owner := range []string{"python", "go"} {
		t.Run(owner, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state", "relay.sqlite3")
			writeFixture(t, path)
			// Reversion: the unfenced fixture is refused by Go (Python would adopt it as its own,
			// which is exactly what a test on Go must not rely on).
			if got := goAdmission(t, path, ""); !strings.HasPrefix(got, "refused: store_owned_by_other") {
				t.Fatalf("Go admitted an unfenced fixture: %s", got)
			}
			testsupport.Fence(t, path, owner)
			before, err := os.ReadFile(filepath.Join(filepath.Dir(path), "takeover.json"))
			if err != nil {
				t.Fatal(err)
			}
			testsupport.Fence(t, path, owner)
			after, err := os.ReadFile(filepath.Join(filepath.Dir(path), "takeover.json"))
			if err != nil || string(before) != string(after) {
				t.Fatalf("a second Fence republished the mirror: %v", err)
			}
			requireOwnedBy(t, path, "", owner)
			// The fixture's own socket_path decides the mirror's socket and scope key.
			record := mirror(t, path)
			key, err := ownership.ScopeKey("/fixture/python.sock")
			if err != nil || record["appServerSocket"] != "/fixture/python.sock" || record["scopeKey"] != key {
				t.Fatalf("mirror socket %v scope %v, want the fixture's /fixture/python.sock %s: %v", record["appServerSocket"], record["scopeKey"], key, err)
			}
		})
	}
}

func TestFence_refuses_partial_foreign_and_copied_stores(t *testing.T) {
	t.Run("write gate only", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "relay.sqlite3")
		writeFixture(t, path)
		if err := os.WriteFile(filepath.Join(filepath.Dir(path), "write-gate.lock"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		requireFatal(t, "partially fenced store: 0/6 ownership keys, mirror=false, write gate=true", func(tb testing.TB) { testsupport.Fence(tb, path, "go") })
	})
	t.Run("keys without a mirror", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "relay.sqlite3")
		testsupport.Create(t, path, "", "go")
		if err := os.Remove(filepath.Join(filepath.Dir(path), "takeover.json")); err != nil {
			t.Fatal(err)
		}
		requireFatal(t, "partially fenced store: 6/6 ownership keys, mirror=false, write gate=true", func(tb testing.TB) { testsupport.Fence(tb, path, "go") })
	})
	t.Run("owned by the other runtime", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "relay.sqlite3")
		testsupport.Create(t, path, "", "python")
		requireFatal(t, "store is owned by python at epoch 1 (moving it is HandOver)", func(tb testing.TB) { testsupport.Fence(tb, path, "go") })
	})
	t.Run("copied", func(t *testing.T) {
		original := filepath.Join(t.TempDir(), "relay.sqlite3")
		testsupport.Create(t, original, "", "go")
		copied := filepath.Join(t.TempDir(), "relay.sqlite3")
		for _, name := range []string{"relay.sqlite3", "takeover.json", "write-gate.lock"} {
			copyFile(t, filepath.Join(filepath.Dir(original), name), filepath.Join(filepath.Dir(copied), name))
		}
		requireFatal(t, "fenced store does not validate (a copied store is Rehome)", func(tb testing.TB) { testsupport.Fence(tb, copied, "go") })
	})
	t.Run("not absent", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "relay.sqlite3")
		writeFixture(t, path)
		requireFatal(t, "store is not absent", func(tb testing.TB) { testsupport.Create(tb, path, "", "go") })
	})
}

// A takeover moves a stopped store between the runtimes in either direction; each step leaves the
// store admitted by the new owner alone, with the installed transition both validators require.
func TestHandOver_completes_a_takeover_either_way(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "state", "relay.sqlite3")
	socket := filepath.Join(root, "app.sock")
	testsupport.Create(t, path, socket, "python")
	// Reversion: without the takeover Go refuses the store Python owns.
	if got := goAdmission(t, path, socket); got != goRefusesPy {
		t.Fatalf("Go before the takeover: %s", got)
	}
	hex32 := regexp.MustCompile(`^[0-9a-f]{32}$`)
	epoch := 1
	for _, step := range [][2]string{{"python", "go"}, {"go", "python"}, {"python", "go"}} {
		from, to := step[0], step[1]
		testsupport.HandOver(t, path, to)
		epoch++
		requireOwnedBy(t, path, socket, to)
		values := meta(t, path)
		if values["owner"] != to || values["owner_epoch"] != fmt.Sprint(epoch) || !hex32.MatchString(values["takeover_id"]) || values["rollback_allowed"] != "1" {
			t.Fatalf("stamp after %s->%s: %v", from, to, ownershipKeys(values))
		}
		record := mirror(t, path)
		want := map[string]any{"id": values["takeover_id"], "from": from, "to": to, "targetEpoch": float64(epoch)}
		if fmt.Sprint(record["transition"]) != fmt.Sprint(want) || record["phase"] != "active" || record["owner"] != to || record["epoch"] != float64(epoch) || record["holder"] != nil || record["controller"] != nil {
			t.Fatalf("mirror after %s->%s: %v", from, to, record)
		}
		// Handing a store to its owner changes nothing.
		before := mirror(t, path)
		testsupport.HandOver(t, path, to)
		if fmt.Sprint(before) != fmt.Sprint(mirror(t, path)) {
			t.Fatal("a HandOver to the owner republished the mirror")
		}
	}
}

func TestHandOver_refuses_a_store_in_use_or_unfenced(t *testing.T) {
	t.Run("in use", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "relay.sqlite3")
		testsupport.Create(t, path, "", "go")
		s, err := store.Open(context.Background(), path, "")
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		requireFatal(t, "write-gate.lock (is the store in use?)", func(tb testing.TB) { testsupport.HandOver(tb, path, "python") })
		if values := meta(t, path); values["owner"] != "go" || values["owner_epoch"] != "1" {
			t.Fatalf("a refused HandOver changed the stamp: %v", ownershipKeys(values))
		}
	})
	t.Run("unfenced", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "relay.sqlite3")
		writeFixture(t, path)
		requireFatal(t, "not a fenced store (0/6 ownership keys, mirror=false, write gate=false): a fixture is Fence", func(tb testing.TB) { testsupport.HandOver(tb, path, "go") })
	})
}

// A helper's fence locks are free the moment it returns, whatever else its test binary is doing.
// The live-Python parity tests start processes in parallel while other tests hand stores over.
// A process started while a helper held its locks shares both lock descriptions until its exec
// closes them, and a busy host keeps a new child short of its exec for milliseconds: the next
// helper on that store then failed with "takeover.lock: resource temporarily unavailable",
// held by nothing but a child of its own binary. Here processes start without pause while a
// store is rehomed and handed over, and every helper's return is followed at once by the
// exclusive attempt on both locks that the next helper or runtime makes.
func TestHelpers_leave_no_fence_lock_to_a_process_their_binary_starts(t *testing.T) {
	command, err := exec.LookPath("true")
	if err != nil {
		t.Skip("no true(1) to start")
	}
	path := filepath.Join(t.TempDir(), "relay.sqlite3")
	testsupport.Create(t, path, "", "python")
	stop := make(chan struct{})
	var starters sync.WaitGroup
	defer func() { close(stop); starters.Wait() }()
	for range 4 {
		starters.Add(1)
		go func() {
			defer starters.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = exec.Command(command).Run()
			}
		}()
	}
	// The attempt holds two descriptors of its own, which a starting process could copy just as
	// well, so it takes them under ForkLock as the helpers do: a failure is the helper's alone.
	free := func(i int, helper string) {
		syscall.ForkLock.RLock()
		defer syscall.ForkLock.RUnlock()
		for _, name := range []string{"takeover.lock", "write-gate.lock"} {
			lock, err := ownership.Lock(filepath.Join(filepath.Dir(path), name), true, false)
			if err != nil {
				t.Fatalf("iteration %d: %s still held when %s returned: %v", i, name, helper, err)
			}
			if err = lock.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
	for i := range 100 {
		testsupport.Rehome(t, path)
		free(i, "Rehome")
		testsupport.HandOver(t, path, []string{"go", "python"}[i%2])
		free(i, "HandOver")
	}
}

// A fenced store copied elsewhere names the original's inode, so both runtimes refuse the copy
// until Rehome gives it its own identity; the original is untouched.
func TestRehome_gives_a_copied_store_its_own_identity(t *testing.T) {
	for _, owner := range []string{"python", "go"} {
		for _, withMirror := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/mirror=%t", owner, withMirror), func(t *testing.T) {
				original := filepath.Join(t.TempDir(), "relay.sqlite3")
				testsupport.Create(t, original, "", otherOwner(owner))
				testsupport.HandOver(t, original, owner) // the copy carries an installed transition
				copied := filepath.Join(t.TempDir(), "copy", "relay.sqlite3")
				copyFile(t, original, copied)
				if withMirror {
					copyFile(t, filepath.Join(filepath.Dir(original), "takeover.json"), filepath.Join(filepath.Dir(copied), "takeover.json"))
				}
				// Reversion: Go refuses the copy before Rehome, whoever owns it. (The Python fence's
				// refusal of a Python-owned copy was checked here until todo 44.)
				if refused := goAdmission(t, copied, ""); !strings.HasPrefix(refused, "refused: store_owned_by_other") {
					t.Fatalf("Go admitted a copied %s store before Rehome: %s", owner, refused)
				}
				testsupport.Rehome(t, copied)
				requireOwnedBy(t, copied, "", owner)
				requireOwnedBy(t, original, "", owner)
				if got, want := comparableMirror(mirror(t, copied)), comparableMirror(mirror(t, original)); fmt.Sprint(got) != fmt.Sprint(want) {
					t.Fatalf("Rehome changed more than the identity:\ncopy     %v\noriginal %v", got, want)
				}
			})
		}
	}
}

func TestRehome_refuses_a_shared_directory_and_an_unfenced_store(t *testing.T) {
	t.Run("shared directory", func(t *testing.T) {
		original := filepath.Join(t.TempDir(), "relay.sqlite3")
		testsupport.Create(t, original, "", "go")
		sibling := filepath.Join(filepath.Dir(original), "other.sqlite3")
		copyFile(t, original, sibling)
		requireFatal(t, "one takeover.json per directory", func(tb testing.TB) { testsupport.Rehome(tb, sibling) })
		requireOwnedBy(t, original, "", "go")
	})
	t.Run("unfenced", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "relay.sqlite3")
		writeFixture(t, path)
		requireFatal(t, "not a fenced store", func(tb testing.TB) { testsupport.Rehome(tb, path) })
	})
}

// A copy of a store its creator still owns, restamped for the other runtime, is admitted by that
// runtime alone, leaves the original as it was, and differs from it in the owner row alone - the
// row OwnerNeutral neutralizes - so a whole-state comparison of the two needs nothing more.
func TestRestamp_gives_a_copy_to_the_other_runtime_as_its_creator(t *testing.T) {
	for _, owner := range []string{"python", "go"} {
		for _, withMirror := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/mirror=%t", owner, withMirror), func(t *testing.T) {
				root := t.TempDir()
				socket := filepath.Join(root, "app.sock")
				original := filepath.Join(root, "original", "relay.sqlite3")
				testsupport.Create(t, original, socket, otherOwner(owner))
				copied := filepath.Join(root, "copy", "relay.sqlite3")
				copyFile(t, original, copied)
				if withMirror {
					copyFile(t, filepath.Join(filepath.Dir(original), "takeover.json"), filepath.Join(filepath.Dir(copied), "takeover.json"))
				}
				// Reversion: Go refuses the copy before Restamp, whoever it is for. (The Python fence's
				// refusal of a copy restamped for it was checked here until todo 44.)
				if refused := goAdmission(t, copied, socket); !strings.HasPrefix(refused, "refused: store_owned_by_other") {
					t.Fatalf("Go admitted the copy for %s before Restamp: %s", owner, refused)
				}
				testsupport.Restamp(t, copied, owner)
				requireOwnedBy(t, copied, socket, owner)
				requireOwnedBy(t, original, socket, otherOwner(owner))
				from, to := meta(t, original), meta(t, copied)
				var differing []string
				for key, value := range from {
					if to[key] != value {
						differing = append(differing, key)
					}
				}
				if len(from) != len(to) || fmt.Sprint(differing) != "[owner]" || to["owner"] != owner || to["owner_epoch"] != "1" || to["takeover_id"] != "" {
					t.Fatalf("Restamp changed %v, want [owner]\noriginal %v\ncopy     %v", differing, from, to)
				}
				for key := range from {
					original, restamped := testsupport.Runtime(otherOwner(owner)), testsupport.Runtime(owner)
					if testsupport.OwnerNeutral(t, original, key, from[key]) != testsupport.OwnerNeutral(t, restamped, key, to[key]) {
						t.Fatalf("schema_meta %s still differs after OwnerNeutral: %q, %q", key, from[key], to[key])
					}
				}
				// The mirror is the one owner's initializer publishes for a store it created.
				created := filepath.Join(root, "created", "relay.sqlite3")
				testsupport.Create(t, created, socket, owner)
				if got, want := comparableMirror(mirror(t, copied)), comparableMirror(mirror(t, created)); fmt.Sprint(got) != fmt.Sprint(want) {
					t.Fatalf("restamped mirror differs from a created one:\ncopy    %v\ncreated %v", got, want)
				}
			})
		}
	}
}

func TestRestamp_refuses_an_original_a_transferred_a_shared_and_an_unfenced_store(t *testing.T) {
	t.Run("original", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "relay.sqlite3")
		testsupport.Create(t, path, "", "python")
		requireFatal(t, "not a copy: the mirror names this very file", func(tb testing.TB) { testsupport.Restamp(tb, path, "go") })
		requireOwnedBy(t, path, "", "python")
	})
	t.Run("changed hands", func(t *testing.T) {
		original := filepath.Join(t.TempDir(), "relay.sqlite3")
		testsupport.Create(t, original, "", "python")
		testsupport.HandOver(t, original, "go")
		copied := filepath.Join(t.TempDir(), "relay.sqlite3")
		copyFile(t, original, copied)
		requireFatal(t, "store has changed hands (go at epoch 2", func(tb testing.TB) { testsupport.Restamp(tb, copied, "python") })
	})
	t.Run("shared directory", func(t *testing.T) {
		original := filepath.Join(t.TempDir(), "relay.sqlite3")
		testsupport.Create(t, original, "", "go")
		sibling := filepath.Join(filepath.Dir(original), "other.sqlite3")
		copyFile(t, original, sibling)
		requireFatal(t, "one takeover.json per directory", func(tb testing.TB) { testsupport.Restamp(tb, sibling, "python") })
		requireOwnedBy(t, original, "", "go")
	})
	t.Run("unfenced", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "relay.sqlite3")
		writeFixture(t, path)
		requireFatal(t, "not a fenced store", func(tb testing.TB) { testsupport.Restamp(tb, path, "go") })
	})
}

// Python's (recorded) and Go's absent-store initializers leave schema_meta rows that differ only
// in the owner value and the store's own identity; OwnerNeutral removes the first and nothing
// else.
func TestOwnerNeutral_is_the_only_runtime_difference_in_schema_meta(t *testing.T) {
	root := t.TempDir()
	socket := fixedSocket
	pythonMeta, _ := pythonCreated(t, root)
	goPath := filepath.Join(root, "go", "relay.sqlite3")
	s, err := store.Open(context.Background(), goPath, socket)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	goMeta := meta(t, goPath)
	for _, key := range []string{"store_id", "store_created_at"} {
		delete(pythonMeta, key)
		delete(goMeta, key)
	}
	// Reversion: the raw rows differ, by the owner and nothing else.
	var differing []string
	for key, value := range pythonMeta {
		if goMeta[key] != value {
			differing = append(differing, key)
		}
	}
	if len(pythonMeta) != len(goMeta) || fmt.Sprint(differing) != "[owner]" {
		t.Fatalf("raw schema_meta differs in %v\nPython %v\nGo     %v", differing, pythonMeta, goMeta)
	}
	for key := range pythonMeta {
		pythonMeta[key] = testsupport.OwnerNeutral(t, testsupport.Python, key, pythonMeta[key])
		goMeta[key] = testsupport.OwnerNeutral(t, testsupport.Go, key, goMeta[key])
	}
	if fmt.Sprint(pythonMeta) != fmt.Sprint(goMeta) {
		t.Fatalf("neutral schema_meta differs\nPython %v\nGo     %v", pythonMeta, goMeta)
	}
	for _, c := range []struct {
		writer testsupport.Runtime
		key    string
		in     any
		want   any
	}{
		{testsupport.Python, "owner", "python", testsupport.RuntimeOwner}, {testsupport.Go, "owner", "go", testsupport.RuntimeOwner},
		{testsupport.Go, "owner_epoch", "1", "1"}, {testsupport.Python, "store_id", "go", "go"},
	} {
		if got := testsupport.OwnerNeutral(t, c.writer, c.key, c.in); got != c.want {
			t.Fatalf("OwnerNeutral(%s, %q, %v) = %v, want %v", c.writer, c.key, c.in, got, c.want)
		}
	}
	// The other runtime's owner, or one naming neither, fails the comparison (identity_test.go).
}

func otherOwner(owner string) string {
	if owner == "python" {
		return "go"
	}
	return "python"
}
