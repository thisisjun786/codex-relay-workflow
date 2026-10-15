package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/evidence"
	"golang.org/x/sys/unix"
)

// The flow evidence assignment of a session call (CRW-1482, 1209 field trial defect C9). An executor subagent of a session run works
// in the call's directory P, while the stop hook judged its receipt under the owner's native cwd, where the child never writes: no
// assignment named P, so three blocks spent the budget and a tombstone released the child. The engine is the trusted writer of
// that contract. When the run has an EvidenceCwd, every spawn step of a gated agent type (executor, executor-t1, the QA capture call
// included) gets a flow record (evidence.NewFlowAssignment) rooted at P with the step's marker as its op; the spawn message and the
// packet name its one receipt file; the bind places the same record under the host cwd the hook reads when that differs; and the
// completed row records what the hook accepted (evidence_receipt).
//
// A record lives and ends with its call attempt: its id names the call's generation (evidenceCallKey) and the attempt's marker, so
// no later attempt or generation takes it over, and it is released (removed while still open and unclaimed) whenever its attempt ends
// without its subagent: a lost attempt, an ended call, a held call that goes on with a new attempt, a call superseded by a resume or
// prepared again by one (evidenceRelease). Only a call held with its step published and no subagent bound keeps it, because the held
// call goes on in place with that step. An attempt held because the subagent that took its step could not be bound with its record
// (evidenceUnbound) has ended: it is never bound again, and the held call goes on with a new attempt and a new subagent.
//
// Write after naming (CRW-1496). The stop hook claims under its payload cwd, the cwd the owner thread runs in, which its subagents
// inherit, or the host cwd the host reports for the subagent; the run may have been started in another directory (EvidenceCwd) and
// may be resumed from yet another. A step's record is written under its base directory (EvidenceCwd when the step is first
// published, the directory it was first written under when a resumed run publishes it again), under the owner's cwd as the host
// reports it (thread/read of the owner), both before the step_published row, and under the bound subagent's host cwd before its
// thread_started row. The engine writes a record under a directory only through evidencePlace, and only once a durable row of the
// call's newest generation (the rows from its newest prepared row on) names that directory: the prepared row's evidence_cwd and
// evidence_cwds, a step_published row's evidence_cwds, a thread_started row's evidence_cwd, or an evidence_placed row, the intent
// evidencePlace commits first when no other row names the directory yet. So a process that dies after any write leaves a row naming
// where it wrote, and every restore, reuse and release reads the directories from the rows (evidenceRowCwds), never only the
// process's own EvidenceCwd. P's .crw/evidence holds a .gitignore of "*" the engine writes when it is missing, so a receipt never
// enters the pending tree or a commit of P in a repository that does not ignore .crw/.

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
// and a record that is open or already claimed for the same root and op is the contract the step was dispatched with). The record
// is placed (evidencePlace) under the step's base directory (EvidenceCwd, or for a step published again the directory its row names
// first) and under the owner thread's cwd when the host reports one that differs, before the step is published, so a child that
// stops before it is bound finds its contract where the hook reads it. It makes P's .crw/evidence directory with its .gitignore
// (realEvidenceDir). An error holds the call: no step may be dispatched with a contract the hook cannot read.
func (c *callRun) ensureEvidence(ctx context.Context, marker string) error {
	s, sc := c.sess, c.e.cfg.Session
	if err := realEvidenceDir(c.cwd); err != nil {
		return fmt.Errorf("the receipt directory in %s: %w", c.cwd, err)
	}
	id := evidence.FlowAssignmentID(sc.Owner, c.e.runID, c.evidenceCallKey(), marker)
	a, err := evidence.NewFlowAssignment(sc.Owner, id, c.cwd, marker, c.e.cfg.Now())
	if err != nil {
		return err
	}
	prev := s.evidence
	if prev != nil && prev.id != id {
		prev = nil
	}
	base := sc.EvidenceCwd
	if prev != nil && len(prev.cwds) > 0 {
		base = prev.cwds[0]
	}
	rec, err := c.evidencePlace(base, a)
	if err != nil {
		return fmt.Errorf("the flow evidence assignment under %s: %w", base, err)
	}
	ev := &sessionEvidence{id: id, attempt: s.attempt, op: marker, rec: rec, receipt: evidence.FlowReceiptPath(rec), cwds: []string{base}, readCwd: base}
	if prev != nil {
		// a recovered step keeps where its record was written and read (the owner's cwd, the bind's host cwd)
		for _, cwd := range prev.cwds {
			if !slices.Contains(ev.cwds, cwd) {
				ev.cwds = append(ev.cwds, cwd)
			}
		}
		if prev.readCwd != "" {
			ev.readCwd = prev.readCwd
		}
		ev.ignored = prev.ignored
	}
	if owner := c.e.evidenceOwnerCwd(ctx); owner != "" && !slices.ContainsFunc(ev.cwds, func(cwd string) bool { return filepath.Clean(cwd) == filepath.Clean(owner) }) {
		if _, err := c.evidencePlace(owner, rec); err != nil {
			return fmt.Errorf("the flow evidence assignment under the owner's cwd %s: %w", owner, err)
		}
		ev.cwds = append(ev.cwds, owner)
		if prev == nil || prev.readCwd == "" || prev.readCwd == base {
			ev.readCwd = owner
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

// evidencePlace writes the flow record rec under cwd (evidence.EnsureFlowAssignment) once a durable row of the call's newest
// generation names cwd. When none does, it first appends the call's evidence_placed row (evidence_assignment, evidence_cwd, attempt,
// marker), an intent the journal commits before Append returns; a row that cannot be stored is an error, and nothing is written. It
// is the only writer of a flow record in the engine: every write of ensureEvidence and evidenceBind goes through it, so a process
// that dies after a write always leaves a row naming the directory, which the release and the restore read (CRW-1496).
func (c *callRun) evidencePlace(cwd string, rec evidence.Assignment) (evidence.Assignment, error) {
	if cwds, _ := evidenceRowCwds(evidenceRows(CallEvents(c.e.j, c.id))); !slices.Contains(cwds, filepath.Clean(cwd)) {
		attempt := 0
		if c.sess != nil {
			attempt = c.sess.attempt
		}
		// appended directly, not through c.event: the row is no progress of the call, so the call's last row (lastSeq, lastEvent)
		// stays the one an interrupt reads
		c.e.j.Append(Event{RunID: c.e.runID, Kind: KindCall, CallID: c.id, Path: c.path.id, Position: c.position, Op: "agent", Label: c.label, Event: EvEvidencePlaced,
			Data: map[string]any{"evidence_assignment": rec.ID, "evidence_cwd": cwd, "attempt": attempt, "marker": rec.Op}})
		if err := journalErr(c.e.j); err != nil {
			return evidence.Assignment{}, fmt.Errorf("the evidence_placed row of %s cannot be stored: %w", cwd, err)
		}
	}
	return evidence.EnsureFlowAssignment(cwd, rec)
}

// evidenceRows are the rows of a call's newest generation: those from its last prepared row on (all of them when it has none).
func evidenceRows(rows []Event) []Event {
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].Event == EvPrepared {
			return rows[i:]
		}
	}
	return rows
}

// evidenceRowCwds are the directories a call's rows name as holding a flow record (the prepared row's evidence_cwd and
// evidence_cwds, a step_published row's evidence_cwds, a thread_started or evidence_placed row's evidence_cwd), each cleaned
// (filepath.Clean) and once, and the record ids they name (a step_published or evidence_placed row's evidence_assignment).
func evidenceRowCwds(rows []Event) (cwds, ids []string) {
	addCwd := func(cwd string) {
		if cwd == "" {
			return
		}
		if cwd = filepath.Clean(cwd); !slices.Contains(cwds, cwd) {
			cwds = append(cwds, cwd)
		}
	}
	addID := func(id string) {
		if id != "" && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	for _, row := range rows {
		d := row.Data
		switch row.Event {
		case EvPrepared:
			addCwd(rowStr(d, "evidence_cwd"))
			for _, cwd := range rowStrs(d, "evidence_cwds") {
				addCwd(cwd)
			}
		case EvStepPublished:
			addID(rowStr(d, "evidence_assignment"))
			for _, cwd := range rowStrs(d, "evidence_cwds") {
				addCwd(cwd)
			}
		case EvThreadStarted:
			addCwd(rowStr(d, "evidence_cwd"))
		case EvEvidencePlaced:
			addID(rowStr(d, "evidence_assignment"))
			addCwd(rowStr(d, "evidence_cwd"))
		}
	}
	return cwds, ids
}

// evidenceOwner is the owner thread's cwd as the host reports it, read once per process: the cwd the stop hook claims the owner's
// children's receipts under. A read that fails is tried again by the next step; a thread the host does not have is no cwd.
type evidenceOwner struct {
	mu    sync.Mutex
	known bool
	cwd   string
}

// evidenceOwnerCwd is the owner thread's cwd, "" when the host does not say.
func (e *Engine) evidenceOwnerCwd(ctx context.Context) string {
	sc := e.cfg.Session
	if sc == nil || sc.Host == nil || sc.Owner == "" {
		return ""
	}
	o := &e.evOwner
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.known {
		return o.cwd
	}
	ag, err := sc.Host.ReadAgent(ctx, sc.Owner)
	switch {
	case err == nil:
		o.known, o.cwd = true, ag.Cwd
	case errors.Is(err, ErrSessionAgentNotFound):
		o.known = true
	}
	return o.cwd
}

// realEvidenceDir makes <dir>/.crw/evidence, each step a real directory, and its .gitignore of "*" when there is none (an existing
// entry, a link included, is left as it is). It walks from dir by descriptors: dir, .crw and .crw/evidence are each opened with
// O_DIRECTORY|O_NOFOLLOW relative to the one before, and the .gitignore is created relative to the last with O_EXCL|O_NOFOLLOW, so a
// link at any step, there before the call or swapped in while it runs, is refused (an error that wraps crwdir.ErrLink) and nothing
// is created or written through it. The .gitignore ignores itself and every receipt, so neither reaches the pending tree
// (Snapshot) or a commit of P in a repository that does not ignore .crw/. .crw itself gets no .gitignore here: the one the .crw
// tree's writers publish (crwdir.GitignoreText) is tracked by its own rule.
func realEvidenceDir(dir string) error {
	const dirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	fd, err := unix.Open(dir, dirFlags, 0)
	if err != nil {
		return openError(dir, err)
	}
	path := dir
	for _, part := range []string{crwdir.DirName, evidence.Subdir} {
		path = filepath.Join(path, part)
		if err := unix.Mkdirat(fd, part, 0o777); err != nil && !errors.Is(err, unix.EEXIST) {
			_ = unix.Close(fd)
			return &fs.PathError{Op: "mkdir", Path: path, Err: err}
		}
		next, err := unix.Openat(fd, part, dirFlags, 0)
		_ = unix.Close(fd)
		if err != nil {
			return openError(path, err)
		}
		fd = next
	}
	defer unix.Close(fd)
	ignore := filepath.Join(path, ".gitignore")
	gfd, err := unix.Openat(fd, ".gitignore", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o666)
	switch {
	case errors.Is(err, unix.EEXIST):
		return nil
	case err != nil:
		return &fs.PathError{Op: "open", Path: ignore, Err: err}
	}
	f := os.NewFile(uintptr(gfd), ignore)
	_, err = f.WriteString(evidenceGitignore)
	return errors.Join(err, f.Close())
}

// openError is the error of a directory of the walk that could not be opened: the link or non-directory refusal of
// crwdir.RequireDir when that is what stands there, else the open error.
func openError(path string, err error) error {
	if derr := crwdir.RequireDir(path); derr != nil {
		return derr
	}
	return &fs.PathError{Op: "open", Path: path, Err: err}
}

// evidenceGitignore is the .gitignore the engine writes in P's .crw/evidence.
const evidenceGitignore = "# CRW wrote this: flow evidence receipts are local runtime state, never part of the tree.\n*\n"

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
	data["evidence_cwds"] = slices.Clone(ev.cwds)
	if ev.ignored != nil {
		data["evidence_ignored"] = *ev.ignored
	}
}

// evidenceBind places the record under the host cwd the subagent agent runs in when no directory of the record is that cwd (the hook
// reads its payload cwd) before the subagent is bound, so the evidence_placed row that names that cwd, when no row does yet, comes
// before the thread_started row; it adds to the thread_started data the cwd the receipt is claimed under. A record that cannot be
// placed there (its row cannot be stored, or the record cannot be written) leaves the subagent with no contract the hook can see: it
// is not bound, and the reply closes it; the call is then held (evidenceUnbound).
func (c *callRun) evidenceBind(agent, marker, hostCwd string, data map[string]any) (DispatchReply, bool) {
	s := c.sess
	ev := s.evidence
	if ev == nil || ev.op != marker || ev.attempt != s.attempt {
		return DispatchReply{}, true
	}
	if hostCwd != "" {
		if !slices.ContainsFunc(ev.cwds, func(cwd string) bool { return filepath.Clean(cwd) == filepath.Clean(hostCwd) }) {
			if _, err := c.evidencePlace(hostCwd, ev.rec); err != nil {
				ev.bindErr, ev.unbound = fmt.Errorf("under the host cwd %s of %s: %w", hostCwd, agent, err), agent
				return DispatchReply{Status: ReplyClose, Close: agent, Detail: fmt.Sprintf("close %s: its evidence assignment cannot be recorded under its cwd %s: %v", agent, hostCwd, err)}, false
			}
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
// the cwd its bind recorded for that attempt, and every directory an evidence_placed row of the generation names for its record.
func (c *callRun) evidenceRestore(rows []Event) {
	s, sc := c.sess, c.e.cfg.Session
	if sc == nil || sc.EvidenceCwd == "" {
		return
	}
	var ev *sessionEvidence
	var placed []Event
	for _, row := range rows {
		d := row.Data
		switch row.Event {
		case EvPrepared:
			ev, placed = nil, nil
		case EvEvidencePlaced:
			// a write of ensureEvidence comes before the step_published row: merged against the final ev below
			placed = append(placed, row)
		case EvStepPublished:
			if id := rowStr(d, "evidence_assignment"); id != "" && rowStr(d, "action") == StepSpawn {
				ev = &sessionEvidence{id: id, attempt: rowInt(d, "attempt"), op: rowStr(d, "marker"), cwds: []string{sc.EvidenceCwd}, readCwd: sc.EvidenceCwd}
				if cwds := rowStrs(d, "evidence_cwds"); len(cwds) > 0 {
					// where the record was written (a row of before this field: EvidenceCwd); the hook reads the last of them
					ev.cwds, ev.readCwd = cwds, cwds[len(cwds)-1]
				}
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
		for _, row := range placed {
			if cwd := rowStr(row.Data, "evidence_cwd"); cwd != "" && rowStr(row.Data, "evidence_assignment") == ev.id &&
				!slices.ContainsFunc(ev.cwds, func(have string) bool { return filepath.Clean(have) == filepath.Clean(cwd) }) {
				ev.cwds = append(ev.cwds, cwd)
			}
		}
		for _, cwd := range append(slices.Clone(ev.cwds), ev.readCwd) {
			if rec, ok := evidence.RecordedAssignment(cwd, sc.Owner, ev.id); ok {
				ev.rec, ev.receipt = rec, evidence.FlowReceiptPath(rec)
				break
			}
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

// evidenceRelease releases the records the rows of a call name where they are still open and unclaimed: the attempts those rows hold
// have ended without going on, because the call was superseded or is prepared again. It reads every row it is given, of every
// generation (evidenceRowCwds): each spawn step's and each evidence_placed row's evidence_assignment, under every directory the rows
// name (the prepared row's evidence_cwd and evidence_cwds, each step's evidence_cwds, a bind's evidence_cwd, each evidence_placed
// row's evidence_cwd). It also reads EvidenceCwd, the owner's cwd as the host reports it now and the derived ids (evidenceDerivedIDs):
// those cover a journal written before evidence_placed rows, and a record of a step whose process died after writing it and before
// the step_published row that would name its id. A claimed record stays: it is the receipt the hook accepted.
func (e *Engine) evidenceRelease(rows []Event) {
	sc := e.cfg.Session
	if sc == nil || sc.EvidenceCwd == "" {
		return
	}
	cwds := []string{filepath.Clean(sc.EvidenceCwd)}
	add := func(cwd string) {
		if cwd != "" && !slices.Contains(cwds, filepath.Clean(cwd)) {
			cwds = append(cwds, filepath.Clean(cwd))
		}
	}
	add(e.evidenceOwnerCwd(e.ctx))
	named, ids := evidenceRowCwds(rows)
	for _, cwd := range named {
		add(cwd)
	}
	for _, id := range e.evidenceDerivedIDs(rows) {
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	for _, id := range ids {
		releaseRecord(sc.Owner, id, cwds)
	}
}

// evidenceDerivedIDs are the ids of the steps a process could have been about to publish when it died, after writing their records
// and before their step_published rows: for every generation of the call (its key as evidenceCallKey names it), a spawn step of a
// turn k up to one past the largest that generation's rows record, at its largest attempt or the next one (attempts only grow, and
// a new attempt is one more than the largest). That is at most 2*(k+1) ids a generation, linear in the generations; every
// generation is derived, because a process may die between a new generation's prepared row and the supersession that releases the
// generation before it. An id that names no record releases nothing.
func (e *Engine) evidenceDerivedIDs(rows []Event) []string {
	var ids []string
	derive := func(gen []Event, key string) {
		maxK, maxAttempt := 0, 1
		for _, row := range gen {
			maxK, maxAttempt = max(maxK, rowInt(row.Data, "k")), max(maxAttempt, rowInt(row.Data, "attempt"))
		}
		for k := 1; k <= maxK+1; k++ {
			for _, a := range []int{maxAttempt, maxAttempt + 1} {
				ids = append(ids, evidence.FlowAssignmentID(e.cfg.Session.Owner, e.runID, key, sessionOp(fmt.Sprintf("%s/t%d", gen[0].CallID, k), a)))
			}
		}
	}
	start, key := -1, ""
	for i, row := range rows {
		if row.Event != EvPrepared {
			continue
		}
		if start >= 0 {
			derive(rows[start:i], key)
			key = fmt.Sprintf("%s@%d", row.CallID, row.Seq)
		} else {
			key = row.CallID
		}
		start = i
	}
	if start >= 0 {
		derive(rows[start:], key)
	}
	return ids
}

// rowStrs is the list of strings a row's key holds (a []string as written, a []any once read back from the journal).
func rowStrs(d map[string]any, key string) []string {
	var out []string
	switch v := d[key].(type) {
	case []string:
		out = append(out, v...)
	case []any:
		for _, x := range v {
			if s, ok := x.(string); ok && s != "" {
				out = append(out, s)
			}
		}
	}
	return out
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
