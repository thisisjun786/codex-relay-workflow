package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
)

// EvWorktreeRestored is the flow row F07's suffix restore writes once per node and lineage: the worktree it made at C
// with the proven pending tree P on top, and the snapshot digest it verified. The node's first executor call of the
// suffix adopts that worktree as its cwd.
const EvWorktreeRestored = "worktree_restored"

// OpCommit is the Op of the call row crw.commit (F08) journals; the result commit of its last completed row on a node is
// the start commit C of the node's next attempt.
const OpCommit = "crw.commit"

// AllocatorConfig is what the allocator needs.
type AllocatorConfig struct {
	// Repo is the git repository (the project) every worktree is made from.
	Repo string
	// StateDir is the state root: a run's directories are <StateDir>/flow/<run>/...
	StateDir string
	RunID    string
	// Base is the integration target tip read at the node's first release. It is asked only for a node that has no
	// allocation and no commit yet; nil falls back to the repository's HEAD.
	Base func(node string) (string, error)
	// Git runs git commands; nil runs the git binary.
	Git GitFunc
}

// Allocator is the one allocator of the working directories of a run (engine.dispatch.attemptWorktree). Executor
// worktrees are made in exactly two ways: a new allocation at C for a call that creates a thread, or the adoption of
// the worktree a resume suffix restored, which is never re-made or reset. Every other role gets a call-private
// directory. crw.worktree (F08) wraps this type.
type Allocator struct {
	cfg AllocatorConfig
	git GitFunc
}

// NewAllocator returns the allocator of a run.
func NewAllocator(cfg AllocatorConfig) (*Allocator, error) {
	if cfg.Repo == "" || cfg.StateDir == "" || cfg.RunID == "" {
		return nil, errors.New("agent: the allocator needs a repository, a state directory and a run id")
	}
	if !filepath.IsAbs(cfg.Repo) || !filepath.IsAbs(cfg.StateDir) {
		return nil, errors.New("agent: the repository and the state directory must be absolute paths")
	}
	a := &Allocator{cfg: cfg, git: cfg.Git}
	if a.git == nil {
		a.git = ExecGit("git")
	}
	return a, nil
}

// WorktreePath is the path of an attempt worktree of a node.
func (a *Allocator) WorktreePath(node string, attempt, lineage int) string {
	return filepath.Join(a.cfg.StateDir, "flow", a.cfg.RunID, "wt", pathPart(node), fmt.Sprintf("a%d-l%d", attempt, lineage))
}

// CallPath is the private directory of a call of any other role.
func (a *Allocator) CallPath(callID string) string {
	return filepath.Join(a.cfg.StateDir, "flow", a.cfg.RunID, "calls", callID, "wt")
}

// callPathFor is the private directory of the call's next allocation. A call that runs again (a suffix of a resume runs it
// with the call id its position has) never reuses the directory of an earlier allocation: a path belongs to one allocation,
// so the nth allocation of a call is `wt-n`, and a directory that exists already is skipped.
func (a *Allocator) callPathFor(events []engine.Event, callID string) string {
	n := 0
	for _, ev := range events {
		if ev.Kind == engine.KindCall && ev.Event == engine.EvPrepared && ev.CallID == callID {
			if m, ok := ev.Data["allocation"].(map[string]any); ok && strOf(m, "kind") == engine.PlaceCall {
				n++
			}
		}
	}
	for ; ; n++ {
		path := a.CallPath(callID)
		if n > 0 {
			path = filepath.Join(filepath.Dir(path), fmt.Sprintf("wt-%d", n))
		}
		if _, err := os.Lstat(path); err != nil {
			return path
		}
	}
}

// RetiredRef is the ref an attempt worktree's HEAD and working snapshot are kept on when it is retired.
func (a *Allocator) RetiredRef(node string, attempt int) string {
	return fmt.Sprintf("refs/crw/flow/%s/retired/%s/a%d", a.cfg.RunID, node, attempt)
}

// pathPart makes a node id one safe path element.
func pathPart(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '/' || r == '\\' || r == 0 {
			return '_'
		}
		return r
	}, s)
	if s == "" || s == "." || s == ".." {
		return "_" + s
	}
	return s
}

// allocation is one allocation of a node as the journal records it.
type allocation struct {
	kind             string
	attempt, lineage int
	path, start      string
	pending, digest  string
	seq              int
}

func intOf(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

func strOf(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// nodeKey is the node a call's worktrees belong to: its node option, or the call itself when it has none.
func nodeKey(req engine.PlaceRequest) string {
	if req.Node != "" {
		return req.Node
	}
	return "call-" + req.CallID
}

// journaled reads the allocations of a node from the journal: the worktree_restored rows (flow rows) and the prepared
// rows of calls that recorded an allocation.
func journaled(events []engine.Event, node string) (restores, prepared []allocation) {
	for _, ev := range events {
		switch {
		case ev.Kind == engine.KindFlow && ev.Event == EvWorktreeRestored && strOf(ev.Data, "node") == node:
			restores = append(restores, allocation{kind: engine.PlaceRestore, attempt: intOf(ev.Data["attempt"]), lineage: intOf(ev.Data["lineage"]),
				path: strOf(ev.Data, "path"), start: strOf(ev.Data, "C"), pending: strOf(ev.Data, "P"), digest: strOf(ev.Data, "snapshot_digest"), seq: ev.Seq})
		case ev.Kind == engine.KindCall && ev.Event == engine.EvPrepared && strOf(ev.Data, "node") == node:
			if m, ok := ev.Data["allocation"].(map[string]any); ok && (strOf(m, "kind") == engine.PlaceFresh || strOf(m, "kind") == engine.PlaceRestore) {
				prepared = append(prepared, allocation{kind: strOf(m, "kind"), attempt: intOf(m["attempt"]), lineage: intOf(m["lineage"]),
					path: strOf(m, "path"), start: strOf(m, "start_commit"), seq: ev.Seq})
			}
		}
	}
	return restores, prepared
}

// pendingRestore is the node's latest restored worktree that no call has adopted yet.
func pendingRestore(restores, prepared []allocation) *allocation {
	adopted := map[string]bool{}
	for _, p := range prepared {
		if p.kind == engine.PlaceRestore {
			adopted[p.path] = true
		}
	}
	var pending *allocation
	for i := range restores {
		if !adopted[restores[i].path] {
			pending = &restores[i]
		}
	}
	return pending
}

// currentAllocation is the node's newest worktree: the latest of its restores and its recorded fresh or restore allocations.
func currentAllocation(restores, prepared []allocation) *allocation {
	var cur *allocation
	for _, list := range [][]allocation{restores, prepared} {
		for i := range list {
			if list[i].path != "" && (cur == nil || list[i].seq > cur.seq) {
				cur = &list[i]
			}
		}
	}
	return cur
}

// lastCommit is the result commit of the last crw.commit recorded for the node.
func lastCommit(events []engine.Event, node string) string {
	var commit string
	for _, ev := range events {
		if ev.Kind == engine.KindCall && ev.Op == OpCommit && ev.Event == engine.EvCompleted && strOf(ev.Data, "node") == node {
			if c := strOf(ev.Data, "commit"); c != "" {
				commit = c
			}
		}
	}
	return commit
}

// Place decides where a call runs. It reads the journal and writes nothing (engine.Placer).
func (a *Allocator) Place(req engine.PlaceRequest) (engine.Placement, error) {
	node := nodeKey(req)
	var events []engine.Event
	if req.Journal != nil {
		events = req.Journal.Events()
	}
	restores, prepared := journaled(events, node)
	pending := pendingRestore(restores, prepared)

	// the QA capture call is an executor call that edits nothing it keeps: it gets a call-private scratch worktree at the head it
	// names, as the roles that do not write do, and never a worktree of a node
	if req.Family != "executor" || req.QA {
		if req.Continues != nil {
			if req.Fork {
				// inside a suffix the continuation runs on a fork in a private directory of its own
				start, err := a.callStart(req, node)
				if err != nil {
					return engine.Placement{}, err
				}
				return engine.Placement{Kind: engine.PlaceCall, Path: a.callPathFor(events, req.CallID), Start: start, Fork: true}, nil
			}
			if req.Role == "architect" && req.Head != "" && !req.QA {
				// a turn of the architect after its DESIGN names the head it judges (the REFLECTION at the integrated head, the
				// re-plan at the tip it read): it runs in a detached read-only worktree of its own at that head, which the turn
				// names as its cwd, so no turn keeps the directory an earlier turn named (CRW-1202)
				start, err := a.callStart(req, node)
				if err != nil {
					return engine.Placement{}, err
				}
				return engine.Placement{Kind: engine.PlaceCall, Path: a.callPathFor(events, req.CallID), Start: start}, nil
			}
			return engine.Placement{Kind: engine.PlaceContinued}, nil
		}
		start, err := a.callStart(req, node)
		if err != nil {
			return engine.Placement{}, err
		}
		return engine.Placement{Kind: engine.PlaceCall, Path: a.callPathFor(events, req.CallID), Start: start}, nil
	}
	if pending != nil {
		// (B) adoption: the restored worktree is the call's cwd as it is; nothing is made and no file is reset.
		return engine.Placement{Kind: engine.PlaceRestore, Attempt: pending.attempt, Lineage: pending.lineage, Path: pending.path,
			Start: pending.start, Pending: pending.pending, Digest: pending.digest, Fork: req.Continues != nil}, nil
	}
	if req.Continues != nil {
		if req.Fork {
			// a later continuation of the same suffix: the node's worktree is the one the suffix already works in (the restored
			// one a first call adopted, or a fresh one), and the thread of the previous process is forked into it
			if cur := currentAllocation(restores, prepared); cur != nil {
				return engine.Placement{Kind: engine.PlaceContinued, Path: cur.path, Start: cur.start, Fork: true}, nil
			}
		}
		return engine.Placement{Kind: engine.PlaceContinued}, nil
	}
	// (A) a new allocation.
	attempt, lineage := 1+len(restores)+countKind(prepared, engine.PlaceFresh), 0
	for _, r := range restores {
		lineage = max(lineage, r.lineage)
	}
	start := lastCommit(events, node)
	if start == "" {
		start = firstBase(restores, prepared)
	}
	if start == "" {
		var err error
		if start, err = a.base(node); err != nil {
			return engine.Placement{}, err
		}
	}
	return engine.Placement{Kind: engine.PlaceFresh, Attempt: attempt, Lineage: lineage, Path: a.WorktreePath(node, attempt, lineage), Start: start}, nil
}

// firstBase is the start commit of the node's first allocation: the base recorded for its first attempt.
func firstBase(lists ...[]allocation) string {
	var first *allocation
	for _, list := range lists {
		for i := range list {
			if list[i].start != "" && (first == nil || list[i].attempt < first.attempt) {
				first = &list[i]
			}
		}
	}
	if first == nil {
		return ""
	}
	return first.start
}

func countKind(list []allocation, kind string) int {
	n := 0
	for _, l := range list {
		if l.kind == kind {
			n++
		}
	}
	return n
}

// base is the commit a node with no history starts at.
func (a *Allocator) base(node string) (string, error) {
	var ref string
	if a.cfg.Base != nil {
		var err error
		if ref, err = a.cfg.Base(node); err != nil {
			return "", fmt.Errorf("no start commit is recorded for node %q: %w", node, err)
		}
	}
	if ref == "" {
		ref = "HEAD"
	}
	return a.commitOf(context.Background(), ref)
}

// callStart is the commit a call-private directory is made at: the head a reviewer reads, else the node's base.
func (a *Allocator) callStart(req engine.PlaceRequest, node string) (string, error) {
	if req.Head != "" {
		return a.commitOf(context.Background(), req.Head)
	}
	return a.base(node)
}

// commitOf resolves a ref in the repository to a full commit id.
func (a *Allocator) commitOf(ctx context.Context, ref string) (string, error) {
	commit, err := a.git(ctx, a.cfg.Repo, nil, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("%q is not a commit of the repository: %w", ref, err)
	}
	return commit, nil
}

// Establish makes a placement real (engine.Placer): a fresh allocation retires the node's previous worktree and creates
// the new one detached at C; a restore only verifies the live snapshot against the digest it was restored with.
func (a *Allocator) Establish(ctx context.Context, req engine.PlaceRequest, p engine.Placement) error {
	switch p.Kind {
	case engine.PlaceRestore:
		return a.Recheck(ctx, p)
	case engine.PlaceFresh:
		if err := a.retirePrevious(ctx, req, p); err != nil {
			return err
		}
		return a.add(ctx, p.Path, p.Start)
	case engine.PlaceCall:
		return a.add(ctx, p.Path, p.Start)
	}
	return nil
}

// add creates a detached worktree at a commit. The directory must not exist: a path belongs to one allocation.
func (a *Allocator) add(ctx context.Context, path, commit string) error {
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("the worktree path %s already exists: a path belongs to one allocation", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	_, err := a.git(ctx, a.cfg.Repo, nil, "worktree", "add", "--detach", "--quiet", path, commit)
	return err
}

// Recheck compares the live snapshot of a restored worktree with the digest it was verified with.
func (a *Allocator) Recheck(ctx context.Context, p engine.Placement) error {
	if p.Digest == "" {
		return nil
	}
	got, err := a.Snapshot(ctx, p.Path)
	if err != nil {
		return err
	}
	if got != p.Digest {
		return &engine.SnapshotMismatch{Want: p.Digest, Got: got}
	}
	return nil
}

// retirePrevious keeps the HEAD and working snapshot of the node's previous executor worktree on its retired ref (the
// uncommitted edits of a null_model attempt stay there and are not carried forward). The previous worktree is never
// written to or proven again.
func (a *Allocator) retirePrevious(ctx context.Context, req engine.PlaceRequest, p engine.Placement) error {
	var events []engine.Event
	if req.Journal != nil {
		events = req.Journal.Events()
	}
	restores, prepared := journaled(events, nodeKey(req))
	var prev *allocation
	for _, list := range [][]allocation{restores, prepared} {
		for i := range list {
			if list[i].path == p.Path || list[i].path == "" {
				continue
			}
			if prev == nil || list[i].attempt > prev.attempt {
				prev = &list[i]
			}
		}
	}
	if prev == nil {
		return nil
	}
	if _, err := os.Lstat(prev.path); err != nil {
		return nil
	}
	return a.Retire(ctx, nodeKey(req), prev.attempt, prev.path)
}

// Retire commits a worktree's HEAD and working snapshot to the retired ref of its attempt.
func (a *Allocator) Retire(ctx context.Context, node string, attempt int, path string) error {
	tree, err := a.Snapshot(ctx, path)
	if err != nil {
		return err
	}
	head, err := a.Head(ctx, path)
	if err != nil {
		return err
	}
	commit, err := a.git(ctx, a.cfg.Repo, identity, "commit-tree", tree, "-p", head, "-m", fmt.Sprintf("retired %s attempt %d of run %s", node, attempt, a.cfg.RunID))
	if err != nil {
		return err
	}
	_, err = a.git(ctx, a.cfg.Repo, nil, "update-ref", a.RetiredRef(node, attempt), commit)
	return err
}

// SupersededRef is the ref the HEAD and working snapshot of a worktree a resume superseded are kept on
// (refs/crw/flow/<run>/superseded/<lineage>/<path>).
func (a *Allocator) SupersededRef(lineage int, flowPath, node string) string {
	return engine.SupersededRef(a.cfg.RunID, lineage, flowPath, node)
}

// Preserve implements engine.Restorer: the old worktree's HEAD and working snapshot are kept on the superseded ref of the
// suffix, and the worktree is never written to again (the node's next allocation is the restored one).
func (a *Allocator) Preserve(ctx context.Context, req engine.PreserveRequest) (engine.Preserved, error) {
	ref := a.SupersededRef(req.Lineage, req.FlowPath, req.Node)
	msg := fmt.Sprintf("superseded %s of run %s (lineage %d, path %s)", req.Node, a.cfg.RunID, req.Lineage, req.FlowPath)
	git := ops.GitFunc(a.git)
	kept, err := ops.Preserve(ctx, git, a.cfg.Repo, req.Worktree, ref, msg)
	if err != nil {
		return engine.Preserved{}, err
	}
	out := engine.Preserved{Ref: ref, Commit: kept.Commit, Head: kept.Head, Snapshot: kept.Snapshot}
	for _, held := range req.Held {
		if held.Tree == kept.Snapshot {
			continue
		}
		heldRef := engine.SupersededHeldRef(a.cfg.RunID, req.Lineage, req.FlowPath, req.Node, held.CallID)
		if _, err := ops.PreserveTree(ctx, git, a.cfg.Repo, held.Tree, kept.Head, heldRef, "held state "+held.CallID+" of "+msg); err != nil {
			return engine.Preserved{}, err
		}
		if out.HeldRefs == nil {
			out.HeldRefs = map[string]string{}
		}
		out.HeldRefs[held.CallID] = heldRef
	}
	return out, nil
}

// Restore implements engine.Restorer: a new worktree for the node at the boundary of the replayed prefix, at
// <state>/flow/<run>/wt/<node>/a<attempt>-l<lineage>. The attempt is the next one the node has (its restores and the fresh
// allocations its calls recorded).
func (a *Allocator) Restore(ctx context.Context, req engine.RestoreRequest) (engine.Restored, error) {
	var events []engine.Event
	if req.Journal != nil {
		events = req.Journal.Events()
	}
	node := req.Node
	restores, prepared := journaled(events, node)
	attempt := 1 + len(restores) + countKind(prepared, engine.PlaceFresh)
	path := a.WorktreePath(node, attempt, req.Lineage)
	digest, err := ops.Restore(ctx, ops.GitFunc(a.git), a.cfg.Repo, path, req.Base, req.Pending)
	if err != nil {
		return engine.Restored{}, err
	}
	return engine.Restored{Attempt: attempt, Path: path, Digest: digest}, nil
}

// WorktreeExists reports whether the directory of a worktree is still there.
func (a *Allocator) WorktreeExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// RemoveDir removes the worktree of a call that has its terminal row: the architect turn at a fixed head reads its worktree
// only while it runs, and a resume that runs the same call again makes it again at the head the call's row records.
func (a *Allocator) RemoveDir(ctx context.Context, path string) error {
	if _, err := os.Lstat(path); err != nil {
		return nil
	}
	if _, err := a.git(ctx, a.cfg.Repo, nil, "worktree", "remove", "--force", path); err != nil {
		return err
	}
	return nil
}
