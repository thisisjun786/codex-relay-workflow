package migrate

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// Options names the roots of a run. An empty root takes its default (W the working directory, U $CODEXCLAW_HOME or ~/.codexclaw,
// V $CRW_HOME or ~/.crw, C $CODEX_HOME or ~/.codex); an explicit root replaces the default for this run only.
type Options struct {
	Scope                            Scope
	Cwd, FromHome, ToHome, CodexHome string
}

// Pair is a source tree and its destination; a nil Source or Dest is a directory that does not exist yet.
type Pair struct {
	SourcePath, DestPath string
	Source, Dest         *Dir
	parent               *Dir   // holds Dest, where EnsureDest creates it
	created              fileID // the identity of Dest when this process's own creation put it there
}

// Roots are the pinned roots of a scope: Project (W/.codexclaw to W/.crw), User (U to V) and Codex (C, whose files are mapped in place).
type Roots struct {
	Project, User *Pair
	CodexPath     string
	Codex         *Dir
}

// Open resolves and pins the roots of o.Scope and creates nothing. It refuses trees that overlap or lie inside one another (by path,
// then by file identity, then by the identity of every directory below them, which catches a bind mount), a Codex home inside a
// selected tree, a link at any component of a root, a root that is not a directory, a ".." element and the filesystem root.
func Open(o Options) (_ *Roots, err error) {
	scope, err := ParseScope(string(o.Scope))
	if err != nil {
		return nil, err
	}
	r := &Roots{}
	defer func() {
		if err != nil {
			err = errors.Join(err, r.Close())
		}
	}()
	if scope.Has(ScopeProject) {
		w, werr := absRoot(o.Cwd) // an empty Cwd is the working directory: filepath.Abs("")
		if werr != nil {
			return nil, werr
		}
		r.Project = &Pair{SourcePath: filepath.Join(w, ProjectSourceName), DestPath: filepath.Join(w, crwdir.DirName)}
	}
	if scope.Has(ScopeUser) {
		r.User = &Pair{}
		if r.User.SourcePath, err = resolveRoot(o.FromHome, "CODEXCLAW_HOME", ".codexclaw"); err == nil {
			r.User.DestPath, err = resolveRoot(o.ToHome, "CRW_HOME", ".crw")
		}
	}
	if err == nil && scope.Has(ScopeCodex) {
		r.CodexPath, err = resolveRoot(o.CodexHome, "CODEX_HOME", ".codex")
	}
	if err != nil {
		return nil, err
	}
	var trees []string
	for _, p := range []*Pair{r.Project, r.User} {
		if p != nil {
			trees = append(trees, p.SourcePath, p.DestPath)
		}
	}
	for i, a := range trees {
		for _, b := range trees[i+1:] {
			if within(a, b) || within(b, a) {
				return nil, refuse(ReasonOverlap, b, "overlaps "+a)
			}
		}
		if r.CodexPath != "" && within(a, r.CodexPath) {
			return nil, refuse(ReasonOverlap, r.CodexPath, "lies inside "+a)
		}
	}
	var pins []pinned // the source and destination of each selected pair
	for _, p := range []*Pair{r.Project, r.User} {
		if p == nil {
			continue
		}
		src, dst := pinned{path: p.SourcePath}, pinned{path: p.DestPath}
		if p.Source, src.chain, err = pinDir(p.SourcePath); err != nil {
			return nil, err
		}
		if p.parent, p.Dest, dst.chain, err = pinRoot(p.DestPath); err != nil {
			return nil, err
		}
		src.dir, dst.dir = p.Source, p.Dest
		pins = append(pins, src, dst)
	}
	var codex *pinned
	if r.CodexPath != "" {
		c := pinned{path: r.CodexPath}
		if r.Codex, c.chain, err = pinDir(r.CodexPath); err != nil {
			return nil, err
		}
		c.dir, codex = r.Codex, &c
	}
	if err = overlapByIdentity(pins, codex); err != nil {
		return nil, err
	}
	if err = overlapByContents(pins, codex); err != nil {
		return nil, err
	}
	return r, nil
}

// Close releases every pinned handle.
func (r *Roots) Close() error {
	var err error
	for _, p := range []*Pair{r.Project, r.User} {
		if p != nil {
			err = errors.Join(err, p.Source.Close(), p.Dest.Close(), p.parent.Close())
		}
	}
	return errors.Join(err, r.Codex.Close())
}

// EnsureDest returns the pinned destination root, creating it with perm (under the umask) when absent, and reports whether this
// call's own creation made it. A creation that ended in EEXIST is not this run's, so made is false then and the caller must not
// give the directory a mode. What this process's own creation of this pair put there is remembered by the directory's identity,
// not by a flag, so a retry with the same pinned pair reports that root as this run's only while the name still holds that very
// directory: a root another actor moved aside and replaced keeps its own mode and is reported. The directory that holds it is
// synced either way, so a run interrupted between the mkdir and its sync finishes the entry on the next one.
func (p *Pair) EnsureDest(perm uint32) (*Dir, bool, error) {
	if p.Dest != nil {
		return p.Dest, false, p.parent.Sync()
	}
	name := filepath.Base(p.DestPath)
	// A retry with this pinned pair must not read the root this process already created as another
	// actor's, and must not adopt a root another actor put in its place. The identity recorded when
	// this pair's own creation renamed the root into place decides both: only the name still holding
	// that very directory is this run's, and it is this run's even when a later step failed.
	if cur, err := p.parent.Child(name); err == nil {
		if cur.id == p.created {
			p.Dest = cur
			return cur, true, p.parent.Sync()
		}
		_ = cur.Close()
	}
	d, made, err := p.parent.EnsureChild(name, perm)
	if made && d != nil {
		// The rename put this pair's own directory at the name, so this identity is this run's from here
		// on, whether or not a later step of the creation succeeded.
		p.created = d.id
	}
	if err != nil {
		if d != nil {
			_ = d.Close()
		}
		return nil, made, err
	}
	p.Dest = d
	return d, made, nil
}

// resolveRoot picks the explicit root, else the environment variable, else the default under the home directory.
func resolveRoot(explicit, env, def string) (string, error) {
	p := explicit
	if p == "" {
		p = os.Getenv(env)
	}
	if p == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, def)
	}
	abs, err := absRoot(p)
	if err == nil && abs == string(filepath.Separator) {
		err = fmt.Errorf("root %q: the filesystem root is not accepted", p)
	}
	return abs, err
}

// absRoot refuses a ".." element, so a link followed by ".." is never silently rewritten, and makes the path absolute.
func absRoot(p string) (string, error) {
	if slices.Contains(strings.Split(p, string(filepath.Separator)), "..") {
		return "", fmt.Errorf("root %q: a .. element is not accepted", p)
	}
	return filepath.Abs(p)
}

// within reports whether p is dir or lies inside it; both are absolute and clean.
func within(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Dir is a directory held open by descriptor. Every lookup goes through it with one path component and no link followed, so
// replacing an ancestor after the pin cannot redirect a later read or write.
type Dir struct {
	f    *os.File
	path string
	id   fileID
}

// fileID names a directory by device and inode, which two spellings of one path (a case-insensitive volume, a bind mount) share.
type fileID struct{ dev, ino uint64 }

// dirIdentity reads the identity of an open directory. It is a variable so a test can make one path report another
// directory's identity, which is what a bind mount does on Linux; no other code replaces it.
var dirIdentity = func(f *os.File) (fileID, error) {
	info, err := f.Stat()
	if err != nil {
		return fileID{}, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok { // not reachable on Linux or Darwin; a platform without inode identity cannot compare roots, so it refuses
		return fileID{}, errors.New("this platform reports no inode identity")
	}
	return fileID{uint64(st.Dev), uint64(st.Ino)}, nil
}

func newDir(fd int, path string) (*Dir, error) {
	f := os.NewFile(uintptr(fd), path)
	id, err := dirIdentity(f)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &Dir{f, path, id}, nil
}

// pinned is a root as found on disk: its handle (nil when its last component is absent) and the identity of every directory on
// the way down to it, itself included.
type pinned struct {
	path  string
	dir   *Dir
	chain []fileID
}

// holds reports whether q is p or one of its ancestors, by identity.
func (q pinned) holds(p pinned) bool { return q.dir != nil && slices.Contains(p.chain, q.dir.id) }

// alike reports whether two roots that do not exist yet could be one directory on a case-insensitive volume: the same existing
// parent and names that differ at most in case.
func (a pinned) alike(b pinned) bool {
	return a.dir == nil && b.dir == nil && a.chain[len(a.chain)-1] == b.chain[len(b.chain)-1] &&
		strings.EqualFold(filepath.Base(a.path), filepath.Base(b.path))
}

// overlapByIdentity refuses trees that are, or lie inside, one another, and a Codex home that is, or lies inside, a tree. The
// lexical check cannot see two spellings of one directory; file identity can. Two roots that do not exist yet have no identity to
// compare, so roots below one directory whose names differ only in case are refused too: on a case-insensitive volume they would
// be one directory.
func overlapByIdentity(trees []pinned, codex *pinned) error {
	for i, a := range trees {
		for _, b := range trees[i+1:] {
			if a.holds(b) || b.holds(a) {
				return refuse(ReasonOverlap, b.path, "the same directory as, or inside, "+a.path)
			}
			if a.alike(b) {
				return refuse(ReasonOverlap, b.path, "spelled like "+a.path+" below the same directory")
			}
		}
		if codex != nil && a.holds(*codex) {
			return refuse(ReasonOverlap, codex.path, "is, or lies inside, "+a.path)
		}
		if codex != nil && a.alike(*codex) {
			return refuse(ReasonOverlap, codex.path, "spelled like "+a.path+" below the same directory")
		}
	}
	return nil
}

// overlapWalkLimit bounds one tree's identity walk. A tree the walk enters more directories of than this, a walk that nests
// deeper than this, or a directory the walk cannot open or read refuses: the comparison against the other trees cannot be
// proven, and a run must not guess. The walk counts every directory it enters, not the distinct identities it records,
// because two spellings of one directory are both visited and a tree of aliases must not slip past the bound.
const overlapWalkLimit = 200000

// overlapByContents refuses a tree, or the Codex home, that reaches another selected tree through a second spelling of a
// directory. A bind mount gives one directory a second name whose identity (device and inode) is unchanged, so the aliased
// directory's identity lies inside the other tree while the two paths differ; neither the lexical check nor
// overlapByIdentity can see that, because the alias is not the other tree's root. A bind mount below a selected root is
// invisible to the root chains, so two trees that hold one directory below both roots are refused as well: the same
// directory is reachable under each of them, and a write under one lands in the other. The walk fails closed: a tree it
// cannot read refuses rather than passing an unproven comparison.
func overlapByContents(trees []pinned, codex *pinned) error {
	sets := make([]map[fileID]struct{}, len(trees))
	for i, t := range trees {
		if t.dir == nil {
			continue
		}
		set, err := identitySet(t.path, t.dir)
		if err != nil {
			return err
		}
		sets[i] = set
	}
	for i, a := range trees {
		for j, set := range sets {
			if i == j || set == nil {
				continue
			}
			if chainHits(a.chain, set) || setsShare(sets[i], set) {
				return refuse(ReasonOverlap, a.path, "reaches "+trees[j].path+" through another spelling (a bind mount)")
			}
		}
	}
	if codex == nil {
		return nil
	}
	for j, set := range sets {
		if set == nil {
			continue
		}
		if chainHits(codex.chain, set) {
			return refuse(ReasonOverlap, codex.path, "reaches "+trees[j].path+" through another spelling (a bind mount)")
		}
	}
	return nil
}

// chainHits reports whether any identity of chain lies in set.
func chainHits(chain []fileID, set map[fileID]struct{}) bool {
	for _, id := range chain {
		if _, ok := set[id]; ok {
			return true
		}
	}
	return false
}

// setsShare reports whether two identity sets hold one directory in common.
func setsShare(a, b map[fileID]struct{}) bool {
	if len(a) > len(b) {
		a, b = b, a
	}
	for id := range a {
		if _, ok := b[id]; ok {
			return true
		}
	}
	return false
}

// identitySet collects the identity of root and of every directory reachable below it, walking from the pinned handle
// with Child only, so a link entry is skipped and never opened. Every spelling is walked: two names of one directory can
// show different submounts, so a directory already seen is visited again under its second name, and only the walk's depth
// or the tree's size stops it. Recursion bounds the open descriptors by the tree's depth, so a directory holding many
// children cannot exhaust them.
func identitySet(path string, root *Dir) (map[fileID]struct{}, error) {
	set := map[fileID]struct{}{root.id: {}}
	visited := 1
	if err := walkIdentity(path, root, set, 0, &visited); err != nil {
		return nil, err
	}
	return set, nil
}

// walkIdentity adds every directory below cur to set, at most overlapWalkLimit levels down and counting each directory it
// enters in visited. cur stays open for the whole call and each child is closed before the next sibling is opened, so a
// directory holding many children costs two descriptors rather than one per child.
func walkIdentity(path string, cur *Dir, set map[fileID]struct{}, depth int, visited *int) error {
	if depth >= overlapWalkLimit {
		return refuse(ReasonOverlap, path, "nests more than "+strconv.Itoa(overlapWalkLimit)+" directories deep")
	}
	names, err := cur.Names()
	if err != nil {
		return refuse(ReasonOverlap, path, "cannot be read to compare identities: "+err.Error())
	}
	for _, name := range names {
		typ, err := cur.typeOf(name)
		if err != nil {
			return refuse(ReasonOverlap, path, "cannot be inspected to compare identities: "+err.Error())
		}
		if typ != unix.S_IFDIR {
			continue
		}
		child, err := cur.Child(name)
		if err != nil {
			return refuse(ReasonOverlap, path, "cannot be opened to compare identities: "+err.Error())
		}
		*visited++
		if *visited > overlapWalkLimit {
			_ = child.Close()
			return refuse(ReasonOverlap, path, "holds more than "+strconv.Itoa(overlapWalkLimit)+" directories (visited "+strconv.Itoa(*visited)+")")
		}
		set[child.id] = struct{}{}
		err = walkIdentity(path, child, set, depth+1, visited)
		_ = child.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// Close closes the directory; a nil Dir is an absent root and closing it is a no-op.
func (d *Dir) Close() error {
	if d == nil {
		return nil
	}
	return d.f.Close()
}

func (d *Dir) Sync() error             { return d.f.Sync() }
func (d *Dir) fd() int                 { return int(d.f.Fd()) }
func (d *Dir) join(name string) string { return filepath.Join(d.path, name) }

func checkName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") {
		return fmt.Errorf("invalid path component %q", name)
	}
	return nil
}

// pinRoot opens the directory at an absolute path with no link followed at any component, and returns the identity of every
// directory on the way. When only the last component is absent it returns the pinned parent and a nil Dir; a missing ancestor is an
// error.
func pinRoot(path string) (parent, dir *Dir, chain []fileID, err error) {
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, nil, err
	}
	cur, err := newDir(fd, "/")
	if err != nil {
		return nil, nil, nil, err
	}
	chain = []fileID{cur.id}
	names := strings.Split(strings.Trim(path, "/"), "/")
	for _, name := range names[:len(names)-1] {
		next, err := cur.Child(name)
		_ = cur.Close()
		if err != nil {
			return nil, nil, nil, err
		}
		cur = next
		chain = append(chain, cur.id)
	}
	dir, err = cur.Child(names[len(names)-1])
	switch {
	case err == nil:
		return cur, dir, append(chain, dir.id), nil
	case errors.Is(err, fs.ErrNotExist):
		return cur, nil, chain, nil
	}
	_ = cur.Close()
	return nil, nil, nil, err
}

// pinDir is pinRoot for a root that is only read: its parent is released and a missing directory is a nil Dir.
func pinDir(path string) (*Dir, []fileID, error) {
	parent, dir, chain, err := pinRoot(path)
	if parent != nil {
		_ = parent.Close()
	}
	return dir, chain, err
}

// typeOf is the S_IFMT type of name, a link not followed.
func (d *Dir) typeOf(name string) (uint32, error) {
	var st unix.Stat_t
	if err := unix.Fstatat(d.fd(), name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return 0, &fs.PathError{Op: "lstat", Path: d.join(name), Err: err}
	}
	return uint32(st.Mode) & unix.S_IFMT, nil
}

// Child opens the subdirectory name: a link is a link refusal, anything else that is not a directory a not-directory refusal.
func (d *Dir) Child(name string) (*Dir, error) {
	if err := checkName(name); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(d.fd(), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	switch {
	case err == nil:
		return newDir(fd, d.join(name))
	case errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR):
		// The errno of a link under O_DIRECTORY|O_NOFOLLOW differs between Linux and Darwin; the entry itself tells them apart.
		if typ, terr := d.typeOf(name); terr == nil && typ == unix.S_IFLNK {
			return nil, refuse(ReasonLink, d.join(name), "")
		}
		return nil, refuse(ReasonNotDirectory, d.join(name), "")
	}
	return nil, &fs.PathError{Op: "open", Path: d.join(name), Err: err}
}

// migrateOwnedDirMkdirat creates a directory for EnsureChild. It is a variable so a case can model a host whose mkdir does not
// keep the mode it is given - Darwin drops the sticky bit from a directory's creation mode - which is the case the marker chmod
// after a creation exists for; no other code replaces it.
var migrateOwnedDirMkdirat = unix.Mkdirat

// migrateOwnedDirIdentityAt runs at a named step of the creation EnsureChild performs and fails that
// step by returning an error: "open" before the temporary name is pinned, "rename" before the
// no-replace rename, and "sync" before the directory that holds the new entry is synced. It is nil in a
// run, and only a test sets it.
var migrateOwnedDirIdentityAt func(step string) error

// migrateOwnedDirIdentityLstat reads the identity of a name inside a directory without following a link
// at it. It is a variable so a case can model a host whose read of a name it just created fails; no other
// code replaces it.
var migrateOwnedDirIdentityLstat = func(dirfd int, name string, st *unix.Stat_t) error {
	return unix.Fstatat(dirfd, name, st, unix.AT_SYMLINK_NOFOLLOW)
}

// migrateOwnedDirIdentityHandleFlag opens a directory as a handle with no permission on the directory
// itself. It is O_PATH on Linux; the constant is written here because the port builds for platforms that
// do not declare it, and migrateOwnedDirIdentityPin never passes it anywhere but Linux.
const migrateOwnedDirIdentityHandleFlag = 0x200000

// migrateOwnedDirIdentityEmptyPath names the empty path fchmodat2 takes with AT_EMPTY_PATH, so the mode
// is changed through the descriptor. Written here for the same reason as the handle flag.
const migrateOwnedDirIdentityEmptyPath = 0x1000

// errMigrateOwnedDirIdentityNoHandle says this platform has no open that returns a directory handle
// without permission on the directory, so the creation uses the checked by-name chmod instead.
var errMigrateOwnedDirIdentityNoHandle = errors.New("this platform cannot pin a directory without read permission")

// migrateOwnedDirIdentityPin opens the temporary name as a directory, following no link at it, without
// needing any permission on the directory itself, and returns the pinned descriptor. A regular file, a
// link, or a directory this run did not create is refused by the open or by the check that follows it,
// before any mode is given, so a mode is never changed on an entry this run did not create. A platform
// with no such open answers errMigrateOwnedDirIdentityNoHandle, and the creation then checks the name
// immediately before a by-name no-follow chmod and records the two-syscall window that leaves.
// It is a variable so a case can model a host whose pin fails; no other code replaces it.
var migrateOwnedDirIdentityPin = func(dirfd int, name string) (int, error) {
	if runtime.GOOS != "linux" {
		return -1, errMigrateOwnedDirIdentityNoHandle
	}
	return unix.Openat(dirfd, name, migrateOwnedDirIdentityHandleFlag|unix.O_NOFOLLOW|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
}

// migrateOwnedDirIdentityFchmodat2 changes a directory's mode through its descriptor with fchmodat2 and
// AT_EMPTY_PATH. It is a variable so a case can model a kernel that does not have fchmodat2 (Linux before
// 6.5 answers ENOSYS, which the runtime call reports as EOPNOTSUPP); no other code replaces it.
var migrateOwnedDirIdentityFchmodat2 = func(fd int, perm uint32) error {
	return unix.Fchmodat(fd, "", perm, migrateOwnedDirIdentityEmptyPath)
}

// migrateOwnedDirIdentityFchmod gives the pinned directory exactly perm through its descriptor. The
// descriptor is used rather than the name so a name swapped in the meantime cannot redirect the mode,
// and a descriptor needs no read permission, so a umask that removed the owner's read or write bit
// cannot stop this. Linux takes fchmodat2 where the kernel has it (6.5 and later) and the /proc/self/fd
// path otherwise, which is the same call a libc fchmod makes on a descriptor. It is a variable so a case
// can model each answer; no other code replaces it.
var migrateOwnedDirIdentityFchmod = func(fd int, perm uint32) error {
	err := migrateOwnedDirIdentityFchmodat2(fd, perm)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.ENOSYS), errors.Is(err, unix.EOPNOTSUPP), errors.Is(err, unix.ENOTSUP), errors.Is(err, unix.EINVAL):
	}
	return unix.Chmod("/proc/self/fd/"+strconv.Itoa(fd), perm)
}

// EnsureChild creates the subdirectory name unless it exists, pins it, and syncs this directory so the
// new entry survives a crash, and reports whether this call's own creation made the directory.
//
// The creation runs under a temporary name of this run in this directory, and the name becomes visible
// only once it carries perm. mkdirat the temporary; open it as a handle with no link followed and no
// permission needed on it; check with fstat that the pinned handle is the directory this run created, by
// the identity read at the name right after the mkdirat; give exactly perm through that handle; sync it
// so the mode survives with the name; then the package's no-replace rename to name, and pin the name
// again and check it is still the same directory. A name that is not a directory, is another inode or
// belongs to another user is refused with no mode changed. A rename that ended in EEXIST is another
// actor's directory: made is false, this run's temporary is removed, and name is opened as it is. Any
// other failure removes this run's temporary too, addressed by the identity read when it was created, so
// an entry another actor swapped onto that name is left alone rather than deleted. The sync also runs
// when the entry already existed.
func (d *Dir) EnsureChild(name string, perm uint32) (child *Dir, made bool, err error) {
	if err := checkName(name); err != nil {
		return nil, false, err
	}
	tmp, tmpID, err := d.migrateOwnedDirIdentityTemp(perm)
	if err != nil {
		return nil, false, err
	}
	// cleanup removes this run's temporary unless the rename put it at name. The removal is addressed by
	// the identity of the directory this call created, so it never deletes an entry another actor put at
	// the temporary name.
	cleanup := true
	defer func() {
		if !cleanup {
			return
		}
		if rm := d.migrateOwnedDirIdentityDrop(tmp, tmpID); rm != nil {
			if child != nil {
				_ = child.Close()
				child, made = nil, false
			}
			err = errors.Join(err, rm)
		}
	}()
	if err := d.migrateOwnedDirIdentityClaim(tmp, tmpID, perm); err != nil {
		return nil, false, err
	}
	if err := migrateOwnedDirIdentityStep("rename"); err != nil {
		return nil, false, err
	}
	switch err = noReplaceRename(d.fd(), tmp, name); {
	case err == nil:
		cleanup = false
		made = true
	case errors.Is(err, unix.EEXIST):
		// Another actor made the name while this run's temporary was being prepared. Its directory is
		// not this run's, so this run neither gives it a mode nor reports that it created it.
		made = false
	case errors.Is(err, errors.ErrUnsupported) || errors.Is(err, unix.EINVAL):
		return nil, false, refuse(ReasonUnsupported, d.join(name), "no-replace rename: "+err.Error())
	default:
		return nil, false, &fs.PathError{Op: "rename", Path: d.join(name), Err: err}
	}
	if made {
		// The rename put this run's own directory at name. Pinning the name again and checking it is
		// still that directory closes the interval between the rename and this point; a swap there is
		// refused rather than published into.
		if child, err = d.migrateOwnedDirIdentityReopen(name, tmpID, perm); err != nil {
			return nil, made, err
		}
	} else {
		if child, err = d.Child(name); err != nil {
			return nil, false, err
		}
	}
	if err = migrateOwnedDirIdentityStep("sync"); err == nil {
		err = d.Sync()
	}
	if err != nil {
		// The directory is this run's - the rename put it at the name - so it is handed back with
		// made true and the caller records its identity before closing it. Only the sync failed.
		return child, made, err
	}
	return child, made, nil
}

// migrateOwnedDirIdentityStep runs the creation-step seam for step, when a test has set one.
func migrateOwnedDirIdentityStep(step string) error {
	if migrateOwnedDirIdentityAt == nil {
		return nil
	}
	return migrateOwnedDirIdentityAt(step)
}

// migrateOwnedDirIdentityTemp makes the temporary directory this run creates name with perm and returns
// the name it took with the identity of the directory created there. The name is a temporary of this
// package's own rule, so OlderTemps reports a leftover one, and it is unguessable to another actor. A
// name already taken is not the destination's business - a freak collision with a leftover of another
// run, or a squatter - so one fresh name is tried before the run is stopped.
//
// The identity is read at the name right after the mkdirat, before anything else runs. A read that fails
// leaves that directory in place and refuses with its path: this run cannot show the entry is the
// directory it made, so removing the name could delete another actor's entry, and the leftover is
// reported instead.
func (d *Dir) migrateOwnedDirIdentityTemp(perm uint32) (string, fileID, error) {
	var last error
	for range 2 {
		tmp := tempName(rand.Text(), 1)
		if err := migrateOwnedDirMkdirat(d.fd(), tmp, perm); err != nil {
			if !errors.Is(err, unix.EEXIST) {
				return "", fileID{}, &fs.PathError{Op: "mkdir", Path: d.join(tmp), Err: err}
			}
			last = &fs.PathError{Op: "mkdir", Path: d.join(tmp), Err: err}
			continue
		}
		var st unix.Stat_t
		if err := migrateOwnedDirIdentityLstat(d.fd(), tmp, &st); err != nil {
			return "", fileID{}, refuse(ReasonUnreadable, d.join(tmp), "the identity of the directory this run created could not be read, so it is left in place: "+err.Error())
		}
		return tmp, fileID{uint64(st.Dev), uint64(st.Ino)}, nil
	}
	return "", fileID{}, last
}

// migrateOwnedDirIdentityClaim pins the temporary directory this run created, checks the pinned handle is
// that directory, and gives it exactly perm through the handle. The handle is opened before any mode is
// given and needs no permission on the directory, so a regular file, a link or another owner's directory
// at the name is refused with no mode changed. A platform whose open cannot give such a handle checks the
// name with fstatat immediately before a by-name no-follow chmod instead, which leaves the two-syscall
// window the defect record carries as a residual.
func (d *Dir) migrateOwnedDirIdentityClaim(tmp string, want fileID, perm uint32) error {
	if err := migrateOwnedDirIdentityStep("open"); err != nil {
		return err
	}
	fd, err := migrateOwnedDirIdentityPin(d.fd(), tmp)
	switch {
	case err == nil:
	case errors.Is(err, errMigrateOwnedDirIdentityNoHandle), errors.Is(err, unix.EINVAL), errors.Is(err, unix.ENOTSUP), errors.Is(err, unix.EOPNOTSUPP):
		return d.migrateOwnedDirIdentityClaimByName(tmp, want, perm)
	default:
		return err
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return &fs.PathError{Op: "stat", Path: d.join(tmp), Err: err}
	}
	if err := migrateOwnedDirIdentityOwned(d.join(tmp), &st, want); err != nil {
		return err
	}
	if err := migrateOwnedDirIdentityFchmod(fd, perm); err != nil {
		return &fs.PathError{Op: "chmod", Path: d.join(tmp), Err: err}
	}
	// The mode is made durable on the directory itself before its name is published, so a crash
	// cannot leave the final name durable with the corrected mode lost - the state this issue exists
	// to make recoverable. The handle above is opened without permission and cannot be synced, so the
	// directory is opened for reading now that it carries perm and that handle is synced.
	sync, err := unix.Openat(d.fd(), tmp, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return &fs.PathError{Op: "open", Path: d.join(tmp), Err: err}
	}
	defer unix.Close(sync)
	if err := unix.Fsync(sync); err != nil {
		return &fs.PathError{Op: "fsync", Path: d.join(tmp), Err: err}
	}
	return nil
}

// migrateOwnedDirIdentityClaimByName is the mode step where no descriptor-bound handle is available: the
// name is read with no link followed and checked to be the directory this run created, and only then is
// the by-name no-follow chmod made. A name that is not a directory, is another inode or belongs to
// another user is refused with no mode changed.
func (d *Dir) migrateOwnedDirIdentityClaimByName(tmp string, want fileID, perm uint32) error {
	var st unix.Stat_t
	if err := migrateOwnedDirIdentityLstat(d.fd(), tmp, &st); err != nil {
		return &fs.PathError{Op: "lstat", Path: d.join(tmp), Err: err}
	}
	if err := migrateOwnedDirIdentityOwned(d.join(tmp), &st, want); err != nil {
		return err
	}
	err := unix.Fchmodat(d.fd(), tmp, perm, unix.AT_SYMLINK_NOFOLLOW)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.EOPNOTSUPP), errors.Is(err, unix.ENOTSUP):
		return refuse(ReasonUnsupported, d.join(tmp), "this kernel cannot chmod a name without following a link")
	}
	return &fs.PathError{Op: "chmod", Path: d.join(tmp), Err: err}
}

// migrateOwnedDirIdentityOwned refuses a descriptor or a name that is not the directory this run created:
// not a directory, another inode, or another owner.
func migrateOwnedDirIdentityOwned(path string, st *unix.Stat_t, want fileID) error {
	switch {
	case uint32(st.Mode)&unix.S_IFMT != unix.S_IFDIR:
		return refuse(ReasonNotDirectory, path, "the temporary name is not a directory")
	case (fileID{uint64(st.Dev), uint64(st.Ino)}) != want:
		return refuse(applyReasonChanged, path, "the temporary name is not the directory this run created")
	case st.Uid != uint32(os.Geteuid()):
		return refuse(applyReasonChanged, path, "the temporary directory belongs to another user")
	}
	return nil
}

// migrateOwnedDirIdentityReopen pins the directory this run renamed to name and checks it is still that
// directory at exactly perm, so a swap between the rename and this point is refused rather than published
// into. The mode was set through the handle before the rename, so the directory is readable here whatever
// the umask was.
func (d *Dir) migrateOwnedDirIdentityReopen(name string, want fileID, perm uint32) (*Dir, error) {
	fd, err := unix.Openat(d.fd(), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: d.join(name), Err: err}
	}
	child, err := newDir(fd, d.join(name))
	if err != nil {
		return nil, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(child.fd(), &st); err != nil {
		_ = child.Close()
		return nil, &fs.PathError{Op: "stat", Path: child.path, Err: err}
	}
	if err := migrateOwnedDirIdentityOwned(child.path, &st, want); err != nil {
		_ = child.Close()
		return nil, err
	}
	if uint32(st.Mode)&0o7777 != perm {
		_ = child.Close()
		return nil, refuse(applyReasonChanged, child.path, fmt.Sprintf("the directory is %#o, want %#o", uint32(st.Mode)&0o7777, perm))
	}
	return child, nil
}

// migrateOwnedDirIdentityDrop removes this run's temporary directory, addressed by the identity read when
// the name was created. A name already gone is the outcome wanted, and so is a name whose entry is no
// longer that directory - something else took the name, so it is not this run's to remove, and
// OlderTemps reports it like any other leftover. Anything else is reported, because a temporary left
// behind is a directory a later run would have to recognise and never does.
func (d *Dir) migrateOwnedDirIdentityDrop(tmp string, want fileID) error {
	if tmp == "" {
		return nil
	}
	var st unix.Stat_t
	switch err := migrateOwnedDirIdentityLstat(d.fd(), tmp, &st); {
	case errors.Is(err, unix.ENOENT):
		return nil
	case err != nil:
		return &fs.PathError{Op: "lstat", Path: d.join(tmp), Err: err}
	case uint32(st.Mode)&unix.S_IFMT != unix.S_IFDIR:
		return nil
	case (fileID{uint64(st.Dev), uint64(st.Ino)}) != want:
		return nil
	}
	if err := unix.Unlinkat(d.fd(), tmp, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
		return &fs.PathError{Op: "rmdir", Path: d.join(tmp), Err: err}
	}
	return nil
}

// OpenRegular opens the file name for reading. A link, directory, FIFO, socket or device found there is refused without being
// opened; the opened descriptor is then judged by judgeOpened, so what is read is what was judged.
func (d *Dir) OpenRegular(name string) (*os.File, fs.FileInfo, error) {
	if err := checkName(name); err != nil {
		return nil, nil, err
	}
	path := d.join(name)
	typ, err := d.typeOf(name)
	switch {
	case err != nil:
		return nil, nil, err
	case typ == unix.S_IFLNK:
		return nil, nil, refuse(ReasonLink, path, "")
	case typ != unix.S_IFREG:
		return nil, nil, refuse(ReasonNotRegular, path, "")
	}
	fd, err := unix.Openat(d.fd(), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ELOOP) {
		return nil, nil, refuse(ReasonLink, path, "")
	} else if err != nil {
		return nil, nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err == nil {
		err = judgeOpened(path, info)
	}
	if err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	return f, info, nil
}

// judgeOpened refuses what a copy must not read: not a regular file, more than one hard link, a set-user-ID or set-group-ID bit.
func judgeOpened(path string, info fs.FileInfo) error {
	nlink := uint64(1)
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		nlink = uint64(st.Nlink)
	}
	switch mode := info.Mode(); {
	case !mode.IsRegular():
		return refuse(ReasonNotRegular, path, "")
	case nlink > 1:
		return refuse(ReasonHardLinked, path, "")
	case mode&(fs.ModeSetuid|fs.ModeSetgid) != 0:
		return refuse(ReasonSetID, path, "")
	}
	return nil
}

// Names lists the directory, sorted, through a fresh descriptor so the pinned one keeps its offset.
func (d *Dir) Names() ([]string, error) {
	fd, err := unix.Openat(d.fd(), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: d.path, Err: err}
	}
	f := os.NewFile(uintptr(fd), d.path)
	defer f.Close()
	names, err := f.Readdirnames(-1)
	slices.Sort(names)
	return names, err
}
