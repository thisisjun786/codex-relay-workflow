package state

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// The transition ledger and the interview scan events: appendLedger, appendInterviewEvent and readInterviewEvents of CXC v0.2.40
// pabcd-state/src/state.ts (216-242, 696-795). A row is one line appended with no lock and no temp file, as the oracle does it;
// the callers that need one hold their own lock. One departure: a scan row starts on a new line when the file's last line has no line
// feed (CRW-474), where the oracle joins it to that line and both rows are lost.

// LedgerFile is the transition ledger under the state directory; InterviewsSubdir holds one scan ledger per session.
const (
	LedgerFile       = "ledger.jsonl"
	InterviewsSubdir = "interviews"
)

// ScanEvidence is the scan snapshot an I->P override records; the counters are JavaScript numbers.
type ScanEvidence struct{ ScanRounds, HighContradictionCount float64 }

// CloseKey is the dedup key of a D-close row (check epoch, closed work phase); either is JSON null when nil.
type CloseKey struct{ CheckEpoch, ClosedWorkPhaseID *string }

// LedgerEntry is one row of the transition ledger. JSON.stringify keeps the order its caller built the object in, and callers
// differ in one place: evidence trails the row (orchestrate-cli.ts:655, :899, :1040, :1123, orchestrate-apply.ts:150 and :189) or
// follows reason (evidence-cli.ts:88; the hook's close rows, which spread the transition row, evidence included, before the close
// key). EvidenceAfterReason picks the second order; the other keys keep one place: actor, override, scanEvidence, close key.
type LedgerEntry struct {
	TS                  string
	SessionID           string
	From                *Phase // null when nil
	To                  Phase
	Reason              string
	Evidence            *string
	EvidenceAfterReason bool
	Actor               string // "human" or "agent"; the key is absent when empty
	Override            *bool
	ScanEvidence        *ScanEvidence
	Close               *CloseKey
}

// members lists the row's keys in the oracle's order. It is not a MarshalJSON on purpose: json.Marshal would escape HTML and
// U+2028, which JSON.stringify does not.
func (e LedgerEntry) members() []member {
	m := []member{{"ts", e.TS}, {"sessionId", e.SessionID}, {"from", e.From}, {"to", e.To}, {"reason", e.Reason}}
	evidence := func() {
		if e.Evidence != nil {
			m = append(m, member{"evidence", *e.Evidence})
		}
	}
	if e.EvidenceAfterReason {
		evidence()
	}
	if e.Actor != "" {
		m = append(m, member{"actor", e.Actor})
	}
	if e.Override != nil {
		m = append(m, member{"override", *e.Override})
	}
	if e.ScanEvidence != nil {
		m = append(m, member{"scanEvidence", []member{{"scanRounds", jsNumber(e.ScanEvidence.ScanRounds)}, {"highContradictionCount", jsNumber(e.ScanEvidence.HighContradictionCount)}}})
	}
	if e.Close != nil {
		m = append(m, member{"checkEpoch", e.Close.CheckEpoch}, member{"closedWorkPhaseId", e.Close.ClosedWorkPhaseID})
	}
	if !e.EvidenceAfterReason {
		evidence()
	}
	return m
}

// AppendLedger appends the row to <state dir>/ledger.jsonl (appendLedger). The counters of a row are written as JSON.stringify
// writes a number: -0 as 0, NaN and the infinities as null.
func AppendLedger(cwd string, e LedgerEntry) error {
	return appendRow(cwd, "", LedgerFile, e.members(), false)
}

// InterviewScanEvent is the kind of a scan row. The scan ledger is shared with the interview ledger's question and answer rows,
// which the reader skips; the oracle's SCAN_EVENT_KINDS is the isScanKind test of that reader.
type InterviewScanEvent string

// The scan kinds.
const (
	ScanStarted     InterviewScanEvent = "scan_started"
	ScanCompleted   InterviewScanEvent = "scan_completed"
	RescanCompleted InterviewScanEvent = "rescan_completed"
)

func (k InterviewScanEvent) isScanKind() bool {
	switch k {
	case ScanStarted, ScanCompleted, RescanCompleted:
		return true
	}
	return false
}

// MapEntry attributes a question id to a dimension.
type MapEntry struct{ QuestionID, Dimension string }

// InterviewEvent is a scan row; the counters are JavaScript numbers, written as JSON.stringify writes them (-0 as 0, NaN and the
// infinities as null; the reader skips a row whose roundId or contradictionCount is null). Map lists the attributions in the order they were assigned to the JavaScript
// object (nil leaves the key out, an empty list writes {}). The oracle's reader returns the parsed object; a read event keeps it
// as Raw, the line as written (trimmed), beside the typed fields, which read as zero when the key is absent or mistyped, and Map,
// the attributions in the order their keys were written (nil when the row has no map or a value in it is not a string). Raw keeps
// every key the typed fields do not; nothing in v0.2.40 consumes the result.
type InterviewEvent struct {
	TS                     string
	SessionID              string
	Event                  InterviewScanEvent
	RoundID                float64
	ContradictionCount     float64
	HighContradictionCount float64
	Map                    []MapEntry
	Raw                    json.RawMessage
}

// members lists the row as scan-cli.ts:325 builds it; the map keys come in JavaScript's own-key order.
func (e InterviewEvent) members() []member {
	m := []member{{"ts", e.TS}, {"sessionId", e.SessionID}, {"event", e.Event}, {"roundId", jsNumber(e.RoundID)},
		{"contradictionCount", jsNumber(e.ContradictionCount)}, {"highContradictionCount", jsNumber(e.HighContradictionCount)}}
	if e.Map != nil {
		attributions := []member{}
		for _, p := range jsKeyOrder(e.Map) {
			attributions = append(attributions, member{p.QuestionID, p.Dimension})
		}
		m = append(m, member{"map", attributions})
	}
	return m
}

// AppendInterviewEvent appends the row to the session's scan ledger (appendInterviewEvent), the durable record that a scan ran.
// The file is named by SanitizeKey of the row's session id, as the oracle names it. The file is shared with the interview answer
// ledger's rows; the row starts with a line feed when the file ends in a line that has none, or when its last byte cannot be read,
// so a final line left by a crash, a full disk or a hand edit is not joined to this row. No lock is taken, as in the oracle: two
// appenders that meet one unterminated tail each add a line feed, and the blank line between the rows is skipped by both readers.
func AppendInterviewEvent(cwd string, e InterviewEvent) error {
	return appendRow(cwd, InterviewsSubdir, SanitizeKey(e.SessionID)+".jsonl", e.members(), true)
}

// ReadInterviewEvents reads a session's scan rows, best effort: a file that cannot be read is no rows, and the result is never
// nil. A row counts when its event is a scan kind and roundId and contradictionCount are numbers; blank and damaged lines and the
// rest are skipped. Like every reader of this package it parses with encoding/json, which refuses a document nested deeper than
// 10,000 levels: a row that deep reads as damaged, where JSON.parse's limit is the stack.
func ReadInterviewEvents(cwd, sessionID string) []InterviewEvent {
	events := []InterviewEvent{}
	data, err := os.ReadFile(filepath.Join(cwd, crwdir.DirName, InterviewsSubdir, SanitizeKey(sessionID)+".jsonl"))
	if err != nil {
		return events
	}
	for _, line := range text.SplitLines(string(data)) {
		line = text.Trim(line) // JavaScript's trim(), which strips U+FEFF
		row := decodeObject([]byte(line))
		kind, _ := row["event"].(string)
		round, okRound := number(row["roundId"])
		count, okCount := number(row["contradictionCount"])
		if row == nil || !okRound || !okCount || !InterviewScanEvent(kind).isScanKind() {
			continue
		}
		e := InterviewEvent{Event: InterviewScanEvent(kind), RoundID: round, ContradictionCount: count, Raw: json.RawMessage(line)}
		e.TS, _ = row["ts"].(string)
		e.SessionID, _ = row["sessionId"].(string)
		e.HighContradictionCount, _ = number(row["highContradictionCount"])
		e.Map = attributions(line)
		events = append(events, e)
	}
	return events
}

// attributions is the map of a scan row as the oracle's parsed object holds it, in the order its keys were written: nil when the row
// has none or a value in it is not a string (Raw keeps what the row says), empty and not nil for {}. AppendInterviewEvent puts the
// pairs through jsKeyOrder, so a read event appended again writes the same object.
func attributions(line string) []MapEntry {
	var members map[string]json.RawMessage // an exact key match, and the last of a repeated key, as JSON.parse reads it
	if json.Unmarshal([]byte(line), &members) != nil {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(members["map"]))
	if open, err := dec.Token(); err != nil || open != json.Delim('{') {
		return nil
	}
	pairs := []MapEntry{}
	for dec.More() {
		key, _ := dec.Token()
		var value any
		if err := dec.Decode(&value); err != nil {
			return nil
		}
		dimension, isString := value.(string)
		if !isString {
			return nil
		}
		pairs = append(pairs, MapEntry{key.(string), dimension})
	}
	return pairs
}

// jsKeyOrder is the order JavaScript lists an object built by assigning the pairs in sequence: the canonical array indexes
// ascending, then the other keys as first assigned; a repeated key keeps its first place and takes its last value.
func jsKeyOrder(pairs []MapEntry) []MapEntry {
	last, out := map[string]string{}, []MapEntry{}
	for _, p := range pairs {
		if _, seen := last[p.QuestionID]; !seen {
			out = append(out, p)
		}
		last[p.QuestionID] = p.Dimension
	}
	for i := range out {
		out[i].Dimension = last[out[i].QuestionID]
	}
	index := func(k string) (uint64, bool) {
		n, err := strconv.ParseUint(k, 10, 32)
		return n, err == nil && n < math.MaxUint32 && strconv.FormatUint(n, 10) == k
	}
	slices.SortStableFunc(out, func(a, b MapEntry) int {
		na, aIndex := index(a.QuestionID)
		nb, bIndex := index(b.QuestionID)
		switch {
		case aIndex && bIndex:
			return cmp.Compare(na, nb)
		case aIndex:
			return -1
		case bIndex:
			return 1
		}
		return 0
	})
	return out
}

// appendRow is the tail of appendLedger and appendInterviewEvent, in the oracle's order: the .crw directory, sub below it, the
// row text, one appended line. A failed step leaves what the earlier ones made. With closeTail the line is preceded by a line feed
// when the file already ends in a line that has none (endsMidLine), in the same write.
func appendRow(cwd, sub, name string, row []member, closeTail bool) error {
	if _, err := crwdir.EnsureDir(cwd); err != nil {
		return err
	}
	dir := filepath.Join(cwd, crwdir.DirName, sub)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return err
	}
	line, err := object(row)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o666)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if closeTail && endsMidLine(f, path) {
		line = append([]byte{'\n'}, line...)
	}
	_, err = f.Write(line)
	return errors.Join(err, f.Close())
}

// endsMidLine reports whether the open file is a non-empty regular file whose last byte is not a line feed, or whose last byte cannot
// be learned (a write-only file, a failed read): the conservative answer, since a blank line costs nothing and a joined line loses
// rows. A new or empty file, a FIFO or a device has no tail to protect. It is the helper of the same name in
// internal/pabcd/interview/ledger, which imports this package and so cannot be imported here; the two are kept alike on purpose.
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

// member is one key of an object whose key order the caller decides; its value is anything stringify prints, or a []member for a
// nested object (never nil: nil would print {} where a caller meant an absent key).
type member struct {
	key   string
	value any
}

// object is JSON.stringify of an object with those members in that order, spelled by stringify.
func object(members []member) ([]byte, error) {
	b := []byte{'{'}
	for i, m := range members {
		key, err := stringify(m.key, "")
		if err != nil {
			return nil, err
		}
		var value []byte
		if nested, ok := m.value.([]member); ok {
			value, err = object(nested)
		} else {
			value, err = stringify(m.value, "")
		}
		if err != nil {
			return nil, err
		}
		if i > 0 {
			b = append(b, ',')
		}
		b = append(append(append(b, key...), ':'), value...)
	}
	return append(b, '}'), nil
}

// jsNumber is a counter as JSON.stringify prints a number: NaN and the infinities are null, and adding 0 turns a negative zero
// into 0.
func jsNumber(f float64) any {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil
	}
	return f + 0
}
