package hook

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
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
	inputs := []any{row}
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
		inputs = append(inputs, copy)
	}
	script := `import json,sys;sys.path.insert(0,sys.argv[1]);from crw_runtime import completion;print(json.dumps([completion._row_shape(v) for v in json.load(sys.stdin)]))`
	cmd := exec.Command(python(t), "-c", script, filepath.Join(testRoot, "scripts"))
	cmd.Stdin = strings.NewReader(evidence.Dumps(inputs, false, false, true))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v %s", err, out)
	}
	var answers []bool
	if err = json.Unmarshal(out, &answers); err != nil {
		t.Fatal(err)
	}
	if len(answers) != len(inputs) {
		t.Fatal(answers)
	}
	for i, answer := range answers {
		if answer != (i == 0) {
			t.Fatalf("Python/Go mismatch %d: %s", i, out)
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
func Test33NativeJournalReaderPythonLive(t *testing.T) {
	cmd := exec.Command(python(t), "-m", "pytest", "-q", "testdata/test_journal_reader.py")
	cmd.Env = append(os.Environ(), "CRW_TEST_BINARY="+binary(t))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("live reader parity %v\n%s", err, out)
	}
	t.Logf("%s", out)
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
