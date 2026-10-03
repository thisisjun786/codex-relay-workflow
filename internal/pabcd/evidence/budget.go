package evidence

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// HasSpentBudget reports whether a counter of the session still sits at the cap, or cannot be shown to sit below it. The counter
// reaches MaxAttempts during normal operation and only a valid receipt clears it, so it outlives a tombstone, the corruption
// sentinel and a marker that could not be written, and closes the case of a failure at terminal time followed by recovery. Only a
// missing attempts directory is an absence; an unreadable one, and a counter that is not a JSON object with an integer attempts
// from 0 below MaxAttempts, count as spent. A counter is a file named <session>-...json, so the counters of a session whose id
// continues with a dash after this one's count too. A counter nested deeper than Go's JSON limit of 10,000 levels reads as spent,
// where the oracle parses it and may find the attempts beside the nesting unspent.
func HasSpentBudget(cwd, sessionID string) bool {
	dir := filepath.Join(cwd, crwdir.DirName, AttemptsSubdir)
	names, err := dirNames(dir)
	if err != nil {
		return !errors.Is(err, fs.ErrNotExist)
	}
	prefix := state.SanitizeKey(sessionID) + "-"
	return slices.ContainsFunc(names, func(n string) bool {
		return strings.HasPrefix(n, prefix) && strings.HasSuffix(n, ".json") && !counterUnspent(filepath.Join(dir, n))
	})
}

// counterUnspent parses the counter the way JSON.parse does (all of the file, a number beyond float64 is Infinity and not an
// error) and applies the oracle's test to attempts: a safe integer from 0 below MaxAttempts. The two bounds already exclude
// an infinity and an integer beyond 2^53, so only the integer test is left to make.
func counterUnspent(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var o map[string]any
	if dec.Decode(&o) != nil {
		return false
	}
	if _, err := dec.Token(); err != io.EOF {
		return false
	}
	n, isNumber := o["attempts"].(json.Number)
	f, _ := strconv.ParseFloat(string(n), 64)
	return isNumber && f == math.Trunc(f) && f >= 0 && f < MaxAttempts
}

// ResolveTombstone clears the tombstone of an agent and turn, for the gate to call when a late valid receipt arrives, so the
// parent is not left blocked on work that was verified after all. It reports whether an entry was removed and written; every
// failure is false. The whole read-modify-write runs under the session lock and reads again inside it, with the non-strict read:
// an unreadable file reads as a default state, holds no entry to remove and is not written. An empty agent id matches the
// tombstones with an empty agent id and the same turn, which are marked not resolvable because their owners cannot be told
// apart.
//
// Changed from the oracle (a loss of state): the oracle writes back the verdicts its read kept, so the verdicts past the cap of 64
// and the entries the read cannot parse vanish from the file, with only the corruption flag left. Here nothing is written, and
// the call reports false, when the file holds more verdicts than the read kept.
func ResolveTombstone(cwd, sessionID string, p Payload) bool {
	return resolveTombstone(cwd, sessionID, p, state.WithSessionLock)
}

func resolveTombstone(cwd, sessionID string, p Payload, lock lockFunc) bool {
	agentID, turnID, _ := tombstoneIdentity(p)
	removed := false
	_ = lock(cwd, sessionID, func() error { // a lock that cannot be had never runs the function, so removed stays false
		s := state.ReadState(cwd, sessionID)
		next := slices.DeleteFunc(slices.Clone(s.UnverifiedSubagents), func(e state.UnverifiedSubagent) bool {
			return sameAgent(e, agentID, turnID)
		})
		if len(next) == len(s.UnverifiedSubagents) || storedVerdicts(cwd, sessionID) != len(s.UnverifiedSubagents) {
			return nil
		}
		s.SessionID, s.UnverifiedSubagents = sessionID, next
		if err := state.WriteState(cwd, s); err != nil {
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
