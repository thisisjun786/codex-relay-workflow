package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/evidence"
)

// The flow evidence assignment of a session call (CRW-1482, 1209 field trial defect C9). An executor subagent of a session run works
// in the call's directory P, while the stop hook judged its receipt under the owner's native cwd, where the child never writes: no
// assignment named P, so three blocks spent the budget and a tombstone released the child. The engine is now the trusted writer of
// that contract. When the run has an EvidenceCwd, every spawn step of a gated agent type (executor, executor-t1, the QA capture call
// included) gets a flow record (evidence.NewFlowAssignment) under EvidenceCwd before its step_published row, rooted at P with the
// step's marker as its op; the spawn message and the packet name its one receipt file; the bind writes the same record under the
// host cwd the hook reads when that differs; and the completed row records what the hook accepted (evidence_receipt). No row kind is
// added: the facts go into the rows of before.
//
// A record lives and ends with its call attempt: its id names the call's generation (evidenceCallKey) and the attempt's marker, so
// no later attempt or generation takes it over, and it is released (removed while still open and unclaimed) whenever its attempt ends
// without its subagent: a lost attempt, an ended call, a held call that goes on with a new attempt, a call superseded by a resume or
// prepared again by one (evidenceRelease). Only a call held with its step published and no subagent bound keeps it, because the held
// call goes on in place with that step. An attempt held because the subagent that took its step could not be bound with its record
// (evidenceUnbound) has ended: it is never bound again, and the held call goes on with a new attempt and a new subagent.

// ReasonEvidenceAssignmentFailed holds a session call whose flow evidence assignment could not be recorded (CRW-1482).
const ReasonEvidenceAssignmentFailed = "evidence_assignment_failed"

// sessionEvidence is the flow evidence assignment of a session call's attempt.
type sessionEvidence struct {
	id      string
	attempt int
	// op is the spawn marker the record is bound to; rec the record as written or read back (the zero value when a resumed process
	// could not read it).
	op  string
	rec evidence.Assignment
	// receipt is the one receipt file, cwds the directories the record is written under (EvidenceCwd first), and readCwd the one
	// the hook claims it under: the bound subagent's host cwd, else EvidenceCwd.
	receipt string
	cwds    []string
	readCwd string
	// ignored is whether git ignores the receipt in P, nil when git could not say.
	ignored *bool
	// bindErr is why a subagent that took the step could not be bound with the record (its host cwd cannot hold it), and unbound
	// that subagent: the call is held (evidenceUnbound).
	bindErr error
	unbound string
}

// evidenceGated is whether the call's spawns get a flow evidence assignment: the run records them, and the role's agent type is one
// the stop hook gates.
func (c *callRun) evidenceGated() bool {
	sc := c.e.cfg.Session
	return sc != nil && sc.EvidenceCwd != "" && evidence.IsGatedAgentType(agentTypeOf(c.plan.role))
}

// ensureEvidence records the flow evidence assignment of the spawn step marker of the call's current attempt, or reuses the record
// it already has (a step published again, an adopted subagent: the id is derived from the owner, the run, the call and the marker,
// and a record that is open or already claimed for the same root and op is the contract the step was dispatched with). It makes
// P's .crw/evidence directory (real directories, no .gitignore written: whether the repository ignores the receipt is P's own fact,
// recorded as evidence_ignored). An error holds the call: no step may be dispatched with a contract the hook cannot read.
func (c *callRun) ensureEvidence(marker string) error {
	s, sc := c.sess, c.e.cfg.Session
	if err := realEvidenceDir(c.cwd); err != nil {
		return fmt.Errorf("the receipt directory in %s: %w", c.cwd, err)
	}
	id := evidence.FlowAssignmentID(sc.Owner, c.e.runID, c.evidenceCallKey(), marker)
	a, err := evidence.NewFlowAssignment(sc.Owner, id, c.cwd, marker, c.e.cfg.Now())
	if err != nil {
		return err
	}
	rec, err := evidence.EnsureFlowAssignment(sc.EvidenceCwd, a)
	if err != nil {
		return fmt.Errorf("the flow evidence assignment under %s: %w", sc.EvidenceCwd, err)
	}
	ev := &sessionEvidence{id: id, attempt: s.attempt, op: marker, rec: rec, receipt: evidence.FlowReceiptPath(rec), cwds: []string{sc.EvidenceCwd}, readCwd: sc.EvidenceCwd}
	if prev := s.evidence; prev != nil && prev.id == id {
		// a recovered step keeps where its record was written and read (the bind's host cwd)
		ev.cwds, ev.readCwd = prev.cwds, prev.readCwd
		if prev.ignored != nil {
			ev.ignored = prev.ignored
		}
	}
	if ev.ignored == nil {
		if ignored, err := evidence.ReceiptIgnored(rec); err == nil {
			ev.ignored = &ignored
		}
	}
	s.evidence = ev
	return nil
}

// realEvidenceDir makes <dir>/.crw/evidence, each step a real directory (a link is refused, crwdir.RequireDir).
func realEvidenceDir(dir string) error {
	path := dir
	for _, part := range []string{crwdir.DirName, evidence.Subdir} {
		path = filepath.Join(path, part)
		if err := os.Mkdir(path, 0o777); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		if err := crwdir.RequireDir(path); err != nil {
			return err
		}
	}
	return nil
}

// evidenceCallKey is the call as its assignment ids name it: the call id for its first prepared row, and the call id with the
// sequence of its prepared row for a call a resumed run prepared again (its position ran live), so the attempts of a later generation
// never take over a record of an earlier one (one a subagent of that generation claimed). Only a resumed run has earlier rows of the
// call (the previous processes'), and a process that resumes the generation in place reads the same prepared row (startSeq) and
// derives the same ids.
func (c *callRun) evidenceCallKey() string {
	for _, ev := range c.e.prior.rowsOf(c.id, "agent") {
		if ev.Event == EvPrepared && ev.Seq < c.startSeq {
			return fmt.Sprintf("%s@%d", c.id, c.startSeq)
		}
	}
	return c.id
}

// evidenceHold is the held result of a call whose assignment could not be recorded: nothing was dispatched. unbound is the subagent
// that took the step and was closed unbound (evidenceUnbound), "" when there is none; the held row names it (evidence_unbound), and a
// held call that goes on reads it there and starts a new attempt (sessionResumeHeld).
func (c *callRun) evidenceHold(err error, unbound string) CallResult {
	data := map[string]any{"input_state": "not_delivered",
		"detail": "the flow evidence assignment of the step could not be recorded, so no subagent was dispatched: " + err.Error()}
	if unbound != "" {
		data["evidence_unbound"] = unbound
	}
	return c.terminal(EvHeld, StatusHeld, ReasonEvidenceAssignmentFailed, data)
}

// evidenceStepData adds the step's assignment to its step_published data.
func (s *sessionCall) evidenceStepData(data map[string]any) {
	ev := s.evidence
	if ev == nil {
		return
	}
	data["evidence_assignment"] = ev.id
	if ev.ignored != nil {
		data["evidence_ignored"] = *ev.ignored
	}
}

// evidenceBind writes the record under the host cwd the subagent agent runs in when that is not EvidenceCwd (the hook reads its
// payload cwd) before the subagent is bound, and adds to the thread_started data the cwd the receipt is claimed under. A record that
// cannot be written there leaves the subagent with no contract the hook can see: it is not bound, and the reply closes it; the call
// is then held (evidenceUnbound).
func (c *callRun) evidenceBind(agent, marker, hostCwd string, data map[string]any) (DispatchReply, bool) {
	s, sc := c.sess, c.e.cfg.Session
	ev := s.evidence
	if ev == nil || ev.op != marker || ev.attempt != s.attempt {
		return DispatchReply{}, true
	}
	if hostCwd != "" && filepath.Clean(hostCwd) != filepath.Clean(sc.EvidenceCwd) {
		if _, err := evidence.EnsureFlowAssignment(hostCwd, ev.rec); err != nil {
			ev.bindErr, ev.unbound = fmt.Errorf("under the host cwd %s of %s: %w", hostCwd, agent, err), agent
			return DispatchReply{Status: ReplyClose, Close: agent, Detail: fmt.Sprintf("close %s: its evidence assignment cannot be recorded under its cwd %s: %v", agent, hostCwd, err)}, false
		}
		if !slices.Contains(ev.cwds, hostCwd) {
			ev.cwds = append(ev.cwds, hostCwd)
		}
		ev.readCwd = hostCwd
	}
	data["evidence_cwd"] = ev.readCwd
	return DispatchReply{}, true
}

// evidenceUnbound is the held result of a call whose step a subagent took that could not be bound with its record (evidenceBind),
// and whether there is one. The attempt ends with it: the held row naming the rejected subagent is written first (a process that dies
// after it never binds that subagent, which the marker search still finds for the step), then the subagent is closed and the
// attempt's record released. The held call that goes on starts a new attempt, which a new subagent takes (sessionResumeHeld).
func (c *callRun) evidenceUnbound() (CallResult, bool) {
	ev := c.sess.evidence
	if ev == nil || ev.bindErr == nil {
		return CallResult{}, false
	}
	err, agent := ev.bindErr, ev.unbound
	ev.bindErr, ev.unbound = nil, ""
	res := c.evidenceHold(err, agent)
	c.sessionClose(agent, c.sess.attempt)
	c.evidenceDrop()
	return res, true
}

// evidenceRestore reads back what a call's rows of its newest attempt say of its assignment: the last spawn step that recorded one,
// and the cwd its bind recorded for that attempt.
func (c *callRun) evidenceRestore(rows []Event) {
	s, sc := c.sess, c.e.cfg.Session
	if sc == nil || sc.EvidenceCwd == "" {
		return
	}
	var ev *sessionEvidence
	for _, row := range rows {
		d := row.Data
		switch row.Event {
		case EvPrepared:
			ev = nil
		case EvStepPublished:
			if id := rowStr(d, "evidence_assignment"); id != "" && rowStr(d, "action") == StepSpawn {
				ev = &sessionEvidence{id: id, attempt: rowInt(d, "attempt"), op: rowStr(d, "marker"), cwds: []string{sc.EvidenceCwd}, readCwd: sc.EvidenceCwd}
				if v, ok := d["evidence_ignored"].(bool); ok {
					ev.ignored = &v
				}
			}
		case EvThreadStarted:
			if ev != nil && rowStr(d, "via") == "session" && rowInt(d, "attempt") == ev.attempt {
				if cwd := rowStr(d, "evidence_cwd"); cwd != "" {
					ev.readCwd = cwd
					if !slices.Contains(ev.cwds, cwd) {
						ev.cwds = append(ev.cwds, cwd)
					}
				}
			}
		}
	}
	if ev != nil {
		if rec, ok := evidence.RecordedAssignment(sc.EvidenceCwd, sc.Owner, ev.id); ok {
			ev.rec, ev.receipt = rec, evidence.FlowReceiptPath(rec)
		} else if rec, ok := evidence.RecordedAssignment(ev.readCwd, sc.Owner, ev.id); ok {
			ev.rec, ev.receipt = rec, evidence.FlowReceiptPath(rec)
		}
	}
	s.evidence = ev
}

// evidenceDrop removes the attempt's record where it is still open and unclaimed, at every cwd it was written to, and forgets it:
// its attempt ended without its subagent (a lost attempt, an ended call), and an open record no child will claim would refuse the
// owner's children that show no packet.
func (c *callRun) evidenceDrop() {
	s := c.sess
	if s == nil || s.evidence == nil {
		return
	}
	releaseRecord(c.e.cfg.Session.Owner, s.evidence.id, s.evidence.cwds)
	s.evidence = nil
}

// evidenceRelease releases the records the rows of a call name (each spawn step's evidence_assignment) where they are still open and
// unclaimed, under EvidenceCwd and every cwd a bind of the rows recorded (evidence_cwd): the attempts those rows hold have ended
// without going on, because the call was superseded or is prepared again. A claimed record stays: it is the receipt the hook
// accepted.
func (e *Engine) evidenceRelease(rows []Event) {
	sc := e.cfg.Session
	if sc == nil || sc.EvidenceCwd == "" {
		return
	}
	cwds := []string{sc.EvidenceCwd}
	var ids []string
	for _, row := range rows {
		switch row.Event {
		case EvStepPublished:
			if id := rowStr(row.Data, "evidence_assignment"); id != "" && !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		case EvThreadStarted:
			if cwd := rowStr(row.Data, "evidence_cwd"); cwd != "" && !slices.Contains(cwds, cwd) {
				cwds = append(cwds, cwd)
			}
		}
	}
	for _, id := range ids {
		releaseRecord(sc.Owner, id, cwds)
	}
}

// releaseRecord removes the owner's record id under each of cwds where it is open and unclaimed (Assignment.Remove).
func releaseRecord(owner, id string, cwds []string) {
	for _, cwd := range cwds {
		if rec, ok := evidence.RecordedAssignment(cwd, owner, id); ok {
			rec.Remove(cwd)
		}
	}
}

// evidenceReceipt records on a completed turn's data what the hook accepted for the call's assignment (evidence_receipt): an
// accepted receipt the bound subagent claimed and whose bytes still hash to the claim's digest is copied to the step bus (which
// outlives P) and recorded with its digest, the claiming subagent and turn; anything else is missing (never claimed) or unverified
// (claimed, but by another agent or with bytes that changed since). It only records: what holds on it is the acceptance path's.
func (c *callRun) evidenceReceipt(data map[string]any) {
	s, sc := c.sess, c.e.cfg.Session
	ev := s.evidence
	if ev == nil || ev.attempt != s.attempt {
		return
	}
	out := map[string]any{"assignment": ev.id}
	if ev.ignored != nil {
		out["ignored"] = *ev.ignored
	}
	data["evidence_receipt"] = out
	if got, ok := evidence.AcceptedReceipt(ev.readCwd, sc.Owner, ev.id); ok && got.AgentID == s.agent {
		out["state"], out["path"], out["sha256"], out["agent_id"], out["turn_id"] = "accepted", got.Path, got.Digest, got.AgentID, got.TurnID
		out["turn_match"] = got.TurnID != "" && got.TurnID == s.turn
		if copied, err := sc.Bus.WriteFile(c.id, "evidence-"+ev.id+".md", got.Bytes); err != nil {
			out["copy_error"] = err.Error()
		} else {
			out["copy"] = copied
		}
		return
	}
	rec, ok := evidence.RecordedAssignment(ev.readCwd, sc.Owner, ev.id)
	switch {
	case !ok:
		out["state"], out["detail"] = "missing", "no record of the assignment under "+ev.readCwd
	case rec.Status != evidence.AssignmentClaimed || rec.AgentID == "":
		out["state"], out["detail"] = "missing", "the stop hook never accepted a receipt for the assignment"
	case rec.AgentID != s.agent:
		out["state"], out["detail"] = "unverified", fmt.Sprintf("the assignment was claimed by %s, not by the bound subagent %s", rec.AgentID, s.agent)
	default:
		out["state"], out["detail"] = "unverified", "the receipt "+ev.receipt+" is not the one the stop hook accepted (changed, moved or unreadable since)"
	}
}

// evidenceMessage is the paragraph a spawn message ends with when its step has an assignment.
func (s *sessionCall) evidenceMessage(marker string) string {
	ev := s.evidence
	if ev == nil || ev.op != marker {
		return ""
	}
	return "\n\n" + evidence.AssignmentMarker + ":" + ev.id + "]\nWrite your evidence receipt to the file " + ev.receipt +
		" before your final message; no EVIDENCE_RECORDED line is needed after the JSON."
}

// evidencePacket is the Evidence section of a packet whose call has an assignment (its spawn and its repair).
func (s *sessionCall) evidencePacket() string {
	ev := s.evidence
	if ev == nil || ev.attempt != s.attempt {
		return ""
	}
	return "## Evidence\n\nBefore your final message, write your evidence receipt to the file " + ev.receipt +
		" (its directory exists): the commands you ran and what they showed, for a change the failing run before it and the passing run after it. " +
		"The stop hook reads that file. Your final message stays the one JSON value the output contract names; no EVIDENCE_RECORDED line is needed after it.\n\n"
}
