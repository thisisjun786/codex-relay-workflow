package ledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/interview"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// The B-class tests of CXC v0.2.40 pabcd-state/test/interview-ledger.test.ts (lines 45-228) in Go, then the Go-specific ones:
// reading rules, provenance, the ledger shared with the state package's scan rows, the repaired append, the clock and the
// row text. Payloads are written as JSON with single quotes and decoded as the hook's reader does (numbers stay json.Number).

const (
	toolInputJSON    = "{'questions':[{'id':'q_scope','header':'Scope','question':'How wide should the rescan be?'},{'id':'q_chat','header':'ChatSearch','question':'Remove or keep chat-search?'}]}"
	toolResponseJSON = "{'answers':{'q_scope':{'answers':['adaptive 1-N']},'q_chat':{'answers':['remove it']}}}"
)

func j(s string) string { return strings.ReplaceAll(s, "'", "\"") }

func decode(t *testing.T, s string) any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(j(s)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("%s: %v", s, err)
	}
	return v
}

func fixed() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

func run(t *testing.T, cwd, session, turn, input, response string) CaptureResult {
	t.Helper()
	return capture(CaptureInput{Cwd: cwd, SessionID: session, TurnID: turn, ToolInput: decode(t, input), ToolResponse: decode(t, response)}, fixed)
}

func ids(events []QaEvent) []string {
	out := []string{}
	for _, e := range events {
		out = append(out, e.EventID)
	}
	return out
}

func writeLedger(t *testing.T, cwd, session, content string) {
	t.Helper()
	p := ledgerPath(cwd, session)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readLedger(t *testing.T, cwd, session string) string {
	t.Helper()
	b, err := os.ReadFile(ledgerPath(cwd, session))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func wantEq(t *testing.T, what string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s = %#v, want %#v", what, got, want)
	}
}

func TestParseExtractsTheRequestUserInputShape(t *testing.T) {
	qs := ParseQuestions(decode(t, toolInputJSON))
	wantEq(t, "questions", qs, []ParsedQuestion{{"q_scope", "How wide should the rescan be?"}, {"q_chat", "Remove or keep chat-search?"}})
	wantEq(t, "answers", ParseAnswers(decode(t, toolResponseJSON))["q_scope"], []string{"adaptive 1-N"})
}

func TestParseAcceptsJSONStringPayloads(t *testing.T) {
	wantEq(t, "string response", ParseAnswers(j(toolResponseJSON))["q_chat"], []string{"remove it"})
	wantEq(t, "string input", ParseQuestions(j(toolInputJSON)), []ParsedQuestion{{"q_scope", "How wide should the rescan be?"}, {"q_chat", "Remove or keep chat-search?"}})
}

func TestCaptureRecordsQuestionAndAnswerPerQuestion(t *testing.T) {
	cwd := t.TempDir()
	res := run(t, cwd, "sess-1", "turn-1", toolInputJSON, toolResponseJSON)
	want := []string{"turn-1:q_scope:question_asked", "turn-1:q_scope:answer_recorded", "turn-1:q_chat:question_asked", "turn-1:q_chat:answer_recorded"}
	wantEq(t, "written", ids(res.Written), want)
	events := ReadQaEvents(cwd, "sess-1")
	wantEq(t, "read back", ids(events), want)
	if len(events) == 4 {
		wantEq(t, "q_chat answers", events[3].Answers, []string{"remove it"})
		wantEq(t, "q_scope question", events[0].Question, "How wide should the rescan be?")
	}
}

func TestCaptureHandlesStringPayloads(t *testing.T) {
	// the host may hand over either side, or both, as a JSON string (260802 wp2: 222 question_asked rows and no answer_recorded)
	for name, p := range map[string][2]any{
		"string response": {decode(t, toolInputJSON), j(toolResponseJSON)},
		"string round":    {j(toolInputJSON), j(toolResponseJSON)},
	} {
		cwd := t.TempDir()
		res := capture(CaptureInput{Cwd: cwd, SessionID: "s", TurnID: "turn-1", ToolInput: p[0], ToolResponse: p[1]}, fixed)
		if len(res.Written) != 4 || len(ReadQaEvents(cwd, "s")) != 4 {
			t.Errorf("%s: wrote %d rows, want 4", name, len(res.Written))
		}
	}
}

func TestMalformedStringPayloadsStayTotal(t *testing.T) {
	for _, bad := range []string{"{", "null", "[]", "[1,2]", "5", "\"str\"", ""} {
		if got := ParseAnswers(bad); len(got) != 0 {
			t.Errorf("ParseAnswers(%q) = %v", bad, got)
		}
		if got := ParseQuestions(bad); len(got) != 0 {
			t.Errorf("ParseQuestions(%q) = %v", bad, got)
		}
	}
	cwd := t.TempDir()
	if res := capture(CaptureInput{Cwd: cwd, SessionID: "s", TurnID: "t", ToolInput: "{", ToolResponse: "{"}, fixed); len(res.Written) != 0 {
		t.Errorf("wrote %d rows for broken payloads", len(res.Written))
	}
}

func TestParseAnswersDecodesEveryTransportShape(t *testing.T) {
	body := "{'answers':{'q_scope':{'answers':['adaptive 1-N']}}}"
	encoded, _ := json.Marshal(j(body))
	shapes := map[string]any{
		"plain object":  decode(t, body),
		"json string":   j(body),
		"double encode": string(encoded),
		"array of one":  []any{decode(t, body)},
		"content block": []any{map[string]any{"type": "input_text", "text": j(body)}},
		"output string": map[string]any{"output": j(body)},
		"text object":   map[string]any{"text": decode(t, body)},
	}
	for name, v := range shapes {
		wantEq(t, name, ParseAnswers(v)["q_scope"], []string{"adaptive 1-N"})
	}
}

func TestUnrecoverableShapesDegradeToEmpty(t *testing.T) {
	for _, bad := range []any{nil, json.Number("42"), 42, true, "plain prose, not json", []any{}, []any{nil}, map[string]any{}} {
		if got := ParseAnswers(bad); got == nil || len(got) != 0 {
			t.Errorf("ParseAnswers(%#v) = %#v, want an empty map", bad, got)
		}
	}
}

func TestReservedQuestionIDsCannotForgeAnAnswer(t *testing.T) {
	hostile := "{'answers':{'__proto__':{'answers':['pwn']},'constructor':{'answers':['c']},'prototype':{'answers':['p']},'q_real':{'answers':['ok']}}}"
	out := ParseAnswers(j(hostile))
	wantEq(t, "answers", out, map[string][]string{"q_real": {"ok"}})
	cwd := t.TempDir()
	in := "{'questions':[{'id':'__proto__','question':'a'},{'id':'constructor','question':'b'},{'id':'prototype','question':'c'},{'id':'q_real','question':'d'}]}"
	res := run(t, cwd, "s", "t", in, hostile)
	wantEq(t, "written", ids(res.Written), []string{"t:__proto__:question_asked", "t:constructor:question_asked", "t:prototype:question_asked", "t:q_real:question_asked", "t:q_real:answer_recorded"})
}

func TestCaptureIsIdempotentPerTurnQuestionKind(t *testing.T) {
	cwd := t.TempDir()
	if n := len(run(t, cwd, "s", "t1", toolInputJSON, toolResponseJSON).Written); n != 4 {
		t.Fatalf("first capture wrote %d", n)
	}
	if n := len(run(t, cwd, "s", "t1", toolInputJSON, toolResponseJSON).Written); n != 0 {
		t.Errorf("a re-fired round wrote %d rows", n)
	}
	if n := len(ReadQaEvents(cwd, "s")); n != 4 {
		t.Errorf("%d rows after the re-fire", n)
	}
	if n := len(run(t, cwd, "s", "t2", toolInputJSON, toolResponseJSON).Written); n != 4 {
		t.Errorf("a new turn wrote %d rows", n)
	}
}

func TestDeriveEventID(t *testing.T) {
	wantEq(t, "id", DeriveEventID("t", "q", QuestionAsked), "t:q:question_asked")
	if DeriveEventID("t", "q", QuestionAsked) == DeriveEventID("t", "q", AnswerRecorded) {
		t.Error("the two kinds share an id")
	}
}

func TestQuestionWithoutAnAnswerWritesOnlyTheQuestion(t *testing.T) {
	res := run(t, t.TempDir(), "s", "t", "{'questions':[{'id':'q1','question':'unanswered?'}]}", "{'answers':{}}")
	if len(res.Written) != 1 || res.Written[0].Event != QuestionAsked {
		t.Errorf("written = %+v", res.Written)
	}
}

func TestMalformedPayloadsFailSafe(t *testing.T) {
	cwd := t.TempDir()
	if res := capture(CaptureInput{Cwd: cwd, SessionID: "s", TurnID: "t", ToolInput: nil, ToolResponse: "garbage"}, fixed); len(res.Written) != 0 {
		t.Errorf("wrote %d rows for garbage", len(res.Written))
	}
	// no session id: nothing is written, not even the state directory
	if res := run(t, cwd, "", "t", toolInputJSON, toolResponseJSON); len(res.Written) != 0 {
		t.Errorf("wrote %d rows without a session id", len(res.Written))
	}
	if _, err := os.Stat(filepath.Join(cwd, ".crw")); err == nil {
		t.Error("the state directory was created")
	}
}

func TestReadQaEventsKeepsTheRowAndSkipsWhatIsNotOne(t *testing.T) {
	cwd := t.TempDir()
	rows := []string{
		"", "  ", "{bad", "[]", "5", "null",
		"{'event':'question_asked','eventId':'a','turnId':'t','questionId':'q','question':'?','extra':1}",
		"{'event':'answer_recorded','eventId':'b','answers':['x',1,null,'y']}",
		"{'event':'answer_recorded','eventId':'c','answers':'x'}",
		"{'event':'question_asked'}", "{'event':'scan_completed','eventId':'d'}", "{'event':'question_asked','eventId':5}",
	}
	writeLedger(t, cwd, "s", j(strings.Join(rows, "\r\n"))+"\n")
	events := ReadQaEvents(cwd, "s")
	wantEq(t, "ids", ids(events), []string{"a", "b", "c"})
	if len(events) != 3 {
		return
	}
	wantEq(t, "typed fields", []any{events[0].TurnID, events[0].QuestionID, events[0].Question, events[0].Answers == nil}, []any{"t", "q", "?", true})
	wantEq(t, "string answers only", events[1].Answers, []string{"x", "y"})
	wantEq(t, "answers that are no array", events[2].Answers, []string(nil))
	if !strings.Contains(string(events[0].Raw), "\"extra\":1") {
		t.Errorf("Raw = %s", events[0].Raw)
	}
}

func TestDimensionsBackedByAnswers(t *testing.T) {
	// the rules of the oracle's provenance check are replayed in oracle_test.go (the read_ cases); this one pins the result type
	cwd := t.TempDir()
	run(t, cwd, "s", "t1", toolInputJSON, toolResponseJSON)
	scan := state.InterviewEvent{TS: "t", SessionID: "s", Event: state.ScanCompleted, RoundID: 1, Map: []state.MapEntry{{QuestionID: "q_scope", Dimension: "goal"}, {QuestionID: "q_chat", Dimension: "success"}}}
	if err := state.AppendInterviewEvent(cwd, scan); err != nil {
		t.Fatal(err)
	}
	wantEq(t, "backed", DimensionsBackedByAnswers(cwd, "s"), map[interview.Dimension]bool{"goal": true, "success": true})
	wantEq(t, "no file", DimensionsBackedByAnswers(t.TempDir(), "s"), map[interview.Dimension]bool{})
}
func TestTheLedgerIsSharedWithTheStatePackagesScanRows(t *testing.T) {
	cwd := t.TempDir()
	run(t, cwd, "s", "t1", toolInputJSON, toolResponseJSON)
	scan := state.InterviewEvent{TS: "t", SessionID: "s", Event: state.ScanCompleted, RoundID: 1}
	if err := state.AppendInterviewEvent(cwd, scan); err != nil {
		t.Fatal(err)
	}
	if n := len(run(t, cwd, "s", "t2", toolInputJSON, toolResponseJSON).Written); n != 4 {
		t.Fatalf("second round wrote %d rows", n)
	}
	if n := len(ReadQaEvents(cwd, "s")); n != 8 {
		t.Errorf("ReadQaEvents read %d rows, want the 8 question and answer rows", n)
	}
	if got := state.ReadInterviewEvents(cwd, "s"); len(got) != 1 || got[0].Event != state.ScanCompleted {
		t.Errorf("ReadInterviewEvents read %+v, want the one scan row", got)
	}
}
func TestAppendKeepsAFinalLineThatHasNoLineFeed(t *testing.T) {
	// The oracle joins the new row to such a line and loses both (data loss, repaired: known-defects, port: fixed). Whatever the
	// old tail is, the round that follows is read in full, and a valid old row stays one of the rows.
	row := j("{'ts':'t','sessionId':'s','turnId':'t1','event':'question_asked','questionId':'q_scope','eventId':'t1:q_scope:question_asked','question':'How wide?'}")
	want := []string{"t1:q_scope:question_asked", "t1:q_scope:answer_recorded", "t1:q_chat:question_asked", "t1:q_chat:answer_recorded"}
	for name, tail := range map[string]string{"valid row": row, "partial row": "{\"partial\":", "lone CR": row + "\r", "terminated row": row + "\n"} {
		cwd := t.TempDir()
		writeLedger(t, cwd, "s", tail)
		run(t, cwd, "s", "t1", toolInputJSON, toolResponseJSON)
		wantEq(t, name+": rows read", ids(ReadQaEvents(cwd, "s")), want)
		if strings.Contains(readLedger(t, cwd, "s"), "\n\n") {
			t.Errorf("%s: a blank line was written", name)
		}
	}
	cwd := t.TempDir() // a new file gets no leading line feed
	run(t, cwd, "s", "t1", toolInputJSON, toolResponseJSON)
	if got := readLedger(t, cwd, "s"); !strings.HasPrefix(got, "{") || strings.Contains(got, "\n\n") {
		t.Errorf("a new file reads %q", got)
	}
}

func TestAppendToAFileWhoseTailCannotBeRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a write-only file")
	}
	ev := QaEvent{TS: "t", SessionID: "s", TurnID: "t1", Event: QuestionAsked, QuestionID: "q", EventID: "t1:q:question_asked", Question: "?"}
	line := "{\"ts\":\"t\",\"sessionId\":\"s\",\"turnId\":\"t1\",\"event\":\"question_asked\",\"questionId\":\"q\",\"eventId\":\"t1:q:question_asked\",\"question\":\"?\"}\n"
	old := j("{'event':'question_asked','questionId':'p','eventId':'t0:p:question_asked'}")
	for name, c := range map[string]struct{ before, want string }{"unterminated": {old, old + "\n" + line}, "empty": {"", line}} {
		cwd := t.TempDir()
		writeLedger(t, cwd, "s", c.before)
		p := ledgerPath(cwd, "s")
		if err := os.Chmod(p, 0o200); err != nil {
			t.Fatal(err)
		}
		if f, err := os.Open(p); err == nil {
			_ = f.Close()
			t.Skip("the file stays readable")
		}
		if err := appendEvent(cwd, ev); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := os.Chmod(p, 0o600); err != nil {
			t.Fatal(err)
		}
		wantEq(t, name, readLedger(t, cwd, "s"), c.want)
	}
}

func TestAFifoIsOpenedForWritingOnlyAndGetsNoLineFeed(t *testing.T) {
	cwd := t.TempDir()
	p := ledgerPath(cwd, "s")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Skip("no fifo:", err)
	}
	got := make(chan string, 1)
	go func() { b, _ := os.ReadFile(p); got <- string(b) }()
	ev := QaEvent{TS: "t", SessionID: "s", TurnID: "t1", Event: AnswerRecorded, QuestionID: "q", EventID: "t1:q:answer_recorded", Answers: []string{}}
	if err := appendEvent(cwd, ev); err != nil {
		t.Fatal(err)
	}
	select {
	case s := <-got:
		wantEq(t, "bytes", s, "{\"ts\":\"t\",\"sessionId\":\"s\",\"turnId\":\"t1\",\"event\":\"answer_recorded\",\"questionId\":\"q\",\"eventId\":\"t1:q:answer_recorded\",\"answers\":[]}\n")
	case <-time.After(5 * time.Second):
		t.Fatal("the reader got nothing")
	}
}

func TestAFailingAppendIsSwallowed(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, ".crw"), []byte("a file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res := run(t, cwd, "s", "t", toolInputJSON, toolResponseJSON); len(res.Written) != 0 {
		t.Errorf("reported %d rows written", len(res.Written))
	}
}

func TestTheClockIsReadOncePerEventBuilt(t *testing.T) {
	cwd, calls := t.TempDir(), 0
	tick := func() time.Time { calls++; return fixed().Add(time.Duration(calls) * time.Second) }
	in := CaptureInput{Cwd: cwd, SessionID: "s", TurnID: "t", ToolInput: decode(t, toolInputJSON), ToolResponse: decode(t, toolResponseJSON)}
	capture(in, tick)
	capture(in, tick) // every id is recorded: no event is built, the clock is not read
	events := ReadQaEvents(cwd, "s")
	var stamps []string
	for _, e := range events {
		stamps = append(stamps, e.TS)
	}
	wantEq(t, "stamps", stamps, []string{"2026-01-01T00:00:01.000Z", "2026-01-01T00:00:02.000Z", "2026-01-01T00:00:03.000Z", "2026-01-01T00:00:04.000Z"})
	if calls != 4 {
		t.Errorf("the clock was read %d times, want 4", calls)
	}
}

func TestSessionIDsThatSanitiseAlikeShareOneLedger(t *testing.T) { // oracle behaviour, port: kept (known-defects)
	cwd := t.TempDir()
	run(t, cwd, "a/b", "t", toolInputJSON, toolResponseJSON)
	if n := len(run(t, cwd, "a-b", "t", toolInputJSON, toolResponseJSON).Written); n != 0 {
		t.Errorf("the alias session wrote %d rows", n)
	}
}
func TestRowTextFollowsJSONStringify(t *testing.T) {
	// the escapes of a hazardous text are replayed against the oracle (capture_escapes); an invalid UTF-8 byte has no oracle value
	wantEq(t, "invalid byte", quote("bad \xff byte"), "\"bad \ufffd byte\"")
	wantEq(t, "literals", quote("\x7f\u2028\u2029\\u2028"), "\"\x7f\u2028\u2029\\\\u2028\"")
}
func TestALoneSurrogateEscapeReadsAsTheReplacementCharacter(t *testing.T) {
	// a Go string cannot hold one (documented limit): two ids that differ only in lone surrogates read as one
	qs := ParseQuestions("{\"questions\":[{\"id\":\"a\\ud800\",\"question\":\"b\\udc00\"}]}")
	wantEq(t, "question", qs, []ParsedQuestion{{"a\ufffd", "b\ufffd"}})
}
