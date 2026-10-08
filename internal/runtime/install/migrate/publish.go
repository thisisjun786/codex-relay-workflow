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

// tempRun returns the run of a name tempName could have produced: the sequence is a plain positive integer, as strconv.Itoa writes it.
func tempRun(name string) (string, bool) {
	rest, ok := strings.CutPrefix(name, tempPrefix)
	if ok {
		rest, ok = strings.CutSuffix(rest, tempSuffix)
	}
	run, seq, cut := strings.Cut(rest, "-")
	n, err := strconv.Atoi(seq)
	ok = ok && cut && len(run) == runLen && strings.Trim(run, runAlphabet) == "" && err == nil && n > 0 && strconv.Itoa(n) == seq
	return run, ok
}

// Publisher publishes files into pinned directories and never replaces one; it is not safe for concurrent use. rename and at are test
// seams: rename is the platform's no-replace rename, and at is called before a named step ("root", "rename", "dirsync") and fails
// it by returning an error. ensureDest is the seam for the destination root's creation, so a test can fail the step EnsureChild runs
// between the mkdir and EnsureProjectRoot's mode chmod; it reports whether its own mkdir created the root, like Pair.EnsureDest.
// createTemp is the seam for this run's temporary creation, so a test can fail it (or put a competing writer's file in the window it
// opens) without a real filesystem error.
type Publisher struct {
	run        string
	seq        int
	rename     func(dirfd int, oldName, newName string) error
	at         func(step string) error
	ensureDest func(pair *Pair, perm uint32) (*Dir, bool, error)
	createTemp func(dir *Dir, name string) (int, error)
}

// NewPublisher starts a run. A platform without a no-replace rename is refused here, so nothing is written there.
func NewPublisher() (*Publisher, error) {
	if !noReplaceSupported {
		return nil, refuse(ReasonUnsupported, "", "this platform has no no-replace rename")
	}
	return &Publisher{
		run:        rand.Text(),
		rename:     noReplaceRename,
		ensureDest: func(pair *Pair, perm uint32) (*Dir, bool, error) { return pair.EnsureDest(perm) },
		createTemp: migrateReviewFollowupCreateTemp,
	}, nil
}

// migrateReviewFollowupCreateTemp creates this run's exclusive temporary in dir: the default createTemp seam. O_EXCL and
// O_NOFOLLOW mean a name something else already holds is never opened or written through.
func migrateReviewFollowupCreateTemp(dir *Dir, name string) (int, error) {
	return unix.Openat(dir.fd(), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
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
// run is never touched, and none of this run's is unlinked once its rename has happened. The temporary is addressed by name until
// the rename, so a process of the same user that swaps it in that window is a residual risk, as it is for Dir.OpenRegular.
func (p *Publisher) Publish(dir *Dir, leaf string, src io.ReaderAt, size int64, mode fs.FileMode) (Result, error) {
	res, _, err := p.migrateReviewFollowupPublish(dir, leaf, src, size, mode)
	return res, err
}

// migrateReviewFollowupPublish is the publication Publish performs, and also reports whether this run's own no-replace
// rename completed. A failure after the rename (renamed true, ResultFailed) left a whole final file at the destination,
// so it is this run's write; a failure before any rename (renamed false) wrote nothing, whatever the destination holds,
// because a racer can publish the plan's bytes between settle's look and this run's own create (CRW-879). The Result
// contract Publish documents is unchanged: a failure is ResultFailed and a refusal is ResultRefused, whichever step it
// happened at.
func (p *Publisher) migrateReviewFollowupPublish(dir *Dir, leaf string, src io.ReaderAt, size int64, mode fs.FileMode) (Result, bool, error) {
	res, renamed, err := p.publish(dir, leaf, src, size, mode)
	switch {
	case err == nil:
		return res, renamed, nil
	case refusal(err) != nil:
		return ResultRefused, false, err
	case res == ResultAlreadyEqual:
		// The destination already held these bytes, so this run wrote nothing: the failure is the directory sync's alone and
		// the result says so, so a caller never counts it as a write of its own.
		return res, renamed, err
	}
	return ResultFailed, renamed, err
}

// refusal returns err as a refusal, or nil when it is anything else, a refusal that came with another error included: a temporary
// that could not be removed is a failure to report, never a plain conflict and never a reason to let a run go on.
func refusal(err error) *RefusedError {
	r, _ := err.(*RefusedError)
	return r
}

// publish performs the steps and reports whether this run's own no-replace rename completed, so a caller can tell a
// failure after the rename (the whole final file is there) from one before it (nothing of this run's was renamed).
func (p *Publisher) publish(dir *Dir, leaf string, src io.ReaderAt, size int64, mode fs.FileMode) (_ Result, renamed bool, err error) {
	if err = checkName(leaf); err != nil {
		return "", false, err
	}
	if mode&^fs.ModePerm != 0 {
		return "", false, errors.New("only permission bits can be published, not " + mode.String())
	}
	if res, err := p.settle(dir, leaf, src, size); res != "" || err != nil {
		return res, false, err
	}
	p.seq++
	tmp := tempName(p.run, p.seq)
	fd, err := p.createTemp(dir, tmp)
	if err != nil {
		return "", false, &fs.PathError{Op: "create", Path: dir.join(tmp), Err: err}
	}
	defer func() {
		if renamed {
			return
		}
		if rm := unix.Unlinkat(dir.fd(), tmp, 0); rm != nil && !errors.Is(rm, unix.ENOENT) {
			err = errors.Join(err, rm)
		}
	}()
	if err = fillTemp(os.NewFile(uintptr(fd), dir.join(tmp)), src, size, mode); err != nil {
		return "", false, err
	}
	if err = p.step("rename"); err != nil {
		return "", false, err
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
		return res, false, serr
	case errors.Is(err, errors.ErrUnsupported) || errors.Is(err, unix.EINVAL):
		return "", false, refuse(ReasonUnsupported, dir.join(leaf), "no-replace rename: "+err.Error())
	default:
		return "", false, &fs.PathError{Op: "rename", Path: dir.join(leaf), Err: err}
	}
	if err = p.step("dirsync"); err == nil {
		err = dir.Sync()
	}
	if err != nil {
		return "", renamed, err
	}
	return ResultCopied, renamed, nil
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
	if err := p.step("dirsync"); err != nil {
		return ResultAlreadyEqual, err
	}
	return ResultAlreadyEqual, dir.Sync()
}

func sum(r io.Reader) ([sha256.Size]byte, error) {
	h := sha256.New()
	_, err := io.Copy(h, r)
	return [sha256.Size]byte(h.Sum(nil)), err
}

// EnsureProjectRoot returns the pinned W/.crw, created when absent at the private mode 0700 (under the umask), and whether this
// call's own mkdir created it, once its .gitignore is there: published as crwdir.GitignoreText whenever it is absent, also in a
// root that already exists, so a run interrupted between the mkdir and the publication is repaired by the next one before any
// state is copied (crwdir.EnsureDir stops at an existing root). A regular .gitignore that was already there when the call began
// belongs to its owner and is kept as it is, so a differing one is published past and the call succeeds; when it was absent and a
// racer makes a differing one before the rename, that ReasonDiffers refusal is returned so the caller stops before copying state
// (state-migration.md Preflight 3-4). A link, directory, hard-linked or set-ID one is refused, and one that cannot be read fails.
func (p *Publisher) EnsureProjectRoot(pair *Pair) (*Dir, bool, error) {
	// Was .gitignore already there when this call started? Capture that before EnsureDest can create the root, so a .gitignore
	// a racer makes in the window that creation opens (or in the root step) is a conflicting initialization race, not a
	// retained owner file: the root did not exist before the call, so no .gitignore could have.
	pinned := pair.Dest
	pre := false
	if pinned != nil {
		present, err := migrateFollowupGitignorePresent(pinned)
		if err != nil {
			return nil, false, err
		}
		pre = present
	}
	// The root's mkdir itself is private (CRW-878), so no failure between it and the chmod below can leave a root another
	// user can read through, whatever a retry then makes of the root it finds. Only a root this call made may be tightened.
	root, made, err := p.ensureDest(pair, 0o700)
	if err == nil && pinned != nil && root != pinned {
		// The pinned directory was moved aside and the name holds another one, which this call publishes into. Whether its
		// .gitignore was there when the call began is answered for that directory.
		pre, err = migrateFollowupGitignorePresent(root)
	}
	if err == nil && made {
		// Give the root the private marker mode here, before the fallible .gitignore publication, so a failure below cannot
		// leave a widened root that a retry would then find as an existing one.
		err = applyChmodRaw(root, applyTempRaw)
	}
	if err == nil {
		err = p.step("root")
	}
	if err != nil {
		return nil, false, err
	}
	text := crwdir.GitignoreText
	_, _, err = p.publish(root, ".gitignore", strings.NewReader(text), int64(len(text)), 0o644)
	if r := refusal(err); r != nil && r.Reason == ReasonDiffers && pre {
		err = nil
	}
	if err != nil {
		return nil, false, err
	}
	return root, made, nil
}

// migrateFollowupGitignorePresent reports whether dir holds a .gitignore entry of any type.
func migrateFollowupGitignorePresent(dir *Dir) (bool, error) {
	_, err := dir.typeOf(".gitignore")
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	}
	return false, err
}

// OlderTemps lists the temporaries other runs left in dir. It only reports: nothing adopts, renames or removes them.
func (p *Publisher) OlderTemps(dir *Dir) ([]string, error) {
	names, err := dir.Names()
	return slices.DeleteFunc(names, func(n string) bool { run, ok := tempRun(n); return !ok || run == p.run }), err
}
