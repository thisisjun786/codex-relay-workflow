package evidence

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// Overflow verdicts (CRW-1110, port: fixed). The session reader keeps state.MaxUnverifiedSubagents (64) unresolved verdicts and
// flags a longer list as overflowed, but the oracle's recordTombstone appends a 65th anyway: the file then holds 65, every read keeps
// 64 and marks the list corrupt, hasTombstone cannot find the new verdict, and no writer can publish the list again (the rewrite
// would lose the 65th), so even a valid receipt could not repair the session.
//
// Here the main list never grows past the cap. A verdict that does not fit is written beside it, in the session's overflow
// directory (OverflowSubdir/<session dir>/<tuple digest>.json, one file per exact agent and turn, the record repeating its
// session). HasTombstone, the late receipt (ResolveTombstone), crw pabcd evidence resolve and the goal-complete gate all read it
// by the exact session, agent and turn. A file written before the fix, holding more than the cap, is recovered under the session
// lock by the next writer: every stored verdict is checked first, then the verdicts past the cap move to the overflow directory and
// the main list is rewritten with the first 64; nothing is dropped and the corruption flag is cleared only when it came from the
// overflow alone.

// OverflowSubdir is the directory, under the state directory, of the verdicts recorded beside a full main list.
const OverflowSubdir = "evidence-overflow"

// overflowRecord is one verdict beside the main list.
type overflowRecord struct {
	SessionID      string  `json:"sessionId"`
	AgentID        string  `json:"agentId"`
	TurnID         string  `json:"turnId"`
	AgentType      string  `json:"agentType"`
	Attempts       float64 `json:"attempts"`
	ReceiptClaimed string  `json:"receiptClaimed"`
	RecordedAt     string  `json:"recordedAt"`
	Resolvable     bool    `json:"resolvable"`
}

func overflowDir(cwd, sessionID string) string {
	return filepath.Join(cwd, crwdir.DirName, OverflowSubdir, sessionRecordDir(sessionID))
}

func overflowPath(cwd, sessionID, agentID, turnID string) string {
	return filepath.Join(overflowDir(cwd, sessionID), tupleDigest(agentID, turnID)+".json")
}

// writeOverflow records e beside the main list, replacing the verdict of the same agent and turn. It returns nil only when the
// record and every directory on its way are durable (CRW-1110): an error leaves the caller's copy of the verdict in place.
func writeOverflow(cwd, sessionID string, e state.UnverifiedSubagent) error {
	if _, err := ensureRecordDir(cwd, OverflowSubdir, sessionRecordDir(sessionID)); err != nil {
		return err
	}
	if err := writeRecord(overflowPath(cwd, sessionID, e.AgentID, e.TurnID), overflowRecord{SessionID: sessionID, AgentID: e.AgentID,
		TurnID: e.TurnID, AgentType: e.AgentType, Attempts: e.Attempts, ReceiptClaimed: e.ReceiptClaimed, RecordedAt: e.RecordedAt,
		Resolvable: e.Resolvable}); err != nil {
		return err
	}
	// The record's own directory entry is synced by writeRecord; the entries of the directories on the way to it are synced here,
	// so a recovery that shortens the main list afterwards never leaves the only copy of a verdict in a directory the disk lacks.
	return syncRecordChain(cwd, OverflowSubdir, sessionRecordDir(sessionID))
}

// readOverflow decodes the overflow record at path, named name in the session's directory: a single object of the session whose
// tuple digest is its name.
func readOverflow(path, name, sessionID string) (state.UnverifiedSubagent, bool) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return state.UnverifiedSubagent{}, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return state.UnverifiedSubagent{}, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var r overflowRecord
	if dec.Decode(&r) != nil {
		return state.UnverifiedSubagent{}, false
	}
	if _, err := dec.Token(); err != io.EOF || r.SessionID != sessionID || tupleDigest(r.AgentID, r.TurnID)+".json" != name || r.RecordedAt == "" {
		return state.UnverifiedSubagent{}, false
	}
	return state.UnverifiedSubagent{AgentID: r.AgentID, TurnID: r.TurnID, AgentType: r.AgentType, Attempts: r.Attempts,
		ReceiptClaimed: r.ReceiptClaimed, RecordedAt: r.RecordedAt, Resolvable: r.Resolvable}, true
}

// OverflowVerdicts is the session's verdicts beside the main list, and whether they could all be read: a directory that cannot be
// listed, or an entry that is not one of its records, makes unreadable true, so a completion gate denies rather than reads it as
// no verdict. A missing directory holds none.
func OverflowVerdicts(cwd, sessionID string) (verdicts []state.UnverifiedSubagent, unreadable bool) {
	dir, err := existingRecordDir(cwd, OverflowSubdir, sessionRecordDir(sessionID))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false
	}
	if err != nil {
		return nil, true // a link or a file anywhere in the chain: records behind it are not this session's to read
	}
	names, err := dirNames(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false
	}
	if err != nil {
		return nil, true
	}
	for _, name := range names {
		if strings.HasSuffix(name, ".tmp") {
			continue // a write in flight, or one that failed and was not cleaned
		}
		e, ok := readOverflow(filepath.Join(dir, name), name, sessionID)
		if !ok {
			unreadable = true
			continue
		}
		verdicts = append(verdicts, e)
	}
	return verdicts, unreadable
}

// hasOverflow reports whether the exact verdict is recorded beside the main list.
func hasOverflow(cwd, sessionID, agentID, turnID string) bool {
	dir, err := existingRecordDir(cwd, OverflowSubdir, sessionRecordDir(sessionID)) // every step a real directory (CRW-1112)
	if err != nil {
		return false
	}
	name := tupleDigest(agentID, turnID) + ".json"
	_, ok := readOverflow(filepath.Join(dir, name), name, sessionID)
	return ok
}

// removeOverflow removes the exact verdict beside the main list and reports whether it was there and is gone.
func removeOverflow(cwd, sessionID, agentID, turnID string) bool {
	if !hasOverflow(cwd, sessionID, agentID, turnID) {
		return false
	}
	return os.Remove(overflowPath(cwd, sessionID, agentID, turnID)) == nil
}

// ResolveOverflowVerdict removes the exact verdict beside the main list, for crw pabcd evidence resolve; the caller holds the
// session lock and has checked the receipt.
func ResolveOverflowVerdict(cwd, sessionID, agentID, turnID string) bool {
	return removeOverflow(cwd, sessionID, agentID, turnID)
}

// publishable reports whether a list written as the main list reads back as itself: within the cap, every record whole.
func publishable(list []state.UnverifiedSubagent) bool {
	if len(list) > state.MaxUnverifiedSubagents {
		return false
	}
	raw, err := json.Marshal(list)
	if err != nil {
		return false
	}
	var back any
	if json.Unmarshal(raw, &back) != nil {
		return false
	}
	read, corrupt := state.ReconstructUnverified(back)
	return !corrupt && (len(list) == 0 && len(read) == 0 || reflect.DeepEqual(read, list))
}

// RecoverOverflow moves the verdicts past the cap of a session file written before CRW-1110 to the overflow directory and rewrites
// the main list with the first state.MaxUnverifiedSubagents, through write. The caller holds the session lock. It changes nothing
// unless the stored list is longer than the cap and every stored record reads back whole (state.RewriteKeepsStored over the whole
// list): a file with a record the reader cannot keep is left as it is, still denied as corrupt. The corruption flag the file
// stores is kept; only the flag the overflow itself raised goes away. An error means the recovery did not complete; the overflow
// records it wrote first are the same verdicts the main file still holds, so a later recovery writes them again.
func RecoverOverflow(cwd, sessionID string, write func(string, state.State) error) error {
	raw, err := state.ReadStateFile(cwd, sessionID)
	if err != nil {
		return nil
	}
	var doc map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&doc) != nil {
		return nil
	}
	list, ok := doc["unverifiedSubagents"].([]any)
	if !ok || len(list) <= state.MaxUnverifiedSubagents {
		return nil
	}
	var all []state.UnverifiedSubagent
	for start := 0; start < len(list); start += state.MaxUnverifiedSubagents {
		chunk, corrupt := state.ReconstructUnverified(list[start:min(start+state.MaxUnverifiedSubagents, len(list))])
		if corrupt {
			return nil
		}
		all = append(all, chunk...)
	}
	s, unreadable := state.ReadStateStrict(cwd, sessionID)
	if unreadable || len(all) != len(list) {
		return nil
	}
	full := s
	full.UnverifiedSubagents = all
	if !state.RewriteKeepsStored(raw, full) {
		return nil
	}
	for _, e := range all[state.MaxUnverifiedSubagents:] {
		if err := writeOverflow(cwd, sessionID, e); err != nil {
			return err
		}
	}
	s.SessionID, s.UnverifiedSubagents, s.UnverifiedCorrupt = sessionID, all[:state.MaxUnverifiedSubagents], doc["unverifiedCorrupt"] == true
	if err := write(cwd, s); err != nil && !state.Published(err) {
		return err
	}
	return nil
}
