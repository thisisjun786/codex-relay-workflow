// Package state is the PABCD session state: the Go form of CXC v0.2.40 pabcd-state/src/state.ts (commit 3c1459ac), with the
// session file at .crw/sessions/<id>.json (name-substitution R26). state.go and restore.go are the model and the read path (the
// top of the file to readStateStrict); write.go and lock.go the write path: ensureState, writeState and withSessionLock; ledger.go
// the ledgers: LedgerEntry and appendLedger, the interview scan events (appendInterviewEvent) and readInterviewEvents.
//
// Behaviour is ported as-is, oracle defects included. A file never becomes a State by decoding it into the struct:
// ReadStateStrict rebuilds every field from known keys and defaults or drops what is malformed, so a hand-written or
// foreign-version file cannot widen what a session may do. A file that cannot be read or decoded is reported unreadable: a
// fail-closed caller must treat that as a denial, any other caller as a fresh session.
//
// Encode is JSON.stringify(state, null, 2) in the order of State's fields, the order readStateStrict builds. The oracle
// appends boundSourceRoot last, and keeps the capture order of phaseEntrySource (treeHash before capturedAt), when it sets
// either on a state it did not read back; that is a writer's detail, and Encode always writes the read-back order.
//
// Not literal: a string holding a lone surrogate (an escape such as \ud800 in a file, or receiptClaimed cut inside an astral
// character) cannot exist in Go and becomes U+FFFD, as does each invalid UTF-8 byte. Defaulted updatedAt values come from
// time.Now; WriteState stamps updatedAt from its clock.
//
// The write path differs from the oracle in ways no file shows: a temp file is named by the pid and a random UUID, where
// writeState uses Date.now(), which two goroutines of one process could share; a rename onto a directory fails with EEXIST
// from os.Rename where rename(2) and Node say EISDIR. ReadInterviewEvents returns each scan row's text in Raw beside typed fields (the
// oracle returns the parsed object): a key the row lacks reads as zero.
package state

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
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

// SessionsSubdir is the directory of the session files under the state directory (STATE_DIR is crwdir.DirName).
const SessionsSubdir = "sessions"

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
// decision. A nil NextWorkPhaseID with Legacy false is an authoritative "no successor"; Legacy marks a marker whose successor was absent
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

	// Stop-continuation stagnation guard (L6): where the Stop hook last blocked, how often, the metric high-water mark.
	StopBlockPhase       *Phase  `json:"stopBlockPhase"`
	StopBlockCount       float64 `json:"stopBlockCount"`
	StopBlockWorkPhaseID *string `json:"stopBlockWorkPhaseId"`
	StopMetricCursor     float64 `json:"stopMetricCursor"`
	StopBlockTotal       float64 `json:"stopBlockTotal"`
	StopBlockTurnID      *string `json:"stopBlockTurnId"`
	StopBlockCapNotified bool    `json:"stopBlockCapNotified"`

	// IDLE-edit advisory and the memory-write gate (a remember request, its turn, an operator grant).
	LoopArmSeen          bool    `json:"loopArmSeen"`
	IdleEditNudges       float64 `json:"idleEditNudges"`
	MemoryWriteRequested bool    `json:"memoryWriteRequested"`
	MemoryWriteTurn      *string `json:"memoryWriteTurn"`
	MemoryWriteGrant     bool    `json:"memoryWriteGrant"`

	UnverifiedSubagents []UnverifiedSubagent `json:"unverifiedSubagents"`
	UnverifiedCorrupt   bool                 `json:"unverifiedCorrupt"` // the list could not be trusted

	// PhaseEntrySource is the source identity captured on entry to B, read back only in B (SOURCE-DELTA-01); BoundSourceRoot
	// the worktree root pinned then.
	PhaseEntrySource *SourceIdentity `json:"phaseEntrySource"`
	BoundSourceRoot  *string         `json:"boundSourceRoot,omitempty"`

	// PlanUnit and PlanEpoch bind a review to a plan, read back only in A (REVIEW-BINDING-01); CheckEpoch binds a test
	// receipt to one check, read back only in C, or in IDLE with a D-close marker (CHECK-BINDING-01).
	PlanUnit       *string               `json:"planUnit"`
	PlanEpoch      *string               `json:"planEpoch"`
	CheckEpoch     *string               `json:"checkEpoch"`
	DcloseRecovery *DcloseRecoveryMarker `json:"dcloseRecovery"`
}

// SanitizeKey makes a session id usable as a file name: each run of characters outside [A-Za-z0-9._-] becomes one "-",
// leading and trailing "-" are dropped, and nothing left is "missing".
func SanitizeKey(value string) string {
	var b []byte
	inRun := false
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
			b, inRun = append(b, byte(r)), false
		} else if !inRun {
			b, inRun = append(b, '-'), true
		}
	}
	if s := string(bytes.Trim(b, "-")); s != "" {
		return s
	}
	return "missing"
}

// IsCanonicalSessionID reports whether sanitising leaves id unchanged, so a state file is never published under a
// different or colliding key.
func IsCanonicalSessionID(id string) bool { return id != "" && SanitizeKey(id) == id }

// DefaultState is a fresh IDLE session.
func DefaultState(sessionID, slug string) State { return defaultState(sessionID, slug, time.Now()) }

func defaultState(sessionID, slug string, now time.Time) State {
	return State{
		Phase: PhaseIdle, SessionID: sessionID, Slug: slug, UpdatedAt: now.UTC().Format("2006-01-02T15:04:05.000Z"),
		InjectedTurns: []string{}, UnverifiedSubagents: []UnverifiedSubagent{},
	}
}

// StatePath is cwd/.crw/sessions/<sanitised id>.json.
func StatePath(cwd, sessionID string) string {
	return filepath.Join(cwd, crwdir.DirName, SessionsSubdir, SanitizeKey(sessionID)+".json")
}

// FindForeignSessionCopies lists the state files of candidates' trees that hold the same session id as cwd's own, so a
// thread whose cwd is one tree while its work is in another can see the split (#48). Detection only: nothing is read from
// or written to the other trees. cwd itself and duplicates are skipped.
func FindForeignSessionCopies(cwd, sessionID string, candidates []string) []string {
	abs := func(root string) string { p, _ := filepath.Abs(StatePath(root, sessionID)); return p }
	seen, found := map[string]bool{abs(cwd): true}, []string{}
	for _, root := range candidates {
		if candidate := abs(root); root != "" && !seen[candidate] {
			seen[candidate] = true
			if _, err := os.Stat(candidate); err == nil {
				found = append(found, candidate)
			}
		}
	}
	return found
}

// MatchesDcloseRecovery reports whether s holds a D-close marker of its own session and current check epoch that closes the
// work phase closePhaseID.
func MatchesDcloseRecovery(s State, closePhaseID string) bool {
	m := s.DcloseRecovery
	return m != nil && m.SessionID == s.SessionID && s.CheckEpoch != nil && m.CheckEpoch == *s.CheckEpoch && m.ClosedWorkPhaseID == closePhaseID
}

// Encode is JSON.stringify(s, null, 2) for a state ReadStateStrict or DefaultState built (a NaN or infinite counter, or a
// negative zero, set by hand is an error or prints differently): the bytes the oracle publishes, without a trailing newline.
// HTML is not escaped, and U+2028 and U+2029 are written literally.
func Encode(s State) ([]byte, error) { return stringify(s, "  ") }

// stringify is JSON.stringify(v, null, indent) with an empty indent for the compact form, for any value the oracle prints.
func stringify(v any, indent string) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", indent)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	in, out := bytes.TrimSuffix(b.Bytes(), []byte("\n")), make([]byte, 0, b.Len())
	// encoding/json always escapes U+2028 and U+2029, JSON.stringify never does. A backslash and the byte after it are copied
	// together, so an escaped backslash before "u2028" stays text.
	for i := 0; i < len(in); i++ {
		switch {
		case in[i] != '\\':
			out = append(out, in[i])
		case bytes.HasPrefix(in[i:], []byte(`\u2028`)):
			out, i = append(out, "\u2028"...), i+5
		case bytes.HasPrefix(in[i:], []byte(`\u2029`)):
			out, i = append(out, "\u2029"...), i+5
		default:
			out, i = append(out, in[i], in[i+1]), i+1
		}
	}
	return out, nil
}
