package evidence

import (
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
		s.SessionID, s.UnverifiedCorrupt = sessionID, true
		return state.WriteState(cwd, s)
	}
	if lock(cwd, sessionID, raiseSentinel) != nil && marker != nil {
		_ = marker(cwd, sessionID, agentID)
	}
	return false
}
