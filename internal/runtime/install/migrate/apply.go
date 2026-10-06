package migrate

// apply.go is the M3 write step of docs/port-cxc/state-migration.md: it publishes the plan classify() produced, in
// dependency order, through M1's Publisher, and never changes a payload byte. It creates the destination directories
// first, gives each the private applyTempMode, publishes the files in rank order (artifacts, plans and backups before the
// bindings, ledgers and control records that reference them), and finishes the directories' modes deepest first. A
// refusal or an error stops the run and leaves every published file whole, so a rerun skips what is already equal.

import (
	"cmp"
	"crypto/sha256"
	"errors"
	"io/fs"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

// applyTempMode is the private mode every directory this run creates is given before its children are published, so a
// directory is never readable more widely than its final mode allows; finishModes restores the source mode. It is also
// the mark of a directory this migration made, because nothing else widens a directory to exactly 0o700.
const applyTempMode fs.FileMode = 0o700

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

// applyRank orders the files: 0 the artifacts, plans and backups a referencing record depends on, 1 the rest, 2 the
// bindings, ledgers and control records that reference them (state-migration.md Preflight 3).
func applyRank(it Item) int {
	if it.Scope == ScopeCodex { // the install manifest, the self-heal marker and the config backups
		return 0
	}
	p := it.Source
	switch {
	case p == "plan" || strings.HasPrefix(p, "plan/"), p == "evidence" || strings.HasPrefix(p, "evidence/"), p == "interview/freeze.json":
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

// ensureRoots creates and pins the destination root of every scope the plan covers, before any state is written, and
// gives a root this call created the private applyTempMode at once. EnsureProjectRoot also publishes CRW's canonical
// .gitignore no-replace when it is absent and refuses a conflicting initialization race.
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
		made := pair.Dest == nil
		var root *Dir
		var err error
		if scope == ScopeProject {
			root, err = a.pub.EnsureProjectRoot(pair)
		} else {
			root, err = pair.EnsureDest(uint32(applyTempMode))
		}
		if err != nil {
			return err
		}
		if made {
			if err = applyChmod(root, applyTempMode); err != nil {
				return err
			}
		}
		a.made[applyKey(scope, "")] = made
	}
	return nil
}

// makeDirectories creates, in plan order (the planner emits a parent before its children), every destination directory
// the plan names and does not already hold, giving each the private applyTempMode, and rechecks the source mode against
// the preflight inventory so a moved source stops the run. The scope-root items belong to ensureRoots and are skipped.
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

// ensureDestDir returns the pinned destination directory, creating it with applyTempMode when it is absent, and records
// that this run created it.
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
	if child, err = parent.EnsureChild(base, uint32(applyTempMode)); err != nil {
		return nil, err
	}
	if err := applyChmod(child, applyTempMode); err != nil {
		_ = child.Close()
		return nil, err
	}
	a.dirs[applyKey(scope, rel)] = child
	a.made[applyKey(scope, rel)] = true
	return child, nil
}

// writeFiles publishes every file item in rank order, keeping plan order inside a rank, and rechecks each source against
// the preflight inventory immediately before its own write, so a source that moved stops the run instead of publishing
// other bytes. The source is opened once and both hashed and published through that one descriptor.
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
		return err
	}
	switch res {
	case ResultCopied:
		a.result.WritesCompleted++
		if err := a.checkDestMode(parent, leaf, it.Mode.Perm()); err != nil {
			return err
		}
	case ResultAlreadyEqual:
		// Nothing was written, and equality never changes a destination's mode or times, so a difference is reported.
		if got, err := a.destMode(parent, leaf); err == nil && got != it.Mode.Perm() {
			a.result.Items[i].Note = "destination kept its mode " + got.String()
		}
	}
	return nil
}

// destMode returns the permission bits of a file in a pinned directory.
func (a *applyRun) destMode(parent *Dir, leaf string) (fs.FileMode, error) {
	f, info, err := parent.OpenRegular(leaf)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	return info.Mode().Perm(), nil
}

// checkDestMode rechecks the mode of a file this run published against the preflight inventory.
func (a *applyRun) checkDestMode(parent *Dir, leaf string, want fs.FileMode) error {
	got, err := a.destMode(parent, leaf)
	if err == nil && got != want {
		err = &fs.PathError{Op: "stat", Path: parent.join(leaf), Err: errors.New("the destination mode is " + got.String() + ", want " + want.String())}
	}
	return err
}

// finishModes applies the source mode to every directory the plan names, deepest first. A directory this run created is
// finished outright. One that was already there is finished only when its mode is exactly the private applyTempMode this
// run leaves behind (so a rerun repairs an interruption) and every entry is one the plan expects; others keep their mode.
func (a *applyRun) finishModes() error {
	order := make([]int, 0, len(a.plan.Items))
	for i, it := range a.plan.Items {
		if applyDir(it) {
			order = append(order, i)
		}
	}
	depth := func(i int) int {
		rel := applyRel(a.plan.Items[i].Destination)
		if rel == "" {
			return 0
		}
		return strings.Count(rel, "/") + 1
	}
	slices.SortStableFunc(order, func(x, y int) int { return cmp.Compare(depth(y), depth(x)) })
	for _, i := range order {
		it := a.plan.Items[i]
		rel := applyRel(it.Destination)
		dir, err := a.open(it.Scope, rel, true)
		if err != nil {
			return err
		}
		cur, err := classifyDirMode(dir)
		if err != nil {
			return err
		}
		switch {
		case a.made[applyKey(it.Scope, rel)], cur == applyTempMode && cur != it.Mode.Perm() && a.entriesExpected(it.Scope, rel, dir):
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
// written item names it, a reported item names it, or it is the canonical .gitignore of a project root, which
// EnsureProjectRoot publishes and which no plan item names when the source .codexclaw holds none.
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
func applyChmod(d *Dir, mode fs.FileMode) error {
	if err := unix.Fchmod(d.fd(), uint32(mode.Perm())); err != nil {
		return &fs.PathError{Op: "fchmod", Path: d.path, Err: err}
	}
	return nil
}
