package state

import (
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

// LedgerFile is the transition ledger under the state directory; InterviewsSubdir holds one scan ledger per session.
const (
	LedgerFile       = "ledger.jsonl"
	InterviewsSubdir = "interviews"
)

// ScanEvidence is the scan snapshot an I->P override records.
type ScanEvidence struct {
	ScanRounds             int64 `json:"scanRounds"`
	HighContradictionCount int64 `json:"highContradictionCount"`
}

// CloseKey is the dedup key of a D-close row (check epoch, closed work phase); either is JSON null when nil.
type CloseKey struct{ CheckEpoch, ClosedWorkPhaseID *string }

// LedgerEntry is one row of the transition ledger. JSON.stringify keeps the order its caller built the object in, and callers
// differ in one place: evidence trails the row (orchestrate-cli.ts:655, :899, :1040, :1123, orchestrate-apply.ts) or follows
// reason (evidence-cli.ts:88; the hook's close row, which spreads the transition row, evidence included, before the close key).
// EvidenceAfterReason picks the second order; the other keys keep one place: actor, override, scanEvidence, close key.
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

// MarshalJSON writes the row in the oracle's key order and with JSON.stringify's spelling.
func (e LedgerEntry) MarshalJSON() ([]byte, error) {
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
		m = append(m, member{"scanEvidence", *e.ScanEvidence})
	}
	if e.Close != nil {
		m = append(m, member{"checkEpoch", e.Close.CheckEpoch}, member{"closedWorkPhaseId", e.Close.ClosedWorkPhaseID})
	}
	if !e.EvidenceAfterReason {
		evidence()
	}
	return object(m)
}

// AppendLedger appends the row to <state dir>/ledger.jsonl (appendLedger).
func AppendLedger(cwd string, e LedgerEntry) error { return appendRow(cwd, "", LedgerFile, e) }

// InterviewScanEvent is the kind of a scan row. The scan ledger is shared with the interview ledger's question and answer rows,
// which a reader must skip.
type InterviewScanEvent string

// The scan kinds (SCAN_EVENT_KINDS).
const (
	ScanStarted     InterviewScanEvent = "scan_started"
	ScanCompleted   InterviewScanEvent = "scan_completed"
	RescanCompleted InterviewScanEvent = "rescan_completed"
)

// MapEntry attributes a question id to a dimension.
type MapEntry struct{ QuestionID, Dimension string }

// InterviewEvent is a scan row; the counters are JavaScript numbers. Map lists the attributions in the order they were assigned
// to the JavaScript object (nil leaves the key out). A read event has Raw, the row as written, which keeps every key the typed
// fields do not (the oracle returns the parsed object); a key the row lacks reads as zero in its typed field, and Map is not read.
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

// MarshalJSON writes the row as scan-cli.ts:325 builds it; the map keys come in JavaScript's own-key order.
func (e InterviewEvent) MarshalJSON() ([]byte, error) {
	m := []member{{"ts", e.TS}, {"sessionId", e.SessionID}, {"event", e.Event}, {"roundId", e.RoundID},
		{"contradictionCount", e.ContradictionCount}, {"highContradictionCount", e.HighContradictionCount}}
	if e.Map != nil {
		attrs := []member{}
		for _, p := range jsKeyOrder(e.Map) {
			attrs = append(attrs, member{p.QuestionID, p.Dimension})
		}
		raw, err := object(attrs)
		if err != nil {
			return nil, err
		}
		m = append(m, member{"map", json.RawMessage(raw)})
	}
	return object(m)
}

// AppendInterviewEvent appends the row to the session's scan ledger (appendInterviewEvent), the durable record that a scan ran.
func AppendInterviewEvent(cwd string, e InterviewEvent) error {
	return appendRow(cwd, InterviewsSubdir, SanitizeKey(e.SessionID)+".jsonl", e)
}

// ReadInterviewEvents reads a session's scan rows, best effort: a file that cannot be read is no rows. A row counts when its
// event is a scan kind and roundId and contradictionCount are numbers; blank and damaged lines and the rest are skipped.
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
		if row == nil || !okRound || !okCount || !slices.Contains([]InterviewScanEvent{ScanStarted, ScanCompleted, RescanCompleted}, InterviewScanEvent(kind)) {
			continue
		}
		e := InterviewEvent{Event: InterviewScanEvent(kind), RoundID: round, ContradictionCount: count, Raw: json.RawMessage(line)}
		e.TS, _ = row["ts"].(string)
		e.SessionID, _ = row["sessionId"].(string)
		e.HighContradictionCount, _ = number(row["highContradictionCount"])
		events = append(events, e)
	}
	return events
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

// appendRow is the tail of appendLedger and appendInterviewEvent: the .crw directory, sub below it, one appended line.
func appendRow(cwd, sub, name string, row any) error {
	if _, err := crwdir.EnsureDir(cwd); err != nil {
		return err
	}
	dir := filepath.Join(cwd, crwdir.DirName, sub)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return err
	}
	line, err := stringify(row, "")
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o666)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	return errors.Join(err, f.Close())
}

// member is one key of an object whose key order the caller decides; object is JSON.stringify of such an object.
type member struct {
	key   string
	value any
}

func object(members []member) ([]byte, error) {
	b := []byte{'{'}
	for i, m := range members {
		key, err := stringify(m.key, "")
		if err != nil {
			return nil, err
		}
		value, err := stringify(m.value, "")
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
