package job

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"testing/iotest"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

func cliResult(t *testing.T, ws string, argv ...string) CLIResult {
	t.Helper()
	r, err := RunCLI(argv, ws, os.LookupEnv, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCLIStdinAndPayload(t *testing.T) {
	for _, s := range []string{"{}", strings.Repeat("é", MaxCLIStdinBytes), strings.Repeat("😀", MaxCLIStdinBytes/2)} {
		if got := ReadCLIStdin(strings.NewReader(s)); got != s {
			t.Errorf("stdin of %d bytes lost", len(s))
		}
	}
	for _, in := range []io.Reader{strings.NewReader(strings.Repeat("a", MaxCLIStdinBytes+1)), iotest.ErrReader(errors.New("read"))} {
		if got := ReadCLIStdin(in); got != "" {
			t.Errorf("refused input: %q", got)
		}
	}
	for _, s := range []string{"null", "false", "3", "broken", "{} trailing"} {
		if got := ParseCLIPayload(s); !reflect.DeepEqual(got, map[string]any{}) {
			t.Errorf("%q: %v", s, got)
		}
	}
	if got := ParseCLIPayload(`{"session_id":"S1"}`); !reflect.DeepEqual(got, map[string]any{"session_id": "S1"}) {
		t.Errorf("object: %v", got)
	}
	if got := ParseCLIPayload(`[]`); !reflect.DeepEqual(got, []any{}) {
		t.Errorf("array: %v", got)
	}
}

func TestCLIMissingAndUsage(t *testing.T) {
	ws := t.TempDir()
	for _, c := range []struct {
		args []string
		code int
		out  string
	}{
		{[]string{"cancel", "ghost"}, 0, "없는 id: ghost"},
		{[]string{"get", "ghost"}, 1, "없는 id: ghost"},
		{[]string{"list"}, 0, "백그라운드 작업 없음"},
		{[]string{"drain", "--session", "S1"}, 0, "미전달 완료 없음"},
	} {
		r := cliResult(t, ws, c.args...)
		if r.Code != c.code || r.Out != c.out {
			t.Errorf("%v: %+v", c.args, r)
		}
	}
	for _, args := range [][]string{{"run", "echo"}, {"run", "--"}, {"drain"}, {"unknown"}, {}} {
		r := cliResult(t, ws, args...)
		if s, _ := r.Out.(string); !strings.HasPrefix(s, "crw relay job run") {
			t.Errorf("usage: %+v", r)
		}
	}
}

func TestCLIListGetAndFinishedCancel(t *testing.T) {
	ws := t.TempDir()
	r := save(t, ws, finished(ws, "one", "2026-09-09T00:01:00.000Z"))
	if err := os.WriteFile(OutPath(ws, "one"), []byte("a\nb\nc\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ tail, suffix string }{{"2", "\nc"}, {"0", "sleep 1"}, {"3x", "\nb\nc"}, {"nope", "sleep 1"}, {"-2", "sleep 1"}, {"999999999999999999999", "\na\nb\nc"}} {
		got := cliResult(t, ws, "get", "one", "--tail", c.tail).Out.(string)
		if !strings.HasSuffix(got, c.suffix) {
			t.Errorf("tail %q: %q", c.tail, got)
		}
	}
	if got := cliResult(t, ws, "list").Out.(string); !strings.HasSuffix(got, "[미전달]") {
		t.Errorf("list: %q", got)
	}
	items, ok := cliResult(t, ws, "list", "--json").Out.([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("JSON list: %v", items)
	}
	o := items[0].(pyjson.Object)
	if o.Get("id") != "one" || o.Get("deliveredAt") != nil {
		t.Errorf("record: %v", o)
	}
	if got := cliResult(t, ws, "cancel", "one").Out; got != "one complete" {
		t.Errorf("cancel: %v", got)
	}
	if next, _ := ReadRecord(ws, "one"); next.Status != r.Status {
		t.Errorf("finished outcome changed")
	}
}

func TestCLIWakeSwitchAndDrain(t *testing.T) {
	t.Setenv(EnvVar, "")
	ws := t.TempDir()
	save(t, ws, finished(ws, "one", "2026-09-09T00:01:00.000Z"))
	if got := cliResult(t, ws, "on").Out; got != "bg wake 이미 ON" {
		t.Errorf("on: %v", got)
	}
	if _, err := os.Stat(EnabledAtPath(ws)); !os.IsNotExist(err) {
		t.Errorf("redundant on moved gate")
	}
	cliResult(t, ws, "off")
	if !ReadDisabledState(ws).Disabled {
		t.Fatal("off did not persist")
	}
	d := cliResult(t, ws, "drain", "--session", "S1", "--json").Out.(pyjson.Object)
	if d.Get("delivered") != true || !strings.Contains(d.Get("text").(string), "one") {
		t.Errorf("drain: %v", d)
	}
	if r, _ := ReadRecord(ws, "one"); r.DeliveredAt == nil {
		t.Error("drain did not stamp")
	}
	cliResult(t, ws, "on")
	if ReadDisabledState(ws).Disabled {
		t.Error("on still disabled")
	}
	save(t, ws, finished(ws, "early", "2026-09-09T00:01:00.000Z"))
	if got := cliResult(t, ws, "drain", "--session", "S1").Out; got != "미전달 완료 없음" {
		t.Errorf("on gate: %v", got)
	}
	t.Setenv(EnvVar, "off")
	if got := cliResult(t, ws, "status").Out.(string); !strings.Contains(got, "wake: OFF") || !strings.Contains(got, "CRW_BGWAKE: off") {
		t.Errorf("status: %q", got)
	}
}

func TestCLIRunDetached(t *testing.T) {
	t.Setenv("CODEX_THREAD_ID", "S1")
	for _, command := range [][]string{{"sh", "-c", "echo hello; exit 3"}, {"printf", "%s", "it's a test"}, {"exit", "7"}, {"definitely-not-a-real-binary-xyz"}} {
		t.Run(strings.Join(command, " "), func(t *testing.T) {
			ws := t.TempDir()
			result := cliResult(t, ws, append([]string{"run", "--note", "smoke", "--"}, command...)...)
			id, _ := result.Out.(string)
			rec, found := ReadRecord(ws, id)
			if found && rec.PID != nil {
				pid, token := *rec.PID, rec.StartToken
				t.Cleanup(func() {
					if PidAlive(pid) && (token == nil || startsAt(pid, *token)) {
						_ = syscall.Kill(-pid, syscall.SIGKILL)
					}
					until(t, "owned PID termination", func() bool { return !PidAlive(pid) })
				})
			}
			if !found || result.Code != 0 {
				t.Fatalf("start: %+v", result)
			}
			done := settled(t, ws, id)
			if done.SessionID == nil || *done.SessionID != "S1" || done.Note == nil || *done.Note != "smoke" {
				t.Errorf("owner/note lost")
			}
			if command[0] == "sh" && (done.ExitCode == nil || *done.ExitCode != 3) {
				t.Errorf("exit: %+v", done)
			}
			if command[0] == "exit" && (done.ExitCode == nil || *done.ExitCode != 7) {
				t.Errorf("subshell exit: %+v", done)
			}
			if command[0] == "printf" && !strings.Contains(cliResult(t, ws, "get", id).Out.(string), "it's a test") {
				t.Error("quoting lost")
			}
		})
	}
}

func TestCLIDrainBatchAndOtherSession(t *testing.T) {
	ws := t.TempDir()
	for _, id := range []string{"a", "b", "c", "d", "e", "f"} {
		save(t, ws, finished(ws, id, "2026-09-09T00:01:00.000Z"))
	}
	other := finished(ws, "other", "2026-09-09T00:00:01.000Z")
	other.SessionID = sp("S2")
	save(t, ws, other)
	cliResult(t, ws, "drain", "--session", "S1")
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		if r, _ := ReadRecord(ws, id); r.DeliveredAt == nil {
			t.Errorf("unstamped %s", id)
		}
	}
	for _, id := range []string{"f", "other"} {
		if r, _ := ReadRecord(ws, id); r.DeliveredAt != nil {
			t.Errorf("unexpected stamp %s", id)
		}
	}
	if got := cliResult(t, ws, "drain", "--session", "S1").Out.(string); !strings.Contains(got, "- f ") {
		t.Errorf("remaining batch: %s", got)
	}
}

func TestCLIRecordExtrasAndFault(t *testing.T) {
	ws := t.TempDir()
	r := finished(ws, "x", "2026-09-09T00:01:00.000Z")
	r.Extra = []Member{{"extra", "kept"}}
	save(t, ws, r)
	items := cliResult(t, ws, "list", "--json").Out.([]any)
	if items[0].(pyjson.Object).Get("extra") != "kept" {
		t.Fatal("unknown record member lost")
	}
	if err := os.Mkdir(DisabledPath(ws), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := RunCLI([]string{"off"}, ws, os.LookupEnv, time.Now); err == nil {
		t.Fatal("write fault suppressed")
	}
	if got := cliResult(t, ws, "removal").Out; got != RemovalText() {
		t.Fatal("removal delegate differs")
	}
}

func TestCLIOnRetainsTheOffFlagWhenGateWriteFails(t *testing.T) {
	ws := t.TempDir()
	cliResult(t, ws, "off")
	if err := os.Mkdir(EnabledAtPath(ws), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := RunCLI([]string{"on"}, ws, os.LookupEnv, time.Now); err == nil {
		t.Fatal("gate write unexpectedly succeeded")
	}
	if !ReadDisabledState(ws).Disabled {
		t.Fatal("failed on lost the disabled state file")
	}
}

func TestCLIJSONPreservesUnknownSurrogate(t *testing.T) {
	ws := t.TempDir()
	r := finished(ws, "surrogate", "2026-09-09T00:01:00.000Z")
	r.Extra = []Member{{"extra", json.RawMessage(`"\ud800"`)}}
	save(t, ws, r)
	items := cliResult(t, ws, "list", "--json").Out.([]any)
	b, err := pyjson.Encode(items[0], pyjson.Options{})
	if err != nil || !strings.Contains(string(b), `"extra": "\ud800"`) {
		t.Fatalf("surrogate lost: %s %v", b, err)
	}
}
