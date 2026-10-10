package job

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// CRW-1095: one hook event reconciles each record at most once, a finished record costs no ps, and the wake text has a byte budget.

// psLog puts a ps on PATH that logs each call and answers token, and returns the log's path.
func psLog(t *testing.T, token string) string {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(bin, "calls")
	put(t, filepath.Join(bin, "ps"), "#!/bin/sh\necho \"$@\" >> "+shellQuotePosix(log)+"\necho "+shellQuotePosix(token)+"\n")
	if err := os.Chmod(filepath.Join(bin, "ps"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return log
}

func calls(t *testing.T, log string) int {
	t.Helper()
	b, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(log)
	return strings.Count(string(b), "\n")
}

func TestBgHookEventReconcilesEachRecordOnce(t *testing.T) {
	live := child(t).Process.Pid
	ws := workspace(t)
	const running = 80
	for i := range running {
		r := mk(ws, "r"+strconv.Itoa(i))
		r.PID, r.StartToken = &live, sp("TOKEN")
		save(t, ws, r)
	}
	for i := range 3 { // finished: the exit file answers, so no ps is asked, then or later
		r := mk(ws, "f"+strconv.Itoa(i))
		r.PID, r.StartToken = &live, sp("TOKEN")
		save(t, ws, r)
		put(t, ExitPath(ws, r.ID), "0")
	}
	log := psLog(t, "TOKEN")
	for _, event := range []struct {
		name string
		run  func() string
	}{
		{"session-start", func() string {
			return HandleSessionStart(HookPayload{SessionID: "S2", Cwd: ws}, ws, hookEnv(nil), noon)
		}},
		{"stop", func() string { return HandleStop(HookPayload{SessionID: "S1", Cwd: ws}, ws, hookEnv(nil), noon) }},
		{"prompt", func() string {
			return HandleUserPromptSubmit(HookPayload{SessionID: "S1", Cwd: ws}, ws, hookEnv(nil), noon)
		}},
		{"session-start again", func() string {
			return HandleSessionStart(HookPayload{SessionID: "S3", Cwd: ws}, ws, hookEnv(nil), noon)
		}},
	} {
		event.run()
		if n := calls(t, log); n != running {
			t.Errorf("%s: %d ps calls for %d running records, want one each and none for the finished ones", event.name, n, running)
		}
	}
}

func TestBgWakeTextKeepsABudget(t *testing.T) {
	long := strings.Repeat("é", 40000) + "\U0001F600"
	ws := workspace(t)
	for i := range 6 {
		r := hookDone(t, ws, "long"+strconv.Itoa(i), "OLD")
		r.Command, r.Note = []string{"sh", "-c", long + strconv.Itoa(i)}, sp(long)
		r.EndedAt = sp("2026-09-09T00:0" + strconv.Itoa(i) + ":00.000Z")
		save(t, ws, r)
	}
	check := func(name, out, field string, ids []string) {
		t.Helper()
		var v map[string]any
		if !utf8.ValidString(out) || json.Unmarshal([]byte(out), &v) != nil || len(out) > 4096 {
			t.Fatalf("%s: %d bytes, valid UTF-8 %v", name, len(out), utf8.ValidString(out))
		}
		text := v[field]
		if field == "additionalContext" {
			text = v["hookSpecificOutput"].(map[string]any)[field]
		}
		for _, id := range ids {
			if !strings.Contains(text.(string), "- "+id+" (") || !strings.Contains(text.(string), "crw relay job get "+id) {
				t.Errorf("%s: %s or its get pointer is missing:\n%s", name, id, text)
			}
		}
	}
	five := []string{"long0", "long1", "long2", "long3", "long4"}
	check("session-start", HandleSessionStart(HookPayload{SessionID: "S1", Cwd: ws}, ws, hookEnv(nil), noon), "additionalContext", five)
	check("stop", HandleStop(HookPayload{SessionID: "S1", Cwd: ws}, ws, hookEnv(nil), noon), "reason", five)
	full, _ := ReadRecord(ws, "long0")
	if full.Command[2] != long+"0" || *full.Note != long {
		t.Error("the record lost its full command or note")
	}
	if drained := DrainNow(ws, sp("S1"), noon); !utf8.ValidString(drained) || len(drained) > 4096 || !strings.Contains(drained, "crw relay job get long5") {
		t.Errorf("drain: %d bytes", len(drained))
	}
}

// The budget is the serialized envelope's (CRW-1095): JSON escapes a control character as six bytes, and a long id appears twice in
// a line, so the lines are measured as the envelope spells them. A wake that cannot describe every due job inside it describes the
// first ones that fit, and the others stay pending for the next wake.
func TestBgWakeEnvelopeBudgetCountsEscapesAndLongIDs(t *testing.T) {
	const limit = 4096
	ctl := strings.Repeat("\x01", 40000)
	ws := workspace(t)
	var all []string
	for i := range 5 {
		id := strings.Repeat("\x01", 100) + strings.Repeat("i", 100) + strconv.Itoa(i) // the temporary name of a write still fits
		all = append(all, id)
		r := hookDone(t, ws, id, "OLD") // adopted by S1 at the session start
		r.Command, r.Note = []string{"sh", "-c", ctl}, sp(ctl)
		r.EndedAt = sp("2026-09-09T00:0" + strconv.Itoa(i) + ":00.000Z")
		save(t, ws, r)
	}
	text := func(name, out, field string) string {
		t.Helper()
		var v map[string]any
		if !utf8.ValidString(out) || json.Unmarshal([]byte(out), &v) != nil || len(out) > limit {
			t.Fatalf("%s: %d bytes, valid UTF-8 %v", name, len(out), utf8.ValidString(out))
		}
		if field == "additionalContext" {
			return v["hookSpecificOutput"].(map[string]any)[field].(string)
		}
		return v[field].(string)
	}
	start := text("session-start", HandleSessionStart(HookPayload{SessionID: "S1", Cwd: ws}, ws, hookEnv(nil), noon), "additionalContext")
	if !strings.Contains(start, "5건") || !strings.Contains(start, "- "+all[0]+" (") {
		t.Errorf("session-start lost the count or the first job:\n%q", start)
	}
	seen := map[string]bool{}
	for wake := 0; len(seen) < len(all); wake++ {
		if wake == len(all) {
			t.Fatalf("five wakes delivered only %d jobs", len(seen))
		}
		body := text("stop", HandleStop(HookPayload{SessionID: "S1", Cwd: ws}, ws, hookEnv(nil), noon), "reason")
		shown := 0
		for _, id := range all {
			in := strings.Contains(body, "- "+id+" (")
			if in && !strings.Contains(body, "crw relay job get "+id) {
				t.Errorf("%q has no get pointer", id)
			}
			if in != delivered(t, ws, id) && !seen[id] {
				t.Errorf("job ending %q: shown %v, delivered %v", id[len(id)-1:], in, delivered(t, ws, id))
			}
			if in {
				seen[id], shown = true, shown+1
			}
		}
		if shown == 0 || !strings.Contains(body, strconv.Itoa(shown)+"건이 끝났습니다") {
			t.Fatalf("a wake showed %d jobs:\n%q", shown, body)
		}
	}
	// One job alone always fits, even with the longest id a file name allows, every byte of it escaped. Its record is put in place
	// by hand: a write's temporary name would be too long for it.
	ws = workspace(t)
	worst := strings.Repeat("\x01", 250)
	r := hookDone(t, ws, "x", "S1")
	r.Command, r.Note = []string{ctl}, sp(ctl)
	save(t, ws, r)
	body := strings.Replace(get(t, RecordPath(ws, "x")), `"id": "x"`, `"id": `+quoteJS(worst), 1)
	if err := os.WriteFile(RecordPath(ws, worst), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(RecordPath(ws, "x")); err != nil {
		t.Fatal(err)
	}
	drained := DrainNow(ws, sp("S1"), noon)
	if len(quoteJS(drained)) > limit || !strings.Contains(drained, "crw relay job get "+worst) {
		t.Errorf("drain of the longest id: %d bytes escaped", len(quoteJS(drained)))
	}
	if body := text("stop", HandleStop(HookPayload{SessionID: "S1", Cwd: ws}, ws, hookEnv(nil), noon), "reason"); !strings.Contains(body, "crw relay job get "+worst) {
		t.Errorf("the longest id has no get pointer")
	}
}

func quoteJS(s string) string { b, _ := value(s, 0); return string(b) }
