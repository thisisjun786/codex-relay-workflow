package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// The ledger_ and interview_ cases of testdata/oracle-writes.json (recorded once by testdata/record-writes.mjs under Node 24, no
// Node runs here), and the ledger and interview file modes of its modes_umask_ cases, replayed through the Go API.
// oracle_writes_test.go replays the other cases of the file and skips these by prefix; this test fails on a ledger_ or interview_
// case it does not replay, and when the file holds other than the 21 such cases (13 ledger_, 6 interview_, 2 modes_umask_), so a case
// added to or dropped from the file cannot go unnoticed.

// withUmask runs fn under mask, which is process-wide: the tests of this package do not run in parallel.
func withUmask(mask int, fn func()) {
	defer syscall.Umask(syscall.Umask(mask))
	fn()
}

func listNames(dir string) (names []string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestLedgerWritesMatchTheRecordedOracle(t *testing.T) {
	data, err := os.ReadFile("testdata/oracle-writes.json")
	var cases []recorded
	if err != nil || json.Unmarshal(data, &cases) != nil {
		t.Fatalf("recorded cases: %v", err)
	}
	ph, yes, ev := func(p Phase) *Phase { return &p }, true, "planned <a&b> \u2028 é 😀"
	row := func(from *Phase, to Phase, reason string) LedgerEntry {
		return LedgerEntry{TS: "t1", SessionID: "rec-s1", From: from, To: to, Reason: reason}
	}
	closed := func(e LedgerEntry, key *CloseKey, evidence string) LedgerEntry { // a D-close row of the CLI: close key, then evidence
		e.Close, e.Evidence = key, &evidence
		return e
	}
	cli, done := row(ph(PhaseP), PhaseA, "cli"), row(ph(PhaseC), PhaseIdle, "done")
	override, resolve := row(ph(PhaseI), PhaseP, "cli"), row(ph(PhaseB), PhaseB, "evidence resolve: agent=a1")
	override.Actor, override.Override, override.ScanEvidence, override.Evidence = "agent", &yes, &ScanEvidence{}, str("skip")
	resolve.Actor, resolve.Override, resolve.Evidence, resolve.EvidenceAfterReason = "agent", new(bool), str("r.json"), true
	chat, chatOverride := row(ph(PhaseP), PhaseA, "chat"), row(ph(PhaseI), PhaseP, "chat")
	chat.Actor, chat.Evidence = "human", str("ok")
	chatOverride.Actor, chatOverride.Override, chatOverride.ScanEvidence, chatOverride.Evidence = "human", &yes, &ScanEvidence{2, 1}, str("go")
	hook := closed(done, nil, "verified") // the hook's close row spreads the transition row, evidence included, before the close key
	hook.Close, hook.EvidenceAfterReason = &CloseKey{CheckEpoch: str("c1")}, true
	withEvidence := cli
	withEvidence.Evidence = &ev
	hookClose := done
	hookClose.Close = &CloseKey{str("c1"), str("wp1")}
	ledger := map[string]LedgerEntry{
		"ledger_cli_transition": withEvidence, "ledger_no_evidence": row(ph(PhaseIdle), PhaseP, "cli"), "ledger_cli_override": override,
		"ledger_cli_dclose": closed(done, &CloseKey{str("c1"), str("wp1")}, "verified"), "ledger_cli_dclose_null_key": closed(done, &CloseKey{}, "verified"),
		"ledger_hook_dclose_with_evidence": hook, "ledger_evidence_resolve": resolve, "ledger_chat_transition": chat, "ledger_chat_override": chatOverride,
		"ledger_hook_dclose": hookClose, "ledger_reset": row(ph(PhaseB), PhaseIdle, "reset"), "ledger_from_null": row(nil, PhaseA, "cli"),
	}
	seen := 0
	for _, c := range cases {
		id := c.str("id")
		if !strings.HasPrefix(id, "ledger_") && !strings.HasPrefix(id, "interview_") && !strings.HasPrefix(id, "modes_umask_") {
			continue
		}
		seen++
		cwd := t.TempDir()
		ledgerFile, ivDir := filepath.Join(cwd, crwdir.DirName, LedgerFile), filepath.Join(cwd, crwdir.DirName, InterviewsSubdir)
		same := func(what string, got, want any) {
			t.Helper()
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s %s:\n got %#v\nwant %#v", id, what, got, want)
			}
		}
		switch {
		case ledger[id].TS != "":
			same("line", []any{AppendLedger(cwd, ledger[id]), fileText(t, ledgerFile)}, []any{nil, c.str("file")})
		case id == "ledger_two_rows":
			_, _ = AppendLedger(cwd, ledger["ledger_reset"]), AppendLedger(cwd, ledger["ledger_no_evidence"])
			same("lines", fileText(t, ledgerFile), c.str("file"))
		case id == "interview_events" || id == "interview_map_key_order":
			event := func(kind InterviewScanEvent, round float64) InterviewEvent {
				return InterviewEvent{TS: "t1", SessionID: "rec-s1", Event: kind, RoundID: round, ContradictionCount: 3, HighContradictionCount: 1}
			}
			events := []InterviewEvent{event(ScanStarted, 1), event(ScanCompleted, 2), event(RescanCompleted, 3)}
			events[1].Map, events[2].SessionID = []MapEntry{{"q-b", "goal"}, {"q-a", "constraint"}}, "a/b"
			if id == "interview_map_key_order" { // JavaScript lists the array indexes first, ascending: 1, 2, 10, then x, 01, 4294967295, y
				events = []InterviewEvent{event(ScanCompleted, 1)}
				events[0].ContradictionCount, events[0].HighContradictionCount = 0, 0
				events[0].Map = []MapEntry{{"10", "a"}, {"2", "b"}, {"x", "c"}, {"1", "d"}, {"01", "e"}, {"4294967295", "f"}, {"y", "g"}}
			}
			for _, e := range events {
				same("append", AppendInterviewEvent(cwd, e), error(nil))
			}
			got := []any{fileText(t, filepath.Join(ivDir, "rec-s1.jsonl"))}
			want := []any{c.str("file")}
			if id == "interview_events" {
				got, want = append(got, fileText(t, filepath.Join(ivDir, "a-b.jsonl")), listNames(ivDir)), append(want, c.str("aliasFile"), c.strs("listing"))
			}
			same("files", got, want)
			compareEvents(t, id, ReadInterviewEvents(cwd, "rec-s1"), c["events"].([]any))
		case strings.HasPrefix(id, "interview_read_"):
			_ = os.MkdirAll(ivDir, 0o777)
			sid := map[string]string{"interview_read_mixed": "iv", "interview_read_missing": "nope", "interview_read_directory": "dir", "interview_read_map_key_order": "raw"}[id]
			if input := c.str("input"); input != "" {
				_ = os.WriteFile(filepath.Join(ivDir, sid+".jsonl"), []byte(input), 0o644)
			} else if id == "interview_read_directory" {
				_ = os.Mkdir(filepath.Join(ivDir, "dir.jsonl"), 0o777)
			}
			got := ReadInterviewEvents(cwd, sid)
			same("not nil", got != nil, true)
			compareEvents(t, id, got, c["events"].([]any))
		case id == "modes_umask_022" || id == "modes_umask_077":
			mask, _ := strconv.ParseInt(strings.TrimPrefix(id, "modes_umask_"), 8, 32)
			withUmask(int(mask), func() {
				_ = AppendLedger(cwd, LedgerEntry{TS: "t", SessionID: "wr", To: PhaseP, Reason: "x"})
				_ = AppendInterviewEvent(cwd, InterviewEvent{TS: "t", SessionID: "wr", Event: ScanStarted, RoundID: 1})
			})
			mode := func(p string) string { info, _ := os.Stat(p); return strconv.FormatUint(uint64(info.Mode().Perm()), 8) }
			same("modes", []string{mode(ledgerFile), mode(filepath.Join(ivDir, "wr.jsonl"))}, []string{c.str("ledger"), c.str("interview")})
		default:
			t.Errorf("%s is replayed nowhere", id)
		}
	}
	if seen != 21 { // 13 ledger_, 6 interview_ and the 2 modes_umask_ cases
		t.Errorf("%d recorded ledger, interview and modes cases, want 21", seen)
	}
}

// compareEvents checks what ReadInterviewEvents returned against the objects the oracle returned: the row kept in Raw has the
// same keys, presence and values, and the typed fields agree wherever the key is present.
func compareEvents(t *testing.T, id string, got []InterviewEvent, want []any) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d events, oracle %d", id, len(got), len(want))
	}
	for i, w := range want {
		var raw any
		o := w.(map[string]any)
		if err := json.Unmarshal(got[i].Raw, &raw); err != nil || !reflect.DeepEqual(raw, w) || string(got[i].Event) != o["event"] || got[i].RoundID != o["roundId"] || got[i].ContradictionCount != o["contradictionCount"] {
			t.Errorf("%s event %d: raw %s (%v), oracle %v", id, i, got[i].Raw, err, w)
		}
		if ts, ok := o["ts"]; ok && got[i].TS != ts || got[i].SessionID != o["sessionId"] && o["sessionId"] != nil {
			t.Errorf("%s event %d: ts %q session %q", id, i, got[i].TS, got[i].SessionID)
		}
		if high, ok := o["highContradictionCount"]; ok && got[i].HighContradictionCount != high {
			t.Errorf("%s event %d: high %v, oracle %v", id, i, got[i].HighContradictionCount, high)
		}
	}
}
