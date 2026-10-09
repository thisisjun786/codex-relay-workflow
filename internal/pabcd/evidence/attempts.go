package evidence

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"unicode/utf16"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// attemptsPath is the counter file the oracle names for an agent in a turn: the sanitised ids, kept so that a person can read the
// directory, and a digest of the raw agent and turn, because sanitising is not injective ("a/b" and "a-b" are one string). An absent
// turn has the same name shape without the turn part. For a canonical session id (state.IsCanonicalSessionID: sanitising changes
// nothing) the name is exact for the tuple: the digest fixes the agent and the turn, and then the rest of the name fixes the
// session. It is the counter path of every canonical session, so its files stay the oracle's.
func attemptsPath(cwd, sessionID, agentID, turnID string) string {
	name := state.SanitizeKey(sessionID) + "-" + state.SanitizeKey(agentID) + "-"
	if turnID != "" {
		name += state.SanitizeKey(turnID) + "-"
	}
	return filepath.Join(cwd, crwdir.DirName, AttemptsSubdir, name+tupleDigest(agentID, turnID)+".json")
}

// counterVersionDir holds the counters of a session whose id is not canonical (CRW-1106): two such ids can sanitise to one name
// (a/b and a-b), so their counters live in a directory per exact session (sessionRecordDir), one file per (agent, turn) named by
// tupleDigest, and the record repeats its identity.
const counterVersionDir = "v2"

func counterDir(cwd, sessionID string) string {
	return filepath.Join(cwd, crwdir.DirName, AttemptsSubdir, counterVersionDir, sessionRecordDir(sessionID))
}

// counterPath is where the counter of the exact tuple lives: the oracle's name for a canonical session, the session's own
// directory otherwise.
func counterPath(cwd, sessionID, agentID, turnID string) string {
	if state.IsCanonicalSessionID(sessionID) {
		return attemptsPath(cwd, sessionID, agentID, turnID)
	}
	return filepath.Join(counterDir(cwd, sessionID), tupleDigest(agentID, turnID)+".json")
}

// tupleDigest is the first 32 hex digits of the SHA-256 of "<len>:<agent>:<len>:<turn>" taken as UTF-16 code units in
// little-endian order, the lengths counted in code units, as the oracle's Buffer.from(.., "utf16le") hashes it.
func tupleDigest(agentID, turnID string) string {
	units := func(s string) []uint16 { return utf16.Encode([]rune(s)) }
	agent, turn := units(agentID), units(turnID)
	framed := append(units(fmt.Sprintf("%d:", len(agent))), agent...)
	framed = append(framed, units(fmt.Sprintf(":%d:", len(turn)))...)
	framed = append(framed, turn...)
	raw := make([]byte, 0, 2*len(framed))
	for _, u := range framed {
		raw = binary.LittleEndian.AppendUint16(raw, u)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:16])
}

// CounterState is what a counter file says about an agent's retry budget (CRW-1106): one reader for the SubagentStop gate and the
// goal-complete gate, so one file never means two things.
type CounterState int

const (
	CounterMissing    CounterState = iota // no counter: the budget has not started
	CounterActive                         // attempts from 0 below MaxAttempts
	CounterExhausted                      // attempts at MaxAttempts
	CounterCorrupt                        // a file that is not a counter of this tuple: empty, cut, not an object, a bad count
	CounterUnreadable                     // a file that cannot be read, or something that is not a regular file
)

// Counter is a counter snapshot. Attempts is the count of an active or exhausted counter, else 0.
type Counter struct {
	State    CounterState
	Attempts int
}

// Spent reports whether the counter ends the budget: exhausted, corrupt or unreadable. Only a missing or active counter leaves
// room for another attempt.
func (c Counter) Spent() bool { return c.State != CounterMissing && c.State != CounterActive }

// counterRecord is a counter file. A canonical session's file holds attempts only, as the oracle writes it; the record of a session
// whose id is not canonical repeats its identity.
type counterRecord struct {
	Attempts  int    `json:"attempts"`
	SessionID string `json:"sessionId,omitempty"`
	AgentID   string `json:"agentId,omitempty"`
	TurnID    string `json:"turnId,omitempty"`
}

// ReadCounter reads the budget of the exact (session, agent, turn) without changing anything. Only a file that does not exist is
// missing: a file that cannot be read (or a file where a directory of its path belongs) is unreadable, and one that holds anything
// but an object whose attempts is an integer from 0 to MaxAttempts (and, for a session whose id is not canonical, this tuple's
// identity) is corrupt.
//
// Changed from the oracle (port: fixed, CRW-1106): readAttempts reads an unreadable or garbled file as 0, so a truncated counter
// restarted the budget and the next write replaced the evidence, while hasSpentBudget read the same file as spent.
func ReadCounter(cwd, sessionID, agentID, turnID string) Counter {
	var owns func(counterRecord) bool
	if !state.IsCanonicalSessionID(sessionID) {
		owns = func(r counterRecord) bool {
			return r.SessionID == sessionID && r.AgentID == agentID && r.TurnID == turnID
		}
	}
	c, _ := readCounterFile(counterPath(cwd, sessionID, agentID, turnID), owns)
	return c
}

// ReadAttempts is ReadCounter as a count: 0 for a missing counter, the count of an active one, MaxAttempts for any counter that ends
// the budget.
func ReadAttempts(cwd, sessionID, agentID, turnID string) int {
	switch c := ReadCounter(cwd, sessionID, agentID, turnID); c.State {
	case CounterMissing, CounterActive:
		return c.Attempts
	}
	return MaxAttempts
}

// readCounterFile classifies one counter file. owns, when set, is the identity check of a CRW-1106 record; a legacy file has none.
// The error is fs.ErrNotExist exactly when the file does not exist.
func readCounterFile(path string, owns func(counterRecord) bool) (Counter, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Counter{State: CounterMissing}, fs.ErrNotExist
	}
	if err != nil || !info.Mode().IsRegular() {
		return Counter{State: CounterUnreadable}, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) { // removed between the two calls
			return Counter{State: CounterMissing}, fs.ErrNotExist
		}
		return Counter{State: CounterUnreadable}, nil
	}
	return classifyCounter(raw, owns), nil
}

// classifyCounter parses a counter the way JSON.parse does (all of the file; a number beyond float64 is Infinity, not an error).
func classifyCounter(raw []byte, owns func(counterRecord) bool) Counter {
	corrupt := Counter{State: CounterCorrupt}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var o map[string]any
	if dec.Decode(&o) != nil || o == nil {
		return corrupt
	}
	if _, err := dec.Token(); err != io.EOF {
		return corrupt
	}
	n, isNumber := o["attempts"].(json.Number)
	f, _ := strconv.ParseFloat(string(n), 64)
	if !isNumber || math.IsInf(f, 0) || f != math.Trunc(f) || f < 0 || f > MaxAttempts {
		return corrupt
	}
	if owns != nil {
		session, _ := o["sessionId"].(string)
		agent, _ := o["agentId"].(string)
		turn, _ := o["turnId"].(string)
		if !owns(counterRecord{SessionID: session, AgentID: agent, TurnID: turn}) {
			return corrupt
		}
	}
	if f == MaxAttempts {
		return Counter{State: CounterExhausted, Attempts: MaxAttempts}
	}
	return Counter{State: CounterActive, Attempts: int(f)}
}

// WithCounterLock runs fn holding the lock of the exact (session, agent, turn), so the check, the reservation of the next attempt and
// its publication, and a receipt's resolution, are one step for that child (CRW-1106). The lock file lives in the state directory,
// which it creates when missing, and is removed when the lock is released (withFileLock), so it leaves nothing behind. An error
// (a link or a file at the state directory, a lock held past fileLockWait) means fn did not run. It is taken before the session
// lock.
func WithCounterLock(cwd, sessionID, agentID, turnID string, fn func() error) error {
	dir, err := ensureStateDir(cwd)
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d:%s:%s:%s", len(sessionID), sessionID, tupleDigest(agentID, turnID), "counter")))
	return withFileLock(filepath.Join(dir, "evidence-lock-"+hex.EncodeToString(sum[:16])+".lock"), fn)
}

// WriteAttempts persists the counter of the exact tuple through a temp file and a rename, and reports whether it did: a false means
// nothing durable was written, and the caller ends the budget instead of blocking again on a counter it cannot advance. A failed
// write removes the temp file this call made and nothing else (port: fixed, CRW-1106; the oracle leaves it behind).
func WriteAttempts(cwd, sessionID, agentID string, attempts int, turnID string) bool {
	path := counterPath(cwd, sessionID, agentID, turnID)
	record := counterRecord{Attempts: attempts}
	if state.IsCanonicalSessionID(sessionID) {
		if _, err := crwdir.EnsureDir(cwd); err != nil {
			return false
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
			return false
		}
	} else {
		if _, err := ensureRecordDir(cwd, AttemptsSubdir, counterVersionDir, sessionRecordDir(sessionID)); err != nil {
			return false
		}
		record = counterRecord{Attempts: attempts, SessionID: sessionID, AgentID: agentID, TurnID: turnID}
	}
	return writeRecord(path, record) == nil
}

// ClearAttempts removes the counter file of the tuple, best effort: a missing file is fine and a directory in its place stays.
func ClearAttempts(cwd, sessionID, agentID, turnID string) {
	removeFile(counterPath(cwd, sessionID, agentID, turnID))
}

func removeFile(path string) {
	if info, err := os.Lstat(path); err == nil && !info.IsDir() {
		_ = os.Remove(path)
	}
}
