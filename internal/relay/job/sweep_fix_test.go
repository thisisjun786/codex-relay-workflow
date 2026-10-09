package job

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// CRW-1134, verification fix round 1.

// json.Unmarshal reads a null into a string without an error: a null in a key that is not nullable, or in an element of command, is
// a broken record, whose bytes stay.
func TestNullInANonNullableKeyIsABrokenRecord(t *testing.T) {
	ws := workspace(t)
	good := finished(ws, "good", "2026-09-09T00:05:00.000Z")
	save(t, ws, good)
	text := get(t, RecordPath(ws, "good"))
	as := func(id string) string { return strings.Replace(text, `"id": "good"`, `"id": "`+id+`"`, 1) }
	broken := map[string]string{
		"nullcwd":     strings.Replace(as("nullcwd"), `"cwd": "`+ws+`"`, `"cwd": null`, 1),
		"nullstarted": strings.Replace(as("nullstarted"), `"startedAt": "2026-09-09T00:00:00.000Z"`, `"startedAt": null`, 1),
		"nullelement": strings.Replace(as("nullelement"), `"sleep",`, `null,`, 1),
		"nullstatus":  strings.Replace(as("nullstatus"), `"status": "complete"`, `"status": null`, 1),
	}
	for id, body := range broken {
		if body == as(id) {
			t.Fatalf("%s: the fixture did not change", id)
		}
		put(t, RecordPath(ws, id), body)
		if _, ok := ReadRecord(ws, id); ok {
			t.Errorf("%s reads as a record", id)
		}
	}
	_, _ = ListRecords(ws, noonClock)
	HandleStop(HookPayload{SessionID: "S1", Cwd: ws}, ws, hookEnv(nil), noon)
	list := cliResult(t, ws, "list").Out.(string)
	for id, body := range broken {
		if get(t, RecordPath(ws, id)) != body {
			t.Errorf("%s: the bytes changed", id)
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
	if len(BrokenRecords(ws)) != len(broken) {
		t.Errorf("broken records: %v", BrokenRecords(ws))
	}
}

// list --json is a list of records while the store is sound; a broken record file is not "no job" for a caller that reads JSON.
func TestListJSONReportsBrokenRecords(t *testing.T) {
	ws := workspace(t)
	store(t, ws)
	put(t, RecordPath(ws, "bad"), "{ not json")
	r := cliResult(t, ws, "list", "--json")
	obj, ok := r.Out.(map[string]any)
	if !ok {
		// the answer may be another ordered-object type: compare through its text
		obj = nil
	}
	text := fmt.Sprintf("%v", r.Out)
	if r.Code != 1 || !strings.Contains(text, "bad") || (ok && len(obj) == 0) {
		t.Errorf("list --json of a broken store: code %d, %v", r.Code, r.Out)
	}
	if get(t, RecordPath(ws, "bad")) != "{ not json" {
		t.Errorf("the broken bytes changed")
	}
	save(t, ws, finished(ws, "good", "2026-09-09T00:05:00.000Z"))
	r = cliResult(t, ws, "list", "--json")
	if text := fmt.Sprintf("%v", r.Out); r.Code != 1 || !strings.Contains(text, "bad") || !strings.Contains(text, "good") {
		t.Errorf("list --json of a mixed store: code %d, %v", r.Code, r.Out)
	}
	if err := os.Remove(RecordPath(ws, "bad")); err != nil {
		t.Fatal(err)
	}
	items, isList := cliResult(t, ws, "list", "--json").Out.([]any)
	if !isList || len(items) != 1 {
		t.Errorf("list --json of a sound store is not a list of its records: %v", items)
	}
}

// A failed on changes neither the switch nor the enabled-at time that holds back the completions of the time it was off.
func TestAFailedOnKeepsTheEnabledAtTime(t *testing.T) {
	ws := workspace(t)
	store(t, ws)
	put(t, EnabledAtPath(ws), "2026-09-09T00:01:00.000Z\n")
	mkdir(t, DisabledPath(ws)) // an off switch that cannot be removed
	if r, err := RunCLI([]string{"on"}, ws, os.LookupEnv, time.Now); err == nil && r.Code == 0 {
		t.Fatalf("on reported success: %+v", r)
	}
	if got := get(t, EnabledAtPath(ws)); got != "2026-09-09T00:01:00.000Z\n" {
		t.Errorf("enabled-at after a failed on: %q", got)
	}
	os.Remove(EnabledAtPath(ws))
	if _, err := RunCLI([]string{"on"}, ws, os.LookupEnv, time.Now); err == nil {
		t.Fatal("on succeeded")
	}
	if _, err := os.Lstat(EnabledAtPath(ws)); !os.IsNotExist(err) {
		t.Errorf("a failed on left an enabled-at that was not there: %v", err)
	}
}

// CRW-1134, verification fix round 2.

// Two ons that both read the switch off before either took the lock: the second must see the first one's ON under the lock and
// leave the enabled-at time alone, or a job that ended between the two would never wake.
func TestConcurrentOnsEnableOnce(t *testing.T) {
	ws := workspace(t)
	store(t, ws)
	put(t, DisabledPath(ws), "2026-09-09T00:00:00.000Z\n")
	r := finished(ws, "between", "2026-09-09T00:02:00.000Z")
	r.SessionID = sp("S1")
	save(t, ws, r)
	var mu sync.Mutex
	minute := 0
	clock := func() time.Time { // 00:01, 00:03, 00:05, ...
		mu.Lock()
		defer mu.Unlock()
		minute += 2
		return time.Date(2026, 9, 9, 0, minute-1, 0, 0, time.UTC)
	}
	unlock, err := lockStore(ws)
	if err != nil {
		t.Fatal(err)
	}
	outs := make([]string, 2)
	var wg sync.WaitGroup
	for i := range outs {
		wg.Go(func() {
			res, err := RunCLI([]string{"on"}, ws, os.LookupEnv, clock)
			if err != nil {
				t.Errorf("on: %v", err)
			}
			outs[i] = fmt.Sprint(res.Out)
		})
	}
	time.Sleep(300 * time.Millisecond) // both ons are past their switch read and wait on the lock
	unlock()
	wg.Wait()
	if got := get(t, EnabledAtPath(ws)); got != "2026-09-09T00:01:00.000Z\n" {
		t.Errorf("enabled-at %q after two ons, want the first one's time; answers %q", got, outs)
	}
	if !strings.Contains(outs[0]+outs[1], "이미 ON") {
		t.Errorf("neither on saw the other: %q", outs)
	}
	if n := strings.Count(get(t, filepath.Join(BGDir(ws), LedgerFile)), `"enabled"`); n != 1 {
		t.Errorf("%d enabled rows", n)
	}
	if out := HandleStop(HookPayload{SessionID: "S1", Cwd: ws}, ws, hookEnv(nil), noon); !strings.Contains(out, "between") {
		t.Errorf("the job that ended after the first on does not wake: %q", out)
	}
}

// An enabled-at that does not read cannot be put back, so on stops before it changes anything.
func TestAnOnThatCannotReadEnabledAtChangesNothing(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a file without read permission")
	}
	ws := workspace(t)
	store(t, ws)
	put(t, EnabledAtPath(ws), "2026-09-09T00:01:00.000Z\n")
	if err := os.Chmod(EnabledAtPath(ws), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(EnabledAtPath(ws), 0o600) })
	mkdir(t, DisabledPath(ws)) // an off switch that cannot be removed
	if r, err := RunCLI([]string{"on"}, ws, os.LookupEnv, time.Now); err == nil && r.Code == 0 {
		t.Fatalf("on reported success: %+v", r)
	}
	if err := os.Chmod(EnabledAtPath(ws), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := get(t, EnabledAtPath(ws)); got != "2026-09-09T00:01:00.000Z\n" {
		t.Errorf("enabled-at after a failed on: %q", got)
	}
}
