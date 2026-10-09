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
		{"session-start", func() string { return HandleSessionStart(HookPayload{SessionID: "S2", Cwd: ws}, ws, hookEnv(nil), noon) }},
		{"stop", func() string { return HandleStop(HookPayload{SessionID: "S1", Cwd: ws}, ws, hookEnv(nil), noon) }},
		{"prompt", func() string { return HandleUserPromptSubmit(HookPayload{SessionID: "S1", Cwd: ws}, ws, hookEnv(nil), noon) }},
		{"session-start again", func() string { return HandleSessionStart(HookPayload{SessionID: "S3", Cwd: ws}, ws, hookEnv(nil), noon) }},
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
