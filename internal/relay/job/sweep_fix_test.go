package job

import (
	"fmt"
	"os"
	"strings"
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
