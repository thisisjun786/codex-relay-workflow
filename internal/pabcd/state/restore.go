package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"slices"
	"strconv"
	"time"
	"unicode/utf16"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/interview"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
)

// ReadState is ReadStateStrict without the verdict: a session whose file is absent or cannot be trusted reads as a fresh IDLE
// one. Use it for advisory fields only.
func ReadState(cwd, sessionID string) State {
	s, _ := ReadStateStrict(cwd, sessionID)
	return s
}

// ReadStateStrict rebuilds the session's state from its file and says whether the file was unreadable. An absent file is a
// clean default; a file that cannot be read for another reason, is not a JSON object, or has no valid phase is a default
// marked unreadable, so a gate that must fail closed can tell "nothing to report" from "cannot tell".
func ReadStateStrict(cwd, sessionID string) (State, bool) {
	now := time.Now()
	raw, err := os.ReadFile(StatePath(cwd, sessionID))
	if err != nil {
		return defaultState(sessionID, "", now), !errors.Is(err, fs.ErrNotExist)
	}
	return restore(sessionID, raw, now)
}

// restore is the part of readStateStrict that follows the read; now stamps the defaults.
func restore(sessionID string, raw []byte, now time.Time) (State, bool) {
	m := decodeObject(raw)
	phase, _ := m["phase"].(string)
	if !slices.Contains(AllPhases(), Phase(phase)) {
		return defaultState(sessionID, "", now), true
	}
	slug, _ := m["slug"].(string)
	s := defaultState(sessionID, slug, now)
	s.Phase = Phase(phase)
	if v, ok := m["updatedAt"].(string); ok {
		s.UpdatedAt = v
	}
	dclose := reconstructDcloseRecovery(m["dcloseRecovery"], sessionID)
	s.Interview = interview.ReconstructInterview(m["interview"])
	flags, _ := m["flags"].(map[string]any)
	s.Flags = Flags{Interview: interview.IsInterviewReady(s.Interview), AuditPassed: flags["auditPassed"] == true, CheckPassed: flags["checkPassed"] == true}
	if v, ok := m["supersededBy"].(string); ok {
		s.SupersededBy = &v
	}
	if turns, ok := m["injectedTurns"].([]any); ok {
		if all := stringsOf(turns); all != nil {
			s.InjectedTurns = all
		}
	}
	s.LastInjectedPhase = phaseOf(m["lastInjectedPhase"], WorkPhases())
	s.OrchestrationActive = s.Phase != PhaseIdle && m["orchestrationActive"] == true
	s.StopBlockPhase = phaseOf(m["stopBlockPhase"], AllPhases())
	s.StopBlockCount = count(m["stopBlockCount"])
	s.StopBlockWorkPhaseID = nonEmpty(m["stopBlockWorkPhaseId"])
	s.StopMetricCursor = count(m["stopMetricCursor"])
	s.StopBlockTotal = count(m["stopBlockTotal"])
	s.StopBlockTurnID = nonEmpty(m["stopBlockTurnId"])
	s.StopBlockCapNotified = m["stopBlockCapNotified"] == true
	s.LoopArmSeen = m["loopArmSeen"] == true
	s.IdleEditNudges = count(m["idleEditNudges"])
	s.MemoryWriteRequested = m["memoryWriteRequested"] == true
	s.MemoryWriteTurn = nonEmpty(m["memoryWriteTurn"])
	s.MemoryWriteGrant = m["memoryWriteGrant"] == true
	var corrupt bool
	s.UnverifiedSubagents, corrupt = ReconstructUnverified(m["unverifiedSubagents"])
	s.UnverifiedCorrupt = m["unverifiedCorrupt"] == true || corrupt
	if s.Phase == PhaseB {
		s.PhaseEntrySource = reconstructSourceIdentity(m["phaseEntrySource"])
	}
	if root, ok := m["boundSourceRoot"].(string); ok && path.IsAbs(root) {
		s.BoundSourceRoot = &root
	}
	if s.Phase == PhaseA {
		s.PlanUnit, s.PlanEpoch = nonEmpty(m["planUnit"]), nonEmpty(m["planEpoch"])
	}
	if s.Phase == PhaseC || (s.Phase == PhaseIdle && dclose != nil) {
		s.CheckEpoch = nonEmpty(m["checkEpoch"])
	}
	s.DcloseRecovery = dclose
	return s, false
}

// decodeObject is JSON.parse of a state file: numbers stay json.Number, anything after the first value is an error, and nil
// stands for every shape the oracle treats as no state (not JSON, or not an object).
func decodeObject(raw []byte) map[string]any {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return nil
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil
	}
	m, _ := v.(map[string]any)
	return m
}

// nonEmpty is a string of at least one character, else null.
func nonEmpty(v any) *string {
	if s, ok := v.(string); ok && s != "" {
		return &s
	}
	return nil
}

// phaseOf is v as one of the phases, else null.
func phaseOf(v any, among []Phase) *Phase {
	if s, ok := v.(string); ok && slices.Contains(among, Phase(s)) {
		p := Phase(s)
		return &p
	}
	return nil
}

// stringsOf is the elements when every one is a string (an empty array too), else nil: one bad element voids the array.
func stringsOf(arr []any) []string {
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		s, ok := e.(string)
		if !ok {
			return nil
		}
		out = append(out, s)
	}
	return out
}

// number is typeof v === "number" for a value decoded with UseNumber; a literal too large for a float64 is ±Inf, as JSON.parse
// reads it.
func number(v any) (float64, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := strconv.ParseFloat(string(n), 64)
	return f, err == nil || errors.Is(err, strconv.ErrRange)
}

// count is a finite non-negative number floored, else 0; adding 0 turns a negative zero into 0, as JSON.stringify prints it.
func count(v any) float64 {
	if f, ok := number(v); ok && f >= 0 && !math.IsInf(f, 0) {
		return math.Floor(f) + 0
	}
	return 0
}

// ReconstructUnverified rebuilds the unverified-subagent list defensively. An absent or null list is the old-schema case and
// is clean; a present but malformed one is corruption, which must not launder into an empty list (every verdict resolved).
// Overflow past MaxUnverifiedSubagents is flagged rather than dropping an unresolved verdict.
func ReconstructUnverified(raw any) (entries []UnverifiedSubagent, corrupt bool) {
	entries = []UnverifiedSubagent{}
	if raw == nil {
		return entries, false
	}
	items, ok := raw.([]any)
	if !ok {
		return entries, true
	}
	for _, item := range items {
		o, ok := item.(map[string]any)
		agentID, hasAgent := o["agentId"].(string)
		recordedAt, hasTime := o["recordedAt"].(string)
		if !ok || !hasAgent || !hasTime {
			corrupt = true
			continue
		}
		if len(entries) >= MaxUnverifiedSubagents {
			return entries, true
		}
		e := UnverifiedSubagent{AgentID: agentID, AgentType: "worker", RecordedAt: recordedAt, Resolvable: o["resolvable"] != false}
		e.TurnID, _ = o["turnId"].(string)
		if t, ok := o["agentType"].(string); ok {
			e.AgentType = t
		}
		if f, ok := number(o["attempts"]); ok && !math.IsInf(f, 0) && f == math.Floor(f) { // Number.isInteger
			e.Attempts = f + 0
		}
		if c, ok := o["receiptClaimed"].(string); ok {
			e.ReceiptClaimed = c
			if u := utf16.Encode([]rune(c)); len(u) > MaxReceiptClaimLen {
				e.ReceiptClaimed = string(utf16.Decode(u[:MaxReceiptClaimLen])) // a cut inside an astral character leaves U+FFFD
			}
		}
		entries = append(entries, e)
	}
	return entries, corrupt
}

// reconstructSourceIdentity rebuilds a persisted source identity, or nil when the shape is not one the oracle wrote: a
// half-parsed identity would compare unequal against everything and refuse every B>C, and coercing dirty:"yes" to false made
// a "clean" snapshot. A dirty identity needs a tree hash (an empty one counts) and a source root must be an absolute path.
func reconstructSourceIdentity(v any) *SourceIdentity {
	o, ok := v.(map[string]any)
	kind, _ := o["kind"].(string)
	commit, hasCommit := o["commitSha"].(string)
	captured, hasTime := o["capturedAt"].(string)
	dirty, hasDirty := o["dirty"].(bool)
	if !ok || (kind != string(source.KindResolved) && kind != string(source.KindUnavailable)) || !hasCommit || !hasTime || !hasDirty {
		return nil
	}
	id := SourceIdentity{Kind: source.Kind(kind), CommitSha: commit, Dirty: dirty, CapturedAt: captured}
	if raw, present := o["treeHash"]; present || dirty {
		hash, isString := raw.(string)
		if !isString {
			return nil
		}
		id.TreeHash = &hash
	}
	if raw, present := o["sourceRoot"]; present {
		root, isString := raw.(string)
		if !isString || !path.IsAbs(root) {
			return nil
		}
		id.SourceRoot = &root
	}
	return &id
}

// reconstructDcloseRecovery rebuilds the D-close marker with the strictness of every other persisted field. An absent or
// malformed successor is kept as Legacy and never promoted to an explicit null: null authoritatively says "this close had no
// successor", and laundering damage into it would let a corrupt marker skip a real successor. (The flag does not survive a
// write and read: the persisted null reads back as explicit.) A marker of another session is dropped.
func reconstructDcloseRecovery(v any, sessionID string) *DcloseRecoveryMarker {
	m, ok := v.(map[string]any)
	epoch, _ := m["checkEpoch"].(string)
	closed, _ := m["closedWorkPhaseId"].(string)
	if !ok || m["sessionId"] != sessionID || epoch == "" || closed == "" {
		return nil
	}
	marker := &DcloseRecoveryMarker{SessionID: sessionID, CheckEpoch: epoch, ClosedWorkPhaseID: closed}
	next, present := m["nextWorkPhaseId"]
	switch n := next.(type) {
	case nil:
		marker.Legacy = !present
	case string:
		if n == "" {
			marker.Legacy = true
		} else {
			marker.NextWorkPhaseID = &n
		}
	default:
		marker.Legacy = true
	}
	return marker
}
