package state

import "encoding/json"

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

// CloseKey is the dedup key of a D-close row; either field is null when nil.
type CloseKey struct{ CheckEpoch, ClosedWorkPhaseID *string }

// LedgerEntry is one row of the transition ledger.
type LedgerEntry struct {
	TS                  string
	SessionID           string
	From                *Phase
	To                  Phase
	Reason              string
	Evidence            *string
	EvidenceAfterReason bool
	Actor               string
	Override            *bool
	ScanEvidence        *ScanEvidence
	Close               *CloseKey
}

// MarshalJSON writes the row in the oracle's key order.
func (e LedgerEntry) MarshalJSON() ([]byte, error) { return nil, unimplemented() }

// AppendLedger appends the row to <state dir>/ledger.jsonl.
func AppendLedger(cwd string, e LedgerEntry) error { return unimplemented() }

// InterviewScanEvent is the kind of a scan row.
type InterviewScanEvent string

// The scan kinds.
const (
	ScanStarted     InterviewScanEvent = "scan_started"
	ScanCompleted   InterviewScanEvent = "scan_completed"
	RescanCompleted InterviewScanEvent = "rescan_completed"
)

// MapEntry attributes a question id to a dimension.
type MapEntry struct{ QuestionID, Dimension string }

// InterviewEvent is a scan row.
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

// MarshalJSON writes the row as scan-cli.ts builds it.
func (e InterviewEvent) MarshalJSON() ([]byte, error) { return nil, unimplemented() }

// AppendInterviewEvent appends the row to the session's scan ledger.
func AppendInterviewEvent(cwd string, e InterviewEvent) error { return unimplemented() }

// ReadInterviewEvents reads the scan rows of a session's ledger.
func ReadInterviewEvents(cwd, sessionID string) []InterviewEvent { return []InterviewEvent{} }
