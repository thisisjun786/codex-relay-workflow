package ops

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// The Op of the call rows an effect proof reads. A row is a KindCall journal row whose terminal event is completed; its
// Data carries the fields named here.
const (
	// OpCommit: node, commit, parent, tree, out_of_scope.
	OpCommit = "crw.commit"
	// OpVerify: node, head, command, ok, digest.
	OpVerify = "crw.verify"
	// OpReview: node, head, criteria_digest.
	OpReview = "crw.review"
	// OpAccept: node, head, acceptance_id.
	OpAccept = "crw.accept"
	// OpIntegrate: node, batch_commit, integration_ref.
	OpIntegrate = "crw.integrate"
	// OpGate: head, command (the plan's required set, joined with ";"), range, ok. The row of crw.finalGate (finalgate.go).
	OpGate = "crw.finalGate"
	// OpReviewerFinal: range, criteria_digest.
	OpReviewerFinal = "crw.reviewerFinal"
)

// recordOps are the calls that only record a fact (to the relay, to the parent, to the plan): a recorded one is never sent
// again.
var recordOps = []string{"crw.releaseChild", "crw.escalate", "crw.reportUp", "crw.hold", "crw.tierStep",
	"crw.spawnDecision", "crw.stepRecord", "crw.exploreRoute", "crw.exploreRecord", "crw.designAudit", "crw.designLedger",
	"crw.qaReceipt", "crw.qaCapture", "crw.reviewerFinal", "crw.audit", "crw.archive",
	"crw.replanSignals", "crw.currentDesign", "crw.reflection", "crw.signalBind", "crw.signalDone", "crw.signalClose",
	"crw.designHead", "crw.releasedPlans"}

// The keys of the rows' Data that the executor calls carry: the pending tree P a completed call recorded, and P_held for a
// held one.
const (
	keyPending = "pending_tree"
	keyHeld    = "p_held"
)

// Verdict is the outcome of a proof: whether the effect holds and, when it does not, why.
type Verdict struct {
	Proven bool   `json:"proven"`
	Why    string `json:"why,omitempty"`
	// CallID is the call whose row proves it, when a row does.
	CallID string `json:"call_id,omitempty"`
}

func ok(callID string) Verdict { return Verdict{Proven: true, CallID: callID} }

func no(format string, args ...any) Verdict { return Verdict{Why: fmt.Sprintf(format, args...)} }

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func flag(m map[string]any, key string) bool {
	b, _ := m[key].(bool)
	return b
}

// completed returns, in journal order, the completed rows of op that match.
func completed(events []engine.Event, op string, match func(map[string]any) bool) (engine.Event, bool) {
	var found engine.Event
	var hit bool
	for _, ev := range events {
		if ev.Kind != engine.KindCall || ev.Op != op || ev.Event != engine.EvCompleted {
			continue
		}
		if match(ev.Data) {
			found, hit = ev, true
		}
	}
	return found, hit
}

// VerifyProof: a verification of the same head with the same command was recorded and passed.
func VerifyProof(events []engine.Event, node, head, command string) Verdict {
	ev, hit := completed(events, OpVerify, func(d map[string]any) bool {
		return str(d, "node") == node && str(d, "head") == head && str(d, "command") == command && flag(d, "ok")
	})
	if !hit {
		return no("no passing verification of %s with the command %q is recorded for node %s", head, command, node)
	}
	return ok(ev.CallID)
}

// ReviewProof: a review of the same head against the same criteria digest was recorded.
func ReviewProof(events []engine.Event, node, head, criteriaDigest string) Verdict {
	ev, hit := completed(events, OpReview, func(d map[string]any) bool {
		return str(d, "node") == node && str(d, "head") == head && str(d, "criteria_digest") == criteriaDigest
	})
	if !hit {
		return no("no review of %s against the criteria %s is recorded for node %s", head, criteriaDigest, node)
	}
	return ok(ev.CallID)
}

// Acceptance is what the relay says about an acceptance now: whether it is the active one, whether a later change made it
// stale, and the head it accepted.
type Acceptance struct {
	Active bool
	Stale  bool
	Head   string
}

// AcceptProof: the acceptance the call recorded is active, not stale and of the same head.
func AcceptProof(events []engine.Event, node, head string, live func(acceptanceID string) (Acceptance, bool)) Verdict {
	ev, hit := completed(events, OpAccept, func(d map[string]any) bool {
		return str(d, "node") == node && str(d, "head") == head
	})
	if !hit {
		return no("no acceptance of %s is recorded for node %s", head, node)
	}
	id := str(ev.Data, "acceptance_id")
	a, found := live(id)
	switch {
	case !found:
		return no("acceptance %s is not in the relay", id)
	case !a.Active:
		return no("acceptance %s is not active", id)
	case a.Stale:
		return no("acceptance %s is stale", id)
	case a.Head != head:
		return no("acceptance %s accepted %s, not %s", id, a.Head, head)
	}
	return ok(ev.CallID)
}

// IntegrateProof: the batch commit the call recorded is an ancestor of the integration ref. Another actor moving the target
// on does not undo it: a commit that was integrated stays an ancestor of every later head.
func (p Prover) IntegrateProof(ctx context.Context, events []engine.Event, repo, node, integrationHead string) (Verdict, error) {
	ev, hit := completed(events, OpIntegrate, func(d map[string]any) bool { return str(d, "node") == node })
	if !hit {
		return no("no integration is recorded for node %s", node), nil
	}
	batch := str(ev.Data, "batch_commit")
	anc, err := IsAncestor(ctx, p.Git, repo, batch, integrationHead)
	if err != nil {
		return Verdict{}, err
	}
	if !anc {
		return no("the batch commit %s is not an ancestor of %s", batch, integrationHead), nil
	}
	return ok(ev.CallID), nil
}

// GateProof: a passing check of the current integration head with the same command set over the same range was recorded.
func GateProof(events []engine.Event, head, command, rng string) Verdict {
	ev, hit := completed(events, OpGate, func(d map[string]any) bool {
		return str(d, "head") == head && str(d, "command") == command && str(d, "range") == rng && flag(d, "ok")
	})
	if !hit {
		return no("no passing gate of %s with the command %q over %s is recorded", head, command, rng)
	}
	return ok(ev.CallID)
}

// ReviewerFinalProof: a final review of the same integration range against the same criteria digest was recorded.
func ReviewerFinalProof(events []engine.Event, rng, criteriaDigest string) Verdict {
	ev, hit := completed(events, OpReviewerFinal, func(d map[string]any) bool {
		return str(d, "range") == rng && str(d, "criteria_digest") == criteriaDigest
	})
	if !hit {
		return no("no final review of %s against the criteria %s is recorded", rng, criteriaDigest)
	}
	return ok(ev.CallID)
}

// IsRecordOp reports whether op is a call that only records a fact.
func IsRecordOp(op string) bool { return slices.Contains(recordOps, op) }

// RequestID is the idempotent request id a recording call sends with: the same for the same call of the same run, so a
// call that is sent again after a crash is recognised by whoever recorded it the first time. It is the id the engine gives an
// op (engine.RequestID).
func RequestID(runID, callID string) string { return engine.RequestID(runID, callID) }

// Replay is what a resumed run does with a recording call.
type Replay struct {
	// Recorded is set when the journal holds the call's completed row.
	Recorded bool
	// Resend is whether the call is sent again. It is never set for a recorded call; an unrecorded one is executed live
	// under the same call id and RequestID.
	Resend    bool
	CallID    string
	RequestID string
	Row       *engine.Event
}

// DecideRecord decides a recording call (escalate, reportUp, hold, tierStep, and an old run's releaseChild) from the journal alone: a call
// with a completed row is not sent again, whatever the state of the worktree or the host is; a call without one is executed
// live with the same call id and the same idempotent request id.
func DecideRecord(events []engine.Event, runID, callID string) Replay {
	d := Replay{CallID: callID, RequestID: RequestID(runID, callID)}
	for i := range events {
		ev := events[i]
		if ev.Kind == engine.KindCall && ev.CallID == callID && IsRecordOp(ev.Op) && ev.Event == engine.EvCompleted {
			d.Recorded, d.Row = true, &ev
		}
	}
	d.Resend = !d.Recorded
	return d
}

// Prover reads a worktree through git.
type Prover struct{ Git GitFunc }

// NewProver returns a prover that runs the git binary.
func NewProver() Prover { return Prover{Git: ExecGit("git")} }

// ProveNode implements engine.Prover: the proof of the node's executor calls against its current worktree. A node with no
// attempt worktree in the journal proves nothing (its calls are not proven).
func (p Prover) ProveNode(ctx context.Context, events []engine.Event, node string) (engine.NodeProof, error) {
	np, err := p.Node(ctx, events, node)
	if errors.Is(err, ErrNoWorktree) {
		return noProof{}, nil
	}
	if err != nil {
		return nil, err
	}
	return np, nil
}

type noProof struct{}

func (noProof) Proven(string) bool { return false }

// ErrNoWorktree is the refusal to prove a node that has no current attempt worktree in the journal.
var ErrNoWorktree = errors.New("ops: the node has no attempt worktree in the journal")

// CallProof is the proof of one executor call of the node's current attempt.
type CallProof struct {
	CallID string `json:"call_id"`
	Role   string `json:"role,omitempty"`
	// Event is the terminal event that recorded the state: completed (pending tree P) or held (P_held).
	Event string `json:"event"`
	// Pending is P, or P_held for a held call.
	Pending string `json:"pending"`
	// Committed is set when a recorded commit on the chain covers the call.
	Committed bool `json:"committed"`
	// Abandoned is set for a call of a retired attempt that no recorded commit carries: a later fresh attempt left its work (Prover.earlier).
	Abandoned bool `json:"abandoned,omitempty"`
	Proven    bool `json:"proven"`
	Why       string
	seq       int
}

// NodeProof is what the worktree of the node's current attempt says about the effects of its executor calls.
type NodeProof struct {
	Node     string `json:"node"`
	Worktree string `json:"worktree"`
	// Start is the commit the attempt began at; Head is the worktree's HEAD now.
	Start string `json:"start"`
	Head  string `json:"head"`
	// Snapshot is the working snapshot now.
	Snapshot string `json:"snapshot"`
	// LastCommit is the last recorded commit on the worktree's chain (Start when none).
	LastCommit string `json:"last_commit"`
	// Foreign is "commit" when the HEAD is not on the recorded chain, "snapshot" when the working files are none of the
	// recorded states, and empty otherwise.
	Foreign string `json:"foreign,omitempty"`
	// InvalidFrom is the first call whose effect is no longer there; it and every later call are not proven.
	InvalidFrom string      `json:"invalid_from,omitempty"`
	Calls       []CallProof `json:"calls"`
	// Earlier are the executor calls of the attempts before the current one (their worktrees were retired: a tier step, a rebuild),
	// each proven by a recorded commit of its own attempt that carries its state and that the current HEAD descends from, or, when no
	// recorded commit carries it, by the fresh attempt that abandoned it (Prover.earlier).
	Earlier []CallProof `json:"earlier,omitempty"`
}

// Proven reports whether the effect of an executor call holds: a call of the current attempt by the worktree, a call of a retired
// attempt by a commit of its attempt the current worktree grew from, or by the fresh attempt that abandoned its uncommitted work. A call
// that is none of them (a call of a retired attempt whose commit the current worktree did not grow from, a call that recorded no state)
// is not proven.
func (n *NodeProof) Proven(callID string) bool {
	for _, c := range n.Calls {
		if c.CallID == callID {
			return c.Proven
		}
	}
	for _, c := range n.Earlier {
		if c.CallID == callID {
			return c.Proven
		}
	}
	return false
}

// attemptCall is an executor call of the node as the journal recorded it.
type attemptCall struct {
	id, role, cwd string
	prepared      int
	alloc         map[string]any
	// state is the terminal row that recorded a state, if the call has one.
	termSeq int
	event   string
	pending string
	// strayHead and strayStart are the HEAD a session executor left at a commit of its own and the commit it started from, as its
	// completed row records them (executor_head_moved).
	strayHead, strayStart string
}

// Node proves the effects of the executor calls of one node against the worktree of its current attempt, and only that
// worktree: the directory of an attempt that was retired (a null_model attempt's uncommitted edits stay there) is never
// read. The journal says which calls and commits belong to the attempt; git says what the worktree holds now.
//
// A dirty worktree is not foreign as such. Its snapshot is valid when it is the tree of the last recorded commit, or the
// pending tree P of a build or fix journaled after that commit, or the P_held of an executor call held after it. A HEAD that
// is not the last recorded commit of the chain is a foreign commit: the first call after the last recorded commit that is
// on the chain and every later call are not proven. A snapshot that is none of the valid states leaves the commits proven
// and breaks the proof from the latest build or fix after the last valid commit.
func (p Prover) Node(ctx context.Context, events []engine.Event, node string) (*NodeProof, error) {
	calls := nodeCalls(events, node)
	var current *attemptCall
	for i := range calls {
		if k, _ := calls[i].alloc["kind"].(string); k == engine.PlaceFresh || k == engine.PlaceRestore {
			if str(calls[i].alloc, "path") != "" {
				current = &calls[i]
			}
		}
	}
	if current == nil {
		return nil, fmt.Errorf("%w: %s", ErrNoWorktree, node)
	}
	np := &NodeProof{Node: node, Worktree: str(current.alloc, "path"), Start: str(current.alloc, "start_commit")}
	// A restored worktree (a resume's suffix) holds the pending tree P of the build or fix that ended the replayed prefix: the
	// calls of the prefix ran in another worktree, and are proven by this one while its files are still P (the later call of
	// the prefix is the one P belongs to; the calls before it are proven with it).
	prefixEnd := 0
	if k, _ := current.alloc["kind"].(string); k == engine.PlaceRestore {
		if want := str(current.alloc, "pending"); want != "" {
			for i := range calls {
				if c := &calls[i]; c.prepared < current.prepared && c.termSeq > 0 && c.pending == want {
					prefixEnd = max(prefixEnd, c.termSeq)
				}
			}
		}
	}
	var mine []*attemptCall
	for i := range calls {
		c := &calls[i]
		if c.termSeq == 0 || c.pending == "" {
			continue
		}
		inPrefix := prefixEnd > 0 && c.prepared < current.prepared && c.termSeq <= prefixEnd
		if !inPrefix && (c.prepared < current.prepared || c.cwd != np.Worktree) {
			continue
		}
		mine = append(mine, c)
	}
	var commits []engine.Event
	for _, ev := range events {
		if ev.Kind == engine.KindCall && ev.Op == OpCommit && ev.Event == engine.EvCompleted && ev.Seq > current.prepared && str(ev.Data, "node") == node && str(ev.Data, "commit") != "" {
			commits = append(commits, ev)
		}
	}
	slices.SortFunc(commits, func(a, b engine.Event) int { return a.Seq - b.Seq })

	var err error
	if np.Head, err = Head(ctx, p.Git, np.Worktree); err != nil {
		return nil, err
	}
	if np.Snapshot, err = Snapshot(ctx, p.Git, np.Worktree); err != nil {
		return nil, err
	}
	np.Earlier = p.earlier(ctx, events, node, calls, current, mine, np)
	// the recorded chain: the attempt's start, then each recorded commit; k is the last of them that HEAD descends from
	chain := []string{np.Start}
	for _, c := range commits {
		chain = append(chain, str(c.Data, "commit"))
	}
	k := -1
	for i, c := range chain {
		anc, err := IsAncestor(ctx, p.Git, np.Worktree, c, np.Head)
		if err != nil {
			return nil, err
		}
		if anc {
			k = i
		}
	}
	np.LastCommit = np.Start
	if k > 0 {
		np.LastCommit = chain[k]
	}
	onChain := k >= 0 && k == len(chain)-1 && np.Head == chain[k]
	if !onChain && k >= 0 && k == len(chain)-1 {
		// A session executor that committed left HEAD at a commit of its own, which its completed row records as a stray HEAD that
		// grew from the last recorded commit: that HEAD is the call's state when its tree is the call's pending tree (CRW-1316 §6.3).
		for _, c := range mine {
			if c.strayHead != np.Head || c.strayStart != chain[k] || c.pending == "" {
				continue
			}
			if tree, err := TreeOf(ctx, p.Git, np.Worktree, np.Head); err == nil && tree == c.pending {
				onChain = true
				break
			}
		}
	}
	// A recorded commit covers the calls whose state it carries. Its row names the tree it committed; that tree is the pending
	// tree P (or P_held) of the latest call that recorded it before the commit, and the calls up to and including that one
	// are in the commit (each later state of the worktree grew out of the earlier ones). A call recorded after that one, or
	// a commit of a tree no call recorded, covers nothing of the calls it follows: a later commit is not proof of the
	// calls that precede it by sequence alone.
	covers := func(cm engine.Event, c *attemptCall) bool {
		tree := str(cm.Data, "tree")
		if tree == "" || cm.Seq <= c.termSeq {
			return false
		}
		latest := 0
		for _, m := range mine {
			if m.termSeq < cm.Seq && m.pending == tree {
				latest = max(latest, m.termSeq)
			}
		}
		return latest > 0 && c.termSeq <= latest
	}
	// commit index (1-based among the recorded commits) of the first one that covers the call
	var covering = func(c *attemptCall) int {
		for i, cm := range commits {
			if covers(cm, c) {
				return i + 1
			}
		}
		return len(commits) + 1
	}
	var after []*attemptCall // the calls no valid commit covers
	for _, c := range mine {
		cp := CallProof{CallID: c.id, Role: c.role, Event: c.event, Pending: c.pending, seq: c.termSeq}
		if k >= 0 && covering(c) <= k {
			cp.Committed, cp.Proven = true, true
		} else {
			after = append(after, c)
		}
		np.Calls = append(np.Calls, cp)
	}
	byID := func(id string) *CallProof {
		for i := range np.Calls {
			if np.Calls[i].CallID == id {
				return &np.Calls[i]
			}
		}
		return nil
	}
	invalidate := func(from int, why string) {
		for _, c := range after[from:] {
			byID(c.id).Why = why
		}
		if from < len(after) {
			np.InvalidFrom = after[from].id
		}
	}
	switch {
	case !onChain:
		np.Foreign = "commit"
		invalidate(0, fmt.Sprintf("HEAD %s is not on the recorded chain (last recorded commit on it: %s)", np.Head, np.LastCommit))
		return np, nil
	}
	tree, err := TreeOf(ctx, p.Git, np.Worktree, np.LastCommit)
	if err != nil {
		return nil, err
	}
	match := -1 // the latest call after the last valid commit whose recorded state is the snapshot
	for i, c := range after {
		if c.pending == np.Snapshot {
			match = i
		}
	}
	switch {
	case match >= 0:
		for _, c := range after[:match+1] {
			byID(c.id).Proven = true
		}
		if match+1 < len(after) {
			np.Foreign = "snapshot"
			invalidate(match+1, "the working files are the state of an earlier call: its edits are gone")
		}
	case len(after) == 0:
		if np.Snapshot != tree {
			np.Foreign = "snapshot"
		}
	case np.Snapshot == tree:
		np.Foreign = "snapshot"
		invalidate(0, "the working files are the last commit's tree: the recorded edits are gone")
	default:
		np.Foreign = "snapshot"
		invalidate(len(after)-1, "the working files are none of the recorded states: they changed after the call")
	}
	return np, nil
}

// earlier proves the executor calls of the attempts before the current one, whose worktrees were retired (a tier step, a rebuild) and
// are never read. A recorded commit of the node covers a call of such an attempt as a commit covers the current attempt's calls (its
// tree is the pending tree of the latest call of the same attempt recorded before it, and the call is that one or one before it):
// a covered call is proven when such a commit is an ancestor of the current HEAD, since its work is then in the history of the worktree
// the node goes on in, and not proven otherwise (the history it was carried in is gone). A call no recorded commit covers left only
// uncommitted work in its attempt, and a fresh attempt of the node prepared after it ended abandoned that work: a fresh attempt starts
// at the node's last commit and takes nothing of an earlier attempt's working files (the node lane's tier step and rebuild, whose
// packet carries the last findings instead), so nothing reads the call's effect again and the call is proven as the recorded fact it
// is. A git error reading the ancestry proves nothing; it never fails the proof of the current attempt.
func (p Prover) earlier(ctx context.Context, events []engine.Event, node string, calls []attemptCall, current *attemptCall, mine []*attemptCall, np *NodeProof) []CallProof {
	inMine := map[string]bool{}
	for _, c := range mine {
		inMine[c.id] = true
	}
	var commits []engine.Event
	for _, ev := range events {
		if ev.Kind == engine.KindCall && ev.Op == OpCommit && ev.Event == engine.EvCompleted && ev.Seq < current.prepared && str(ev.Data, "node") == node &&
			str(ev.Data, "commit") != "" && str(ev.Data, "tree") != "" {
			commits = append(commits, ev)
		}
	}
	ancestor := map[string]bool{}
	descends := func(commit string) bool {
		if ok, seen := ancestor[commit]; seen {
			return ok
		}
		ok, err := IsAncestor(ctx, p.Git, np.Worktree, commit, np.Head)
		ancestor[commit] = err == nil && ok
		return ancestor[commit]
	}
	// abandoned: a fresh attempt of the node in another worktree was prepared after the call ended
	abandoned := func(c *attemptCall) bool {
		for j := range calls {
			g := &calls[j]
			if k, _ := g.alloc["kind"].(string); k == engine.PlaceFresh && str(g.alloc, "path") != "" && str(g.alloc, "path") != c.cwd && g.prepared > c.termSeq {
				return true
			}
		}
		return false
	}
	var out []CallProof
	for i := range calls {
		c := &calls[i]
		if c.termSeq == 0 || c.pending == "" || c.prepared >= current.prepared || inMine[c.id] {
			continue
		}
		cp := CallProof{CallID: c.id, Role: c.role, Event: c.event, Pending: c.pending, seq: c.termSeq}
		covered := false
		for _, cm := range commits {
			tree := str(cm.Data, "tree")
			if cm.Seq <= c.termSeq {
				continue
			}
			latest := 0
			for j := range calls {
				g := &calls[j]
				if g.cwd == c.cwd && g.prepared < current.prepared && g.termSeq > 0 && g.termSeq < cm.Seq && g.pending == tree {
					latest = max(latest, g.termSeq)
				}
			}
			if latest == 0 || c.termSeq > latest {
				continue
			}
			covered = true
			if descends(str(cm.Data, "commit")) {
				cp.Committed, cp.Proven = true, true
				break
			}
		}
		switch {
		case cp.Proven:
		case covered:
			cp.Why = "a call of a retired attempt whose recorded commit the current HEAD does not descend from"
		case abandoned(c):
			cp.Abandoned, cp.Proven = true, true
		default:
			cp.Why = "a call of a retired attempt that no recorded commit carries and no fresh attempt abandoned"
		}
		out = append(out, cp)
	}
	return out
}

// nodeCalls reads the executor calls of a node from the journal, in the order they were prepared.
func nodeCalls(events []engine.Event, node string) []attemptCall {
	var out []attemptCall
	index := map[string]int{}
	for _, ev := range events {
		if ev.Kind != engine.KindCall || ev.Op != "agent" {
			continue
		}
		switch ev.Event {
		case engine.EvPrepared:
			if str(ev.Data, "node") != node || str(ev.Data, "family") != "executor" {
				continue
			}
			c := attemptCall{id: ev.CallID, role: str(ev.Data, "role"), prepared: ev.Seq, cwd: str(ev.Data, "thread_cwd")}
			if m, isMap := ev.Data["allocation"].(map[string]any); isMap {
				c.alloc = m
				// where the call runs is where its allocation puts it: a continuation that forked into a restored worktree
				// records the handle's thread cwd too, which is the directory of the attempt it left
				if p := str(m, "path"); p != "" {
					c.cwd = p
				}
			}
			index[ev.CallID] = len(out)
			out = append(out, c)
		case engine.EvCompleted, engine.EvHeld:
			i, known := index[ev.CallID]
			if !known {
				continue
			}
			key := keyPending
			if ev.Event == engine.EvHeld {
				key = keyHeld
			}
			c := &out[i]
			if st := str(ev.Data, key); st != "" {
				c.termSeq, c.event, c.pending = ev.Seq, ev.Event, st
			} else {
				c.termSeq, c.event, c.pending = 0, "", ""
			}
			c.strayHead, c.strayStart = "", ""
			if moved, ok := ev.Data["executor_head_moved"].(map[string]any); ok && ev.Event == engine.EvCompleted {
				c.strayHead, c.strayStart = str(moved, "head"), str(moved, "start")
			}
		case engine.EvNullModel, engine.EvRefused, engine.EvSuperseded, engine.EvOutcomeUnknown:
			if i, known := index[ev.CallID]; known {
				out[i].termSeq, out[i].event, out[i].pending = 0, "", ""
			}
		}
	}
	return out
}

// Describe is a one-line account of a node proof for a log or an error.
func (n *NodeProof) Describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "node %s worktree %s head %s", n.Node, n.Worktree, n.Head)
	if n.Foreign != "" {
		fmt.Fprintf(&b, " foreign %s", n.Foreign)
	}
	if n.InvalidFrom != "" {
		fmt.Fprintf(&b, " invalid from %s", n.InvalidFrom)
	}
	return b.String()
}
