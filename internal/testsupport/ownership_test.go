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
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// Each fixture state is judged by Go's real admission, store.Open. A refusal of a store the other
// runtime owns is proven by its last check, the one that runs only after the mirror, the durable
// stamp and the physical identity all agreed. The retained Python fence's own admission of these
// stores was checked beside it until todo 44: rollback to Python closed at todo 43
// (rollback_allowed=0), and the Python runtime leaves in todo 44.
const (
	admitted    = "admitted"
	goRefusesPy = "refused: store_owned_by_other: the relay store belongs to another runtime"
)

// fixedSocket is the App Server socket the initializer comparisons name. It is never connected;
// it is fixed so that the scope key derived from it is the same in a golden and in every run.
const fixedSocket = "/crw-test/app.sock"

// scopeKey is the scope key of fixedSocket as this process's environment derives it: namespaced by
// CODEX_SESSION_RELAY_SCOPE_DIR when that overrides the registry root. A golden spells it
// <SCOPE-KEY>, since the override differs from host to host.
func scopeKey(t *testing.T) string {
	t.Helper()
	key, err := ownership.ScopeKey(fixedSocket)
	if err != nil {
		t.Fatal(err)
	}
	return key
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
	raw, err := testsupport.FrozenStore()
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
// six schema_meta keys and the mirror agree with the golden of Python's Store (it began as the
// rows and mirror the retained Python Store left) and with a store Go's store.Open created from
// nothing, apart from which file and which store they are.
func TestFence_matches_each_runtimes_absent_store_initializer(t *testing.T) {
	for _, owner := range []string{"python", "go"} {
		t.Run(owner, func(t *testing.T) {
			root := t.TempDir()
			socket := fixedSocket
			fixture := filepath.Join(root, "fixture", "relay.sqlite3")
			testsupport.Create(t, fixture, socket, owner)
			stamp, stampMirror := ownershipKeys(meta(t, fixture)), comparableMirror(mirror(t, fixture))
			if len(stamp) != 6 {
				t.Fatalf("stamp %v", stamp)
			}
			if owner == "python" {
				golden.CheckJSON(t, "stamp", stamp)
				golden.CheckJSON(t, "mirror", stampMirror, golden.Substitute(scopeKey(t), "<SCOPE-KEY>"))
			} else {
				real := filepath.Join(root, "go", "relay.sqlite3")
				s, err := store.Open(context.Background(), real, socket)
				if err != nil {
					t.Fatal(err)
				}
				if err = s.Close(); err != nil {
					t.Fatal(err)
				}
				if want := ownershipKeys(meta(t, real)); fmt.Sprint(stamp) != fmt.Sprint(want) {
					t.Fatalf("stamp %v, go initializer %v", stamp, want)
				}
				if want := comparableMirror(mirror(t, real)); fmt.Sprint(stampMirror) != fmt.Sprint(want) {
					t.Fatalf("mirror %v, go initializer %v", stampMirror, want)
				}
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

// A copy of a store Python still owns, restamped for Go, is admitted by Go alone, leaves the
// original as it was, and differs from it in the owner row alone - the row OwnerNeutral
// neutralizes.
func TestRestamp_gives_a_copy_to_the_other_runtime_as_its_creator(t *testing.T) {
	for _, withMirror := range []bool{true, false} {
		t.Run(fmt.Sprintf("go/mirror=%t", withMirror), func(t *testing.T) {
			root := t.TempDir()
			socket := filepath.Join(root, "app.sock")
			original := filepath.Join(root, "original", "relay.sqlite3")
			testsupport.Create(t, original, socket, "python")
			copied := filepath.Join(root, "copy", "relay.sqlite3")
			copyFile(t, original, copied)
			if withMirror {
				copyFile(t, filepath.Join(filepath.Dir(original), "takeover.json"), filepath.Join(filepath.Dir(copied), "takeover.json"))
			}
			// Reversion: Go refuses the copy of a Python store before Restamp. A copy stamped go is
			// Go's to write on its stamp alone (decision 56).
			if refused := goAdmission(t, copied, socket); !strings.HasPrefix(refused, "refused: store_owned_by_other") {
				t.Fatalf("Go admitted the copy before Restamp: %s", refused)
			}
			testsupport.Restamp(t, copied)
			requireOwnedBy(t, copied, socket, "go")
			requireOwnedBy(t, original, socket, "python")
			from, to := meta(t, original), meta(t, copied)
			// Go's admission of the copy above is a writable open of a store from before the
			// settlements backfill's marker, and records it: the one row besides the owner by which
			// the copy differs.
			if to["backfill:assignment_settlements"] != "1" {
				t.Fatalf("the copy Go admitted carries no backfill marker: %v", to)
			}
			delete(to, "backfill:assignment_settlements")
			var differing []string
			for key, value := range from {
				if to[key] != value {
					differing = append(differing, key)
				}
			}
			if len(from) != len(to) || fmt.Sprint(differing) != "[owner]" || to["owner"] != "go" || to["owner_epoch"] != "1" || to["takeover_id"] != "" {
				t.Fatalf("Restamp changed %v, want [owner]\noriginal %v\ncopy     %v", differing, from, to)
			}
			if got := testsupport.OwnerNeutral(t, "owner", to["owner"]); got != testsupport.RuntimeOwner {
				t.Fatalf("restamped owner %q reads %q after OwnerNeutral", to["owner"], got)
			}
			// The mirror is the one Go's initializer publishes for a store it created.
			created := filepath.Join(root, "created", "relay.sqlite3")
			testsupport.Create(t, created, socket, "go")
			if got, want := comparableMirror(mirror(t, copied)), comparableMirror(mirror(t, created)); fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("restamped mirror differs from a created one:\ncopy    %v\ncreated %v", got, want)
			}
		})
	}
}

func TestRestamp_refuses_an_original_a_transferred_a_shared_and_an_unfenced_store(t *testing.T) {
	t.Run("original", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "relay.sqlite3")
		testsupport.Create(t, path, "", "python")
		requireFatal(t, "not a copy: the mirror names this very file", func(tb testing.TB) { testsupport.Restamp(tb, path) })
		requireOwnedBy(t, path, "", "python")
	})
	t.Run("changed hands", func(t *testing.T) {
		original := filepath.Join(t.TempDir(), "relay.sqlite3")
		testsupport.Create(t, original, "", "python")
		testsupport.HandOver(t, original, "go")
		copied := filepath.Join(t.TempDir(), "relay.sqlite3")
		copyFile(t, original, copied)
		requireFatal(t, "store has changed hands (go at epoch 2", func(tb testing.TB) { testsupport.Restamp(tb, copied) })
	})
	t.Run("shared directory", func(t *testing.T) {
		original := filepath.Join(t.TempDir(), "relay.sqlite3")
		testsupport.Create(t, original, "", "go")
		sibling := filepath.Join(filepath.Dir(original), "other.sqlite3")
		copyFile(t, original, sibling)
		requireFatal(t, "one takeover.json per directory", func(tb testing.TB) { testsupport.Restamp(tb, sibling) })
		requireOwnedBy(t, original, "", "go")
	})
	t.Run("unfenced", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "relay.sqlite3")
		writeFixture(t, path)
		requireFatal(t, "not a fenced store", func(tb testing.TB) { testsupport.Restamp(tb, path) })
	})
}

// A store Create stamps for Python - the store the foreign-owner tests put before Go - holds the
// schema_meta rows Go's absent-store initializer leaves but for the owner value, the store's own
// identity and the settlements backfill's marker a store Go creates is born with; OwnerNeutral
// removes Go's owner and nothing else.
func TestOwnerNeutral_is_the_only_runtime_difference_in_schema_meta(t *testing.T) {
	root := t.TempDir()
	socket := fixedSocket
	// Python's rows are those Create stamps for Python, pinned by a golden that began as the rows
	// the retained Python Store left creating an absent store.
	pythonPath := filepath.Join(root, "python", "relay.sqlite3")
	testsupport.Create(t, pythonPath, socket, "python")
	pythonMeta := meta(t, pythonPath)
	delete(pythonMeta, "store_id")
	delete(pythonMeta, "store_created_at")
	golden.CheckJSON(t, "python-meta", pythonMeta)
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
		delete(goMeta, key)
	}
	// Go's initializer has no observation to backfill, so it creates the store with the marker.
	if goMeta["backfill:assignment_settlements"] != "1" {
		t.Fatalf("a store Go created carries no backfill marker: %v", goMeta)
	}
	delete(goMeta, "backfill:assignment_settlements")
	var differing []string
	for key, value := range pythonMeta {
		if goMeta[key] != value {
			differing = append(differing, key)
		}
	}
	if len(pythonMeta) != len(goMeta) || fmt.Sprint(differing) != "[owner]" {
		t.Fatalf("raw schema_meta differs in %v\nPython %v\nGo     %v", differing, pythonMeta, goMeta)
	}
	for key, value := range goMeta {
		want := value
		if key == "owner" {
			want = testsupport.RuntimeOwner
		}
		if got := testsupport.OwnerNeutral(t, key, value); got != want {
			t.Fatalf("OwnerNeutral(%q, %q) = %q, want %q", key, value, got, want)
		}
	}
	// Python's owner, or one naming no runtime, fails the comparison (identity_test.go).
}

func otherOwner(owner string) string {
	if owner == "python" {
		return "go"
	}
	return "python"
}
