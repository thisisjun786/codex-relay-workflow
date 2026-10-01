package record_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/golden"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/reading"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	expected "github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

func TestMain(m *testing.M) {
	golden.Helper()
	cleanup, err := testsupport.IsolateRelayState()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	code := m.Run()
	if err := cleanup(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}

var fixture = filepath.Join("testdata", "host-record-v1.json")

func fixtureCopy(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "codex-relay-workflow", record.Name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The committed fixture is a redacted copy of the relay host's real record (recordVersion 1,
// Python installs, points, outgoing, pointer, selected), written by json.dump. Loading and
// saving it without a change reproduces it byte for byte.
func TestV1RecordRoundTripsByteForByte(t *testing.T) {
	raw, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	read := record.Load(fixture, 1)
	if read.State != reading.Present {
		t.Fatalf("state %s: %s", read.State, read.Detail)
	}
	if got := record.Encode(read.Value); !bytes.Equal(got, raw) {
		t.Fatalf("encoding the loaded record changed its bytes:\n%s", firstDifference(got, raw))
	}
	saved := filepath.Join(t.TempDir(), "nested", record.Name)
	if err := record.Save(saved, read.Value); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(saved); !bytes.Equal(again, raw) {
		t.Fatalf("a saved record differs from the loaded one:\n%s", firstDifference(again, raw))
	}
}

func firstDifference(got, want []byte) string {
	g, w := strings.Split(string(got), "\n"), strings.Split(string(want), "\n")
	for i := 0; i < len(g) && i < len(w); i++ {
		if g[i] != w[i] {
			return fmt.Sprintf("line %d\n go: %q\n py: %q", i+1, g[i], w[i])
		}
	}
	return fmt.Sprintf("lengths differ: go %d lines, python %d lines", len(g), len(w))
}

// json.dumps(indent=2, sort_keys=True) over Python's own JSON: empty containers, escapes,
// floats in repr form, integers of any size, the non-finite constants, a repeated key and a
// lone surrogate. The inputs are a fixture; each output is the golden, which began as Python's.
func TestEncodeIsPythonsIndentedJSON(t *testing.T) {
	var inputs []string
	if err := json.Unmarshal(expected.Fixture(t, "dumps-indent-inputs.json"), &inputs); err != nil {
		t.Fatal(err)
	}
	for _, input := range inputs {
		value, err := reading.Decode([]byte(input))
		if err != nil {
			t.Fatalf("%s: %v", input, err)
		}
		expected.Check(t, input, record.Encode(value))
	}
}

// fixtureObject is the named fixture of this package, decoded with its key order kept.
func fixtureObject(t *testing.T, name string) record.Object {
	t.Helper()
	value, err := reading.Decode(expected.Fixture(t, name))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return golden.Obj(value)
}

func deltaOf(t *testing.T, raw record.Object) record.Delta {
	t.Helper()
	var delta record.Delta
	named := func(key string) []record.Named {
		var out []record.Named
		for _, pair := range golden.List(record.Get(raw, key)) {
			items := golden.List(pair)
			out = append(out, record.Named{Component: items[0].(string), Entry: golden.Obj(items[1])})
		}
		return out
	}
	delta.Installs, delta.Points = named("installs"), named("points")
	for _, f := range golden.Obj(record.Get(raw, "select")) {
		delta.Select = append(delta.Select, f)
	}
	for _, f := range golden.Obj(record.Get(raw, "deselect")) {
		delta.Deselect = append(delta.Deselect, f)
	}
	if v, ok := record.Get(raw, "drop_environment").(string); ok {
		delta.DropEnvironment = &v
	}
	if v, ok := record.Get(raw, "drop_pointer").(string); ok {
		delta.DropPointer = &v
	}
	delta.Pointer = golden.Obj(record.Get(raw, "pointer"))
	if restore := golden.Obj(record.Get(raw, "restore_pointer")); restore != nil {
		delta.RestorePointer = &record.Restore{Wrote: record.Text(restore, "wrote"), Found: golden.Obj(record.Get(restore, "found"))}
	}
	return delta
}

// hostrecord.update's narrow deltas (the fixture update-deltas.json), applied to the same v1
// record, write the golden bytes, which began as Python's: a Go install is additive
// (binaryDigest, target, source; no interpreter fields), a re-install at one location replaces
// that location's entry, a selection or pointer is compare-and-removed or compare-and-replaced,
// and the .crw-lock is gone afterwards.
func TestUpdateWritesWhatPythonWrites(t *testing.T) {
	for _, f := range fixtureObject(t, "update-deltas.json") {
		t.Run(f.Key, func(t *testing.T) {
			path := fixtureCopy(t)
			read, err := record.Update(path, 1, deltaOf(t, golden.Obj(f.Value)))
			if err != nil {
				t.Fatalf("state %s err %v", read.State, err)
			}
			expected.Check(t, "state", []byte(read.State))
			written, _ := os.ReadFile(path)
			expected.Check(t, "written", written)
			if _, err := os.Lstat(path + record.LockSuffix); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the .crw-lock was left behind: %v", err)
			}
		})
	}
}

func TestUpdateOnAnAbsentRecordStartsFromEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", record.Name)
	if _, err := record.Update(path, 1, record.Delta{Select: []contract.Field{{Key: "codex-session-relay", Value: "/x"}}}); err != nil {
		t.Fatal(err)
	}
	read := record.Load(path, 1)
	written := read.Value.(record.Object)
	var keys []any
	for _, key := range []string{"components", "definitionVersion", "host", "recordVersion", "selected", "user"} {
		if _, ok := record.Lookup(written, key); ok {
			keys = append(keys, key)
		}
	}
	if len(written) != len(keys) || record.Get(written, "recordVersion") != int64(1) {
		t.Fatalf("an absent record became %s", golden.Canon(written))
	}
	expected.Check(t, "keys and selected", []byte(golden.Canon(record.Object{{Key: "keys", Value: keys}, {Key: "selected", Value: record.Get(written, "selected")}})))
}

// An unreadable record is never replaced: it is the only evidence anything was exercised.
func TestUpdateNeverReplacesAnUnreadableRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), record.Name)
	if err := os.WriteFile(path, []byte("{ not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	read, err := record.Update(path, 1, record.Delta{Select: []contract.Field{{Key: "codex-session-relay", Value: "/x"}}})
	if err != nil {
		t.Fatalf("state %s exception %s err %v", read.State, read.Exception, err)
	}
	expected.Check(t, "state and exception", []byte(golden.Canon(record.Object{{Key: "state", Value: read.State}, {Key: "exception", Value: read.Exception}})))
	if after, _ := os.ReadFile(path); string(after) != "{ not json" {
		t.Fatalf("the unreadable record was rewritten: %q", after)
	}
}

// Each record in the fixture shape-inputs.json is refused, or accepted, as the golden says
// (which began as hostrecord.shape's answer).
func TestShapeRefusesWhatPythonRefuses(t *testing.T) {
	for _, f := range fixtureObject(t, "shape-inputs.json") {
		refusal := any(nil)
		if err := record.Shape(f.Value); err != nil {
			refusal = err.Error()
		}
		expected.Check(t, f.Key, []byte(golden.Canon(refusal)))
	}
}

// The four states of the host record reading, each its own answer.
func TestLoadKeepsFourAnswers(t *testing.T) {
	dir := t.TempDir()
	if read := record.Load(filepath.Join(dir, "missing.json"), 1); read.State != reading.Absent || record.Get(read.Value.(record.Object), "recordVersion") != int64(1) {
		t.Fatalf("absent: %+v", read)
	}
	if read := record.Load(dir, 1); read.State != reading.Unreadable {
		t.Fatalf("a directory: %s", read.State)
	}
	listed := filepath.Join(dir, "list.json")
	if err := os.WriteFile(listed, []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	if read := record.Load(listed, 1); read.State != reading.Unreadable || read.Exception != "TypeError" {
		t.Fatalf("a list: %+v", read)
	}
	if os.Geteuid() != 0 {
		locked := filepath.Join(dir, "locked")
		if err := os.Mkdir(locked, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(locked, record.Name), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(locked, 0); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(locked, 0o700)
		if read := record.Load(filepath.Join(locked, record.Name), 1); read.State != reading.AccessError || read.Exception != "PermissionError" {
			t.Fatalf("behind a directory without search permission: %+v", read)
		}
	}
}

// A point covers only its own install, binary digest, Codex CLI and host; the App Server is
// symmetric (two absences agree, one does not); a point never exercised, or measured against
// bytes that disagreed with the definition, covers nothing.
func TestPointsForComparesEveryDimensionByItsPolicy(t *testing.T) {
	point := record.Object{{Key: "exercised", Value: true}, {Key: "install", Value: "/rt/bin"}, {Key: "installDigest", Value: "d"}, {Key: "codexCli", Value: "codex-cli 1"}, {Key: "host", Value: "h"}}
	rec := record.AddPoint(record.Empty(1), "codex-session-relay", point)
	wanted := map[string]string{"install": "/rt/bin", "installDigest": "d", "codexCli": "codex-cli 1", "host": "h"}
	if got := record.PointsFor(rec, "codex-session-relay", wanted); len(got) != 1 {
		t.Fatalf("the matching point was not found: %d", len(got))
	}
	for _, change := range []string{"install", "installDigest", "codexCli", "host"} {
		asked := map[string]string{}
		for k, v := range wanted {
			asked[k] = v
		}
		asked[change] = "other"
		if len(record.PointsFor(rec, "codex-session-relay", asked)) != 0 {
			t.Errorf("%s: a different value still matched", change)
		}
		delete(asked, change)
		if len(record.PointsFor(rec, "codex-session-relay", asked)) != 0 {
			t.Errorf("%s: an unobserved mandatory dimension matched a recorded one", change)
		}
	}
	withServer := map[string]string{"appServer": "s"}
	for k, v := range wanted {
		withServer[k] = v
	}
	if len(record.PointsFor(rec, "codex-session-relay", withServer)) != 0 {
		t.Error("an observed App Server matched a point that recorded none")
	}
	for _, gate := range []record.Object{{{Key: "exercised", Value: false}}, {{Key: "digestMatchesDefinition", Value: false}}} {
		gated := append(record.Object{}, point...)
		gated = record.Set(gated, gate[0].Key, gate[0].Value)
		if len(record.PointsFor(record.AddPoint(record.Empty(1), "x", gated), "x", wanted)) != 0 {
			t.Errorf("%s: a gated point covered the install", gate[0].Key)
		}
	}
}

func TestPlacementNeedsNonBlankStrings(t *testing.T) {
	for _, c := range []struct {
		entry any
		want  bool
	}{
		{record.Object{{Key: "path", Value: "/p"}, {Key: "recordedAt", Value: "t"}, {Key: "recordedBy", Value: "CRW-1"}}, true},
		{record.Object{{Key: "path", Value: "/p"}}, false},
		{record.Object{{Key: "path", Value: "/p"}, {Key: "recordedAt", Value: "   "}, {Key: "recordedBy", Value: "CRW-1"}}, false},
		{record.Object{{Key: "path", Value: "/p"}, {Key: "recordedAt", Value: true}, {Key: "recordedBy", Value: "CRW-1"}}, false},
		{record.Object{{Key: "path", Value: ""}, {Key: "recordedAt", Value: "t"}, {Key: "recordedBy", Value: "x"}}, false},
		{"path", false},
		{nil, false},
	} {
		if got := record.PlacementRecorded(c.entry); got != c.want {
			t.Errorf("%v: %v", c.entry, got)
		}
	}
	entry := record.Object{{Key: "path", Value: "/p"}, {Key: "recordedAt", Value: "t"}, {Key: "recordedBy", Value: "CRW-1"}}
	if kept := record.WithoutPlacement(entry); golden.Canon(kept) != `{"path":"/p"}` {
		t.Fatalf("withdrawing the placement kept %s", golden.Canon(kept))
	}
	if record.PointerEntryFor(entry, "/other") != nil || record.PointerEntryFor(entry, "/p") == nil {
		t.Fatal("an entry answers only for its own path")
	}
}

// .crw-lock keeps Python's O_EXCL protocol: a second taker waits and reports Busy, the file is
// removed on release, and a file left by a dead process (older than 300 s) is taken over.
func TestCrwLockIsExclusiveAndExpiresOnlyWhenStale(t *testing.T) {
	target := filepath.Join(t.TempDir(), record.Name)
	first, err := record.Lock(target, 0)
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(target + record.LockSuffix); string(raw) != fmt.Sprint(os.Getpid()) {
		t.Fatalf("the lock file records %q, not this pid", raw)
	}
	started := time.Now()
	_, err = record.Lock(target, 150*time.Millisecond)
	var busy *record.Busy
	if !errors.As(err, &busy) || time.Since(started) < 150*time.Millisecond {
		t.Fatalf("a held lock was taken: %v", err)
	}
	first.Release()
	if _, err := os.Lstat(target + record.LockSuffix); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("release left the lock file")
	}
	if err := os.WriteFile(target+record.LockSuffix, []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	fresh := time.Now()
	if _, err := record.Lock(target, 100*time.Millisecond); !errors.As(err, &busy) {
		t.Fatal("a fresh lock file was treated as stale")
	}
	old := fresh.Add(-record.StaleLock - time.Minute)
	if err := os.Chtimes(target+record.LockSuffix, old, old); err != nil {
		t.Fatal(err)
	}
	taken, err := record.Lock(target, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("a stale lock file blocked for ever: %v", err)
	}
	taken.Release()
}

// The promotion lock is one flock on a file beside the record: a second promotion (another
// process included) is Busy, a probe sees it held, and the file outlives every holder.
func TestPromotionLockContentionAndTheFileIsNeverUnlinked(t *testing.T) {
	path := filepath.Join(t.TempDir(), record.Name)
	if state, _ := record.Probe(path + record.PromotionLockSuffix); state != record.NoFile {
		t.Fatalf("before any promotion: %s", state)
	}
	held, err := record.Promote(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	var busy *record.Busy
	if _, err := record.Promote(path, 120*time.Millisecond); !errors.As(err, &busy) {
		t.Fatalf("a second promotion started while one was held: %v", err)
	}
	if state, _ := record.Probe(path + record.PromotionLockSuffix); state != record.Held {
		t.Fatalf("a held promotion lock probed %s", state)
	}
	held.Release()
	if _, err := os.Lstat(path + record.PromotionLockSuffix); err != nil {
		t.Fatalf("releasing removed the lock file: %v", err)
	}
	if state, _ := record.Probe(path + record.PromotionLockSuffix); state != record.Free {
		t.Fatalf("a released lock probed %s", state)
	}
	_, release := golden.Spawn(t, "", path+record.PromotionLockSuffix)
	if state, _ := record.Probe(path + record.PromotionLockSuffix); state != record.Held {
		t.Fatalf("another process's lock probed %s", state)
	}
	if _, err := record.Promote(path, 120*time.Millisecond); !errors.As(err, &busy) {
		t.Fatalf("a promotion started while another process held the lock: %v", err)
	}
	release()
	again, err := record.Promote(path, time.Second)
	if err != nil {
		t.Fatalf("the lock was not released with its holder: %v", err)
	}
	again.Release()
}

func TestReleaseCandidateKeepsWhatMayBeInUse(t *testing.T) {
	root := t.TempDir()
	environment := filepath.Join(root, "env")
	other := filepath.Join(root, "other")
	for _, d := range []string{environment, other} {
		if err := os.MkdirAll(filepath.Join(d, "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(root, "state", record.Name)
	install := func(env string) record.Object {
		return record.Object{{Key: "location", Value: filepath.Join(env, "bin")}, {Key: "environment", Value: env}}
	}
	if _, err := record.Update(path, 1, record.Delta{Installs: []record.Named{{Component: "c", Entry: install(environment)}, {Component: "c", Entry: install(other)}},
		Select: []contract.Field{{Key: "c", Value: filepath.Join(other, "bin")}}}); err != nil {
		t.Fatal(err)
	}
	yes, no := true, false
	if _, why := record.ReleaseCandidate(path, 1, other, &no); !strings.HasPrefix(why, "kept: this environment is the selected one") {
		t.Fatalf("a selected environment: %s", why)
	}
	if _, why := record.ReleaseCandidate(path, 1, environment, &yes); !strings.HasPrefix(why, "kept: the owned pointer names") {
		t.Fatalf("a pointed-at environment: %s", why)
	}
	if _, why := record.ReleaseCandidate(path, 1, environment, nil); !strings.HasPrefix(why, "kept: whether the owned pointer") {
		t.Fatalf("an unread pointer: %s", why)
	}
	lock, err := record.Lock(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	saved := record.LockTimeout
	record.LockTimeout = 100 * time.Millisecond
	read, why := record.ReleaseCandidate(path, 1, environment, &no)
	record.LockTimeout = saved
	lock.Release()
	if read.State != reading.AccessError || !strings.HasPrefix(why, "kept: the host record lock could not be taken") {
		t.Fatalf("a busy lock: %s %s", read.State, why)
	}
	if _, why := record.ReleaseCandidate(path, 1, environment, &no); why != "dropped: the selection does not name this environment" {
		t.Fatalf("an unselected environment: %s", why)
	}
	_, component := record.Component(record.Load(path, 1).Value.(record.Object), "c")
	if installs := record.Get(component, "installs").([]any); len(installs) != 1 || record.Get(installs[0].(record.Object), "environment") != other {
		t.Fatalf("the wrong installs were dropped: %s", golden.Canon(installs))
	}
}

func TestUnderIsContainmentNotAStringPrefix(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"env/pkg", "env-other/pkg"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "env"), filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	if !record.Under(filepath.Join(root, "env", "pkg"), filepath.Join(root, "env")) || !record.Under(filepath.Join(root, "alias", "pkg"), filepath.Join(root, "env")) {
		t.Fatal("a location inside the environment, or through an alias of it, is under it")
	}
	if record.Under(filepath.Join(root, "env-other", "pkg"), filepath.Join(root, "env")) {
		t.Fatal("a sibling sharing the prefix is not under it")
	}
}

// The outgoing selection is replaced whole by the promotion that replaces a selection, and a
// restore with no outgoing to put back removes the key rather than writing null.
func TestOutgoingIsReplacedWholeAndRemovedWhenNothingWasThere(t *testing.T) {
	path := filepath.Join(t.TempDir(), record.Name)
	outgoing := record.Object{{Key: "codex-session-relay", Value: record.Object{{Key: "selected", Value: "/old/bin"}}}}
	if _, err := record.Update(path, 1, record.Delta{Outgoing: &record.Outgoing{Value: outgoing}}); err != nil {
		t.Fatal(err)
	}
	read := record.Load(path, 1)
	if got := record.Get(read.Value.(record.Object), "outgoing"); golden.Canon(got) != golden.Canon(outgoing) {
		t.Fatalf("outgoing: %s", golden.Canon(got))
	}
	if _, err := record.Update(path, 1, record.Delta{Outgoing: &record.Outgoing{}}); err != nil {
		t.Fatal(err)
	}
	if _, has := record.Lookup(record.Load(path, 1).Value.(record.Object), "outgoing"); has {
		t.Fatal("a restore to nothing left the key")
	}
}
