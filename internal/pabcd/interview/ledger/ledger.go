package ledger

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/interview"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// QaKind is the kind of a question or answer row.
type QaKind string

// The two kinds of row this package writes and reads; the scan rows of the state package are another kind.
const (
	QuestionAsked  QaKind = "question_asked"
	AnswerRecorded QaKind = "answer_recorded"
)

// QaEvent is one row. As read, a key the row lacks or holds with another type reads as zero, Answers is nil unless the row's answers
// is an array and then holds its string elements, and Raw is the trimmed line, with every key the fields leave out. As written, a
// question_asked row carries Question (an empty one included) and an answer_recorded row carries Answers (an empty one included).
type QaEvent struct {
	TS, SessionID, TurnID string
	Event                 QaKind
	QuestionID, EventID   string // EventID is DeriveEventID of the turn, question and kind
	Question              string
	Answers               []string
	Raw                   json.RawMessage
}

// DeriveEventID is the idempotency key of a row, turn:question:kind, so a re-fired hook in one turn records nothing twice. It is not
// injective: ("t:q", "r") and ("t", "q:r") share one id.
func DeriveEventID(turnID, questionID string, kind QaKind) string {
	return turnID + ":" + questionID + ":" + string(kind)
}

// ledgerPath is cwd/.crw/interviews/<sanitised session id>.jsonl, the file of the state package's scan rows too; sessions whose ids
// sanitise alike share it.
func ledgerPath(cwd, sessionID string) string {
	return filepath.Join(cwd, crwdir.DirName, state.InterviewsSubdir, state.SanitizeKey(sessionID)+".jsonl")
}

// ReadQaEvents is the session's question and answer rows, best effort: a row counts when its event is one of the two kinds and its
// eventId is a string; a missing file, blank and damaged lines and every other row (the scan rows included) are skipped. Never nil.
func ReadQaEvents(cwd, sessionID string) []QaEvent {
	events := []QaEvent{}
	eachRow(cwd, sessionID, func(line string, o map[string]any) {
		kind, _ := o["event"].(string)
		eventID, ok := o["eventId"].(string)
		if (kind != string(QuestionAsked) && kind != string(AnswerRecorded)) || !ok {
			return
		}
		e := QaEvent{Event: QaKind(kind), EventID: eventID, Raw: json.RawMessage(line)}
		e.TS, _ = o["ts"].(string)
		e.SessionID, _ = o["sessionId"].(string)
		e.TurnID, _ = o["turnId"].(string)
		e.QuestionID, _ = o["questionId"].(string)
		e.Question, _ = o["question"].(string)
		if list, ok := o["answers"].([]any); ok {
			e.Answers = []string{}
			for _, a := range list {
				if s, ok := a.(string); ok {
					e.Answers = append(e.Answers, s)
				}
			}
		}
		events = append(events, e)
	})
	return events
}

// DimensionsBackedByAnswers is the dimensions that hold a question the user was asked and answered: the file holds the question, an
// answer with a non-blank string, and a scan_completed row whose map attributes the question to the dimension (the last non-empty
// string any such row gave it). The rows need no eventId, turn or session. Provenance, not tamper-proofing: whoever can write the
// file can append the three rows.
func DimensionsBackedByAnswers(cwd, sessionID string) map[interview.Dimension]bool {
	asked, answered, attribution := map[string]bool{}, map[string]bool{}, map[string]string{}
	eachRow(cwd, sessionID, func(_ string, o map[string]any) {
		event, _ := o["event"].(string)
		questionID, hasID := o["questionId"].(string)
		switch {
		case event == string(QuestionAsked) && hasID:
			asked[questionID] = true
		case event == string(AnswerRecorded) && hasID:
			list, _ := o["answers"].([]any)
			for _, a := range list {
				if s, ok := a.(string); ok && text.Trim(s) != "" {
					answered[questionID] = true
				}
			}
		case event == "scan_completed":
			m, _ := o["map"].(map[string]any)
			for q, dimension := range m {
				if s, ok := dimension.(string); ok && s != "" {
					attribution[q] = s
				}
			}
		}
	})
	backed := map[interview.Dimension]bool{}
	for q, dimension := range attribution {
		if asked[q] && answered[q] {
			backed[interview.Dimension(dimension)] = true
		}
	}
	return backed
}

// alreadyRecorded re-reads the file for every event, as the oracle does: no cache and no lock.
func alreadyRecorded(cwd, sessionID, eventID string) bool {
	for _, e := range ReadQaEvents(cwd, sessionID) {
		if e.EventID == eventID {
			return true
		}
	}
	return false
}

// line is the row as JSON.stringify writes the oracle's object: its keys in construction order, then a line feed.
func (e QaEvent) line() string {
	members := [][2]string{{"ts", quote(e.TS)}, {"sessionId", quote(e.SessionID)}, {"turnId", quote(e.TurnID)}, {"event", quote(string(e.Event))},
		{"questionId", quote(e.QuestionID)}, {"eventId", quote(e.EventID)}}
	if e.Event == QuestionAsked {
		members = append(members, [2]string{"question", quote(e.Question)})
	} else {
		answers := make([]string, len(e.Answers))
		for i, a := range e.Answers {
			answers[i] = quote(a)
		}
		members = append(members, [2]string{"answers", "[" + strings.Join(answers, ",") + "]"})
	}
	parts := make([]string, len(members))
	for i, m := range members {
		parts[i] = quote(m[0]) + ":" + m[1]
	}
	return "{" + strings.Join(parts, ",") + "}\n"
}

// quote is JSON.stringify of a string (ECMAScript QuoteJSONString): quote, backslash and the five short controls escaped, the other
// controls below U+0020 as lowercase \u00xx, everything else literal, U+007F, U+2028 and U+2029 too. An invalid UTF-8 byte is U+FFFD.
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\b':
			b.WriteString("\\b")
		case '\f':
			b.WriteString("\\f")
		case '\n':
			b.WriteString("\\n")
		case '\r':
			b.WriteString("\\r")
		case '\t':
			b.WriteString("\\t")
		default:
			if r < 0x20 {
				fmt.Fprintf(&b, "\\u%04x", r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// appendEvent appends the row to its session's ledger: the .crw directory, the interviews directory below it, then one write to a file
// opened for appending, as the oracle does it; an error leaves what the earlier steps made. The write starts with a line feed when the
// file already ends in a line that has none (endsMidLine).
func appendEvent(cwd string, e QaEvent) error {
	if _, err := crwdir.EnsureDir(cwd); err != nil {
		return err
	}
	path := ledgerPath(cwd, e.SessionID)
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o666)
	if err != nil {
		return err
	}
	line := e.line()
	if endsMidLine(f, path) {
		line = "\n" + line
	}
	_, err = f.WriteString(line)
	return errors.Join(err, f.Close())
}

// endsMidLine reports whether the open file is a non-empty regular file whose last byte is not a line feed, or whose last byte cannot
// be learned (a write-only file, a failed read): the conservative answer, since a blank line costs nothing and a joined line loses rows.
// A new or empty file, a FIFO or a device has no tail to protect.
func endsMidLine(f *os.File, path string) bool {
	info, err := f.Stat()
	if err != nil {
		return true
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return false
	}
	r, err := os.Open(path)
	if err != nil {
		return true
	}
	defer func() { _ = r.Close() }()
	var last [1]byte
	n, _ := r.ReadAt(last[:], info.Size()-1) // n first: a byte read with io.EOF is a byte read
	return n != 1 || last[0] != '\n'
}

// CaptureInput is one request_user_input round: the project directory, the session and turn, and the tool's input and response as
// the hook received them.
type CaptureInput struct {
	Cwd, SessionID, TurnID  string
	ToolInput, ToolResponse any
}

// CaptureResult is the rows written by one capture, after the dedup check; never nil.
type CaptureResult struct{ Written []QaEvent }

// CaptureInterviewAnswers records one round: a question_asked row per question and an answer_recorded row per answered question,
// each skipped when its id is already in the file (idempotent per turn, question and kind). A round without a session id records
// nothing and a missing turn id records as no-turn. A failed append loses that row only; it is never an error.
func CaptureInterviewAnswers(in CaptureInput) CaptureResult { return capture(in, time.Now) }

func capture(in CaptureInput, now func() time.Time) CaptureResult {
	written := []QaEvent{}
	if in.SessionID == "" {
		return CaptureResult{written}
	}
	turn := in.TurnID
	if turn == "" {
		turn = "no-turn"
	}
	answers := ParseAnswers(in.ToolResponse)
	record := func(e QaEvent) {
		if alreadyRecorded(in.Cwd, in.SessionID, e.EventID) {
			return
		}
		e.TS, e.SessionID, e.TurnID = now().UTC().Format("2006-01-02T15:04:05.000Z"), in.SessionID, turn
		if appendEvent(in.Cwd, e) == nil {
			written = append(written, e)
		}
	}
	for _, q := range ParseQuestions(in.ToolInput) {
		record(QaEvent{Event: QuestionAsked, QuestionID: q.QuestionID, EventID: DeriveEventID(turn, q.QuestionID, QuestionAsked), Question: q.Question})
		if ans, ok := answers[q.QuestionID]; ok {
			record(QaEvent{Event: AnswerRecorded, QuestionID: q.QuestionID, EventID: DeriveEventID(turn, q.QuestionID, AnswerRecorded), Answers: ans})
		}
	}
	return CaptureResult{written}
}
