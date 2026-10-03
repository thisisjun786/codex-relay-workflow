package job

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// Expectations marked "oracle" are what Node v24 printed running the unmodified CXC v0.2.40 bg-wake/src/registry.ts in a scratch
// workspace. The process cases use only a child this test starts, signals and waits for by its own pid.

func sp(s string) *string               { return &s }
func ip(n int) *int                     { return &n }
func fp(f float64) *float64             { return &f }
func dp(d time.Duration) *time.Duration { return &d }

// noon is the clock of the cases, ten minutes after the startedAt of every record; ticking adds a millisecond to each reading, as the
// oracle's recorder clock does.
func noon() time.Time { return time.Date(2026, 9, 9, 0, 10, 0, 0, time.UTC) }
func ticking() func() time.Time {
	t := noon().Add(-time.Millisecond)
	return func() time.Time { t = t.Add(time.Millisecond); return t }
}

func mk(ws, id string) BgRecord {
	return BgRecord{ID: id, SessionID: sp("S1"), Cwd: ws, Command: []string{"sleep", "1"}, Status: StatusRunning, StartedAt: "2026-09-09T00:00:00.000Z"}
}

func save(t *testing.T, ws string, r BgRecord) BgRecord {
	t.Helper()
	store(t, ws)
	if err := WriteRecord(ws, r); err != nil {
		t.Fatal(err)
	}
	return r
}

func finished(ws, id, end string) BgRecord {
	r := mk(ws, id)
	r.Status, r.ExitCode, r.EndedAt = StatusComplete, fp(0), sp(end)
	return r
}

// child starts a process of its own, which stop ends and reaps; its pid is then dead.
func child(t *testing.T) *exec.Cmd {
	t.Helper()
	c := exec.Command("sleep", "60")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stop(c) })
	return c
}

func stop(c *exec.Cmd) { _ = c.Process.Kill(); _ = c.Wait() }

func deadPID(t *testing.T) int {
	t.Helper()
	c := exec.Command("sh", "-c", "exit 0")
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
	return c.Process.Pid
}

func TestWriteRecordIsJSONStringifyWithTwoSpaces(t *testing.T) {
	ws := workspace(t)
	r := mk("<WS>", "w1")
	r.SessionID, r.Command, r.Note, r.PID = nil, []string{"sh", "-c", "echo \"é\" <&>\u2028"}, sp("n\n"), ip(42)
	r.StartToken, r.Status, r.ExitCode, r.EndedAt = sp("Thu Jan  1 00:00:00 2026"), StatusFailed, fp(-3), sp("2026-09-09T00:00:01.000Z")
	save(t, ws, r)
	want := "{\n  \"id\": \"w1\",\n  \"sessionId\": null,\n  \"adoptedBy\": null,\n  \"cwd\": \"<WS>\",\n  \"command\": [\n    \"sh\",\n    \"-c\",\n    \"echo \\\"é\\\" <&>\u2028\"\n  ],\n" + // oracle
		"  \"note\": \"n\\n\",\n  \"pid\": 42,\n  \"startToken\": \"Thu Jan  1 00:00:00 2026\",\n  \"status\": \"failed\",\n  \"exitCode\": -3,\n" +
		"  \"startedAt\": \"2026-09-09T00:00:00.000Z\",\n  \"endedAt\": \"2026-09-09T00:00:01.000Z\",\n  \"deliveredAt\": null\n}\n"
	if got := get(t, RecordPath(ws, "w1")); got != want {
		t.Errorf("record file:\n%s\nwant:\n%s", got, want)
	}
	if back, ok := ReadRecord(ws, "w1"); !ok || !reflect.DeepEqual(back, r) {
		t.Errorf("round trip: %+v %v", back, ok)
	}
	r.Command = nil
	save(t, ws, r)
	if !strings.Contains(get(t, RecordPath(ws, "w1")), "\"command\": [],") {
		t.Error("no command is the array []")
	}
}

func TestReadRecordKeepsTheOraclesFourTestsAndTheKeysItDoesNotName(t *testing.T) {
	const head = "{\"cwd\":\"x\",\"command\":[],\"status\":\"s\","
	for name, c := range map[string]struct {
		text string
		ok   bool
	}{ // oracle: only the minimal record is listed
		"junk": {"nope", false}, "empty object": {"{}", false}, "array": {"[1]", false}, "no command": {"{\"id\":\"n\",\"cwd\":\"x\",\"status\":\"running\"}", false},
		"upper-case key": {"{\"ID\":\"u\",\"cwd\":\"x\",\"command\":[],\"status\":\"s\"}", false}, "null id": {head + "\"id\":null}", false}, "minimal": {head + "\"id\":\"m\"}", true},
		"another record's id": {head + "\"id\":\"b\"}", false},
	} {
		ws := workspace(t)
		put(t, RecordPath(ws, "m"), c.text)
		if r, ok := ReadRecord(ws, "m"); ok != c.ok || ok && r.Extra != nil {
			t.Errorf("%s: %+v %v", name, r, ok)
		}
	}
	// A key the port does not name is kept, sorted, and written back after the thirteen; a named key of another type reads as null and
	// is written as null, so the operation that changes a named key is never overridden.
	ws := workspace(t)
	put(t, RecordPath(ws, "x"), "{\"zzz\":{\"a\": 1},\"id\":\"x\",\"cwd\":\"c\",\"command\":[],\"status\":\"running\",\"pid\":12.5,\"exitCode\":\"3\",\"aaa\":[1, 2],\"u\":\"\\ud800\"}")
	put(t, ExitPath(ws, "x"), "0")
	r, _ := ReadRecord(ws, "x")
	if got, err := Reconcile(ws, r, noonClock); err != nil || got.Status != StatusComplete || *got.ExitCode != 0 || len(got.Extra) != 3 {
		t.Fatalf("%+v %v", got, err)
	}
	back, _ := ReadRecord(ws, "x")
	if text := get(t, RecordPath(ws, "x")); back.Status != StatusComplete || *back.ExitCode != 0 || back.PID != nil || !strings.Contains(text, "  \"pid\": null,\n") ||
		!strings.HasSuffix(text, "\"deliveredAt\": null,\n  \"aaa\": [\n    1,\n    2\n  ],\n  \"u\": \"\\ud800\",\n  \"zzz\": {\n    \"a\": 1\n  }\n}\n") {
		t.Errorf("record on disk:\n%s", text)
	}
}

func noonClock() time.Time { return noon() }

func TestReconcileTakesTheExitFileAsTheAnswer(t *testing.T) {
	pid := deadPID(t)
	// oracle: the same bodies, now 2026-09-09T00:10:00.000Z
	for body, want := range map[string]struct {
		status string
		code   *float64
	}{"0": {"complete", fp(0)}, "1": {"failed", fp(1)}, "1x": {"failed", fp(1)}, "  7\n": {"failed", fp(7)}, "-0": {"complete", fp(0)}, "+5": {"failed", fp(5)}, "-7": {"failed", fp(-7)},
		"0x10": {"complete", fp(0)}, "12.5": {"failed", fp(12)}, "\ufeff3": {"failed", fp(3)}, "3 4": {"failed", fp(3)}, "99999999999999999999": {"failed", fp(1e20)},
		"abc": {"running", nil}, "": {"running", nil}, "-": {"running", nil}, strings.Repeat("9", 400): {"running", nil}} {
		ws := workspace(t)
		r := mk(ws, "e")
		r.PID = &pid
		save(t, ws, r)
		put(t, ExitPath(ws, "e"), body)
		got, err := Reconcile(ws, r, noonClock)
		onDisk, _ := ReadRecord(ws, "e")
		if err != nil || string(got.Status) != want.status || !reflect.DeepEqual(got.ExitCode, want.code) || !reflect.DeepEqual(onDisk, got) ||
			want.code != nil && *got.EndedAt != "2026-09-09T00:10:00.000Z" {
			t.Errorf("exit body %.12q: %+v %v, want %s %v", body, got, err, want.status, want.code)
		}
	}
	ws := workspace(t)
	r := save(t, ws, mk(ws, "e"))
	put(t, ExitPath(ws, "e"), "2")
	if _, err := Reconcile(ws, r, noonClock); err != nil || !strings.HasSuffix(ledger(t, ws)[0], "\"event\":\"completed\",\"id\":\"e\",\"exitCode\":2}") {
		t.Errorf("ledger: %v", err)
	}
	for _, status := range []BgStatus{StatusComplete, StatusCancelled} { // only a running record is looked at
		done := mk(ws, "d")
		done.Status, done.EndedAt = status, sp("2026-09-09T00:00:09.000Z")
		if got, err := Reconcile(ws, done, noonClock); err != nil || !reflect.DeepEqual(got, done) {
			t.Errorf("%s record changed: %+v %v", status, got, err)
		}
	}
	r.EndedAt = sp("2026-09-09T00:00:05.000Z") // an endedAt already there is kept
	if got, _ := Reconcile(ws, r, noonClock); *got.EndedAt != "2026-09-09T00:00:05.000Z" {
		t.Errorf("endedAt %v", *got.EndedAt)
	}
}

func TestReconcileJudgesAJobWithoutAnExitFile(t *testing.T) {
	live, gone := child(t).Process.Pid, deadPID(t)
	const s = time.Second
	for _, c := range []struct {
		name, started string
		exit          *time.Duration // age of an empty exit file; nil is no exit file
		pid           *int
		clock         func() time.Time
		want          BgStatus
	}{
		{"pid null, fresh", "2026-09-09T00:09:55.000Z", nil, nil, noonClock, StatusRunning},
		{"pid null, old (oracle)", "", nil, nil, noonClock, StatusFailed},
		{"pid null, startedAt unreadable stays running", "junk", nil, nil, noonClock, StatusRunning},
		{"pid dead", "", nil, &gone, noonClock, StatusFailed},
		{"pid alive", "", nil, &live, noonClock, StatusRunning},
		{"empty exit file, fresh, pid dead", "", dp(5 * s), &gone, noonClock, StatusRunning},
		{"empty exit file, stale, pid dead", "", dp(20 * s), &gone, noonClock, StatusFailed},
		{"empty exit file, stale, pid null", "", dp(20 * s), nil, noonClock, StatusFailed},
		{"empty exit file, stale, pid alive", "", dp(20 * s), &live, noonClock, StatusRunning},
		{"15,000 ms is not stale on one reading", "", dp(15 * s), &gone, noonClock, StatusRunning},
		{"it is on the second reading, a millisecond later", "", dp(15 * s), &gone, ticking(), StatusFailed},
	} {
		ws := workspace(t)
		r := mk(ws, "j")
		r.PID = c.pid
		if c.started != "" {
			r.StartedAt = c.started
		}
		save(t, ws, r)
		if c.exit != nil {
			put(t, ExitPath(ws, "j"), "")
			if err := os.Chtimes(ExitPath(ws, "j"), time.Time{}, noon().Add(-*c.exit)); err != nil {
				t.Fatal(err)
			}
		}
		got, err := Reconcile(ws, r, c.clock)
		if err != nil || got.Status != c.want {
			t.Errorf("%s: %s %v, want %s", c.name, got.Status, err, c.want)
		}
		if c.want == StatusFailed && (got.ExitCode != nil || got.EndedAt == nil || !strings.HasSuffix(ledger(t, ws)[0], ",\"exitCode\":null,\"detail\":\"watcher vanished\"}")) {
			t.Errorf("%s: a vanished shell is failed with an unknown code: %+v", c.name, got)
		}
	}
}

func TestReconcileKnowsWhetherThePidIsOurShell(t *testing.T) {
	c := child(t)
	pid := c.Process.Pid
	token, ok := ProcessStartToken(pid)
	if !ok {
		t.Skip("ps does not answer here")
	}
	if again, _ := ProcessStartToken(pid); again != token || !PidAlive(pid) {
		t.Fatalf("token %q then %q", token, again)
	}
	ws := workspace(t)
	for _, tc := range []struct {
		name  string
		token *string
		want  BgStatus
	}{{"the token matches", &token, StatusRunning}, {"no token recorded", nil, StatusRunning}, {"another process owns the pid", sp("Thu Jan  1 00:00:00 1970"), StatusFailed}} {
		r := mk(ws, "p")
		r.PID, r.StartToken = &pid, tc.token
		if got, err := Reconcile(ws, save(t, ws, r), noonClock); err != nil || got.Status != tc.want {
			t.Errorf("%s: %s %v", tc.name, got.Status, err)
		}
	}
	stop(c)
	r := mk(ws, "q")
	r.PID, r.StartToken = &pid, &token
	if got, _ := Reconcile(ws, save(t, ws, r), noonClock); got.Status != StatusFailed || PidAlive(pid) {
		t.Errorf("a pid that was reaped: %s", got.Status)
	}
}

func TestProcessStartTokenIsWhatPsPrints(t *testing.T) {
	pid := child(t).Process.Pid
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	if _, ok := ProcessStartToken(pid); ok {
		t.Error("no ps on PATH has no token")
	}
	for script, want := range map[string]string{"printf '  Thu Jan  1 00:00:00 2026 \\n'": "Thu Jan  1 00:00:00 2026", "echo ' '": "", "exit 1": "", "echo \"$@\"; exit 3": ""} {
		put(t, filepath.Join(bin, "ps"), "#!/bin/sh\n"+script+"\n")
		if err := os.Chmod(filepath.Join(bin, "ps"), 0o755); err != nil {
			t.Fatal(err)
		}
		if got, ok := ProcessStartToken(pid); got != want || ok != (want != "") {
			t.Errorf("ps %q: %q %v", script, got, ok)
		}
	}
}

func TestListRecordsReconcilesAndSkipsJunk(t *testing.T) { // oracle: only the record is listed
	ws := workspace(t)
	for id, text := range map[string]string{"arr": "[1]", "empty": "{}", "nope": "nope", "z-other": "{\"id\":\"b\",\"cwd\":\"x\",\"command\":[],\"status\":\"running\"}", "z-nullid": "{\"id\":null,\"cwd\":\"x\",\"command\":[],\"status\":\"s\"}"} {
		put(t, RecordPath(ws, id), text)
	}
	fresh := mk(ws, "b")
	fresh.StartedAt = "2026-09-09T00:09:59.000Z"
	save(t, ws, fresh)
	save(t, ws, mk(ws, "a"))
	put(t, ExitPath(ws, "a"), "0")
	recs, err := ListRecords(ws, noonClock)
	if err != nil || len(recs) != 2 || recs[0].ID != "a" || recs[0].Status != StatusComplete || recs[1].ID != "b" || recs[1].Status != StatusRunning {
		t.Errorf("%+v %v", recs, err)
	}
	if again, _ := ReadRecord(ws, "a"); again.Status != StatusComplete {
		t.Error("the correction is persisted")
	}
	// The oracle's writes read its clock too: a ticks 3 times (stamp, temporary name, ledger row), so b is read at 00:10:00.003 and its
	// 15,001 ms of age says failed; a record write that left the clock alone would read 14,999 and say running.
	ws = workspace(t)
	put(t, ExitPath(ws, "a"), "0")
	save(t, ws, mk(ws, "a"))
	late := mk(ws, "b")
	late.StartedAt = "2026-09-09T00:09:45.003Z"
	save(t, ws, late)
	if recs, _ := ListRecords(ws, ticking()); len(recs) != 2 || recs[1].Status != StatusFailed {
		t.Errorf("a ticking clock: %+v", recs)
	}
}

func TestTheWakeSwitches(t *testing.T) {
	ws := workspace(t)
	if st := ReadDisabledState(ws); st.Disabled || st.Since != nil {
		t.Errorf("no file: %+v", st)
	}
	store(t, ws)
	put(t, DisabledPath(ws), "")
	if st := ReadDisabledState(ws); !st.Disabled || st.Since != nil {
		t.Errorf("empty file: %+v", st)
	}
	put(t, DisabledPath(ws), " 2026-09-09T00:00:00.000Z\n")
	if st := ReadDisabledState(ws); !st.Disabled || st.Since == nil || *st.Since != "2026-09-09T00:00:00.000Z" {
		t.Errorf("dated file: %+v", st)
	}
	for v, want := range map[string]bool{"0": true, " Off ": true, "FALSE": true, "no": true, "off\n": true, "1": false, "": false, "n": false} { // oracle
		if got := EnvDisabled(func(k string) string { return map[string]string{EnvVar: v}[k] }); got != want {
			t.Errorf("%s=%q: %v", EnvVar, v, got)
		}
	}
	none := func(string) string { return "" }
	if WakeSuppressed(workspace(t), none) || !WakeSuppressed(ws, none) || !WakeSuppressed(workspace(t), func(string) string { return "no" }) {
		t.Error("either switch suppresses the wake")
	}
}

func TestSelectWakeKeepsTheFiveConditionsInOrder(t *testing.T) { // oracle: ids a b c f h for S1, d for S2
	ws := workspace(t)
	for _, c := range []struct {
		id, end string
		edit    func(*BgRecord)
	}{{"c", "00:03", nil}, {"a", "00:01", nil}, {"b", "00:01", nil}, {"d", "00:02", func(r *BgRecord) { r.SessionID, r.AdoptedBy = sp("S9"), sp("S2") }},
		{"e", "00:04", func(r *BgRecord) { r.DeliveredAt = sp("x") }}, {"f", "00:05", func(r *BgRecord) { r.Status = StatusCancelled }},
		{"g", "00:06", func(r *BgRecord) { r.SessionID = sp("S9") }}, {"h", "00:07", func(r *BgRecord) { r.Status = StatusFailed }}, {"r", "", func(r *BgRecord) { r.Status, r.EndedAt = StatusRunning, nil }}} {
		r := finished(ws, c.id, "2026-09-09T"+c.end+":00.000Z")
		if c.edit != nil {
			c.edit(&r)
		}
		save(t, ws, r)
	}
	ids := func(session *string, limit int) []string {
		recs, err := SelectWake(ws, session, limit, noonClock)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range recs {
			out = append(out, r.ID)
		}
		return out
	}
	for name, c := range map[string]struct {
		session *string
		limit   int
		want    []string
	}{"S1": {sp("S1"), 5, []string{"a", "b", "c", "f", "h"}}, "adopted": {sp("S2"), 5, []string{"d"}}, "limit 2": {sp("S1"), 2, []string{"a", "b"}}, "limit 0": {sp("S1"), 0, nil},
		"limit -1": {sp("S1"), -1, nil}, "no session": {nil, 5, nil}, "other session": {sp("S5"), 5, nil}} {
		if got := ids(c.session, c.limit); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
	store(t, ws)
	put(t, EnabledAtPath(ws), " 2026-09-09T00:01:00.000Z \n") // oracle: <= keeps a job that ended at the switch out, one millisecond later is in
	save(t, ws, finished(ws, "n", "2026-09-09T00:01:00.001Z"))
	if got := ids(sp("S1"), 5); !slices.Equal(got, []string{"n", "c", "f", "h", "r"}) {
		t.Errorf("after the gate: %v", got)
	}
	if got := ids(sp("S1"), WakeBatchLimit-3); len(got) != 2 {
		t.Errorf("WakeBatchLimit %d, limit %d: %v", WakeBatchLimit, WakeBatchLimit-3, got)
	}
	// JavaScript compares strings by UTF-16 unit: U+FFFF sorts above U+1F600 there, below it by UTF-8 byte.
	put(t, EnabledAtPath(ws), "\U0001F600")
	save(t, ws, finished(ws, "u", "\uffff"))
	if got := ids(sp("S1"), 9); !slices.Contains(got, "u") || slices.Contains(got, "n") {
		t.Errorf("UTF-16 gate: %v", got)
	}
}

func TestMarkDeliveredAdoptOrphansAndHasAnyTask(t *testing.T) { // oracle: the same sequence
	ws := workspace(t)
	bad := finished(ws, "../x", "2026-09-09T00:08:00.000Z")
	one, two, other := save(t, ws, finished(ws, "a", "2026-09-09T00:01:00.000Z")), save(t, ws, finished(ws, "b", "2026-09-09T00:02:00.000Z")), finished(ws, "g", "2026-09-09T00:03:00.000Z")
	other.SessionID, other.Extra = sp("S9"), []Member{{"zzz", json.RawMessage("1")}}
	save(t, ws, other)
	stamped := MarkDelivered(ws, []BgRecord{one, bad, two}, noonClock)
	if len(stamped) != 2 || stamped[0].DeliveredAt != nil { // the oracle returns the records as they came, and a record it cannot write is skipped
		t.Errorf("returned %+v", stamped)
	}
	if a, _ := ReadRecord(ws, "a"); a.DeliveredAt == nil || *a.DeliveredAt != "2026-09-09T00:10:00.000Z" {
		t.Errorf("stamp on disk: %+v", a)
	}
	rows := ledger(t, ws)
	if len(rows) != 2 || !strings.HasSuffix(rows[1], ",\"event\":\"delivered\",\"id\":\"b\",\"sessionId\":\"S1\"}") {
		t.Errorf("ledger %q", rows)
	}
	if got, _ := SelectWake(ws, sp("S1"), 5, noonClock); len(got) != 0 {
		t.Errorf("delivered once: %+v", got)
	}
	if got, err := AdoptOrphans(ws, nil, noonClock); err != nil || len(got) != 0 {
		t.Errorf("no session adopts nothing: %v %v", got, err)
	}
	adopted, err := AdoptOrphans(ws, sp("S3"), func() time.Time { return time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC) })
	if back, _ := ReadRecord(ws, "g"); err != nil || len(adopted) != 1 || adopted[0].ID != "g" || *adopted[0].AdoptedBy != "S3" || *back.AdoptedBy != "S3" || len(back.Extra) != 1 {
		t.Fatalf("adopted %+v %v", adopted, err)
	}
	if last := ledger(t, ws); last[len(last)-1] != "{\"at\":\"2026-09-09T10:00:00.000Z\",\"event\":\"adopted\",\"id\":\"g\",\"sessionId\":\"S3\"}" {
		t.Errorf("an adopted row carries its own at: %q", last)
	}
	if again, _ := AdoptOrphans(ws, sp("S3"), noonClock); len(again) != 0 {
		t.Errorf("adopted twice: %+v", again)
	}
	for session, want := range map[*string]bool{nil: true, sp("S1"): true, sp("S3"): true, sp("nobody"): false} {
		if got, err := HasAnyTask(ws, session, noonClock); err != nil || got != want {
			t.Errorf("HasAnyTask(%v) = %v %v", session, got, err)
		}
	}
	if got, _ := HasAnyTask(workspace(t), nil, noonClock); got {
		t.Error("an empty store has no task")
	}
}

func TestDurationLabelAndDescribeRecord(t *testing.T) { // oracle
	base := "2026-09-09T00:00:00.000Z"
	for _, c := range [][3]string{{base, "2026-09-09T00:00:59.499Z", "59s"}, {base, "2026-09-09T00:00:59.500Z", "1m00s"}, {base, "2026-09-09T00:01:00.000Z", "1m00s"},
		{base, "2026-09-09T00:02:05.000Z", "2m05s"}, {base, "2026-09-09T01:01:01.600Z", "61m02s"}, {"2026-09-09T00:00:05.000Z", base, "?"}, {"junk", base, "?"},
		{"2026-09-09", "2026-09-09T00:00:30Z", "30s"}, {"2026-09-09T00:00:00+09:00", "2026-09-08T15:00:03Z", "3s"}, {"2026-09-09T00:00", "2026-09-09T00:00:03", "3s"},
		{"2026-09-09 00:00:00", "2026-09-09 00:00:03", "3s"}, {"2026", "2026-01", "0s"}} {
		r := mk("x", "d")
		r.StartedAt, r.EndedAt = c[0], sp(c[1])
		if got := DurationLabel(r); got != c[2] {
			t.Errorf("%s .. %s = %q, want %q", c[0], c[1], got, c[2])
		}
	}
	r := mk("x", "d1")
	if DurationLabel(r) != "running" {
		t.Error("no endedAt is running")
	}
	r.Command, r.ExitCode, r.Status, r.EndedAt = []string{"a b", "c"}, fp(3), StatusFailed, sp("2026-09-09T00:01:05.000Z")
	r2 := mk("x", "d2")
	r2.Status, r2.EndedAt, r2.ExitCode = StatusCancelled, sp("2026-09-09T00:00:02.000Z"), fp(1e21)
	if DescribeRecord(r) != "- d1 (failed, exit 3, 1m05s) — a b c" || DescribeRecord(r2) != "- d2 (cancelled, exit 1e+21, 2s) — sleep 1" {
		t.Errorf("%q %q", DescribeRecord(r), DescribeRecord(r2))
	}
	if r2.ExitCode = nil; DescribeRecord(r2) != "- d2 (cancelled, exit ?, 2s) — sleep 1" {
		t.Error(DescribeRecord(r2))
	}
}

func TestReconcileActsOnTheWorkspaceNotTheRecordsCwd(t *testing.T) { // the oracle looked for the exit file, and wrote, in rec.cwd
	ws, elsewhere := workspace(t), workspace(t)
	r := save(t, ws, mk(elsewhere, "o"))
	put(t, ExitPath(ws, "o"), "2")
	got, err := Reconcile(ws, r, noonClock)
	if err != nil || got.Status != StatusFailed || got.ExitCode == nil || *got.ExitCode != 2 {
		t.Errorf("%+v %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(elsewhere, ".crw")); !os.IsNotExist(err) {
		t.Errorf("the record's own cwd was touched: %v", err)
	}
	if !RecordExists(ws, "o") || RecordExists(ws, "nobody") {
		t.Error("RecordExists")
	}
}
