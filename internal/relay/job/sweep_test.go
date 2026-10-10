package job

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"testing/iotest"
	"time"
)

// CRW-1134: the remaining defects of the bg store, registry, spawn and hooks (docs/port-cxc/known-defects/CRW-1134.md), one case each.

func TestAtomicWriteRemovesItsOwnTemporaryFileWhenTheRenameFails(t *testing.T) { // :142
	cwd := workspace(t)
	dir := store(t, cwd)
	final := RecordPath(cwd, "isdir")
	mkdir(t, final)
	if err := atomicWrite(cwd, final, "text", 42, 7); err == nil {
		t.Fatal("the rename over a directory succeeded")
	}
	if got := names(t, dir); !slices.Equal(got, []string{"isdir.json"}) {
		t.Errorf("the failed write left %v", got)
	}
}

func TestReadExitCodeWantsOneWholeInteger(t *testing.T) { // :256
	for body, want := range map[string]ExitRead{
		"0": {"known", 0}, "  7\n": {"known", 7}, "+5": {"known", 5}, "-7": {"known", -7}, "-0": {"known", 0}, "127": {"known", 127},
		"99999999999999999999": {"known", 1e20},
		"1x":                   {"pending", 0}, "12.5": {"pending", 0}, "0x10": {"pending", 0}, "3 4": {"pending", 0}, "abc": {"pending", 0}, "": {"pending", 0},
		"-": {"pending", 0}, "+": {"pending", 0}, strings.Repeat("9", 400): {"pending", 0},
	} {
		ws := workspace(t)
		put(t, ExitPath(ws, "e"), body)
		if got := readExitCode(ws, "e"); got != want {
			t.Errorf("exit body %.12q: %+v, want %+v", body, got, want)
		}
	}
}

func TestReconcileSettlesAJobWhoseStartCannotBeRead(t *testing.T) { // :257
	ws := workspace(t)
	r := mk(ws, "j")
	r.StartedAt = "junk"
	save(t, ws, r)
	got, err := Reconcile(ws, r, noonClock)
	if err != nil || got.Status != StatusFailed || got.ExitCode != nil || got.EndedAt == nil {
		t.Errorf("a job with no pid, no exit file and an unreadable start: %+v %v", got, err)
	}
	if rows := ledger(t, ws); len(rows) != 1 || !strings.Contains(rows[0], `"detail":"startedAt unreadable"`) {
		t.Errorf("ledger %q", rows)
	}
}

func TestBrokenRecordsAreKeptAndReportedNotWorkedOn(t *testing.T) { // :258
	ws := workspace(t)
	good := finished(ws, "good", "2026-09-09T00:05:00.000Z")
	save(t, ws, good)
	text := get(t, RecordPath(ws, "good"))
	broken := map[string]string{
		"nodelivered": strings.Replace(strings.Replace(text, `"id": "good"`, `"id": "nodelivered"`, 1), ",\n  \"deliveredAt\": null", "", 1),
		"halfpid":     strings.Replace(strings.Replace(text, `"id": "good"`, `"id": "halfpid"`, 1), `"pid": null`, `"pid": 12.5`, 1),
		"badstatus":   strings.Replace(strings.Replace(text, `"id": "good"`, `"id": "badstatus"`, 1), `"status": "complete"`, `"status": "weird"`, 1),
		"badcommand":  strings.Replace(strings.Replace(text, `"id": "good"`, `"id": "badcommand"`, 1), `"sleep",`, `7,`, 1),
		"notjson":     "{ not json",
	}
	for id, body := range broken {
		put(t, RecordPath(ws, id), body)
	}
	for id := range broken {
		if _, ok := ReadRecord(ws, id); ok {
			t.Errorf("%s reads as a record", id)
		}
	}
	p := HookPayload{SessionID: "S1", Cwd: ws}
	out := HandleStop(p, ws, hookEnv(nil), noon) + HandleSessionStart(HookPayload{SessionID: "S2", Cwd: ws}, ws, hookEnv(nil), noon)
	list := cliResult(t, ws, "list").Out.(string)
	for id, body := range broken {
		if strings.Contains(out, "- "+id+" (") || get(t, RecordPath(ws, id)) != body {
			t.Errorf("%s was worked on: woken %v, bytes kept %v", id, strings.Contains(out, id), get(t, RecordPath(ws, id)) == body)
		}
		if !strings.Contains(list, id) || !strings.Contains(list, "손상") {
			t.Errorf("list does not report %s:\n%s", id, list)
		}
		for _, verb := range []string{"get", "cancel"} {
			if got := cliResult(t, ws, verb, id); got.Code != 1 || !strings.Contains(got.Out.(string), "손상") {
				t.Errorf("%s %s: %+v", verb, id, got)
			}
		}
	}
	if !strings.Contains(out, "- good (") {
		t.Errorf("the good record did not wake: %q", out)
	}
}

func TestAnUnreadableOffSwitchKeepsTheWakeOff(t *testing.T) { // :262
	ws := workspace(t)
	hookDone(t, ws, "done", "S1")
	mkdir(t, DisabledPath(ws)) // a directory: the switch is there but does not read
	st := ReadDisabledState(ws)
	if !st.Disabled || !WakeSuppressed(ws, hookEnv(nil)) {
		t.Errorf("an unreadable off switch reads as on: %+v", st)
	}
	if out := HandleStop(HookPayload{SessionID: "S1", Cwd: ws}, ws, hookEnv(nil), noon); out != "" {
		t.Errorf("the wake ran against an off switch that does not read: %q", out)
	}
	if got := cliResult(t, ws, "status").Out.(string); !strings.Contains(got, "wake: OFF") || !strings.Contains(got, "읽을 수 없음") {
		t.Errorf("status: %q", got)
	}
	if r, err := RunCLI([]string{"on"}, ws, os.LookupEnv, time.Now); err == nil && r.Code == 0 {
		t.Errorf("on reported success while the off switch stays: %+v", r)
	}
}

func TestRunBackgroundRefusesAnEmptyCommand(t *testing.T) { // :306
	ws := workspace(t)
	starts := 0
	_, err := runBackground(ws, RunOptions{Command: nil}, time.Now, func(*exec.Cmd) error { starts++; return nil })
	if err == nil || starts != 0 || len(ListRecordIDs(ws)) != 0 {
		t.Errorf("an empty command: %v, started %d times, records %v", err, starts, ListRecordIDs(ws))
	}
}

func TestRunBackgroundReturnsTheFailedRecordOfAShellThatCouldNotStart(t *testing.T) { // :307
	ws := workspace(t)
	rec, err := runBackground(ws, RunOptions{Command: []string{"true"}}, time.Now, func(*exec.Cmd) error {
		return &os.PathError{Op: "fork/exec", Path: "/bin/sh", Err: syscall.ENOENT}
	})
	if onDisk, _ := ReadRecord(ws, rec.ID); err != nil || rec.Status != StatusFailed || onDisk.Status != StatusFailed {
		t.Errorf("returned %+v %v, on disk %+v", rec, err, onDisk)
	}
}

func mode(t *testing.T, path string) fs.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func TestTheBgStoreIsPrivateFromItsFirstWrite(t *testing.T) { // :314, CRW-878.md:33
	old := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(old) })
	ws := workspace(t)
	rec := start(t, ws, RunOptions{Command: []string{"sh", "-c", "echo secret-argument"}})
	settled(t, ws, rec.ID)
	if m := mode(t, BGDir(ws)); m != 0o700 {
		t.Errorf("bg directory %v", m)
	}
	for _, path := range []string{RecordPath(ws, rec.ID), filepath.Join(BGDir(ws), LedgerFile)} {
		if m := mode(t, path); m != 0o600 {
			t.Errorf("%s %v", filepath.Base(path), m)
		}
	}
	if err := AtomicWrite(ws, EnabledAtPath(ws), "x\n"); err != nil || mode(t, EnabledAtPath(ws)) != 0o600 {
		t.Errorf("enabled-at %v %v", mode(t, EnabledAtPath(ws)), err)
	}
	// A bg directory that exists already is not changed; what is written into it is private all the same.
	other := workspace(t)
	mkdir(t, BGDir(other))
	if err := os.Chmod(BGDir(other), 0o755); err != nil {
		t.Fatal(err)
	}
	save(t, other, mk(other, "r"))
	if mode(t, BGDir(other)) != 0o755 || mode(t, RecordPath(other, "r")) != 0o600 {
		t.Errorf("existing directory %v, record %v", mode(t, BGDir(other)), mode(t, RecordPath(other, "r")))
	}
}

func TestReconcileReadsTheExitFileAgainBeforeItCallsAJobVanished(t *testing.T) { // :316
	ws := workspace(t)
	r := mk(ws, "v")
	r.PID, r.StartToken = ip(deadPID(t)), sp("TOKEN")
	save(t, ws, r)
	bin := t.TempDir() // a ps that answers another process and, meanwhile, the job's shell publishes its exit code
	put(t, filepath.Join(bin, "ps"), "#!/bin/sh\nprintf 0 > "+shellQuotePosix(ExitPath(ws, "v"))+"\necho OTHER\n")
	if err := os.Chmod(filepath.Join(bin, "ps"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	got, err := Reconcile(ws, r, noonClock)
	if err != nil || got.Status != StatusComplete || got.ExitCode == nil || *got.ExitCode != 0 {
		t.Errorf("an exit code published while the pid was judged: %+v %v", got, err)
	}
}

func TestBgRunHookDoesNothingWithUnreadableOrOversizedInput(t *testing.T) { // :374
	lookup := func(k string) (string, bool) { return "S2", k == "CODEX_THREAD_ID" }
	for name, in := range map[string]func() any{
		"read error": func() any { return iotest.ErrReader(errors.New("read")) },
		"over 1Mi":   func() any { return strings.NewReader(strings.Repeat(" ", 1<<20) + `{"session_id":"S2"}`) },
	} {
		for _, event := range []string{"stop", "session-start"} {
			ws := workspace(t)
			hookDone(t, ws, "mine", "S2")
			hookDone(t, ws, "orphan", "GONE")
			var out strings.Builder
			if code := RunHook(context.Background(), event, in().(interface{ Read([]byte) (int, error) }), &out, lookup, ws, noon); code != 0 || out.Len() != 0 {
				t.Errorf("%s %s: code %d, output %q", name, event, code, out.String())
			}
			mine, _ := ReadRecord(ws, "mine")
			orphan, _ := ReadRecord(ws, "orphan")
			if mine.DeliveredAt != nil || orphan.AdoptedBy != nil {
				t.Errorf("%s %s: the registry changed: %+v %+v", name, event, mine, orphan)
			}
		}
	}
}

func TestCLIGetCountsLinesNotTheirTerminator(t *testing.T) { // :383
	ws := workspace(t)
	save(t, ws, finished(ws, "one", "2026-09-09T00:01:00.000Z"))
	head := DescribeRecord(finished(ws, "one", "2026-09-09T00:01:00.000Z")) + "\n\n"
	for _, c := range []struct{ out, tail, want string }{
		{"a\nb\nc\n", "2", "b\nc"}, {"a\nb\nc\n", "", "a\nb\nc"}, {"a\nb\nc", "2", "b\nc"},
		{"x  \n  y  \n", "2", "x  \n  y  "}, {"a\n\n", "1", ""}, {"\n", "5", ""},
	} {
		put(t, OutPath(ws, "one"), c.out)
		argv := []string{"get", "one"}
		if c.tail != "" {
			argv = append(argv, "--tail", c.tail)
		}
		if got := cliResult(t, ws, argv...).Out.(string); got != head+c.want && !(c.want == "" && got == strings.TrimSuffix(head, "\n\n")) {
			t.Errorf("output %q, tail %q: %q, want %q", c.out, c.tail, got, head+c.want)
		}
	}
}
