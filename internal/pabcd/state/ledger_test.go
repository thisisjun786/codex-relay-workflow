package state

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// The B-class tests of CXC v0.2.40 pabcd-state/test/state.test.ts for the transition ledger and the interview scan events, named
// by the oracle test they port, and run through the real appenders and the reader. The recorded oracle cases are replayed by
// ledger_oracle_test.go.

func interviewLedgerPath(cwd, session string) string {
	return filepath.Join(cwd, crwdir.DirName, InterviewsSubdir, session+".jsonl")
}

func appendText(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString(text); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAppendLedgerCreatesTaggedNDJSONLines(t *testing.T) { // oracle 359
	cwd, p, a := t.TempDir(), PhaseP, PhaseA
	for _, e := range []LedgerEntry{{TS: "t1", SessionID: "alpha", From: &p, To: PhaseA, Reason: "plan approved"}, {TS: "t2", SessionID: "beta", From: &a, To: PhaseB, Reason: "audit passed"}} {
		if err := AppendLedger(cwd, e); err != nil {
			t.Fatal(err)
		}
	}
	lines := strings.Split(strings.TrimSpace(fileText(t, filepath.Join(cwd, crwdir.DirName, LedgerFile))), "\n")
	var first map[string]any
	if len(lines) != 2 || json.Unmarshal([]byte(lines[0]), &first) != nil || first["sessionId"] != "alpha" || first["to"] != "A" {
		t.Fatalf("%q", lines)
	}
}

func TestAppendInterviewEventWritesParseableScanEventsAndReadsThemBack(t *testing.T) { // oracle 604
	cwd := t.TempDir()
	started := InterviewEvent{TS: "t1", SessionID: "iv", Event: ScanStarted, RoundID: 1, ContradictionCount: 3, HighContradictionCount: 1}
	completed := InterviewEvent{TS: "t2", SessionID: "iv", Event: ScanCompleted, RoundID: 1}
	for _, e := range []InterviewEvent{started, completed} {
		if err := AppendInterviewEvent(cwd, e); err != nil {
			t.Fatal(err)
		}
	}
	events := ReadInterviewEvents(cwd, "iv")
	if len(events) != 2 || events[0].Event != ScanStarted || events[1].Event != ScanCompleted || events[0].ContradictionCount != 3 || events[0].HighContradictionCount != 1 {
		t.Fatalf("%+v", events)
	}
	if _, err := os.Stat(interviewLedgerPath(cwd, "iv")); err != nil { // the file lives under .crw/interviews/
		t.Fatal(err)
	}
	if none := ReadInterviewEvents(cwd, "nope"); none == nil || len(none) != 0 { // a missing session reads as []
		t.Fatalf("%#v", none)
	}
}

func TestReadInterviewEventsReturnsScanOnlyRowsFromAMixedLedger(t *testing.T) { // oracle 622 (G3, L20)
	cwd := t.TempDir()
	if err := AppendInterviewEvent(cwd, InterviewEvent{TS: "t1", SessionID: "mix", Event: ScanStarted, RoundID: 1, ContradictionCount: 2, HighContradictionCount: 1}); err != nil {
		t.Fatal(err)
	}
	// a real session interleaves the interview ledger's question and answer rows with the scan rows in one file
	appendText(t, interviewLedgerPath(cwd, "mix"),
		`{"ts":"t2","sessionId":"mix","turnId":"t1","event":"question_asked","questionId":"q1","eventId":"t1:q1:question_asked","question":"Goal?"}`+"\n"+
			`{"ts":"t3","sessionId":"mix","turnId":"t1","event":"answer_recorded","questionId":"q1","eventId":"t1:q1:answer_recorded","answers":["ship it"]}`+"\n")
	if err := AppendInterviewEvent(cwd, InterviewEvent{TS: "t4", SessionID: "mix", Event: ScanCompleted, RoundID: 1}); err != nil {
		t.Fatal(err)
	}
	var kinds []InterviewScanEvent
	for _, e := range ReadInterviewEvents(cwd, "mix") {
		kinds = append(kinds, e.Event)
	}
	if !slices.Equal(kinds, []InterviewScanEvent{ScanStarted, ScanCompleted}) {
		t.Fatalf("only the 2 scan rows must be returned, not the question and answer rows: %v", kinds)
	}
}

func TestReadInterviewEventsReadsACrlfLedgerLikeAnLfOne(t *testing.T) { // crlf-inputs.test.ts, the scan reader row
	rows := []string{
		`{"ts":"2026-08-21T00:00:00Z","sessionId":"s","event":"scan_started","roundId":1,"contradictionCount":0}`,
		`{"ts":"2026-08-21T00:00:01Z","sessionId":"s","event":"scan_completed","roundId":1,"contradictionCount":2}`,
	}
	read := func(eol string) []InterviewEvent {
		cwd := t.TempDir()
		if err := os.MkdirAll(filepath.Dir(interviewLedgerPath(cwd, "s")), 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(interviewLedgerPath(cwd, "s"), []byte(strings.Join(rows, eol)+eol), 0o644); err != nil {
			t.Fatal(err)
		}
		return ReadInterviewEvents(cwd, "s")
	}
	if lf, crlf := read("\n"), read("\r\n"); len(lf) != len(rows) || !reflect.DeepEqual(crlf, lf) {
		t.Fatalf("LF %+v, CRLF %+v", lf, crlf)
	}
}

func TestAppendersCreateTheStateDirectoryWithItsIgnoreFileAndKeepAnExistingOne(t *testing.T) { // ensureCodexclawDir comes first in both appenders
	appendOne := map[string]func(cwd string) error{
		"ledger": func(cwd string) error {
			return AppendLedger(cwd, LedgerEntry{TS: "t", SessionID: "s", To: PhaseP, Reason: "x"})
		},
		"interview": func(cwd string) error {
			return AppendInterviewEvent(cwd, InterviewEvent{TS: "t", SessionID: "s", Event: ScanStarted, RoundID: 1})
		},
	}
	for name, appendRow := range appendOne {
		fresh := t.TempDir()
		if err := appendRow(fresh); err != nil || fileText(t, filepath.Join(fresh, crwdir.DirName, ".gitignore")) != crwdir.GitignoreText {
			t.Errorf("%s: a fresh state directory must get its ignore file: %v", name, err)
		}
		kept := t.TempDir()
		ignore := filepath.Join(kept, crwdir.DirName, ".gitignore")
		if err := os.MkdirAll(filepath.Dir(ignore), 0o777); err != nil || os.WriteFile(ignore, []byte("mine\n"), 0o644) != nil {
			t.Fatal(err)
		}
		if err := appendRow(kept); err != nil || fileText(t, ignore) != "mine\n" {
			t.Errorf("%s: an existing state directory is left alone: %v, %q", name, err, fileText(t, ignore))
		}
	}
}

func TestInterviewEventsOfAliasedSessionIDsShareOneLedgerAndTheReaderSanitisesToo(t *testing.T) { // recorded interview_events aliasFile; the sharing is a known defect
	cwd := t.TempDir()
	if err := AppendInterviewEvent(cwd, InterviewEvent{TS: "t", SessionID: "a/b", Event: ScanStarted, RoundID: 1}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a/b", "a-b"} {
		if got := ReadInterviewEvents(cwd, id); len(got) != 1 || got[0].SessionID != "a/b" {
			t.Errorf("read as %q: %+v", id, got)
		}
	}
}

func TestAppendInterviewEventWritesAKindTheReaderSkips(t *testing.T) { // known defect: the appender does not check the kind
	cwd := t.TempDir()
	if err := AppendInterviewEvent(cwd, InterviewEvent{TS: "t", SessionID: "s", Event: "question_asked", RoundID: 1}); err != nil {
		t.Fatal(err)
	}
	if file := fileText(t, interviewLedgerPath(cwd, "s")); !strings.Contains(file, `"event":"question_asked"`) || len(ReadInterviewEvents(cwd, "s")) != 0 {
		t.Fatalf("file %q, read %+v", file, ReadInterviewEvents(cwd, "s"))
	}
}

func TestReadInterviewEventsDecodesAttributionsSoAReadEventCanBeAppendedAgain(t *testing.T) { // a review of the port found the typed Map empty on read
	first, second := t.TempDir(), t.TempDir()
	if err := AppendInterviewEvent(first, InterviewEvent{TS: "t", SessionID: "s", Event: ScanCompleted, RoundID: 2,
		Map: []MapEntry{{"q-b", "goal"}, {"10", "x"}, {"q-a", "constraint"}, {"2", "y"}, {"q-b", "scope"}}}); err != nil {
		t.Fatal(err)
	}
	read := ReadInterviewEvents(first, "s")
	// the pairs come back in the order the file holds them: array indexes ascending, then insertion order, the repeated key once
	if want := []MapEntry{{"2", "y"}, {"10", "x"}, {"q-b", "scope"}, {"q-a", "constraint"}}; len(read) != 1 || !slices.Equal(read[0].Map, want) {
		t.Fatalf("%+v", read)
	}
	if err := AppendInterviewEvent(second, read[0]); err != nil || fileText(t, interviewLedgerPath(second, "s")) != fileText(t, interviewLedgerPath(first, "s")) {
		t.Fatalf("appended again: %v\n got %s\nwant %s", err, fileText(t, interviewLedgerPath(second, "s")), fileText(t, interviewLedgerPath(first, "s")))
	}
	for name, c := range map[string]struct {
		row   string
		empty bool
		isNil bool
	}{
		"no map":           {`{"event":"scan_started","roundId":1,"contradictionCount":0}`, false, true},
		"empty map":        {`{"event":"scan_started","roundId":1,"contradictionCount":0,"map":{}}`, true, false},
		"a non-string":     {`{"event":"scan_started","roundId":1,"contradictionCount":0,"map":{"q":1}}`, false, true},
		"a null map":       {`{"event":"scan_started","roundId":1,"contradictionCount":0,"map":null}`, false, true},
		"another key case": {`{"event":"scan_started","roundId":1,"contradictionCount":0,"MAP":{"q":"d"}}`, false, true},
	} {
		cwd := t.TempDir()
		if err := os.MkdirAll(filepath.Dir(interviewLedgerPath(cwd, "s")), 0o777); err != nil || os.WriteFile(interviewLedgerPath(cwd, "s"), []byte(c.row+"\n"), 0o644) != nil {
			t.Fatal(err)
		}
		got := ReadInterviewEvents(cwd, "s")
		if len(got) != 1 || (got[0].Map == nil) != c.isNil || (c.empty && len(got[0].Map) != 0) {
			t.Errorf("%s: %+v", name, got)
		}
	}
}

func TestAppendLedgerReportsAFailedAppend(t *testing.T) { // appendFileSync throws EISDIR when the ledger path is a directory
	cwd := t.TempDir()
	ledger := filepath.Join(cwd, crwdir.DirName, LedgerFile)
	if err := os.MkdirAll(ledger, 0o777); err != nil {
		t.Fatal(err)
	}
	err := AppendLedger(cwd, LedgerEntry{TS: "t", SessionID: "s", To: PhaseP, Reason: "x"})
	if info, statErr := os.Stat(ledger); !errors.Is(err, syscall.EISDIR) || statErr != nil || !info.IsDir() {
		t.Fatalf("err %v, stat %v", err, statErr)
	}
}

// appendedRow is the file AppendInterviewEvent leaves for session "s" after the one row e.
func appendedRow(t *testing.T, e InterviewEvent) string {
	t.Helper()
	cwd := t.TempDir()
	if err := AppendInterviewEvent(cwd, e); err != nil {
		t.Fatal(err)
	}
	return fileText(t, interviewLedgerPath(cwd, "s"))
}

// The expected bytes of the next two tests are what the oracle's appenders wrote when run under Node 24 (JSON.stringify: -0 is 0,
// NaN and the infinities are null, 1e21 is 1e+21, an empty object is {}, a key assigned twice keeps its first place and its last
// value). A negative zero reaches the appender from scan-cli.ts:125, which parses --contradictions -0 and passes it on.

func TestAppendEventsSpellNumbersLikeJSONStringify(t *testing.T) {
	negZero := math.Copysign(0, -1)
	if !math.Signbit(negZero) { // a Go literal -0 is a positive zero and would not catch a missing normalisation
		t.Fatal("not a negative zero")
	}
	row := func(round, contradictions, high float64) InterviewEvent {
		return InterviewEvent{TS: "t", SessionID: "s", Event: ScanCompleted, RoundID: round, ContradictionCount: contradictions, HighContradictionCount: high}
	}
	if got, want := appendedRow(t, row(negZero, math.NaN(), math.Inf(1))), `{"ts":"t","sessionId":"s","event":"scan_completed","roundId":0,"contradictionCount":null,"highContradictionCount":null}`+"\n"; got != want {
		t.Errorf("-0, NaN, +Inf:\n got %q\nwant %q", got, want)
	}
	if got, want := appendedRow(t, row(1e21, 1.5, math.Inf(-1))), `{"ts":"t","sessionId":"s","event":"scan_completed","roundId":1e+21,"contradictionCount":1.5,"highContradictionCount":null}`+"\n"; got != want {
		t.Errorf("1e21, 1.5, -Inf:\n got %q\nwant %q", got, want)
	}
	cwd, from, yes := t.TempDir(), PhaseI, true
	if err := AppendLedger(cwd, LedgerEntry{TS: "t", SessionID: "s", From: &from, To: PhaseP, Reason: "cli", Actor: "agent", Override: &yes, ScanEvidence: &ScanEvidence{negZero, math.NaN()}}); err != nil {
		t.Fatal(err)
	}
	want := `{"ts":"t","sessionId":"s","from":"I","to":"P","reason":"cli","actor":"agent","override":true,"scanEvidence":{"scanRounds":0,"highContradictionCount":null}}` + "\n"
	if got := fileText(t, filepath.Join(cwd, crwdir.DirName, LedgerFile)); got != want {
		t.Errorf("scanEvidence:\n got %q\nwant %q", got, want)
	}
}

func TestAppendInterviewEventMapAttributions(t *testing.T) {
	row := func(attributions []MapEntry) InterviewEvent {
		return InterviewEvent{TS: "t", SessionID: "s", Event: ScanCompleted, RoundID: 1, Map: attributions}
	}
	const head = `{"ts":"t","sessionId":"s","event":"scan_completed","roundId":1,"contradictionCount":0,"highContradictionCount":0`
	for name, c := range map[string]struct {
		attributions []MapEntry
		want         string
	}{
		"none":     {nil, head + "}\n"},
		"empty":    {[]MapEntry{}, head + `,"map":{}}` + "\n"},
		"repeated": {[]MapEntry{{"b", "x"}, {"a", "y"}, {"b", "z"}}, head + `,"map":{"b":"z","a":"y"}}` + "\n"},
	} {
		if got := appendedRow(t, row(c.attributions)); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, c.want)
		}
	}
}

// The scan ledger is the file the interview answer ledger writes its question and answer rows to as well. The oracle appends after a
// final line that has no line feed without a separator, so the new row is joined to that line and the reader drops both (known
// defect, repaired by CRW-474: port: fixed). AppendInterviewEvent starts the row on a line of its own instead; the transition ledger
// is not part of this repair.

const answerLedgerRow = `{"ts":"t","sessionId":"s","turnId":"t1","event":"question_asked","questionId":"q","eventId":"t1:q:question_asked","question":"?"}`

// seedInterviews writes content as the session's scan ledger, with the directories the appender would make.
func seedInterviews(t *testing.T, cwd, session, content string) {
	t.Helper()
	p := interviewLedgerPath(cwd, session)
	if err := os.MkdirAll(filepath.Dir(p), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAppendInterviewEventKeepsAFinalLineThatHasNoLineFeed(t *testing.T) {
	ev := InterviewEvent{TS: "t", SessionID: "s", Event: ScanCompleted, RoundID: 2}
	row := appendedRow(t, ev)
	scan := strings.TrimSuffix(appendedRow(t, InterviewEvent{TS: "t", SessionID: "s", Event: ScanStarted, RoundID: 1}), "\n")
	for name, c := range map[string]struct {
		before, separator string
		scans             int // the scan rows ReadInterviewEvents returns afterwards
	}{
		"scan row":          {scan, "\n", 2},
		"answer ledger row": {answerLedgerRow, "\n", 1},
		"partial row":       {`{"partial":`, "\n", 1},
		"lone CR":           {answerLedgerRow + "\r", "\n", 1},
		"terminated row":    {answerLedgerRow + "\n", "", 1}, // a complete line gets no blank line before the row
		"empty file":        {"", "", 1},
	} {
		cwd := t.TempDir()
		seedInterviews(t, cwd, "s", c.before)
		if err := AppendInterviewEvent(cwd, ev); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got, want := fileText(t, interviewLedgerPath(cwd, "s")), c.before+c.separator+row; got != want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, want)
		}
		if got := ReadInterviewEvents(cwd, "s"); len(got) != c.scans || got[len(got)-1].RoundID != 2 {
			t.Errorf("%s: read %+v, want %d scan rows ending with round 2", name, got, c.scans)
		}
	}
}

func TestAppendInterviewEventToAFileWhoseTailCannotBeRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a write-only file")
	}
	ev := InterviewEvent{TS: "t", SessionID: "s", Event: ScanCompleted, RoundID: 2}
	row := appendedRow(t, ev)
	for name, c := range map[string]struct{ before, want string }{
		"unterminated": {answerLedgerRow, answerLedgerRow + "\n" + row}, // the tail is unknown, so the row starts on a new line
		"empty":        {"", row},                                       // an empty file has no tail to protect
	} {
		cwd := t.TempDir()
		seedInterviews(t, cwd, "s", c.before)
		p := interviewLedgerPath(cwd, "s")
		if err := os.Chmod(p, 0o200); err != nil {
			t.Fatal(err)
		}
		if f, err := os.Open(p); err == nil {
			_ = f.Close()
			t.Skip("the file stays readable")
		}
		err := AppendInterviewEvent(cwd, ev)
		if chmodErr := os.Chmod(p, 0o600); chmodErr != nil {
			t.Fatal(chmodErr)
		}
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := fileText(t, p); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", name, got, c.want)
		}
	}
}
