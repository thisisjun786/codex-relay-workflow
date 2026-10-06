package migrate

// classify.go is the M2 dry planner: it walks the selected scope in a fixed order (project, then user, then codex), claims every
// entry through the inventory rows and returns the ordered items, or stops at the first whole-scope preflight refusal. It holds no
// write handle — the only handles it takes are the read-only pinned roots, opened read-only children and files read through
// M1's Dir.OpenRegular — so a refusal, a dry run and any interrupted classification have written nothing.
//
// The order is: each root depth-first with the entries of a directory in M1's sorted Names() order; the table order decides which
// row claims an entry when two could match.

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/role"
)

type classifier struct {
	roots *Roots
	plan  *Plan
	dests []classifyDest
	seen  map[string]bool
}

// classifyDest is a destination directory that received a mapped item, in first-seen order; classifyDestTemps reports the
// temporaries of older runs left there.
type classifyDest struct {
	scope Scope
	root  *Dir
	path  string
}

// classify returns the ordered dry plan of the selected roots. On a refusal the plan holds the items examined so far, and the
// error is a *RefusedError naming the whole-scope refusal.
func classify(r *Roots) (*Plan, error) {
	c := &classifier{roots: r, plan: &Plan{}, seen: map[string]bool{}}
	if p := r.Project; p != nil && p.Source != nil {
		if err := c.classifyProject(p); err != nil {
			return c.plan, err
		}
	}
	if u := r.User; u != nil {
		if u.Source != nil {
			if err := c.classifyUser(u); err != nil {
				return c.plan, err
			}
		} else if err := c.classifyUserFallback(u); err != nil { // an absent source copies nothing but still reports the literal fallback home
			return c.plan, err
		}
	}
	if r.Codex != nil {
		if err := c.classifyTree(ScopeCodex, r.Codex, "", inventoryCodexRows, nil); err != nil {
			return c.plan, err
		}
	}
	if err := c.classifyDestTemps(); err != nil {
		return c.plan, err
	}
	return c.plan, nil
}

func (c *classifier) add(it Item) { c.plan.Items = append(c.plan.Items, it) }

// destRoot is the pinned destination of a scope; nil when it does not exist yet, which is no conflict.
func (c *classifier) destRoot(scope Scope) *Dir {
	switch scope {
	case ScopeProject:
		if c.roots.Project != nil {
			return c.roots.Project.Dest
		}
	case ScopeUser:
		if c.roots.User != nil {
			return c.roots.User.Dest
		}
	case ScopeCodex:
		return c.roots.Codex
	}
	return nil
}

func (c *classifier) classifyProject(p *Pair) error {
	mode, err := classifyDirMode(p.Source)
	if err != nil {
		return err
	}
	c.add(Item{Scope: ScopeProject, Source: ".", Destination: ".", Disposition: DispTransform,
		Reason: "container to " + crwdir.DirName + "; created only after the preflight", Mode: mode})
	c.noteDest(ScopeProject, "") // the destination root itself, whatever the source root holds
	return c.classifyTree(ScopeProject, p.Source, "", inventoryProjectRows, nil)
}

func (c *classifier) classifyUser(u *Pair) error {
	mode, err := classifyDirMode(u.Source)
	if err != nil {
		return err
	}
	c.add(Item{Scope: ScopeUser, Source: ".", Destination: ".", Disposition: DispCopy, Reason: "user root container", Mode: mode})
	c.noteDest(ScopeUser, "") // the destination root itself, whatever the source root holds
	if err := c.classifyTree(ScopeUser, u.Source, "", inventoryUserRows, nil); err != nil {
		return err
	}
	return c.classifyUserFallback(u)
}

// classifyUserFallback enumerates the literal ~/.codexclaw the messenger service ignores the override for, and reports its
// serve.* entries when U differs. It is a report, never a copy: a literal home that cannot be read leaves the plan unchanged.
func (c *classifier) classifyUserFallback(u *Pair) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	literal := filepath.Join(home, ProjectSourceName)
	if literal == u.SourcePath {
		return nil
	}
	dir, _, err := pinDir(literal)
	if err != nil || dir == nil {
		return nil
	}
	defer dir.Close()
	names, err := dir.Names()
	if err != nil {
		return nil
	}
	for _, name := range names {
		switch name {
		case "serve.out.log", "serve.err.log", "serve.cmd":
		default:
			continue
		}
		typ, err := dir.typeOf(name)
		if err != nil || typ != unix.S_IFREG {
			continue
		}
		c.add(Item{Scope: ScopeUser, Source: filepath.Join(literal, name), Disposition: DispSkip, Reason: inventoryReasonFallback})
	}
	return nil
}

func (c *classifier) classifyTree(scope Scope, dir *Dir, dirPath string, rows []inventoryRow, inherit *inventoryRow) error {
	names, err := dir.Names()
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := c.classifyChild(scope, dir, dirPath, name, rows, inherit); err != nil {
			return err
		}
	}
	return nil
}

func (c *classifier) classifyChild(scope Scope, dir *Dir, dirPath, name string, rows []inventoryRow, inherit *inventoryRow) error {
	childPath := name
	if dirPath != "" {
		childPath = dirPath + "/" + name
	}
	typ, err := dir.typeOf(name)
	if err != nil {
		return err
	}
	path := dir.join(name)

	// Inside evidence/** an exact producer-temporary name is an interrupted writer's leftover: report and skip it (c1).
	if typ == unix.S_IFREG && classifyEvidenceTree(childPath) && classifyProducerTemp(name) {
		return c.skipItem(scope, dir, childPath, name, inventoryReasonIntermediate)
	}

	if inherit != nil { // inside a listed copy subtree every entry is copied
		switch typ {
		case unix.S_IFDIR:
			if err := c.emitDir(scope, dir, childPath, name, DispCopy, ""); err != nil {
				return err
			}
			child, err := dir.Child(name)
			if err != nil {
				return err
			}
			defer child.Close()
			return c.classifyTree(scope, child, childPath, rows, inherit)
		case unix.S_IFREG:
			return c.copyFile(scope, dir, childPath, name, DispCopy, "", "")
		case unix.S_IFLNK:
			return refuse(ReasonLink, path, "")
		default:
			return refuse(ReasonNotRegular, path, "")
		}
	}

	if scope == ScopeCodex && dirPath == "" { // only direct children of the Codex-home root are mapped by leaf name
		if dest, ok := CodexLeaf(name); ok {
			switch typ {
			case unix.S_IFREG:
				return c.copyFile(scope, dir, childPath, name, DispTransform, dest, "")
			case unix.S_IFLNK:
				return refuse(ReasonLink, path, "")
			default:
				return refuse(ReasonNotRegular, path, "a directory or special file where the Codex map lists a record")
			}
		}
	}

	row := inventoryMatchRow(rows, childPath)
	if row != nil && row.lock {
		return refuse(ReasonLocked, path, row.reason)
	}
	if typ == unix.S_IFREG && !classifyUserTree(childPath) {
		if reason := classifyIntermediate(scope, dirPath, name); reason != "" {
			return c.skipItem(scope, dir, childPath, name, reason)
		}
	}
	return c.classifyBranch(scope, dir, childPath, name, rows, row, typ)
}

// classifyBranch decides the entry by its type and the row that claimed it. A directory the table lists as a subtree is entered
// (copy) or reported as one item (skip); a container of listed entries is entered, and is itself copied only when a copy row lies
// below it; a link is refused wherever the run would follow or write it, and an entry it merely reports is left unfollowed. A file
// where the table lists a subtree, a directory where it lists a file, and a special file where it lists either are refused; an
// unclaimed entry is reported and never followed.
func (c *classifier) classifyBranch(scope Scope, dir *Dir, childPath, name string, rows []inventoryRow, row *inventoryRow, typ uint32) error {
	path := dir.join(name)
	switch typ {
	case unix.S_IFDIR:
		switch {
		case row != nil && strings.HasSuffix(row.pattern, "/**"):
			if row.disp == DispSkip {
				return c.skipItem(scope, dir, childPath, name, row.reason)
			}
			if err := c.emitDir(scope, dir, childPath, name, DispCopy, ""); err != nil {
				return err
			}
			child, err := dir.Child(name)
			if err != nil {
				return err
			}
			defer child.Close()
			return c.classifyTree(scope, child, childPath, rows, row)
		case row != nil:
			if row.disp == DispSkip {
				return c.skipItem(scope, dir, childPath, name, row.reason)
			}
			return refuse(ReasonNotRegular, path, "a directory where the table lists a file")
		case inventoryTraverses(rows, childPath):
			disp, reason := DispSkip, inventoryReasonContainer
			if inventoryCopies(rows, childPath) {
				disp, reason = DispCopy, ""
			}
			if err := c.emitDir(scope, dir, childPath, name, disp, reason); err != nil {
				return err
			}
			child, err := dir.Child(name)
			if err != nil {
				return err
			}
			defer child.Close()
			return c.classifyTree(scope, child, childPath, rows, nil)
		default:
			return c.skipItem(scope, dir, childPath, name, inventoryReasonUnknown)
		}
	case unix.S_IFLNK:
		switch {
		case row != nil && row.disp == DispSkip && !strings.HasSuffix(row.pattern, "/**"):
			return c.skipItem(scope, dir, childPath, name, row.reason) // reported, never followed
		case row != nil:
			return refuse(ReasonLink, path, "a listed path must be a real file or directory")
		case inventoryTraverses(rows, childPath):
			return refuse(ReasonLink, path, "a link on the way to a listed entry")
		default:
			return c.skipItem(scope, dir, childPath, name, inventoryReasonUnknown)
		}
	default:
		switch {
		case row == nil:
			return c.skipItem(scope, dir, childPath, name, inventoryReasonUnknown)
		case strings.HasSuffix(row.pattern, "/**"):
			return refuse(ReasonNotDirectory, path, "a listed subtree must be a directory")
		case row.disp == DispSkip:
			return c.skipItem(scope, dir, childPath, name, row.reason)
		case typ != unix.S_IFREG:
			return refuse(ReasonNotRegular, path, "")
		default:
			return c.copyFile(scope, dir, childPath, name, row.disp, "", row.judge)
		}
	}
}

// emitDir records a directory item. A copied directory's mode is what M3 applies after its children are published; a skip row
// writes nothing, takes no destination check and is not registered as a destination directory, so a container that holds only
// skip rows never refuses on what lies at its nominal destination.
func (c *classifier) emitDir(scope Scope, dir *Dir, childPath, name string, disp Disposition, reason string) error {
	if disp != DispSkip {
		if err := inventoryNameSupported(dir.join(name), name); err != nil {
			return err
		}
		if err := c.checkDestDir(c.destRoot(scope), childPath); err != nil {
			return err
		}
		c.noteDest(scope, childPath)
	}
	st, err := classifyLstat(dir, name)
	if err != nil {
		return err
	}
	c.add(Item{Scope: scope, Source: childPath, Destination: childPath, Disposition: disp, Reason: reason, Mode: fs.FileMode(st.Mode).Perm()})
	return nil
}

// copyFile reads one copy or transform item once, read-only, and judges the record state and the destination before admitting it.
func (c *classifier) copyFile(scope Scope, dir *Dir, childPath, name string, disp Disposition, destName, judge string) error {
	path := dir.join(name)
	if err := inventoryNameSupported(path, name); err != nil {
		return err
	}
	f, info, err := dir.OpenRegular(name)
	if err != nil {
		if refusal(err) != nil {
			return err
		}
		return refuse(ReasonUnreadable, path, err.Error())
	}
	defer f.Close()
	var size int64
	var digest [sha256.Size]byte
	if judge == "" {
		size = info.Size()
		if digest, err = sum(f); err != nil {
			return refuse(ReasonUnreadable, path, err.Error())
		}
	} else {
		data, rerr := io.ReadAll(f)
		if rerr != nil {
			return refuse(ReasonUnreadable, path, rerr.Error())
		}
		size = int64(len(data))
		digest = sha256.Sum256(data)
		if err := classifyJudge(judge, path, data); err != nil {
			return err
		}
	}
	destRel := childPath
	if destName != "" {
		if part := classifyDirPart(childPath); part != "" {
			destRel = part + "/" + destName
		} else {
			destRel = destName
		}
	}
	if err := c.checkDestFile(c.destRoot(scope), destRel, size, digest); err != nil {
		return err
	}
	c.noteDest(scope, classifyDirPart(childPath))
	c.add(Item{Scope: scope, Source: childPath, Destination: destRel, Disposition: disp, Size: size, Digest: digest, Mode: info.Mode().Perm()})
	return nil
}

// skipItem records a reported path: a skipped row, an unknown child or a producer intermediate. Nothing is read from it and
// nothing is written for it.
func (c *classifier) skipItem(scope Scope, dir *Dir, childPath, name, reason string) error {
	st, err := classifyLstat(dir, name)
	if err != nil {
		return err
	}
	c.add(Item{Scope: scope, Source: childPath, Disposition: DispSkip, Reason: reason, Size: st.Size, Mode: fs.FileMode(st.Mode).Perm()})
	return nil
}

func (c *classifier) noteDest(scope Scope, dirPath string) {
	root := c.destRoot(scope)
	if root == nil {
		return
	}
	key := string(scope) + ":" + dirPath
	if c.seen[key] {
		return
	}
	c.seen[key] = true
	c.dests = append(c.dests, classifyDest{scope: scope, root: root, path: dirPath})
}

// classifyDestTemps reports the .migrate-<26>-<n>.tmp names an older run left in a destination directory. They are named and put
// in the plan, never adopted, moved or removed (M1's Publisher does the same for a run of its own).
func (c *classifier) classifyDestTemps() error {
	for _, d := range c.dests {
		dir, opened, err := classifyOpenDest(d.root, d.path)
		if err != nil {
			classifyCloseAll(opened)
			return err
		}
		if dir == nil {
			classifyCloseAll(opened)
			continue
		}
		names, err := dir.Names()
		classifyCloseAll(opened)
		if err != nil {
			return err
		}
		for _, name := range names {
			if _, ok := tempRun(name); !ok {
				continue
			}
			rel := name
			if d.path != "" {
				rel = d.path + "/" + name
			}
			c.add(Item{Scope: d.scope, Destination: rel, Disposition: DispSkip, Reason: inventoryReasonOldTemp})
		}
	}
	return nil
}

// checkDestFile refuses a mapped file whose destination exists with other bytes; an equal destination is not a conflict, and a
// missing destination or a missing destination parent is no conflict at all. Links, non-regulars, hard links and set-ID bits at
// the destination are refused by M1's opener.
func (c *classifier) checkDestFile(root *Dir, rel string, size int64, digest [sha256.Size]byte) error {
	if root == nil {
		return nil
	}
	parent, leaf, opened, err := classifyDestParent(root, rel)
	if err != nil {
		return err
	}
	defer classifyCloseAll(opened)
	if parent == nil {
		return nil
	}
	f, info, err := parent.OpenRegular(leaf)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		if refusal(err) != nil {
			return err
		}
		return refuse(ReasonUnreadable, parent.join(leaf), err.Error())
	}
	defer f.Close()
	if info.Size() != size {
		return refuse(ReasonDiffers, parent.join(leaf), "the destination exists with other bytes")
	}
	got, err := sum(f)
	if err != nil {
		return refuse(ReasonUnreadable, parent.join(leaf), err.Error())
	}
	if got != digest {
		return refuse(ReasonDiffers, parent.join(leaf), "the destination exists with other bytes")
	}
	return nil
}

func (c *classifier) checkDestDir(root *Dir, rel string) error {
	if root == nil {
		return nil
	}
	parent, leaf, opened, err := classifyDestParent(root, rel)
	if err != nil {
		return err
	}
	defer classifyCloseAll(opened)
	if parent == nil {
		return nil
	}
	typ, err := parent.typeOf(leaf)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	switch typ {
	case unix.S_IFDIR:
		return nil
	case unix.S_IFLNK:
		return refuse(ReasonLink, parent.join(leaf), "")
	default:
		return refuse(ReasonNotDirectory, parent.join(leaf), "a file where the source holds a directory")
	}
}

// classifyDestParent walks a destination-relative path to the parent of its last component; a missing component is (nil, "", nil),
// which is no conflict, and a link or a non-directory on the way is refused by M1's Child.
func classifyDestParent(root *Dir, rel string) (parent *Dir, leaf string, opened []*Dir, err error) {
	parts := strings.Split(rel, "/")
	parent = root
	for _, seg := range parts[:len(parts)-1] {
		next, err := parent.Child(seg)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, "", opened, nil
		}
		if err != nil {
			return nil, "", opened, err
		}
		opened = append(opened, next)
		parent = next
	}
	return parent, parts[len(parts)-1], opened, nil
}

// classifyOpenDest walks the whole destination-relative directory path; nil when it does not exist.
func classifyOpenDest(root *Dir, rel string) (*Dir, []*Dir, error) {
	if rel == "" {
		return root, nil, nil
	}
	parent, leaf, opened, err := classifyDestParent(root, rel)
	if err != nil || parent == nil {
		return nil, opened, err
	}
	dir, err := parent.Child(leaf)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, opened, nil
	}
	if err != nil {
		return nil, opened, err
	}
	return dir, append(opened, dir), nil
}

func classifyCloseAll(dirs []*Dir) {
	for _, d := range dirs {
		_ = d.Close()
	}
}

func classifyDirPart(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return ""
}

func classifyLstat(d *Dir, name string) (unix.Stat_t, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(d.fd(), name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return st, &fs.PathError{Op: "lstat", Path: d.join(name), Err: err}
	}
	return st, nil
}

func classifyDirMode(d *Dir) (fs.FileMode, error) {
	var st unix.Stat_t
	if err := unix.Fstat(d.fd(), &st); err != nil {
		return 0, &fs.PathError{Op: "stat", Path: d.path, Err: err}
	}
	return fs.FileMode(st.Mode).Perm(), nil
}

// classifyIntermediate names the producer-intermediate shape of a file in a producer-owned directory, or "" when the name is
// ordinary. The older run's .migrate-<26>-<n>.tmp is recognized first, before the producer shapes, and is only ever reported.
// The producer shapes are the exact temporaries row 105 of docs/port-cxc/state-migration.md names, each matched only inside
// the directory of the producer that writes it (inventoryIntermediateRows), so a durable record whose own name holds a
// temp-like substring (for example bg/job.tmp-live.json) is judged by its own row instead of being skipped here.
func classifyIntermediate(scope Scope, dirPath, name string) string {
	if _, ok := tempRun(name); ok {
		return inventoryReasonOldTemp
	}
	if inventoryIntermediateShape(scope, dirPath, name) {
		return inventoryReasonIntermediate
	}
	return ""
}

// classifyUserTree is true inside the user plan and artifact trees, where a .tmp name is ordinary data and no producer
// intermediate rule applies.
func classifyUserTree(path string) bool {
	return path == "plan" || strings.HasPrefix(path, "plan/") || path == "evidence" || strings.HasPrefix(path, "evidence/")
}

// classifyEvidenceTree is true inside evidence/**, the one user tree that also holds producer-written files; plan/** stays
// wholly ordinary.
func classifyEvidenceTree(path string) bool {
	return path == "evidence" || strings.HasPrefix(path, "evidence/")
}

// classifyProducerTemp reports whether name is exactly the temporary shape a producer writes beside its final file: the CRW
// shape "." + final + "." + 26 base32 characters + ".tmp" (crwdir/atomic.go:92) and the CXC shapes final + "." + pid + "." +
// ms + ".tmp" and final + "." + pid + "." + uuid + ".tmp" (subagent-evidence.ts:221, state.ts:387,625). Every other ".tmp"
// name is ordinary data. The final-name part must be non-empty, so a user file such as .123.1760000000000.tmp (an empty final
// name) stays ordinary data; a dotfile final name such as .receipt.json is not empty and keeps its previous disposition.
func classifyProducerTemp(name string) bool {
	core, ok := strings.CutSuffix(name, tempSuffix)
	if !ok || core == "" {
		return false
	}
	if rest, ok := strings.CutPrefix(core, "."); ok { // CRW atomic publish: "." + final + "." + 26 base32 characters
		if base, run, cut := classifyCutLast(rest, "."); cut && base != "" && classifyRandText(run) {
			return true
		}
	}
	head, last, cut := classifyCutLast(core, ".")
	if !cut || head == "" { // CXC writers: final + "." + pid + "." + ms, final + "." + pid + "." + uuid, final + "." + uuid
		return false
	}
	if classifyUUID(last) {
		return true
	}
	if classifyMillis(last) {
		if base, pid, cut := classifyCutLast(head, "."); cut && base != "" && classifyPid(pid) {
			return true
		}
	}
	return false
}

// classifyCutLast splits s at its last sep.
func classifyCutLast(s, sep string) (before, after string, found bool) {
	if i := strings.LastIndex(s, sep); i >= 0 {
		return s[:i], s[i+len(sep):], true
	}
	return s, "", false
}

// classifyRandText is true for exactly rand.Text's shape: runLen characters of runAlphabet (the constants M1's publisher uses).
func classifyRandText(s string) bool { return len(s) == runLen && strings.Trim(s, runAlphabet) == "" }

// classifyPid and classifyMillis bound the pid and millisecond widths, so "v1.2.3.tmp" is not mistaken for a temporary.
func classifyPid(s string) bool    { return classifyDigits(s, 1, 7) }
func classifyMillis(s string) bool { return classifyDigits(s, 10, 16) }

func classifyDigits(s string, min, max int) bool {
	if len(s) < min || len(s) > max {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// classifyUUID is true for a canonical 8-4-4-4-12 hexadecimal UUID (randomUUID's shape).
func classifyUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				return false
			}
		}
	}
	return true
}

func classifyJudge(kind, path string, data []byte) error {
	switch kind {
	case "dispatch":
		return classifyJudgeDispatch(path, data)
	case "bg":
		return classifyJudgeBG(path, data)
	case "session":
		return classifyJudgeSession(path, data)
	case "object":
		return classifyJudgeObject(path, data)
	case "render-observations":
		return classifyJudgeRenderObservations(path, data)
	}
	return refuse(ReasonUnreadable, path, "unknown record kind "+kind)
}

// classifyJudgeDispatch judges a dispatch record the way internal/role/dispatch_ledger.go reads it: the store's own
// decoder decides the shape (DispatchRecordStatuses calls dispatchPinnedDecode unchanged), and this function only
// decides the disposition. A record the reader refuses, or whose status is not one the store writes, is unreadable.
// A record that is still active, or that has an attempt ready, claimed, running or in reconcile, refuses the scope:
// reconcile is unresolved (dispatch_ledger.go:779-780, 808, 841 checks whether a child exists and stops). Only a
// record stopped, complete or main-direct whose attempts are all failed or complete is a record the copy may carry.
func classifyJudgeDispatch(path string, data []byte) error {
	session, id := filepath.Base(filepath.Dir(path)), strings.TrimSuffix(filepath.Base(path), ".json")
	record, attempts, err := role.DispatchRecordStatuses(data, session, id)
	if err != nil {
		return refuse(ReasonUnreadable, path, err.Error())
	}
	switch record {
	case "stopped", "complete", "main-direct":
	case "active":
		return refuse(ReasonActive, path, "the dispatch record is still active")
	default:
		return refuse(ReasonUnreadable, path, "the dispatch record status is not one the store writes: "+record)
	}
	for _, status := range attempts {
		switch status {
		case "failed", "complete":
		case "ready", "claimed", "running", "reconcile":
			return refuse(ReasonActive, path, "an attempt is still "+status)
		default:
			return refuse(ReasonUnreadable, path, "an attempt status is not one the store writes: "+status)
		}
	}
	return nil
}

// classifyJudgeSession judges a session record with the state package's own verdict (state.RecordUnreadable, which is
// what ReadStateStrict says after reading the same bytes). A session file the state reader would call unreadable - one
// that is not a JSON object, or whose phase is not a known phase - refuses the scope. The session id is the file name
// without ".json", as StatePath names it.
func classifyJudgeSession(path string, data []byte) error {
	session := strings.TrimSuffix(filepath.Base(path), ".json")
	if state.RecordUnreadable(session, data) {
		return refuse(ReasonUnreadable, path, "the session record is not one the state reader can read")
	}
	return nil
}

// classifyJudgeObject judges a single-object JSON record by the shape every reader of these records needs before it
// reads a field: the whole bytes are one JSON object and nothing follows it (pyjson's strict reading, so an array, a
// scalar, an incomplete document or trailing data is refused). The reader then applies its own, typed checks; this
// function decides only that there is an object to read.
func classifyJudgeObject(path string, data []byte) error {
	value, err := pyjson.Loads(string(data), pyjson.LoadOptions{Map: true})
	if err != nil {
		return refuse(ReasonUnreadable, path, "the record is not one JSON object")
	}
	if _, ok := value.(map[string]any); !ok {
		return refuse(ReasonUnreadable, path, "the record is not one JSON object")
	}
	return nil
}

// classifyJudgeRenderObservations judges the render-observation ledger with the hook package's own rule
// (hook.RenderObsLedgerMalformed, the check NativeObservationLedgerMalformed applies). This is the one JSONL row this
// change judges: the row reader skips a damaged line, but the hook's own malformed check refuses the whole file for one,
// and the oracle's Stop hook consults that check before it trusts the ledger (render-observations.ts:161, hook.ts:1910),
// so a copy would carry a ledger the hook already calls unusable. Every other JSONL row stays unjudged.
func classifyJudgeRenderObservations(path string, data []byte) error {
	if hook.RenderObsLedgerMalformed(data) {
		return refuse(ReasonUnreadable, path, "the render-observation ledger has a line the hook reader calls malformed")
	}
	return nil
}

// classifyJudgeBG judges a job record by the store of internal/relay/job (registry.go): the record must be the shape the store
// writes and name the id of its file; only complete, failed and cancelled copy, and running or an unknown state refuses.
func classifyJudgeBG(path string, data []byte) error {
	var shape map[string]json.RawMessage
	if err := json.Unmarshal(data, &shape); err != nil {
		return refuse(ReasonUnreadable, path, "the job record is not readable JSON")
	}
	is := func(key string, first byte) bool { v := shape[key]; return len(v) > 0 && v[0] == first }
	if !is("id", '"') || !is("cwd", '"') || !is("command", '[') || !is("status", '"') {
		return refuse(ReasonUnreadable, path, "the job record is not the shape the store writes")
	}
	var rec struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	_ = json.Unmarshal(data, &rec)
	if rec.ID != strings.TrimSuffix(filepath.Base(path), ".json") {
		return refuse(ReasonUnreadable, path, "the job record names another id")
	}
	switch rec.Status {
	case "complete", "failed", "cancelled":
		return nil
	case "running":
		return refuse(ReasonActive, path, "the job is still running")
	default:
		return refuse(ReasonActive, path, "the job state is unknown: "+rec.Status)
	}
}
