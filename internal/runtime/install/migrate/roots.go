package migrate

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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
	parent               *Dir // holds Dest, where EnsureDest creates it
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
// call's own mkdir created it. A creation that ended in EEXIST is not this run's, so made is false then and the caller must not
// give the directory a mode. The directory that holds it is synced either way, so a run interrupted between the mkdir and its
// sync finishes the entry on the next one.
func (p *Pair) EnsureDest(perm uint32) (*Dir, bool, error) {
	if p.Dest != nil {
		return p.Dest, false, p.parent.Sync()
	}
	d, made, err := p.parent.EnsureChild(filepath.Base(p.DestPath), perm)
	if err != nil {
		return nil, false, err
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

// EnsureChild creates the subdirectory name with perm (under the umask) unless it exists, pins it, and syncs this directory so
// the new entry survives a crash, and reports whether its own mkdir created the directory. A creation that ended in EEXIST is
// another actor's directory, so made is false then. The sync also runs when the entry already existed.
func (d *Dir) EnsureChild(name string, perm uint32) (*Dir, bool, error) {
	if err := checkName(name); err != nil {
		return nil, false, err
	}
	made := true
	if err := migrateOwnedDirMkdirat(d.fd(), name, perm); err != nil {
		if !errors.Is(err, unix.EEXIST) {
			return nil, false, &fs.PathError{Op: "mkdir", Path: d.join(name), Err: err}
		}
		made = false
	}
	child, err := d.Child(name)
	if err == nil {
		if err = d.Sync(); err != nil {
			_ = child.Close()
		}
	}
	if err != nil {
		return nil, false, err
	}
	return child, made, nil
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
