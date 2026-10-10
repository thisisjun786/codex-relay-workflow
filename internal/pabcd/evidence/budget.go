package evidence

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// HasSpentBudget reports whether a counter of the session still sits at the cap, or cannot be shown to sit below it. The counter
// reaches MaxAttempts during normal operation and only a valid receipt clears it, so it outlives a tombstone, the corruption
// sentinel and a marker that could not be written, and closes the case of a failure at terminal time followed by recovery. Every
// counter is judged by ReadCounter's classification, so this gate and the SubagentStop gate read one file alike: only an active
// counter below the cap is unspent. A directory that cannot be read counts as spent; only a missing one is an absence.
//
// Changed from the oracle (port: fixed, CRW-1106): the oracle finds a session's counters by the name prefix <session>-, so the
// counters of a session whose id continues with a dash after this one's (s1-x for s1) count too. A counter name is
// <session>-<agent>-[<turn>-]<digest of the raw agent and turn>.json, so a name is this session's when one way of reading the part
// after the prefix as an agent and a turn reproduces the digest, and another session's when a reading under a longer session
// does; such a file is skipped. A name that no reading explains (a file of an earlier version for an agent or turn id that
// sanitising changed) may be anyone's and still counts, the denying direction. Counters of canonical sessions whose agent or
// turn sanitising changes live in the session's own directory (counterVersionDir), each matched by the identity it repeats.
// CRW-1108 refuses a non-canonical or empty session id without reading any counter; that session always answers spent.
func HasSpentBudget(cwd, sessionID string) bool {
	if !state.IsCanonicalSessionID(sessionID) { // CRW-1108: no alias read
		return true
	}
	if sessionCountersSpent(cwd, sessionID) {
		return true
	}
	dir := filepath.Join(cwd, crwdir.DirName, AttemptsSubdir)
	names, err := dirNames(dir)
	if err != nil {
		return !errors.Is(err, fs.ErrNotExist)
	}
	key := state.SanitizeKey(sessionID)
	return slices.ContainsFunc(names, func(n string) bool {
		if !strings.HasPrefix(n, key+"-") || !strings.HasSuffix(n, ".json") {
			return false
		}
		if owner := counterOwner(n); owner != "" && owner != key {
			return false
		}
		c, _ := readCounterFile(filepath.Join(dir, n), nil)
		return c.Spent()
	})
}

// counterOwner is the session a counter name in the oracle's layout belongs to, or "" when no reading of the name explains it. The
// name is <session>-<agent>-[<turn>-]<digest>.json with each id sanitised; a reading splits the part before the digest at two (or,
// without a turn, one) of its dashes and is right when the digest of the agent and turn it names is the name's digest. Two
// readings that reproduce one digest name one agent and turn, and the name then fixes the session.
func counterOwner(name string) string {
	base, ok := strings.CutSuffix(name, ".json")
	if !ok || len(base) < 34 || base[len(base)-33] != '-' {
		return ""
	}
	digest, head := base[len(base)-32:], base[:len(base)-33]
	dashes := []int{}
	for i := range len(head) {
		if head[i] == '-' {
			dashes = append(dashes, i)
		}
	}
	for i, a := range dashes {
		session, rest := head[:a], head[a+1:]
		if state.SanitizeKey(rest) == rest && (tupleDigest(rest, "") == digest || rest == "missing" && tupleDigest("", "") == digest) {
			return session
		}
		for _, b := range dashes[i+1:] {
			agent, turn := head[a+1:b], head[b+1:]
			if state.SanitizeKey(agent) != agent || state.SanitizeKey(turn) != turn {
				continue
			}
			if tupleDigest(agent, turn) == digest || agent == "missing" && tupleDigest("", turn) == digest {
				return session
			}
		}
	}
	return ""
}

// sessionCountersSpent is HasSpentBudget for the counters in the session's own directory, each record matched by the identity it
// repeats.
func sessionCountersSpent(cwd, sessionID string) bool {
	dir := counterDir(cwd, sessionID)
	names, err := dirNames(dir)
	if err != nil {
		return !errors.Is(err, fs.ErrNotExist)
	}
	if requireDirectory(dir) != nil {
		return true
	}
	return slices.ContainsFunc(names, func(n string) bool {
		if !strings.HasSuffix(n, ".json") {
			return false // a temp file
		}
		c, _ := readCounterFile(filepath.Join(dir, n), func(r counterRecord) bool {
			return r.SessionID == sessionID && tupleDigest(r.AgentID, r.TurnID)+".json" == n
		})
		return c.Spent()
	})
}

// ResolveTombstone clears the tombstone of an agent and turn, for the gate to call when a late valid receipt arrives, so the
// parent is not left blocked on work that was verified after all. It reports whether an entry was removed and written; every
// failure is false. The whole read-modify-write runs under the session lock and reads again inside it, with the non-strict read:
// an unreadable file reads as a default state, holds no entry to remove and is not written.
//
// Changed from the oracle in two ways, each a fix. A loss of state: the oracle writes back the verdicts its read kept, so the
// verdicts past the cap of 64 and the entries the read cannot parse vanish from the file, with only the corruption flag left.
// Here nothing is written, and the call reports false, when the file holds more verdicts than the read kept, or when the read
// changed one it kept (a receipt past 256 units, a field of the wrong type, an interview tracker longer than the reader keeps;
// state.RewriteKeepsStored). A security
// weakness: the oracle ignores whether the identity is resolvable, so a receipt from an agent with no id removed every tombstone
// with an empty agent id and the same turn, the verdicts of other agents whose ids were missing among them, which are marked
// not resolvable because their owners cannot be told apart. Here an empty agent id resolves nothing: the call returns false
// before the lock is taken and nothing is read or written.
func ResolveTombstone(cwd, sessionID string, p Payload) bool {
	return resolveTombstone(cwd, sessionID, p, state.WithSessionLock)
}

func resolveTombstone(cwd, sessionID string, p Payload, lock lockFunc, publishedWrite ...publishedWriteFunc) bool {
	write := publishedWriteOf(publishedWrite)
	agentID, turnID, resolvable := tombstoneIdentity(p)
	if !resolvable {
		return false
	}
	removed := false
	_ = lock(cwd, sessionID, func() error { // a lock that cannot be had never runs the function, so removed stays false
		if err := RecoverOverflow(cwd, sessionID, write); err != nil {
			return err
		}
		s := state.ReadState(cwd, sessionID)
		next := slices.DeleteFunc(slices.Clone(s.UnverifiedSubagents), func(e state.UnverifiedSubagent) bool {
			return sameAgent(e, agentID, turnID)
		})
		if len(next) == len(s.UnverifiedSubagents) {
			removed = removeOverflow(cwd, sessionID, agentID, turnID) // CRW-1110: a verdict recorded beside the main list
			return nil
		}
		if storedVerdicts(cwd, sessionID) != len(s.UnverifiedSubagents) || !rewriteGuardKeeps(cwd, sessionID, s) {
			return nil
		}
		s.SessionID, s.UnverifiedSubagents = sessionID, next
		// The removal was published when the write reports state.Published, so it counts as removed although the
		// directory sync failed afterwards.
		if err := write(cwd, s); err != nil && !state.Published(err) {
			return err
		}
		removed = true
		return nil
	})
	return removed
}

// storedVerdicts is the number of verdicts the session file holds under the exact key ReadState reads, or -1 when that cannot
// be told (encoding/json would also match a key that differs in case, so the lookup goes through a map).
func storedVerdicts(cwd, sessionID string) int {
	var f map[string]json.RawMessage
	var list []json.RawMessage
	raw, err := os.ReadFile(state.StatePath(cwd, sessionID))
	if err != nil || json.Unmarshal(raw, &f) != nil || json.Unmarshal(f["unverifiedSubagents"], &list) != nil {
		return -1
	}
	return len(list)
}
