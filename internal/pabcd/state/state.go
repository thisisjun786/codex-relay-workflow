// Package state is the PABCD session state model and its read path: the Go form of CXC v0.2.40
// pabcd-state/src/state.ts (commit 3c1459ac) from the top of the file to readStateStrict, under the CRW names of
// contract/schema/cxc/name-substitution.json (the session file .codexclaw/sessions/<id>.json is
// .crw/sessions/<id>.json). It reads and rebuilds; it writes nothing. Left to the state-writes issue: writeState, the
// session lock, the ledgers (LedgerEntry, appendLedger, the interview scan events) and ensureState, the exclusive
// create of a fresh session file.
//
// Behaviour is ported as-is, oracle defects included. A persisted file never becomes a State by decoding it into the
// struct: ReadStateStrict rebuilds every field from the decoded value, keeps only known keys and defaults or drops
// what is malformed, so a file written by hand or by another version cannot widen what a session may do. A file that
// cannot be read or decoded is reported as unreadable: a fail-closed caller must treat that as a denial, any other
// caller as a fresh session.
//
// Encode is JSON.stringify(state, null, 2). Its keys follow the object readStateStrict builds, the order of State's
// fields. Two details belong to the writer: the oracle appends boundSourceRoot last, and keeps the capture order of
// phaseEntrySource (treeHash before capturedAt), when it sets either on a state it did not read back; Encode always
// writes the order a read-back produces.
//
// Representations that are not literal:
//   - A string holding a lone surrogate cannot exist in Go. One persisted as an escape such as \ud800, or made by
//     cutting receiptClaimed inside an astral character, becomes U+FFFD; so does each invalid UTF-8 byte.
//   - The clock behind updatedAt defaults is time.Now; the writes issue stamps updatedAt itself and needs its own.
package state

import (
	"encoding/json"
	"path/filepath"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/interview"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
)

// Phase is where a session stands in the IPABCD cycle; IDLE is the rest state a closed cycle returns to.
type Phase string

// The phases of state.ts Phase.
const (
	PhaseIdle Phase = "IDLE"
	PhaseI    Phase = "I"
	PhaseP    Phase = "P"
	PhaseA    Phase = "A"
	PhaseB    Phase = "B"
	PhaseC    Phase = "C"
	PhaseD    Phase = "D"
)

// WorkPhases is the oracle's WORK_PHASES, also its PHASES (the list hook directives iterate): I to D.
func WorkPhases() []Phase { return []Phase{PhaseI, PhaseP, PhaseA, PhaseB, PhaseC, PhaseD} }

// AllPhases is the oracle's ALL_PHASES: IDLE, then the work phases.
func AllPhases() []Phase { return append([]Phase{PhaseIdle}, WorkPhases()...) }

// The names under the state directory (STATE_DIR is crwdir.DirName).
const (
	SessionsSubdir   = "sessions"
	LedgerFile       = "ledger.jsonl"
	InterviewsSubdir = "interviews"
)

// Retention caps of the unverified-subagent list; overflow sets State.UnverifiedCorrupt.
const (
	MaxUnverifiedSubagents = 64
	MaxReceiptClaimLen     = 256 // UTF-16 code units, as the oracle's String.slice counts
)

// Flags are the derived gates of a session; Interview is recomputed from the tracker on every read.
type Flags struct {
	Interview   bool `json:"interview"`
	AuditPassed bool `json:"auditPassed"`
	CheckPassed bool `json:"checkPassed"`
}

// UnverifiedSubagent is a subagent whose evidence verification ran out of retries (EVIDENCE-TERMINAL-01). Identity is
// (AgentID, TurnID); a record without a canonical agent id is not Resolvable and cannot be cleared by id. Attempts is
// a JavaScript number, any integer.
type UnverifiedSubagent struct {
	AgentID        string  `json:"agentId"`
	TurnID         string  `json:"turnId"`
	AgentType      string  `json:"agentType"`
	Attempts       float64 `json:"attempts"`
	ReceiptClaimed string  `json:"receiptClaimed"` // the path the child claimed, cut at MaxReceiptClaimLen
	RecordedAt     string  `json:"recordedAt"`
	Resolvable     bool    `json:"resolvable"`
}

// DcloseRecoveryMarker is what a D-close recorded about itself before it started writing, so a retry replays the same
// decision. A nil NextWorkPhaseID is an authoritative "no successor"; Legacy marks a marker whose successor was absent
// or malformed, which recovery must refuse instead of guessing.
type DcloseRecoveryMarker struct {
	SessionID         string  `json:"sessionId"`
	CheckEpoch        string  `json:"checkEpoch"`
	ClosedWorkPhaseID string  `json:"closedWorkPhaseId"`
	NextWorkPhaseID   *string `json:"nextWorkPhaseId"`
	Legacy            bool    `json:"legacy,omitempty"`
}

// SourceIdentity is source.Identity as a state file stores phaseEntrySource: the object reconstructSourceIdentity
// rebuilds, with its key order, and a TreeHash that keeps an empty value apart from an absent one.
type SourceIdentity struct {
	Kind       source.Kind `json:"kind"`
	CommitSha  string      `json:"commitSha"`
	Dirty      bool        `json:"dirty"`
	CapturedAt string      `json:"capturedAt"`
	TreeHash   *string     `json:"treeHash,omitempty"`
	SourceRoot *string     `json:"sourceRoot,omitempty"`
}

// Identity is s as the source package holds it, where an absent tree hash is the empty one.
func (s SourceIdentity) Identity() source.Identity {
	id := source.Identity{Kind: s.Kind, CommitSha: s.CommitSha, Dirty: s.Dirty, CapturedAt: s.CapturedAt, SourceRoot: s.SourceRoot}
	if s.TreeHash != nil {
		id.TreeHash = *s.TreeHash
	}
	return id
}

// State is one session's persisted PABCD state, its fields in the key order of the oracle's rebuilt object. Nullable
// fields are pointers (SupersededBy may point to the empty string, the other ids are never empty) and the counters are
// JavaScript numbers. Slices are never nil.
type State struct {
	Phase     Phase  `json:"phase"`
	SessionID string `json:"sessionId"`
	Slug      string `json:"slug"`
	UpdatedAt string `json:"updatedAt"` // toISOString of the last write, or whatever string was persisted
	Flags     Flags  `json:"flags"`

	SupersededBy        *string            `json:"supersededBy"`
	InjectedTurns       []string           `json:"injectedTurns"`
	LastInjectedPhase   *Phase             `json:"lastInjectedPhase"`   // a work phase, never IDLE
	OrchestrationActive bool               `json:"orchestrationActive"` // false whenever Phase is IDLE
	Interview           *interview.Tracker `json:"interview"`

	// Stop-continuation stagnation guard (L6): the phase and work phase the Stop hook last blocked on, the consecutive
	// blocks there, the objective-metric high-water mark and the blocks of the current turn.
	StopBlockPhase       *Phase  `json:"stopBlockPhase"`
	StopBlockCount       float64 `json:"stopBlockCount"`
	StopBlockWorkPhaseID *string `json:"stopBlockWorkPhaseId"`
	StopMetricCursor     float64 `json:"stopMetricCursor"`
	StopBlockTotal       float64 `json:"stopBlockTotal"`
	StopBlockTurnID      *string `json:"stopBlockTurnId"`
	StopBlockCapNotified bool    `json:"stopBlockCapNotified"`

	// IDLE-edit advisory (a loop-arm request seen, the gated-edit counter) and the memory-write gate (an explicit remember
	// request, the turn that raised it, an operator grant).
	LoopArmSeen          bool    `json:"loopArmSeen"`
	IdleEditNudges       float64 `json:"idleEditNudges"`
	MemoryWriteRequested bool    `json:"memoryWriteRequested"`
	MemoryWriteTurn      *string `json:"memoryWriteTurn"`
	MemoryWriteGrant     bool    `json:"memoryWriteGrant"`

	UnverifiedSubagents []UnverifiedSubagent `json:"unverifiedSubagents"`
	UnverifiedCorrupt   bool                 `json:"unverifiedCorrupt"` // the list could not be trusted

	// PhaseEntrySource is the source identity captured on entry to B, read back only while the phase is B (SOURCE-DELTA-01);
	// BoundSourceRoot the worktree root pinned then, omitted when absent.
	PhaseEntrySource *SourceIdentity `json:"phaseEntrySource"`
	BoundSourceRoot  *string         `json:"boundSourceRoot,omitempty"`

	// PlanUnit and PlanEpoch bind a review to the plan P>A validated, read back only in A (REVIEW-BINDING-01); CheckEpoch
	// binds a test receipt to one check, read back only in C, or in IDLE with a D-close marker (CHECK-BINDING-01).
	PlanUnit       *string               `json:"planUnit"`
	PlanEpoch      *string               `json:"planEpoch"`
	CheckEpoch     *string               `json:"checkEpoch"`
	DcloseRecovery *DcloseRecoveryMarker `json:"dcloseRecovery"`
}

// The functions below are the skeleton the tests were first run against: every body answers the empty value.

func SanitizeKey(value string) string { return value }

func IsCanonicalSessionID(id string) bool { return false }

func DefaultState(sessionID, slug string) State { return State{} }

func defaultState(sessionID, slug string, now time.Time) State { return State{} }

func StatePath(cwd, sessionID string) string {
	return filepath.Join(cwd, crwdir.DirName, SessionsSubdir, sessionID+".json")
}

func FindForeignSessionCopies(cwd, sessionID string, candidates []string) []string { return nil }

func MatchesDcloseRecovery(s State, closePhaseID string) bool { return false }

func Encode(s State) ([]byte, error) { return json.MarshalIndent(s, "", "  ") }
