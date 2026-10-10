package integrator

import (
	"crypto/sha1" //nolint:gosec // names a retry marker, as the shell runner's sha1sum did
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The queue's directories and files below Config.QueueDir.
const (
	dirPending  = "pending"
	dirDone     = "done"
	dirConflict = "conflict"
	dirBlocked  = "blocked"
	dirRetried  = "retried"
	fileStop    = "stop"
	fileLog     = "runner.log"
	fileSolo    = "solo-heads"
)

var (
	headRE      = regexp.MustCompile(`^[0-9a-f]{40}$`)
	queueNameRE = regexp.MustCompile(`^[0-9]{1,18}$`)
)

// Entry is one pending file: the issue number it is named after and the head it holds.
type Entry struct {
	Name   string `json:"name"`
	Key    string `json:"key"`
	Node   string `json:"node"`
	Head   string `json:"head"`
	HeadOK bool   `json:"head_ok"`
	num    uint64
}

// Spec is the "node=head" argument of the integration run.
func (e Entry) Spec() string { return e.Node + "=" + e.Head }

func (r *Runner) qpath(parts ...string) string {
	return filepath.Join(append([]string{r.Cfg.QueueDir}, parts...)...)
}

// ensureQueueDirs makes the queue's directories, as the runner's mkdir -p did.
func (r *Runner) ensureQueueDirs() error {
	for _, d := range []string{dirPending, dirDone, dirConflict, dirBlocked} {
		if err := os.MkdirAll(r.qpath(d), 0o775); err != nil {
			return err
		}
	}
	return nil
}

// readHead is the head a pending file holds: its content without spaces and newlines.
func readHead(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if len(data) > 4096 {
		return "", errors.New("pending file is too large")
	}
	return strings.NewReplacer(" ", "", "\n", "", "\r", "", "\t", "").Replace(string(data)), nil
}

// listPending is the pending entries in issue-number order and the names it did not take (a
// directory, a link, or a name that is not an issue number).
func (r *Runner) listPending() (entries []Entry, ignored []string, err error) {
	des, err := os.ReadDir(r.qpath(dirPending))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	for _, de := range des {
		name := de.Name()
		num, perr := strconv.ParseUint(name, 10, 64)
		if !queueNameRE.MatchString(name) || perr != nil || !de.Type().IsRegular() {
			ignored = append(ignored, name)
			continue
		}
		head, rerr := readHead(r.qpath(dirPending, name))
		if rerr != nil {
			ignored = append(ignored, name)
			continue
		}
		entries = append(entries, Entry{Name: name, Key: r.Cfg.key(name), Node: r.Cfg.node(name), Head: head, HeadOK: headRE.MatchString(head), num: num})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].num != entries[j].num {
			return entries[i].num < entries[j].num
		}
		return entries[i].Name < entries[j].Name
	})
	sort.Strings(ignored)
	return entries, ignored, nil
}

// moveEntry moves a pending file to one of the other queue directories, only while it still
// holds the head the run worked on: a coordinator that refreshed the head meanwhile keeps its
// new file in pending. It reports whether the file moved.
//
// The head is read before the move and again after it, from the file that was moved: a read
// and a rename of the same path are two steps, and a refresh that lands between them would
// otherwise carry the new, unprocessed head out of pending. The file first goes to a private
// name beside its destination; if it holds another head than the run worked on it is put back
// as pending/<name> without replacing a file the coordinator wrote since (that newer file
// wins), and only a file that held the processed head replaces the destination. A file that can
// be neither moved on nor put back stays under its private name (restoreClaim), and the next
// pass puts it back first (recoverClaims).
func (r *Runner) moveEntry(e Entry, to string) bool {
	src := r.qpath(dirPending, e.Name)
	if now, err := readHead(src); err != nil || now != e.Head {
		r.logf("%s %s changed or went away during the run: left in pending", e.Key, e.Head)
		return false
	}
	if r.hookMove != nil {
		r.hookMove(src)
	}
	if err := os.MkdirAll(r.qpath(to), 0o775); err != nil {
		r.logf("%s: %s: %v", e.Key, to, err)
		return false
	}
	claim := r.qpath(to, claimPrefix+e.Name+"-"+strconv.Itoa(os.Getpid()))
	if err := os.Rename(src, claim); err != nil {
		r.logf("%s: move to %s: %v", e.Key, to, err)
		return false
	}
	if moved, err := readHead(claim); err != nil || moved != e.Head {
		// The refresh slipped in between the check and the move: give it back.
		r.logf("%s %s changed or went away during the run: left in pending", e.Key, e.Head)
		r.restoreClaim(claim, e.Name)
		return false
	}
	if err := os.Rename(claim, r.qpath(to, e.Name)); err != nil {
		r.logf("%s: move to %s: %v", e.Key, to, err)
		r.restoreClaim(claim, e.Name)
		return false
	}
	return true
}

// claimPrefix starts the private name a pending file has while moveEntry moves it; the name
// goes on with the queue name and the mover's pid.
const claimPrefix = ".moving-"

var claimNameRE = regexp.MustCompile(`^\.moving-([0-9]{1,18})-[0-9]+$`)

// claimDirs are the directories moveEntry moves into, where a claim can be left.
var claimDirs = []string{dirDone, dirConflict, dirBlocked}

func (r *Runner) linkFile(oldname, newname string) error {
	if r.hookLink != nil {
		return r.hookLink(oldname, newname)
	}
	return os.Link(oldname, newname)
}

// restoreClaim puts a claimed file back as pending/<name> without replacing a file there, and
// removes the claim only once pending/ holds a file for the name: the one put back, or a
// regular file the coordinator wrote after the claim was taken (that newer file wins). When the
// file cannot be put back the claim is kept, so the queue entry is never lost, and the next
// pass tries again. It reports whether the claim is gone.
func (r *Runner) restoreClaim(claim, name string) bool {
	src := r.qpath(dirPending, name)
	err := r.linkFile(claim, src)
	switch {
	case err == nil:
	case errors.Is(err, fs.ErrExist) && isRegularHead(src):
		r.logf("%s: %s not put back, a newer file in pending wins", r.Cfg.key(name), filepath.Base(claim))
	default:
		r.logf("%s: could not be put back into pending (%v): kept as %s, the next pass puts it back", r.Cfg.key(name), err, claim)
		return false
	}
	if err := os.Remove(claim); err != nil && !errors.Is(err, fs.ErrNotExist) {
		r.logf("%s: remove %s: %v", r.Cfg.key(name), claim, err)
		return false
	}
	return true
}

// isRegularHead says path is a regular file a pass can read as a pending head.
func isRegularHead(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	_, err = readHead(path)
	return err == nil
}

// listClaims is the claims moveEntry left behind (a put-back that failed, or a run that ended
// mid-move), as paths with the queue name each belongs to.
func (r *Runner) listClaims() (claims []claimFile) {
	for _, d := range claimDirs {
		des, err := os.ReadDir(r.qpath(d))
		if err != nil {
			continue
		}
		for _, de := range des {
			if m := claimNameRE.FindStringSubmatch(de.Name()); m != nil && de.Type().IsRegular() {
				claims = append(claims, claimFile{Path: r.qpath(d, de.Name()), Name: m[1]})
			}
		}
	}
	return claims
}

type claimFile struct {
	Path string
	Name string
}

// recoverClaims puts every claim left behind back into pending (restoreClaim). It runs under
// the queue lock, so no other run is moving a file meanwhile.
func (r *Runner) recoverClaims() {
	for _, c := range r.listClaims() {
		if r.restoreClaim(c.Path, c.Name) {
			r.logf("%s: %s put back into pending", r.Cfg.key(c.Name), c.Path)
		}
	}
}

// batchKey names a batch for its retry marker: the first 16 hex characters of the sha1 of the
// specs, one per line (the shell runner's printf '%s\n' | sha1sum | cut -c1-16).
func batchKey(entries []Entry) string {
	h := sha1.New() //nolint:gosec // a marker name, not a security property
	for _, e := range entries {
		_, _ = h.Write([]byte(e.Spec() + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func (r *Runner) batchMarker(entries []Entry) string {
	return r.qpath(dirRetried, "batch-"+batchKey(entries))
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// touch makes an empty marker file.
func (r *Runner) touch(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o775); err != nil {
		return err
	}
	return os.WriteFile(path, nil, 0o664)
}

// soloOwed is the specs (node=head) of the failed batch's members that have not yet had a run of
// their own. Solo mode lasts while any of them is still pending with that head.
func (r *Runner) soloOwed() []string {
	data, err := os.ReadFile(r.qpath(fileSolo))
	if err != nil {
		return nil
	}
	var owed []string
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			owed = append(owed, line)
		}
	}
	return owed
}

func (r *Runner) setSoloOwed(specs []string) {
	path := r.qpath(fileSolo)
	if len(specs) == 0 {
		_ = os.Remove(path)
		return
	}
	if err := os.WriteFile(path, []byte(strings.Join(specs, "\n")+"\n"), 0o664); err != nil {
		r.logf("solo state: %v", err)
	}
}

// soloPrune drops the owed heads that are no longer pending with the head they were owed for (a
// run moved them, or the owner refreshed or removed the file), writes the state back when it
// changed, and returns what is still owed. entries is the current pending list.
func (r *Runner) soloPrune(entries []Entry) []string {
	owed := r.soloOwed()
	pending := make(map[string]bool, len(entries))
	for _, e := range entries {
		pending[e.Spec()] = true
	}
	var kept []string
	for _, spec := range owed {
		if pending[spec] {
			kept = append(kept, spec)
		}
	}
	if len(kept) != len(owed) {
		r.setSoloOwed(kept)
		if len(kept) == 0 {
			r.logf("solo pass over: batching again")
		}
	}
	return kept
}
