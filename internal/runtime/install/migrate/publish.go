package migrate

import (
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// A run's temporary is tempPrefix + run + "-" + sequence + tempSuffix, run being the 26 characters crypto/rand.Text returns: the name
// does not grow with the leaf, and OlderTemps tells another run's temporary from a user's file.
const (
	tempPrefix, tempSuffix = ".migrate-", ".tmp"
	runLen, runAlphabet    = 26, "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
)

func tempName(run string, seq int) string {
	return tempPrefix + run + "-" + strconv.Itoa(seq) + tempSuffix
}

// tempRun returns the run of a name tempName could have produced.
func tempRun(name string) (string, bool) {
	rest, ok := strings.CutPrefix(name, tempPrefix)
	if ok {
		rest, ok = strings.CutSuffix(rest, tempSuffix)
	}
	run, seq, cut := strings.Cut(rest, "-")
	ok = ok && cut && len(run) == runLen && strings.Trim(run, runAlphabet) == "" && seq != "" && strings.Trim(seq, "0123456789") == ""
	return run, ok
}

// Publisher publishes files into pinned directories and never replaces one; it is not safe for concurrent use. rename and at are test
// seams: rename is the platform's no-replace rename, and at is called before a named step ("root", "rename", "dirsync") and fails
// it by returning an error.
type Publisher struct {
	run    string
	seq    int
	rename func(dirfd int, oldName, newName string) error
	at     func(step string) error
}

// NewPublisher starts a run. A platform without a no-replace rename is refused here, so nothing is written there.
func NewPublisher() (*Publisher, error) {
	if !noReplaceSupported {
		return nil, refuse(ReasonUnsupported, "", "this platform has no no-replace rename")
	}
	return &Publisher{run: rand.Text(), rename: noReplaceRename}, nil
}

func (p *Publisher) step(name string) error {
	if p.at == nil {
		return nil
	}
	return p.at(name)
}

// Publish writes size bytes of src as dir/leaf with the permission bits mode, or leaves what is there. The bytes go to an exclusive
// temporary of this run in dir, are written, given their mode and synced, then renamed to leaf with a no-replace rename addressed by
// (dir, leaf), and dir is synced. A leaf holding the same bytes is skipped (ResultAlreadyEqual; its mode and times stay, and dir is
// synced, which completes the durability of a rename an earlier run was interrupted after), a different one is ReasonDiffers, and a
// link, directory, hard-linked or set-ID leaf is refused. A rename the platform cannot do is ReasonUnsupported: nothing falls back to
// replacing or to writing in place. An error after the rename (ResultFailed) leaves the whole final file, and a rerun finds it equal.
// Only permission bits can be published: a set-ID source is refused by the preflight before it gets here. A temporary of an older
// run is never touched, and none of this run's is unlinked once its rename has happened.
func (p *Publisher) Publish(dir *Dir, leaf string, src io.ReaderAt, size int64, mode fs.FileMode) (Result, error) {
	res, err := p.publish(dir, leaf, src, size, mode)
	switch {
	case err == nil:
		return res, nil
	case refusal(err) != nil:
		return ResultRefused, err
	}
	return ResultFailed, err
}

// refusal returns err as a refusal, or nil when it is anything else.
func refusal(err error) *RefusedError {
	var r *RefusedError
	if errors.As(err, &r) {
		return r
	}
	return nil
}

func (p *Publisher) publish(dir *Dir, leaf string, src io.ReaderAt, size int64, mode fs.FileMode) (_ Result, err error) {
	if err = checkName(leaf); err != nil {
		return "", err
	}
	if mode&^fs.ModePerm != 0 {
		return "", errors.New("only permission bits can be published, not " + mode.String())
	}
	if res, err := p.settle(dir, leaf, src, size); res != "" || err != nil {
		return res, err
	}
	p.seq++
	tmp := tempName(p.run, p.seq)
	fd, err := unix.Openat(dir.fd(), tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return "", &fs.PathError{Op: "create", Path: dir.join(tmp), Err: err}
	}
	renamed := false
	defer func() {
		if renamed {
			return
		}
		if rm := unix.Unlinkat(dir.fd(), tmp, 0); rm != nil && !errors.Is(rm, unix.ENOENT) {
			err = errors.Join(err, rm)
		}
	}()
	if err = fillTemp(os.NewFile(uintptr(fd), dir.join(tmp)), src, size, mode); err != nil {
		return "", err
	}
	if err = p.step("rename"); err != nil {
		return "", err
	}
	switch err = p.rename(dir.fd(), tmp, leaf); {
	case err == nil:
		renamed = true
	case errors.Is(err, unix.EEXIST):
		// A racer made leaf after settle looked: our temporary is dropped and the racer's file is judged as before.
		res, serr := p.settle(dir, leaf, src, size)
		if res == "" && serr == nil {
			serr = &fs.PathError{Op: "rename", Path: dir.join(leaf), Err: err} // it vanished again; no retry
		}
		return res, serr
	case errors.Is(err, errors.ErrUnsupported) || errors.Is(err, unix.EINVAL):
		return "", refuse(ReasonUnsupported, dir.join(leaf), "no-replace rename: "+err.Error())
	default:
		return "", &fs.PathError{Op: "rename", Path: dir.join(leaf), Err: err}
	}
	if err = p.step("dirsync"); err == nil {
		err = dir.Sync()
	}
	if err != nil {
		return "", err
	}
	return ResultCopied, nil
}

// fillTemp writes exactly size bytes, gives the file its final permission bits (it was created 0600, so the content was never
// readable more widely than the final file allows), syncs, and closes.
func fillTemp(f *os.File, src io.ReaderAt, size int64, mode fs.FileMode) error {
	n, err := io.Copy(f, io.NewSectionReader(src, 0, size))
	if err == nil && n != size {
		err = io.ErrUnexpectedEOF
	}
	if err == nil {
		err = f.Chmod(mode)
	}
	if err == nil {
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
}

// settle judges what is at leaf already: "" when nothing is, ResultAlreadyEqual for the same bytes, else a refusal.
func (p *Publisher) settle(dir *Dir, leaf string, src io.ReaderAt, size int64) (Result, error) {
	f, info, err := dir.OpenRegular(leaf)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	defer f.Close()
	same := info.Size() == size
	if same {
		a, aerr := sum(f)
		b, berr := sum(io.NewSectionReader(src, 0, size))
		if same, err = a == b, errors.Join(aerr, berr); err != nil {
			return "", err
		}
	}
	if !same {
		return "", refuse(ReasonDiffers, dir.join(leaf), "")
	}
	return ResultAlreadyEqual, dir.Sync()
}

func sum(r io.Reader) ([sha256.Size]byte, error) {
	h := sha256.New()
	_, err := io.Copy(h, r)
	return [sha256.Size]byte(h.Sum(nil)), err
}

// EnsureProjectRoot returns the pinned W/.crw, created when absent, once its .gitignore holds crwdir.GitignoreText. The .gitignore
// is published whenever it is absent, also in a root that already exists, so a run interrupted between the mkdir and the
// publication is repaired by the next one before any state is copied (crwdir.EnsureDir stops at an existing root). An existing
// regular .gitignore, one a racer made included, belongs to its owner and is kept; a link, directory, hard-linked or set-ID one is
// refused, and one that cannot be read fails.
func (p *Publisher) EnsureProjectRoot(pair *Pair) (*Dir, error) {
	root, err := pair.EnsureDest(0o777)
	if err == nil {
		err = p.step("root")
	}
	if err != nil {
		return nil, err
	}
	text := crwdir.GitignoreText
	_, err = p.publish(root, ".gitignore", strings.NewReader(text), int64(len(text)), 0o644)
	if r := refusal(err); r != nil && r.Reason == ReasonDiffers {
		err = nil
	}
	if err != nil {
		return nil, err
	}
	return root, nil
}

// OlderTemps lists the temporaries other runs left in dir. It only reports: nothing adopts, renames or removes them.
func (p *Publisher) OlderTemps(dir *Dir) ([]string, error) {
	names, err := dir.Names()
	return slices.DeleteFunc(names, func(n string) bool { run, ok := tempRun(n); return !ok || run == p.run }), err
}
