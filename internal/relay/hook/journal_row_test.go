package hook

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func nativeJournalFixture(t *testing.T) (string, Object) {
	t.Helper()
	home := hookHome(t, 5)
	out, err := hookCommand(t, home, `{"session_id":"s","turn_id":"t","stop_hook_active":false}`).CombinedOutput()
	if err != nil || len(out) != 0 {
		t.Fatalf("hook %v %s", err, out)
	}
	paths, err := filepath.Glob(filepath.Join(home, "journal", "*", "*.json"))
	if err != nil || len(paths) != 1 {
		t.Fatal(paths, err)
	}
	raw, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	row, err := decodeObject(raw)
	if err != nil {
		t.Fatal(err)
	}
	return paths[0], row
}
func Test33NativeJournalReader(t *testing.T) {
	path, row := nativeJournalFixture(t)
	if !NativePrescanUnreachable(row) {
		t.Fatal(row)
	}
	if _, ok := ReadNativePrescanRow(path); !ok {
		t.Fatal("native row rejected")
	}
	type mutation struct {
		key    string
		value  any
		remove bool
	}
	cases := []mutation{
		{"identityScanMs", int64(0), false}, {"adapterOutcome", "guard_timed_out", false}, {"errno", nil, true}, {"errno", "ETIMEDOUT", false}, {"errno", "EPERM", false}, {"held", true, false}, {"guardInvoked", false, false}, {"processEnding", "timed_out", false}, {"eventKey", strings.Repeat("a", 64), false}, {"eventIdentity", Object{}, false}, {"acceptedAs", "accepted/x.json", false}, {"guardStderr", "unexpected", false}, {"exitCode", int64(0), false}, {"signal", int64(9), false}, {"guardDecision", "release", false}, {"guardState", "unmanaged", false}, {"assignmentId", "a", false}, {"guardRecordedAs", "hook/s/t/0", false}, {"guardMode", nil, false}, {"sessionId", nil, true}, {"stopHookActive", nil, true}, {"recordVersion", true, false}, {"elapsedMs", true, false}, {"guardElapsedMs", int64(-1), false}, {"at", "2026-99-99T00:00:00Z", false}, {"configuration", "relative", false}, {"detail", "unreachable", false}, {"fault", "extra", false}, {"counters", Object{}, false}, {"observation", "unmanaged", false},
	}
	for _, test := range cases {
		copy := append(Object{}, row...)
		if test.remove {
			out := Object{}
			for _, f := range copy {
				if f.Key != test.key {
					out = append(out, f)
				}
			}
			copy = out
		} else {
			copy = set(copy, test.key, test.value)
		}
		if NativePrescanUnreachable(copy) {
			t.Fatalf("neighbour admitted: %s=%v", test.key, test.value)
		}
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	writeTest(t, path, bytes.TrimSpace(original))
	if _, ok := ReadNativePrescanRow(path); ok {
		t.Fatal("noncanonical bytes admitted")
	}
	writeTest(t, path, original)
	link := filepath.Join(t.TempDir(), "link")
	if err = os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadNativePrescanRow(link); ok {
		t.Fatal("symlink admitted")
	}
}
func Test33NativeJournalDetailErrnos(t *testing.T) {
	_, row := nativeJournalFixture(t)
	for _, tc := range []struct{ name, prefix string }{{"ENOENT", "[Errno 2] No such file or directory"}, {"EACCES", "[Errno 13] Permission denied"}} {
		for _, path := range []string{"/tmp/control.sock", "/tmp/quote'and\"/control.sock", "/tmp/new\nline/control.sock", "/tmp/한글/control.sock"} {
			copy := set(append(Object{}, row...), "errno", tc.name)
			copy = set(copy, "detail", "the configured runtime could not be run: "+tc.prefix+": "+evidence.StrRepr(path))
			if !NativePrescanUnreachable(copy) {
				t.Fatalf("detail rejected: %v", get(copy, "detail"))
			}
		}
	}
}

// A socket or settings path whose name holds a byte that is not UTF-8 is spelled as Python holds
// it, the lone surrogate surrogateescape makes of the byte, and such a path is one the system
// takes: the native pre-scan row a relay state directory like that leaves is still the exempt
// one. A surrogate that stands for no byte is not a path. (The Python reader,
// completion._native_prescan_unreachable, judged every row as want says until todo 44.)
func Test33NativeJournalPathsWhoseNamesAreNotUTF8(t *testing.T) {
	_, row := nativeJournalFixture(t)
	prefix := "the configured runtime could not be run: [Errno 2] No such file or directory: "
	socket := func(spelled string) Object {
		return set(set(append(Object{}, row...), "errno", "ENOENT"), "detail", prefix+spelled)
	}
	configuration := func(path string) Object { return set(append(Object{}, row...), "configuration", path) }
	cases := []struct {
		name string
		row  Object
		want bool
	}{
		{"a state directory holding 0xff", socket(store.PathRepr("/tmp/st\xffate/control.sock")), true},
		{"a state directory holding the bytes ED A0 80", socket(store.PathRepr("/tmp/st\xed\xa0\x80ate/control.sock")), true},
		{"a surrogate that is no byte", socket(`'/tmp/st\ud800ate/control.sock'`), false},
		{"a surrogate pair spelled as two escapes", socket(`'/tmp/\ud83d\ude00/control.sock'`), false},
		{"a surrogate spelled as repr() never writes it", socket(`'/tmp/st\U0000dcffate/control.sock'`), false},
		{"settings under a Codex home holding 0xff", configuration("/tmp/c\xed\xb3\xbf/" + ConfigName), true},
		{"settings under a surrogate that is no byte", configuration("/tmp/c\xed\xa0\x80/" + ConfigName), false},
	}
	for _, c := range cases {
		if got := NativePrescanUnreachable(c.row); got != c.want {
			t.Errorf("%s: %v, want %v (%v)", c.name, got, c.want, get(c.row, "detail"))
		}
	}
}

// ReadNativePrescanRow reads one journal file the way completion._read_record did: a regular
// file, reached without a symlink, holding exactly the bytes RecordBytes writes for a
// native-prescan row. No product path reads journal rows back (crw-dev stop-events has its own
// reader); the tests read what the hook wrote through it.
func ReadNativePrescanRow(path string) (Object, bool) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, false
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxInputBytes+1))
	if err != nil || len(raw) > maxInputBytes {
		return nil, false
	}
	row, err := decodeObject(raw)
	if err != nil || !NativePrescanUnreachable(row) {
		return nil, false
	}
	if !bytes.Equal(raw, RecordBytes(row)) {
		return nil, false
	}
	return row, true
}
