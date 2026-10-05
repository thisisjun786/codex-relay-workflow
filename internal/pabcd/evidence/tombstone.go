package evidence

import (
	"errors"
	"io/fs"
	"os"
	"slices"
	"time"
	"unicode/utf16"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// MarkerWriter is the last resort of RecordTombstone: it records, outside the session file, that a verdict could not be
// recorded (the oracle's writeUnrecordableMarker). After the fix described in the package comment it is the only durable denial
// when the session file is unreadable, so a nil writer is not a supported value for the gate.
type MarkerWriter func(cwd, sessionID, agentID string) error

// lockFunc is state.WithSessionLock; the tests replace it to stage a failed acquisition.
type lockFunc func(cwd, sessionID string, fn func() error) error

type sentinel string

func (e sentinel) Error() string { return string(e) }

const errUnreadable = sentinel("session state is unreadable; refusing to overwrite it")

// rewriteGuardLoses is what a tier returns when writing the rebuilt state back would change a record the file stores.
const rewriteGuardLoses = sentinel("session state holds records a rewrite would change; refusing to overwrite it")

// rewriteGuardKeeps says whether writing kept, the list the strict read rebuilt, over the session file would lose nothing it stores:
// the reader keeps 64 records, cuts a receipt to 256 units and defaults a field of the wrong type, none of which a count shows
// (state.RewriteKeepsUnverified). A file that does not exist stores nothing; one that cannot be read now is refused. It is called
// inside the session lock, after the strict read.
func rewriteGuardKeeps(cwd, sessionID string, kept []state.UnverifiedSubagent) bool {
	raw, err := os.ReadFile(state.StatePath(cwd, sessionID))
	if errors.Is(err, fs.ErrNotExist) {
		return len(kept) == 0
	}
	return err == nil && state.RewriteKeepsUnverified(raw, kept)
}

// tombstoneIdentity is the identity of a tombstone. An empty agent id is not resolvable: such ids collide.
func tombstoneIdentity(p Payload) (agentID, turnID string, resolvable bool) {
	return p.AgentID, p.TurnID, p.AgentID != ""
}

func sameAgent(e state.UnverifiedSubagent, agentID, turnID string) bool {
	return e.AgentID == agentID && e.TurnID == turnID
}

// HasTombstone reports whether the session already holds a terminal record for this agent and turn. It is the latch that makes
// the release stick: without it the next stop would start the budget again. An unreadable session reads as no record.
func HasTombstone(cwd, sessionID string, p Payload) bool {
	agentID, turnID, _ := tombstoneIdentity(p)
	return slices.ContainsFunc(state.ReadState(cwd, sessionID).UnverifiedSubagents, func(e state.UnverifiedSubagent) bool {
		return sameAgent(e, agentID, turnID)
	})
}

// RecordTombstone records the terminal verdict of an agent that ran out of attempts and reports whether it did. The whole
// read-modify-write runs under the session lock and reads the file again inside it, so two agents stopping together keep both
// verdicts, and an unreadable session file is never overwritten with a state rebuilt from nothing. When that fails (a held lock,
// an unreadable file, a write error) the second tier sets the corruption sentinel on a readable file, and when that fails too
// the verdict goes to marker. Every tier's failure is swallowed: the agent is released either way.
//
// Changed from the oracle, a fix for a loss of state: the oracle writes back what its read kept, so a file that stores more than 64
// verdicts, or a receipt past 256 units, lost the rest (after the 65th verdict the 66th dropped it). Here a state whose rewrite
// would change a stored record is unwritable in both tiers, and the verdict goes to marker.
func RecordTombstone(cwd, sessionID string, p Payload, attempts int, marker MarkerWriter) bool {
	return recordTombstone(cwd, sessionID, p, attempts, time.Now(), state.WithSessionLock, marker)
}

func recordTombstone(cwd, sessionID string, p Payload, attempts int, now time.Time, lock lockFunc, marker MarkerWriter) bool {
	agentID, turnID, resolvable := tombstoneIdentity(p)
	claimed, _ := ExtractReceiptPath(p.LastAssistantMessage) // the claimed path only, never the child's prose
	if units := utf16.Encode([]rune(claimed)); len(units) > state.MaxReceiptClaimLen {
		claimed = string(utf16.Decode(units[:state.MaxReceiptClaimLen])) // a cut inside an astral character leaves U+FFFD
	}
	entry := state.UnverifiedSubagent{AgentID: agentID, TurnID: turnID, AgentType: p.AgentType, Attempts: float64(attempts),
		ReceiptClaimed: claimed, RecordedAt: now.UTC().Format("2006-01-02T15:04:05.000Z"), Resolvable: resolvable}
	commit := func() error {
		s, unreadable := state.ReadStateStrict(cwd, sessionID)
		if unreadable {
			return errUnreadable
		}
		if !rewriteGuardKeeps(cwd, sessionID, s.UnverifiedSubagents) {
			return rewriteGuardLoses
		}
		s.SessionID = sessionID
		s.UnverifiedSubagents = append(slices.DeleteFunc(slices.Clone(s.UnverifiedSubagents), func(e state.UnverifiedSubagent) bool {
			return sameAgent(e, agentID, turnID)
		}), entry)
		return state.WriteState(cwd, s)
	}
	if lock(cwd, sessionID, commit) == nil {
		return true
	}
	raiseSentinel := func() error {
		s, unreadable := state.ReadStateStrict(cwd, sessionID)
		if unreadable { // changed: the oracle writes the sentinel over the unreadable file
			return errUnreadable
		}
		if !rewriteGuardKeeps(cwd, sessionID, s.UnverifiedSubagents) { // the sentinel write rebuilds the list too
			return rewriteGuardLoses
		}
		s.SessionID, s.UnverifiedCorrupt = sessionID, true
		return state.WriteState(cwd, s)
	}
	if lock(cwd, sessionID, raiseSentinel) != nil && marker != nil {
		_ = marker(cwd, sessionID, agentID)
	}
	return false
}
