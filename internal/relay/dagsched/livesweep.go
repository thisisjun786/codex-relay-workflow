package dagsched

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The continuous conflict observation (CRW-410). A sweep measures, with git merge-tree, every pair of the live heads of a plan and every live head against the tip of the branch it lands on, and records
// the measurements in one transaction with a ledger row that names what prompted it. It is a measurement: nothing in the scheduler's decisions waits for it, and nothing in it stops a running child.

// What prompted a sweep.
const (
	TriggerLanding = "landing" // a head was observed contained in its target (dag-integration-observe)
	TriggerReceipt = "receipt" // a receipt was taken in (dag-accept) or the parent asked for the sweep of a receipt that is not accepted
	TriggerManual  = "manual"  // the parent asked, or dag-conflict-observe measured one pair
)

// The kinds, statuses and reasons of a sweep member.
const (
	MemberPair = "pair"
	MemberTip  = "tip"

	MemberObserved   = "observed"   // a new observation row was written
	MemberReplayed   = "replayed"   // the same heads over the same base were recorded before: the existing row stands
	MemberUnmeasured = "unmeasured" // git was not asked, or could not be: Reason says why

	ReasonHeadUnknown      = "head_unknown"      // no source names a head for the node
	ReasonCheckoutMismatch = "checkout_mismatch" // the child's checkout is not a worktree of the sweep's repository
	ReasonCommitMissing    = "commit_missing"    // a head is not in the sweep's checkout: fetch it
	ReasonNoCommonAncestor = "no_common_ancestor"
	ReasonTipUnreadable    = "tip_unreadable" // the tip could not be read, or is not in the sweep's checkout: fetch it
)

// Where a head came from.
const (
	HeadExplicit      = "explicit"       // the parent named it
	HeadAcceptance    = "acceptance"     // the head the relay read from the forge when it accepted the node's current result
	HeadChildCheckout = "child_checkout" // the HEAD of the checkout the child works in
)

// How a sweep ended.
const (
	SweepRecorded = "recorded"      // the ledger row and its members were written
	SweepAlready  = "already_swept" // the landing or acceptance the trigger rests on has its sweep
	SweepSkipped  = "skipped"       // no checkout to measure in
	SweepFailed   = "failed"        // a hook's sweep failed and wrote nothing: the next call of the same command runs it again
)

// SweepTip is a tip every live head is measured against: the branch it is the tip of, and the commit. A tip that could not be read has no SHA and is recorded as unmeasured.
type SweepTip struct{ Repository, Ref, SHA string }

func (t SweepTip) label() string { return t.Repository + "@" + t.Ref }

// SweepInput is one sweep. Repository is the local checkout to measure in: empty takes Scheduler.Checkout, then the parent_cwd of the trigger node's relationship (the relay never fetches, so a
// head or tip that is not in it is a member that says so). Heads are explicit heads by node.
type SweepInput struct {
	Repository  string
	Trigger     string
	TriggerNode string
	TriggerRef  string
	Tips        []SweepTip
	// TipSource names more tips once the sweep has a checkout to measure in and has not been done before: a tip that is read from the forge is not read for a sweep that cannot run.
	TipSource func(context.Context) []SweepTip
	Heads     map[string]string
}

// SweepMember is one measurement attempt of a sweep: a pair of nodes (left sorts before right) or one node (left) against a tip (right head), with what came of it.
type SweepMember struct {
	Seq                                                        int64
	Kind, LeftNode, RightNode, LeftHead, RightHead, LeftSource string
	RightSource, Status, Reason, Detail, ObservationID         string
	Conflicts                                                  int
	Files                                                      []string
	Drift                                                      []DriftMark
	base, tipRef                                               string
}

// SweepResult is the answer of ObserveLive.
type SweepResult struct {
	PlanID, Trigger, TriggerNode, TriggerRef, Repository, State, Reason string
	Seq                                                                 int64
	Members                                                             []SweepMember
}

// Counts are the members observed, replayed and unmeasured.
func (r SweepResult) Counts() (observed, replayed, unmeasured int) {
	for _, m := range r.Members {
		switch m.Status {
		case MemberObserved:
			observed++
		case MemberReplayed:
			replayed++
		default:
			unmeasured++
		}
	}
	return
}

func (m SweepMember) object() contract.OrderedObject {
	files := make([]any, len(m.Files))
	for i, f := range m.Files {
		files[i] = f
	}
	drift := make([]any, len(m.Drift))
	for i, d := range m.Drift {
		drift[i] = contract.OrderedObject{{Key: "node_id", Value: d.Node}, {Key: "path", Value: d.Path}}
	}
	return contract.OrderedObject{{Key: "kind", Value: m.Kind}, {Key: "left_node_id", Value: m.LeftNode}, {Key: "right_node_id", Value: optionalText(m.RightNode)},
		{Key: "left_head", Value: optionalText(m.LeftHead)}, {Key: "right_head", Value: optionalText(m.RightHead)}, {Key: "left_head_source", Value: optionalText(m.LeftSource)}, {Key: "right_head_source", Value: optionalText(m.RightSource)},
		{Key: "status", Value: m.Status}, {Key: "reason", Value: optionalText(m.Reason)}, {Key: "detail", Value: optionalText(m.Detail)}, {Key: "observation_id", Value: optionalText(m.ObservationID)},
		{Key: "conflicts", Value: int64(m.Conflicts)}, {Key: "files", Value: files}, {Key: "drift", Value: drift}}
}

// Object is the sweep as the commands print it.
func (r SweepResult) Object() contract.OrderedObject {
	observed, replayed, unmeasured := r.Counts()
	members := make([]any, len(r.Members))
	for i, m := range r.Members {
		members[i] = m.object()
	}
	return contract.OrderedObject{{Key: "state", Value: r.State}, {Key: "reason", Value: optionalText(r.Reason)}, {Key: "sweep_seq", Value: optionalSeq(r.Seq)},
		{Key: "trigger", Value: optionalText(r.Trigger)}, {Key: "trigger_node", Value: optionalText(r.TriggerNode)}, {Key: "trigger_ref", Value: optionalText(r.TriggerRef)},
		{Key: "repository", Value: optionalText(r.Repository)}, {Key: "observed", Value: observed}, {Key: "replayed", Value: replayed}, {Key: "unmeasured", Value: unmeasured}, {Key: "members", Value: members}}
}

// requireParent refuses a task that is not the registered parent of the plan's project: a measurement is the parent's.
func (s *Scheduler) requireParent(ctx context.Context, q store.Querier, snap dag.Snapshot, actor string) error {
	parents, err := projectParents(ctx, q, snap.ProjectKey)
	if err != nil {
		return err
	}
	if len(parents) != 1 || parents[0] != actor {
		return refuse(contract.RefusalScopeRoleMismatch, "task %s is not the registered parent of project %s", actor, snap.ProjectKey)
	}
	return nil
}

// ObserveLive measures every pair of the live heads of a plan and every live head against each tip it is given, and records the measurements and a ledger row in one transaction (CRW-410). Git runs
// first and writes nothing; if recording fails nothing was written and the same call can be made again. A live node is an implementation node that holds its edit regions (running, or accepted and not
// landed); its head is the one the parent named, else the head of its current accepted result, else the HEAD of the checkout its child works in. A head or tip that cannot be measured is a member that
// says why and never fails the sweep.
//
// A landing or an accepted receipt is swept once: the trigger reference is unique in the ledger, so a repeat finds the sweep that was recorded and writes nothing, and a sweep that failed before it wrote
// anything is run again by the repeat. A manual sweep repeats freely. Refusals are the commands' own: a task that is not the project's parent, a plan or node that is not there, a head that is not a commit
// id, a checkout that is not an absolute path git can read.
func (s *Scheduler) ObserveLive(ctx context.Context, plan, actor string, in SweepInput) (SweepResult, error) {
	res := SweepResult{PlanID: plan, Trigger: in.Trigger, TriggerNode: in.TriggerNode, TriggerRef: in.TriggerRef}
	switch in.Trigger {
	case TriggerLanding, TriggerReceipt, TriggerManual:
	default:
		return res, refuse(contract.RefusalMalformedReceipt, "a sweep is prompted by %s, %s or %s, not %q", TriggerLanding, TriggerReceipt, TriggerManual, in.Trigger)
	}
	if in.Trigger == TriggerManual {
		res.TriggerRef, in.TriggerRef = "", ""
	}
	if in.Repository != "" && !filepath.IsAbs(in.Repository) {
		return res, refuse(contract.RefusalMalformedReceipt, "the repository is a local checkout (an absolute path), because the merge is computed from its objects")
	}
	q := s.Store.Q(ctx)
	snap, _, err := dag.SnapshotAt(ctx, q, plan, 0)
	if err != nil {
		return res, err
	}
	if err := s.requireParent(ctx, q, snap, actor); err != nil {
		return res, err
	}
	if in.TriggerNode != "" {
		if _, ok := nodeOf(snap, in.TriggerNode); !ok {
			return res, refuse(contract.RefusalUnregisteredScope, "plan %s has no live node %s", plan, in.TriggerNode)
		}
	}
	for node, head := range in.Heads {
		n, ok := nodeOf(snap, node)
		switch {
		case !ok:
			return res, refuse(contract.RefusalUnregisteredScope, "plan %s has no live node %s", plan, node)
		case n.Kind != dag.NodeImplementation:
			return res, refuse(contract.RefusalDispositionConflict, "node %s is a %s node: it has no branch to measure", node, n.Kind)
		case !commitPattern.MatchString(head):
			return res, refuse(contract.RefusalMalformedReceipt, "a head is a full 40 hexadecimal digit commit id")
		}
	}
	if in.TriggerRef != "" {
		if seq, found, err := s.sweptTrigger(ctx, q, plan, in.Trigger, in.TriggerRef); err != nil {
			return res, err
		} else if found {
			res.State, res.Seq = SweepAlready, seq
			return res, nil
		}
	}
	checkout, err := s.resolveCheckout(ctx, q, plan, in.TriggerNode, in.Repository)
	if err != nil {
		return res, err
	}
	if checkout == "" {
		res.State, res.Reason = SweepSkipped, "no_checkout"
		return res, nil
	}
	res.Repository = checkout
	iso, err := isolate(ctx, checkout)
	if err != nil {
		if in.Repository != "" {
			return res, refuse(contract.RefusalMergeTargetUnreadable, "%s is not a repository git can read: %v", checkout, err)
		}
		res.State, res.Reason = SweepSkipped, "checkout_unreadable"
		return res, nil
	}
	defer iso.close()
	heads, err := s.sweepHeads(ctx, q, plan, snap, checkout, in.Heads)
	if err != nil {
		return res, err
	}
	tips := in.Tips
	if in.TipSource != nil {
		tips = append(append([]SweepTip(nil), tips...), in.TipSource(ctx)...)
	}
	members, err := measureSweep(ctx, iso, heads, tips)
	if err != nil {
		return res, err
	}
	if err := s.recordSweep(ctx, plan, actor, &res, repositoryNames(ctx, checkout), members); err != nil {
		return res, err
	}
	return res, nil
}

// sweptTrigger is the sequence of the ledger row of a landing or an accepted receipt, when there is one.
func (s *Scheduler) sweptTrigger(ctx context.Context, q store.Querier, plan, trigger, ref string) (int64, bool, error) {
	var seq int64
	found, err := queryOne(ctx, q, "SELECT sweep_seq FROM dag_conflict_sweeps WHERE plan_id = ? AND trigger_kind = ? AND trigger_ref = ?", []any{plan, trigger, ref}, &seq)
	return seq, found, err
}

// isGitCheckout is whether a path is an absolute path to a working tree or repository git reads.
func isGitCheckout(ctx context.Context, p string) bool {
	if !filepath.IsAbs(p) {
		return false
	}
	_, err := runGit(ctx, p, nil, "rev-parse", "--git-dir")
	return err == nil
}

// resolveCheckout is the checkout a sweep measures in: the one it was given, else Scheduler.Checkout, else the parent_cwd of an execution of the plan (the trigger node's first) that is a git checkout.
// "" is no checkout: the sweep is skipped, never refused, unless the caller named one.
func (s *Scheduler) resolveCheckout(ctx context.Context, q store.Querier, plan, node, given string) (string, error) {
	if given != "" {
		return given, nil
	}
	if s.Checkout != "" {
		return s.Checkout, nil
	}
	rows, err := q.QueryContext(ctx, "SELECT DISTINCT r.parent_cwd FROM dag_node_executions e JOIN relationships r ON r.relationship_id = e.relationship_id"+
		" WHERE e.plan_id = ? AND r.parent_cwd IS NOT NULL AND r.parent_cwd <> '' ORDER BY CASE WHEN e.node_id = ? THEN 0 ELSE 1 END, r.parent_cwd", plan, node)
	if err != nil {
		return "", err
	}
	var candidates []string
	for rows.Next() {
		var cwd string
		if err := rows.Scan(&cwd); err != nil {
			_ = rows.Close()
			return "", err
		}
		candidates = append(candidates, cwd)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return "", err
	}
	if err := rows.Close(); err != nil {
		return "", err
	}
	for _, cwd := range candidates {
		if isGitCheckout(ctx, cwd) {
			return cwd, nil
		}
	}
	return "", nil
}

// recordSweep writes a sweep in one transaction: the observations (with their files and drift), the ledger row and the members. A hook's trigger that already has its row is found again here (another
// writer won) and nothing is written. res is filled only when the transaction commits.
func (s *Scheduler) recordSweep(ctx context.Context, plan, actor string, res *SweepResult, names []string, members []SweepMember) error {
	return s.Store.Compose(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		tx := s.Store.Q(txCtx)
		current, _, err := dag.SnapshotAt(txCtx, tx, plan, 0)
		if err != nil {
			return err
		}
		for _, m := range members {
			for _, id := range []string{m.LeftNode, m.RightNode} {
				if id == "" {
					continue
				}
				if _, ok := nodeOf(current, id); !ok {
					return refuse(contract.RefusalUnregisteredScope, "plan %s no longer has the live node %s", plan, id)
				}
			}
		}
		if res.TriggerRef != "" {
			if seq, found, err := s.sweptTrigger(txCtx, tx, plan, res.Trigger, res.TriggerRef); err != nil {
				return err
			} else if found {
				res.State, res.Seq, res.Members = SweepAlready, seq, nil
				return nil
			}
		}
		declarations, err := loadDeclarations(txCtx, tx, plan)
		if err != nil {
			return err
		}
		var last sql.NullInt64
		if err := tx.QueryRowContext(txCtx, "SELECT MAX(sweep_seq) FROM dag_conflict_sweeps WHERE plan_id = ?", plan).Scan(&last); err != nil {
			return err
		}
		seq := last.Int64 + 1
		if _, err := tx.ExecContext(txCtx, "INSERT INTO dag_conflict_sweeps (plan_id, sweep_seq, trigger_kind, trigger_node, trigger_ref, repository, observed_by, observed_at) VALUES (?,?,?,?,?,?,?,?)",
			plan, seq, res.Trigger, res.TriggerNode, res.TriggerRef, res.Repository, actor, s.now()); err != nil {
			return err
		}
		recorded := make([]SweepMember, len(members))
		for i, m := range members {
			m.Seq = int64(i + 1)
			if m.Status != MemberUnmeasured {
				if err := s.recordMeasured(txCtx, tx, plan, actor, res.Repository, names, declarations, &m); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(txCtx, "INSERT INTO dag_conflict_sweep_members (plan_id, sweep_seq, member_seq, kind, left_node_id, right_node_id, left_head, right_head, left_head_source, right_head_source,"+
				" status, reason, observation_id, conflicts) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
				plan, seq, m.Seq, m.Kind, m.LeftNode, m.RightNode, m.LeftHead, m.RightHead, m.LeftSource, m.RightSource, m.Status, m.Reason, m.ObservationID, m.Conflicts); err != nil {
				return err
			}
			recorded[i] = m
		}
		if s.testInSweepTx != nil {
			if err := s.testInSweepTx(); err != nil {
				return err
			}
		}
		res.State, res.Seq, res.Members = SweepRecorded, seq, recorded
		return nil
	})
}

// recordMeasured writes the observation of a measured member, or finds the one the same heads and base were recorded under, and gives the member its status, observation id and drift.
func (s *Scheduler) recordMeasured(ctx context.Context, tx store.Querier, plan, actor, checkout string, names []string, declarations map[string][]Region, m *SweepMember) error {
	var id, find string
	var findArgs []any
	var nodes []string
	switch m.Kind {
	case MemberPair:
		id = "dco-" + shaOf([]byte(strings.Join([]string{plan, m.LeftNode, m.RightNode, m.LeftHead, m.RightHead, m.base}, "|")))[:32]
		find = "SELECT observation_id, conflict_count FROM dag_conflict_observations WHERE plan_id = ? AND left_node_id = ? AND right_node_id = ? AND left_head = ? AND right_head = ? AND base_sha = ?"
		findArgs = []any{plan, m.LeftNode, m.RightNode, m.LeftHead, m.RightHead, m.base}
		nodes = []string{m.LeftNode, m.RightNode}
	default:
		id = "dto-" + shaOf([]byte(strings.Join([]string{plan, m.LeftNode, m.LeftHead, m.RightHead, m.base}, "|")))[:32]
		find = "SELECT observation_id, conflict_count FROM dag_tip_conflict_observations WHERE plan_id = ? AND node_id = ? AND head = ? AND tip_sha = ? AND base_sha = ?"
		findArgs = []any{plan, m.LeftNode, m.LeftHead, m.RightHead, m.base}
		nodes = []string{m.LeftNode}
	}
	var existing string
	var count int64
	found, err := queryOne(ctx, tx, find, findArgs, &existing, &count)
	if err != nil {
		return err
	}
	if found {
		// the same commits over the same base merge the same way; a different answer would mean the repository changed under the heads, which cannot happen to a commit id
		if int(count) != m.Conflicts {
			return refuse(contract.RefusalDispositionConflict, "the measurement was recorded with %d conflicts and merges with %d now", count, m.Conflicts)
		}
		m.Status, m.ObservationID = MemberReplayed, existing
		m.Drift, err = loadDrift(ctx, tx, existing)
		return err
	}
	m.Status, m.ObservationID = MemberObserved, id
	if m.Kind == MemberPair {
		_, err = tx.ExecContext(ctx, "INSERT INTO dag_conflict_observations (observation_id, plan_id, left_node_id, right_node_id, repository, left_head, right_head, base_sha, conflict_count, method, observed_by, observed_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
			id, plan, m.LeftNode, m.RightNode, checkout, m.LeftHead, m.RightHead, m.base, m.Conflicts, mergeTreeMethod, actor, s.now())
	} else {
		_, err = tx.ExecContext(ctx, "INSERT INTO dag_tip_conflict_observations (observation_id, plan_id, node_id, repository, head, head_source, tip_ref, tip_sha, base_sha, conflict_count, method, observed_by, observed_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)",
			id, plan, m.LeftNode, checkout, m.LeftHead, m.LeftSource, m.tipRef, m.RightHead, m.base, m.Conflicts, mergeTreeMethod, actor, s.now())
	}
	if err != nil {
		return err
	}
	// the files git could not merge, under each name the checkout is known by (CRW-409), and the nodes whose declarations do not cover them (drift)
	for _, name := range names {
		for _, file := range m.Files {
			var err error
			if m.Kind == MemberPair {
				_, err = tx.ExecContext(ctx, "INSERT INTO dag_conflict_observation_files (observation_id, repository, path) VALUES (?,?,?)", id, name, file)
			} else {
				_, err = tx.ExecContext(ctx, "INSERT INTO dag_tip_conflict_observation_files (observation_id, repository, path) VALUES (?,?,?)", id, name, file)
			}
			if err != nil {
				return err
			}
		}
	}
	m.Drift = driftOf(m.Files, nodes, declarations, names)
	for _, d := range m.Drift {
		if _, err := tx.ExecContext(ctx, "INSERT INTO dag_conflict_drift (observation_id, node_id, path) VALUES (?,?,?)", id, d.Node, d.Path); err != nil {
			return err
		}
	}
	return nil
}

// loadDrift is the drift recorded with an observation.
func loadDrift(ctx context.Context, q store.Querier, observation string) ([]DriftMark, error) {
	rows, err := q.QueryContext(ctx, "SELECT node_id, path FROM dag_conflict_drift WHERE observation_id = ? ORDER BY path, node_id", observation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DriftMark
	for rows.Next() {
		var d DriftMark
		if err := rows.Scan(&d.Node, &d.Path); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// sweepAfter runs the sweep a landing or an accepted receipt owes, after the command's own transaction: best effort, because a measurement decides nothing. Whatever goes wrong is the answer's state (failed,
// with the reason), the command still succeeds, and nothing was written, so the same command run again runs the sweep again. A trigger whose sweep exists answers already_swept; no checkout, skipped.
func (s *Scheduler) sweepAfter(ctx context.Context, plan, node, actor, trigger, ref string, tips []SweepTip, tipSource func(context.Context) []SweepTip) *SweepResult {
	res, err := s.ObserveLive(ctx, plan, actor, SweepInput{Trigger: trigger, TriggerNode: node, TriggerRef: ref, Tips: tips, TipSource: tipSource})
	if err != nil {
		res.State, res.Reason, res.Members = SweepFailed, err.Error(), nil
	}
	return &res
}
