package migrate

// apply.go is the M3 write step of docs/port-cxc/state-migration.md: it publishes the plan classify() produced, in
// dependency order, through M1's Publisher, and never changes a payload byte. It creates the destination directories
// first, each at the private marker mode below, then publishes the files in rank order (artifacts and plans before the
// records that reference them), and finishes the directories' modes deepest first. Every source is rechecked against the
// preflight inventory immediately before its own write and every published file is verified afterwards, so a source that
// moved stops the run. A refusal or an error stops the run and leaves every published file whole, so a rerun skips what
// is already equal. No hook, installer, activation or startup path calls this.

import (
	"cmp"
	"crypto/sha256"
	"errors"
	"io/fs"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

// applyTempRaw is the raw mode, permission bits plus the sticky bit, that every directory this run creates is given
// before its children are published: private, and a marker no other writer in this repository uses, so a rerun can tell
// a directory this migration made from one it must leave alone. applyTempMode is the same mode as the permission bits a
// Go FileMode reports, which is what the comparisons below use.
const (
	applyTempRaw  = 0o1700
	applyTempMode = fs.FileMode(0o700)
)

// applyReasonChanged refuses a source that moved between classification and publication, which stops the run.
const applyReasonChanged Reason = "changed"

// ApplyItem is one plan item and what the run did with it. Result is empty for a skip row and for an item the run never
// reached; Note explains a directory that kept its mode, or a destination whose mode a publish left alone.
type ApplyItem struct {
	Item
	Result Result
	Note   string
}

// ApplyResult is one apply run: every plan item in plan order with its result, the number of writes this run completed,
// and whether every copy and transform source was opened, stat'ed and hashed in this run (false when the run stopped).
type ApplyResult struct {
	Items           []ApplyItem
	WritesCompleted int
	SourceVerified  bool
}

// apply publishes plan through M1's Publisher. A nil or empty plan writes nothing.
func apply(r *Roots, plan *Plan) (*ApplyResult, error) {
	pub, err := NewPublisher()
	if err != nil {
		return nil, err
	}
	return applyWith(r, plan, pub)
}

// applyWith is apply with a publisher the caller supplies, so a test can give it its own seams.
func applyWith(r *Roots, plan *Plan, pub *Publisher) (*ApplyResult, error) {
	a := &applyRun{roots: r, plan: plan, pub: pub, dirs: map[string]*Dir{}, srcs: map[string]*Dir{}, made: map[string]bool{}, result: &ApplyResult{}}
	if plan != nil {
		a.result.Items = make([]ApplyItem, len(plan.Items))
		for i, it := range plan.Items {
			a.result.Items[i].Item = it
		}
	}
	err := a.run()
	a.close()
	a.result.SourceVerified = err == nil
	return a.result, err
}

// applyRun is one run: the pinned roots, the publisher, the plan, the opened directories and the ones it created.
type applyRun struct {
	roots  *Roots
	plan   *Plan
	pub    *Publisher
	result *ApplyResult
	dirs   map[string]*Dir
	srcs   map[string]*Dir
	made   map[string]bool
}

func applyKey(scope Scope, rel string) string { return string(scope) + "\x00" + rel }

// applyRel normalises the plan's spelling of a scope root, ".", to the empty relative path.
func applyRel(dest string) string {
	if dest == "." {
		return ""
	}
	return dest
}

// applySplit cuts a root-relative path into its directory part and its last component.
func applySplit(rel string) (dir, base string) {
	if i := strings.LastIndexByte(rel, '/'); i >= 0 {
		return rel[:i], rel[i+1:]
	}
	return "", rel
}

// applyWrites reports whether the item is one the run writes; a skip row is only reported.
func applyWrites(it Item) bool { return it.Disposition == DispCopy || it.Disposition == DispTransform }

// applyDir reports whether a written item is a directory: the planner sets Size and Digest for every file and leaves both
// zero for a directory, and sha256 never returns the zero array.
func applyDir(it Item) bool {
	return applyWrites(it) && it.Size == 0 && it.Digest == ([sha256.Size]byte{})
}

// applyRank orders the files: 0 the plans and artifacts, 1 the rest (backups, and the freeze that names its plan files),
// 2 the bindings, ledgers and control records that reference them (state-migration.md Preflight 3).
func applyRank(it Item) int {
	if it.Scope == ScopeCodex { // the install manifest, the self-heal marker and the config backups
		return 0
	}
	p := it.Source
	switch {
	case p == "plan" || strings.HasPrefix(p, "plan/"), p == "evidence" || strings.HasPrefix(p, "evidence/"):
		return 0
	case p == "sessions" || strings.HasPrefix(p, "sessions/"), p == "ledger.jsonl", p == "interviews" || strings.HasPrefix(p, "interviews/"),
		p == "goalplans" || strings.HasPrefix(p, "goalplans/"), p == "sources" || strings.HasPrefix(p, "sources/"),
		p == "dispatches" || strings.HasPrefix(p, "dispatches/"), p == "bg/disabled", p == "bg/enabled-at", p == "bg/ledger.jsonl",
		strings.HasPrefix(p, "bg/") && strings.HasSuffix(p, ".json"), p == "attest.json", p == "subagents.json", p == "config.json":
		return 2
	}
	return 1
}

func (a *applyRun) run() error {
	if a.plan == nil {
		return nil
	}
	if err := a.ensureRoots(); err != nil {
		return err
	}
	if err := a.makeDirectories(); err != nil {
		return err
	}
	if err := a.writeFiles(); err != nil {
		return err
	}
	return a.finishModes()
}

func (a *applyRun) close() {
	for _, d := range a.dirs {
		_ = d.Close()
	}
	for _, d := range a.srcs {
		_ = d.Close()
	}
}

// root returns the pinned root of a scope on the destination or the source side.
func (a *applyRun) root(scope Scope, dest bool) *Dir {
	switch scope {
	case ScopeProject:
		if a.roots.Project != nil {
			if dest {
				return a.roots.Project.Dest
			}
			return a.roots.Project.Source
		}
	case ScopeUser:
		if a.roots.User != nil {
			if dest {
				return a.roots.User.Dest
			}
			return a.roots.User.Source
		}
	case ScopeCodex:
		return a.roots.Codex
	}
	return nil
}

// open returns the pinned directory of a root-relative path, caching each component, and refuses a link or a non-directory.
func (a *applyRun) open(scope Scope, rel string, dest bool) (*Dir, error) {
	if rel == "" || rel == "." {
		root := a.root(scope, dest)
		if root == nil {
			return nil, errors.New("no " + map[bool]string{true: "destination", false: "source"}[dest] + " root for scope " + string(scope))
		}
		return root, nil
	}
	cache := a.dirs
	if !dest {
		cache = a.srcs
	}
	if d, ok := cache[applyKey(scope, rel)]; ok {
		return d, nil
	}
	parentRel, base := applySplit(rel)
	parent, err := a.open(scope, parentRel, dest)
	if err != nil {
		return nil, err
	}
	child, err := parent.Child(base)
	if err != nil {
		return nil, err
	}
	cache[applyKey(scope, rel)] = child
	return child, nil
}

// rootItem returns the plan item of a scope's root, whose Mode the source root is rechecked against.
func (a *applyRun) rootItem(scope Scope) (Item, bool) {
	for _, it := range a.plan.Items {
		if it.Scope == scope && applyDir(it) && applyRel(it.Source) == "" {
			return it, true
		}
	}
	return Item{}, false
}

// ensureRoots rechecks each source root, then creates and pins the destination root of every scope the plan covers before
// any state is written. The root is made at the private marker mode first, because EnsureProjectRoot creates it 0777
// subject to umask and only a root this call made may be chmodded afterwards; EnsureProjectRoot also publishes CRW's
// canonical .gitignore no-replace and refuses a conflicting initialization race. The Codex scope maps its files in place.
func (a *applyRun) ensureRoots() error {
	if a.plan == nil {
		return nil
	}
	for _, scope := range []Scope{ScopeProject, ScopeUser} {
		if !slices.ContainsFunc(a.plan.Items, func(it Item) bool { return it.Scope == scope }) {
			continue
		}
		pair := a.roots.Project
		if scope == ScopeUser {
			pair = a.roots.User
		}
		if pair == nil {
			continue
		}
		if it, ok := a.rootItem(scope); ok && pair.Source != nil {
			cur, err := classifyDirMode(pair.Source)
			if err != nil {
				return err
			}
			if cur != it.Mode.Perm() {
				return refuse(applyReasonChanged, it.Source, "the source root mode changed since classification")
			}
		}
		made := pair.Dest == nil
		root, err := pair.EnsureDest(applyTempRaw)
		if err == nil && scope == ScopeProject {
			root, err = a.pub.EnsureProjectRoot(pair)
		}
		if err != nil {
			return err
		}
		if made {
			if err = applyChmodRaw(root, applyTempRaw); err != nil {
				return err
			}
		}
		a.made[applyKey(scope, "")] = made
	}
	return nil
}

// makeDirectories creates, in plan order (the planner emits a parent before its children), every destination directory
// the plan names and does not already hold, gives each the private marker mode, and rechecks each source mode against the
// preflight inventory. The scope-root items belong to ensureRoots and are skipped.
func (a *applyRun) makeDirectories() error {
	for i, it := range a.plan.Items {
		if !applyDir(it) || applyRel(it.Destination) == "" {
			continue
		}
		src, err := a.open(it.Scope, it.Source, false)
		if err != nil {
			return a.stop(i, it, err)
		}
		cur, err := classifyDirMode(src)
		if err != nil {
			return a.stop(i, it, err)
		}
		if cur != it.Mode.Perm() {
			return a.stop(i, it, refuse(applyReasonChanged, it.Source, "the source directory mode changed since classification"))
		}
		if _, err := a.ensureDestDir(it.Scope, applyRel(it.Destination)); err != nil {
			return a.stop(i, it, err)
		}
	}
	return nil
}

// ensureDestDir returns the pinned destination directory, creating it with the private marker mode when it is absent, and
// records that this run created it.
func (a *applyRun) ensureDestDir(scope Scope, rel string) (*Dir, error) {
	if d, ok := a.dirs[applyKey(scope, rel)]; ok {
		return d, nil
	}
	parentRel, base := applySplit(rel)
	parent, err := a.open(scope, parentRel, true)
	if err != nil {
		return nil, err
	}
	child, err := parent.Child(base)
	switch {
	case err == nil:
		a.dirs[applyKey(scope, rel)] = child
		return child, nil
	case !errors.Is(err, fs.ErrNotExist):
		return nil, err
	}
	if child, err = parent.EnsureChild(base, applyTempRaw); err != nil {
		return nil, err
	}
	if err := applyChmodRaw(child, applyTempRaw); err != nil {
		_ = child.Close()
		return nil, err
	}
	a.dirs[applyKey(scope, rel)] = child
	a.made[applyKey(scope, rel)] = true
	return child, nil
}

// writeFiles publishes every file item in rank order, keeping plan order inside a rank, and rechecks each source against
// the preflight inventory immediately before its own write, so a source that moved stops the run instead of publishing
// other bytes.
func (a *applyRun) writeFiles() error {
	order := make([]int, 0, len(a.plan.Items))
	for i, it := range a.plan.Items {
		if applyWrites(it) && !applyDir(it) {
			order = append(order, i)
		}
	}
	slices.SortStableFunc(order, func(x, y int) int {
		return cmp.Compare(applyRank(a.plan.Items[x]), applyRank(a.plan.Items[y]))
	})
	for _, i := range order {
		if err := a.publishFile(i, a.plan.Items[i]); err != nil {
			return err
		}
	}
	return nil
}

func (a *applyRun) publishFile(i int, it Item) error {
	src, err := a.open(it.Scope, classifyDirPart(it.Source), false)
	if err != nil {
		return a.stop(i, it, err)
	}
	_, base := applySplit(it.Source)
	f, info, err := src.OpenRegular(base)
	if err != nil {
		return a.stop(i, it, err)
	}
	defer f.Close()
	if info.Size() != it.Size || info.Mode().Perm() != it.Mode.Perm() {
		return a.stop(i, it, refuse(applyReasonChanged, it.Source, "the source changed since classification"))
	}
	got, err := sum(f)
	if err != nil {
		return a.stop(i, it, refuse(ReasonUnreadable, it.Source, err.Error()))
	}
	if got != it.Digest {
		return a.stop(i, it, refuse(applyReasonChanged, it.Source, "the source bytes changed since classification"))
	}
	parent, err := a.open(it.Scope, classifyDirPart(it.Destination), true)
	if err != nil {
		return a.stop(i, it, err)
	}
	_, leaf := applySplit(it.Destination)
	res, err := a.pub.Publish(parent, leaf, f, it.Size, it.Mode.Perm())
	a.result.Items[i].Result = res
	if err != nil {
		// A failure after the no-replace rename leaves a whole final file, which the report must count.
		if res == ResultFailed && a.checkDest(parent, leaf, it) == nil {
			a.result.WritesCompleted++
			a.result.Items[i].Note = "the final file is whole but its directory sync failed"
		}
		return err
	}
	switch res {
	case ResultCopied:
		a.result.WritesCompleted++
		return a.checkDest(parent, leaf, it)
	case ResultAlreadyEqual:
		// Nothing was written, and equality never changes a destination's mode or times, so a difference is reported.
		if got, err := a.destMode(parent, leaf); err == nil && got != it.Mode.Perm() {
			a.result.Items[i].Note = "destination kept its mode " + got.String()
		}
	}
	return nil
}

// checkDest verifies a file this run published against the preflight inventory: its size, its bytes and its mode. The
// source was hashed before the publish, so this is the check that the bytes that landed are the bytes the plan
// authorized, even if a writer changed the source while the temporary was being filled.
func (a *applyRun) checkDest(parent *Dir, leaf string, it Item) error {
	f, info, err := parent.OpenRegular(leaf)
	if err != nil {
		return err
	}
	defer f.Close()
	got, err := sum(f)
	if err != nil {
		return err
	}
	if info.Size() != it.Size || got != it.Digest {
		return &fs.PathError{Op: "verify", Path: parent.join(leaf), Err: errors.New("the published bytes differ from the preflight inventory")}
	}
	if info.Mode().Perm() != it.Mode.Perm() {
		return &fs.PathError{Op: "stat", Path: parent.join(leaf), Err: errors.New("the destination mode is " + info.Mode().Perm().String() + ", want " + it.Mode.Perm().String())}
	}
	return nil
}

func (a *applyRun) destMode(parent *Dir, leaf string) (fs.FileMode, error) {
	f, info, err := parent.OpenRegular(leaf)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return info.Mode().Perm(), nil
}

// finishModes applies the source mode to every directory the plan names, deepest first. A directory this run created, or
// one an interrupted run left at the private marker mode, is finished; any other directory keeps its mode and is reported.
// The marker is the raw mode, sticky bit included, which nothing else in this repository writes, so a directory this
// migration did not create is never chmodded.
func (a *applyRun) finishModes() error {
	order := make([]int, 0, len(a.plan.Items))
	for i, it := range a.plan.Items {
		if applyDir(it) {
			order = append(order, i)
		}
	}
	depth := func(i int) int {
		if rel := applyRel(a.plan.Items[i].Destination); rel != "" {
			return strings.Count(rel, "/") + 1
		}
		return 0
	}
	slices.SortStableFunc(order, func(x, y int) int { return cmp.Compare(depth(y), depth(x)) })
	for _, i := range order {
		it := a.plan.Items[i]
		rel := applyRel(it.Destination)
		dir, err := a.open(it.Scope, rel, true)
		if err != nil {
			return err
		}
		raw, err := applyDirRaw(dir)
		if err != nil {
			return err
		}
		switch cur := fs.FileMode(raw).Perm(); {
		case a.made[applyKey(it.Scope, rel)], raw == applyTempRaw && cur != it.Mode.Perm() && a.entriesExpected(it.Scope, rel, dir):
			err = applyChmod(dir, it.Mode.Perm())
		case cur == it.Mode.Perm():
		default:
			a.result.Items[i].Note = "existing directory kept its mode " + cur.String()
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// entriesExpected reports whether every entry of an existing destination directory is one the plan accounts for: a
// written or reported item names it, or it is the canonical .gitignore of a project root, which EnsureProjectRoot
// publishes and no plan item names when the source .codexclaw holds none.
func (a *applyRun) entriesExpected(scope Scope, rel string, dir *Dir) bool {
	names, err := dir.Names()
	if err != nil {
		return false
	}
	for _, name := range names {
		if scope == ScopeProject && rel == "" && name == ".gitignore" {
			continue
		}
		p := name
		if rel != "" {
			p = rel + "/" + name
		}
		named := slices.ContainsFunc(a.plan.Items, func(it Item) bool {
			if it.Scope != scope {
				return false
			}
			if applyWrites(it) {
				return applyRel(it.Destination) == p
			}
			return it.Disposition == DispSkip && (applyRel(it.Destination) == p || applyRel(it.Source) == p)
		})
		if !named {
			return false
		}
	}
	return true
}

// stop records the item the run stopped on and returns the error unchanged.
func (a *applyRun) stop(i int, it Item, err error) error {
	if a.result.Items[i].Result == "" {
		a.result.Items[i].Result = ResultFailed
		if refusal(err) != nil {
			a.result.Items[i].Result = ResultRefused
		}
	}
	return err
}

// applyChmod sets a pinned directory's permission bits through its descriptor, so a replaced ancestor cannot redirect it.
func applyChmod(d *Dir, mode fs.FileMode) error { return applyChmodRaw(d, uint32(mode.Perm())) }

// applyChmodRaw sets a pinned directory's raw mode, the sticky marker included.
func applyChmodRaw(d *Dir, raw uint32) error {
	if err := unix.Fchmod(d.fd(), raw); err != nil {
		return &fs.PathError{Op: "fchmod", Path: d.path, Err: err}
	}
	return nil
}

// applyDirRaw returns a pinned directory's raw mode: its permission bits and its sticky bit.
func applyDirRaw(d *Dir) (uint32, error) {
	var st unix.Stat_t
	if err := unix.Fstat(d.fd(), &st); err != nil {
		return 0, &fs.PathError{Op: "stat", Path: d.path, Err: err}
	}
	return st.Mode & 0o7777, nil
}
