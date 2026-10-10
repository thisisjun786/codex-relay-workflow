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

// publishedWriteFunc is the state write the tombstone writers commit through. It is state.WriteState everywhere but in a
// test, which passes one that publishes the state and then reports the post-rename failure WriteState returns
// (*state.PublishedError). It is a function argument, never a package-level variable, so no test state outlives a call.
type publishedWriteFunc func(cwd string, s state.State) error

// publishedWriteOf is the write a tombstone writer commits through: the one its caller passed, else state.WriteState.
func publishedWriteOf(writes []publishedWriteFunc) publishedWriteFunc {
	if len(writes) > 0 && writes[0] != nil {
		return writes[0]
	}
	return state.WriteState
}

type sentinel string

func (e sentinel) Error() string { return string(e) }

const errUnreadable = sentinel("session state is unreadable; refusing to overwrite it")

// rewriteGuardLoses is what a tier returns when writing the rebuilt state back would change a record the file stores.
const rewriteGuardLoses = sentinel("session state holds records a rewrite would change; refusing to overwrite it")

// rewriteGuardKeeps says whether writing s, the state the strict read rebuilt, over the session file would lose nothing it stores:
// the reader keeps 64 records, cuts a receipt to 256 units, defaults a field of the wrong type and caps the interview tracker arrays
// at interview.MaxTrackerArray, none of which a count shows (state.RewriteKeepsStored). A file that does not exist stores nothing;
// one that cannot be read now is refused. It is called inside the session lock, after the strict read. A legacy D-close marker is
// deliberately not part of it (the CRW-648 allowance: this writer keeps the marker's distinction across the write).
func rewriteGuardKeeps(cwd, sessionID string, s state.State) bool {
	raw, err := os.ReadFile(state.StatePath(cwd, sessionID))
	if errors.Is(err, fs.ErrNotExist) {
		return len(s.UnverifiedSubagents) == 0
	}
	return err == nil && state.RewriteKeepsStored(raw, s)
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
//
// A verdict recorded beside a full main list (CRW-1110) counts too.
func HasTombstone(cwd, sessionID string, p Payload) bool {
	agentID, turnID, _ := tombstoneIdentity(p)
	return slices.ContainsFunc(state.ReadState(cwd, sessionID).UnverifiedSubagents, func(e state.UnverifiedSubagent) bool {
		return sameAgent(e, agentID, turnID)
	}) || hasOverflow(cwd, sessionID, agentID, turnID)
}

// RecordTombstone records the terminal verdict of an agent that ran out of attempts and reports whether it did. The whole
// read-modify-write runs under the session lock and reads the file again inside it, so two agents stopping together keep both
// verdicts, and an unreadable session file is never overwritten with a state rebuilt from nothing. When that fails (a held lock,
// an unreadable file, a write error) the second tier sets the corruption sentinel on a readable file, and when that fails too
// the verdict goes to marker. Every tier's failure is swallowed: the agent is released either way.
//
// Changed from the oracle, a fix for a loss of state: the oracle writes back what its read kept, so a file that stores more than 64
// verdicts, or a receipt past 256 units, lost the rest (after the 65th verdict the 66th dropped it). Here a state whose rewrite
// would change a stored record is unwritable in both tiers, and the verdict goes to marker. Since CRW-1110 the main list never
// passes the cap: a verdict that does not fit is recorded beside it (overflow.go), and a file written before that holds more is
// recovered first (RecoverOverflow).
func RecordTombstone(cwd, sessionID string, p Payload, attempts int, marker MarkerWriter) bool {
	return recordTombstone(cwd, sessionID, p, attempts, time.Now(), state.WithSessionLock, marker)
}

func recordTombstone(cwd, sessionID string, p Payload, attempts int, now time.Time, lock lockFunc, marker MarkerWriter, publishedWrite ...publishedWriteFunc) bool {
	write := publishedWriteOf(publishedWrite)
	agentID, turnID, resolvable := tombstoneIdentity(p)
	claimed, _ := ExtractReceiptPath(p.LastAssistantMessage) // the claimed path only, never the child's prose
	if units := utf16.Encode([]rune(claimed)); len(units) > state.MaxReceiptClaimLen {
		claimed = string(utf16.Decode(units[:state.MaxReceiptClaimLen])) // a cut inside an astral character leaves U+FFFD
	}
	entry := state.UnverifiedSubagent{AgentID: agentID, TurnID: turnID, AgentType: p.AgentType, Attempts: float64(attempts),
		ReceiptClaimed: claimed, RecordedAt: now.UTC().Format("2006-01-02T15:04:05.000Z"), Resolvable: resolvable}
	commit := func() error {
		if err := RecoverOverflow(cwd, sessionID, write); err != nil {
			return err
		}
		if hasOverflow(cwd, sessionID, agentID, turnID) { // this verdict already lives beside the main list
			return writeOverflow(cwd, sessionID, entry)
		}
		s, unreadable := state.ReadStateStrict(cwd, sessionID)
		if unreadable {
			return errUnreadable
		}
		if !rewriteGuardKeeps(cwd, sessionID, s) {
			return rewriteGuardLoses
		}
		kept := slices.DeleteFunc(slices.Clone(s.UnverifiedSubagents), func(e state.UnverifiedSubagent) bool {
			return sameAgent(e, agentID, turnID)
		})
		// CRW-1110 (port: fixed): a new verdict that would take the main list past the cap the reader keeps is recorded beside it,
		// and the list written is checked to read back whole before it is published.
		if len(kept) == len(s.UnverifiedSubagents) && len(kept) >= state.MaxUnverifiedSubagents {
			return writeOverflow(cwd, sessionID, entry)
		}
		s.SessionID, s.UnverifiedSubagents = sessionID, append(kept, entry)
		if !publishable(s.UnverifiedSubagents) {
			return rewriteGuardLoses
		}
		return write(cwd, s)
	}
	// A commit whose state reached the final path is committed, even when the write then failed the directory sync: the
	// tombstone is already in the file every reader sees, so the sentinel tier must not run and stamp unverifiedCorrupt on
	// a healthy session (the goal-complete gate then refuses it as unreadable).
	if err := lock(cwd, sessionID, commit); err == nil || state.Published(err) {
		return true
	}
	raiseSentinel := func() error {
		s, unreadable := state.ReadStateStrict(cwd, sessionID)
		if unreadable { // changed: the oracle writes the sentinel over the unreadable file
			return errUnreadable
		}
		if !rewriteGuardKeeps(cwd, sessionID, s) { // the sentinel write rebuilds the list too
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
